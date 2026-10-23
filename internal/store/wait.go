package store

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The waiting stage (pipeline/wait.go, docs/decisions/2026-10-22-waiting-stage.md): the user, the
// orchestrator or a build or review agent parks an item on items and PRs; loopd releases it to the
// stage it left once they are all done (releaseWaiting, after every transition and every tick); a
// runner reports the PRs it sees merged or closed (WaitPR).

// Wait parks an item on items (on) and PRs (prs: owner/name#444, or #444 for its repo), as
// `loop wait` and Slack's wait do.
func (s *Store) Wait(ctx context.Context, actor string, id int64, on []int64, prs []string, reason string) (pipeline.Item, error) {
	return s.humanTx(ctx, actor, id, "wait", func(tx *sql.Tx, it pipeline.Item) (pipeline.Transition, error) {
		if err := notLeased(tx, it); err != nil {
			return pipeline.Transition{}, err
		}
		return s.park(tx, it, on, prs, reason)
	}, nil)
}

// OrchWait is the orchestrator's wait: an item blocked on other work waits for it instead of
// being escalated.
func (s *Store) OrchWait(ctx context.Context, actor string, id int64, on []int64, prs []string, reason string) (pipeline.Item, error) {
	return s.orch(ctx, actor, id, "wait", reason, func(tx *sql.Tx, it pipeline.Item, _ []Event) (pipeline.Transition, error) {
		if err := notLeased(tx, it); err != nil {
			return pipeline.Transition{}, err
		}
		return s.park(tx, it, on, prs, reason)
	})
}

// Unwait ends an item's wait by hand (pipeline.ApplyUnwait).
func (s *Store) Unwait(ctx context.Context, actor string, id int64) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "unwait", pipeline.ApplyUnwait)
}

// WaitPR records a runner seeing a PR a waiting item waits on merged or closed
// (pipeline.ApplyWaitPR); a merge that was the last thing it waited on releases it.
func (s *Store) WaitPR(ctx context.Context, actor string, id int64, ref, state string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "wait_pr", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyWaitPR(it, ref, state)
	})
}

// notLeased refuses to park an item a run holds: its run ends first (an agent parks its own item
// with the wait outcome).
func notLeased(tx *sql.Tx, it pipeline.Item) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM leases WHERE item_id = ? AND ended_at IS NULL`, it.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errf(pipeline.CodeWrongStage, "a run holds #%d in %s; park it once the run ends", it.ID, it.Stage)
	}
	return nil
}

// park adds the items to wait on to it's dependencies (refusing a missing or closed one and a
// cycle, as link does), and parks it (pipeline.ApplyWait). The item is saved by linkAfter; the
// caller saves the transition's.
func (s *Store) park(tx *sql.Tx, it pipeline.Item, on []int64, prs []string, reason string) (pipeline.Transition, error) {
	if _, ok := pipeline.Waitable(it); !ok {
		return pipeline.ApplyWait(it, on, prs, nil, reason, s.clock.Now())
	}
	on = dedupeIDs(on)
	refs := []string{}
	for _, p := range prs {
		ref, err := pipeline.ParseWaitPR(p, it.Repo)
		if err != nil {
			return pipeline.Transition{}, err
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	for _, d := range on {
		if slices.Contains(it.DependsOn, d) {
			continue // already linked: it waits on it all the same
		}
		var err error
		if it, err = linkAfter(tx, it, d); err != nil {
			return pipeline.Transition{}, err
		}
	}
	stages, err := dependencyStages(tx, []pipeline.Item{it})
	if err != nil {
		return pipeline.Transition{}, err
	}
	return pipeline.ApplyWait(it, on, refs, stages, reason, s.clock.Now())
}

// agentWait is a build or review agent's wait outcome (fields on: "472,483", prs, reason): the item
// is parked as the user's wait would park it; a wait loopd refuses (a cycle, a closed item, nothing
// left to wait on) sends it to the user with the reason instead. Either way the run's lease ends
// (applyAdvance).
func (s *Store) agentWait(tx *sql.Tx, it pipeline.Item, l Lease, fields map[string]string) (pipeline.Transition, error) {
	data := map[string]any{}
	for k, v := range fields {
		data[k] = v
	}
	data["agent"] = l.Agent
	reason := strings.TrimSpace(fields["reason"])
	var on []int64
	for _, f := range strings.FieldsFunc(fields["on"], func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.ParseInt(strings.TrimPrefix(f, "#"), 10, 64)
		if err != nil || n <= 0 {
			return pipeline.Transition{}, errf(pipeline.CodeInvalidField, "on: %q is not an item id", f)
		}
		on = append(on, n)
	}
	prs := strings.FieldsFunc(fields["prs"], func(r rune) bool { return r == ',' || r == ' ' })
	if reason == "" {
		return pipeline.Transition{}, errf(pipeline.CodeMissingField, "reason is required: why it waits")
	}
	if _, err := tx.Exec(`SAVEPOINT agent_wait`); err != nil {
		return pipeline.Transition{}, err
	}
	tr, err := s.park(tx, it, on, prs, reason)
	if err == nil {
		_, err = tx.Exec(`RELEASE agent_wait`)
		if err != nil {
			return pipeline.Transition{}, err
		}
		for k, v := range tr.Data {
			data[k] = v
		}
		tr.Data, tr.Outcome = data, pipeline.Wait
		return tr, nil
	}
	if _, e := tx.Exec(`ROLLBACK TO agent_wait`); e != nil {
		return pipeline.Transition{}, e
	}
	if _, e := tx.Exec(`RELEASE agent_wait`); e != nil {
		return pipeline.Transition{}, e
	}
	msg := err.Error()
	if pe, ok := err.(*pipeline.Error); ok {
		msg = pe.Message
	}
	q := "the agent asked to wait (" + reason + "), but the wait was refused: " + msg
	data["refused"], data["question"] = msg, q
	from := it.Stage
	it.Stage, it.ReturnStage, it.Question = pipeline.NeedsYou, from, q
	return pipeline.Transition{Item: it, From: from, To: pipeline.NeedsYou, Outcome: pipeline.Wait, Kind: "advanced", Data: data}, nil
}

// releaseWaiting moves every waiting item whose items and PRs are all done back to the stage it
// left (pipeline.ApplyRelease), recorded by loopd with the note.
func (s *Store) releaseWaiting(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT ` + itemCols + ` FROM items WHERE stage = 'waiting' ORDER BY id`)
	if err != nil {
		return err
	}
	var waiting []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return err
		}
		waiting = append(waiting, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(waiting) == 0 {
		return err
	}
	stages, err := dependencyStages(tx, waiting)
	if err != nil {
		return err
	}
	for _, it := range waiting {
		tr, ok := pipeline.ApplyRelease(it, stages)
		if !ok {
			continue
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		if err := s.event(tx, transitionEvent(it.ID, "loopd", tr)); err != nil {
			return err
		}
	}
	return nil
}

// WaitRow is a waiting item as the standup and loop status show it.
type WaitRow struct {
	ID      int64     `json:"id"`
	Title   string    `json:"title"`
	WaitsOn string    `json:"waits_on"`
	Return  string    `json:"return_stage"`
	Since   time.Time `json:"since"`
	// PRs are the PRs not seen merged yet.
	PRs []string `json:"prs,omitempty"`
}

// waitsOn is what each waiting item of items waits on (pipeline.WaitsOn), by id.
func waitsOn(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, items []pipeline.Item) (map[int64]string, error) {
	stages, err := dependencyStages(q, items)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, it := range items {
		if it.Stage == pipeline.Waiting {
			out[it.ID] = pipeline.WaitsOn(it, stages)
		}
	}
	return out, nil
}

// Waiting lists the waiting items, longest waiting first.
func (s *Store) Waiting(ctx context.Context) ([]WaitRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE stage = 'waiting' ORDER BY waiting_since, id`)
	if err != nil {
		return nil, err
	}
	var items []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ws, err := waitsOn(s.r, items)
	if err != nil {
		return nil, err
	}
	out := []WaitRow{}
	for _, it := range items {
		out = append(out, WaitRow{ID: it.ID, Title: it.Title, WaitsOn: ws[it.ID], Return: string(it.ReturnStage), Since: it.WaitingSince,
			PRs: pipeline.OpenWaitPRs(it)})
	}
	return out, nil
}
