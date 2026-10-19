package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Plans (docs/phases/07-planning.md, "Plans"). The rules are pipeline's (plans.go); here are the
// tables: plans, plan_docs (append-only versions), plan_turns and their leases (plan_leases).

// ---- rows ----

const planCols = `id, title, stage, priority, repos, slack_channel, slack_ts, slack_header, product_session, engineering_session,
	brief_approved, blueprint_approved, created_at, updated_at`

func scanPlan(row scanner) (pipeline.Plan, error) {
	var (
		p                pipeline.Plan
		stage, repos     string
		created, updated int64
	)
	err := row.Scan(&p.ID, &p.Title, &stage, &p.Priority, &repos, &p.SlackChannel, &p.SlackTS, &p.SlackHeader, &p.ProductSession,
		&p.EngineeringSession, &p.BriefApproved, &p.BlueprintApproved, &created, &updated)
	p.Stage, p.Repos = pipeline.PlanStage(stage), splitList(repos)
	p.CreatedAt, p.UpdatedAt = fromNanos(created), fromNanos(updated)
	return p, err
}

func loadPlan(q interface {
	QueryRow(string, ...any) *sql.Row
}, id int64) (pipeline.Plan, error) {
	p, err := scanPlan(q.QueryRow(`SELECT `+planCols+` FROM plans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, errf(CodeNotFound, "no plan P%d", id)
	}
	return p, err
}

func savePlan(tx *sql.Tx, p pipeline.Plan) error {
	_, err := tx.Exec(`UPDATE plans SET title=?, stage=?, priority=?, repos=?, slack_channel=?, slack_ts=?, slack_header=?,
		product_session=?, engineering_session=?, brief_approved=?, blueprint_approved=?, updated_at=? WHERE id=?`,
		p.Title, string(p.Stage), p.Priority, strings.Join(p.Repos, ","), p.SlackChannel, p.SlackTS, p.SlackHeader, p.ProductSession,
		p.EngineeringSession, p.BriefApproved, p.BlueprintApproved, p.UpdatedAt.UnixNano(), p.ID)
	return err
}

const turnCols = `id, plan_id, persona, messages, slack_ts, back_to_brief, status, runner, agent, reply, ready, error, created_at, updated_at`

func scanTurn(row scanner) (pipeline.PlanTurn, error) {
	var (
		t                 pipeline.PlanTurn
		msgs, tss, status string
		created, updated  int64
	)
	if err := row.Scan(&t.ID, &t.PlanID, &t.Persona, &msgs, &tss, &t.BackToBrief, &status, &t.Runner, &t.Agent, &t.Reply, &t.Ready,
		&t.Error, &created, &updated); err != nil {
		return t, err
	}
	t.Status = pipeline.TurnStatus(status)
	t.CreatedAt, t.UpdatedAt = fromNanos(created), fromNanos(updated)
	if err := json.Unmarshal([]byte(msgs), &t.Messages); err != nil {
		return t, err
	}
	if t.Messages == nil {
		t.Messages = []string{}
	}
	return t, json.Unmarshal([]byte(tss), &t.SlackTS)
}

func loadTurn(q interface {
	QueryRow(string, ...any) *sql.Row
}, planID, turnID int64) (pipeline.PlanTurn, error) {
	t, err := scanTurn(q.QueryRow(`SELECT `+turnCols+` FROM plan_turns WHERE id = ? AND plan_id = ?`, turnID, planID))
	if errors.Is(err, sql.ErrNoRows) {
		return t, errf(CodeNotFound, "plan P%d has no turn %d", planID, turnID)
	}
	return t, err
}

// openTurn is the plan's turn with that status (queued or running), if any.
func openTurn(tx *sql.Tx, planID int64, status pipeline.TurnStatus) (*pipeline.PlanTurn, error) {
	t, err := scanTurn(tx.QueryRow(`SELECT `+turnCols+` FROM plan_turns WHERE plan_id = ? AND status = ?`, planID, string(status)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func saveTurn(tx *sql.Tx, t pipeline.PlanTurn) error {
	msgs, err := json.Marshal(t.Messages)
	if err != nil {
		return err
	}
	tss, err := json.Marshal(nonNil(t.SlackTS))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE plan_turns SET persona=?, messages=?, slack_ts=?, back_to_brief=?, status=?, runner=?, agent=?, reply=?,
		ready=?, error=?, updated_at=? WHERE id=?`,
		t.Persona, string(msgs), string(tss), t.BackToBrief, string(t.Status), t.Runner, t.Agent, t.Reply, t.Ready, t.Error, t.UpdatedAt.UnixNano(), t.ID)
	return err
}

// PlanLease is a running turn's lease.
type PlanLease struct {
	ID        string
	TurnID    int64
	PlanID    int64
	Runner    string
	Agent     string
	ClaimedAt time.Time
	ExpiresAt time.Time
	Note      string
	EndedAt   *time.Time
	Activity  *pipeline.Activity
}

const planLeaseCols = `id, turn_id, plan_id, runner, agent, claimed_at, expires_at, note, ended_at, activity`

func scanPlanLease(row scanner) (PlanLease, error) {
	var (
		l                PlanLease
		claimed, expires int64
		ended            sql.NullInt64
		activity         string
	)
	if err := row.Scan(&l.ID, &l.TurnID, &l.PlanID, &l.Runner, &l.Agent, &claimed, &expires, &l.Note, &ended, &activity); err != nil {
		return l, err
	}
	l.Activity = parseActivity(activity)
	l.ClaimedAt, l.ExpiresAt = fromNanos(claimed), fromNanos(expires)
	if ended.Valid {
		t := fromNanos(ended.Int64)
		l.EndedAt = &t
	}
	return l, nil
}

// lease is the plan lease as the claim's lease: stage "plan", no item.
func (l PlanLease) lease() Lease {
	return Lease{ID: l.ID, Stage: pipeline.PlanTurnStage, Runner: l.Runner, Agent: l.Agent, Resources: []string{},
		ClaimedAt: l.ClaimedAt, ExpiresAt: l.ExpiresAt, Note: l.Note, EndedAt: l.EndedAt, Activity: l.Activity}
}

func livePlanLeases(tx *sql.Tx) ([]PlanLease, error) {
	rows, err := tx.Query(`SELECT ` + planLeaseCols + ` FROM plan_leases WHERE ended_at IS NULL ORDER BY claimed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanLease
	for rows.Next() {
		l, err := scanPlanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func endPlanLease(tx *sql.Tx, id string, at time.Time, reason string) error {
	_, err := tx.Exec(`UPDATE plan_leases SET ended_at = ?, end_reason = ?, activity = '' WHERE id = ? AND ended_at IS NULL`, at.UnixNano(), reason, id)
	return err
}

func (s *Store) planEvent(tx *sql.Tx, planID int64, actor, kind string, from, to pipeline.PlanStage, data map[string]any) error {
	return s.event(tx, Event{PlanID: planID, Actor: actor, Kind: kind, From: string(from), To: string(to), Data: data})
}

// refusePlan records a refused action on a plan, then returns err (as refuse does for items).
func (s *Store) refusePlan(ctx context.Context, actor string, id int64, action string, err error) error {
	var pe *pipeline.Error
	if !errors.As(err, &pe) || pe.Code == CodeNotFound {
		return err
	}
	_ = s.tx(ctx, func(tx *sql.Tx) error {
		return s.planEvent(tx, id, actor, "refused", "", "", map[string]any{"action": action, "code": pe.Code, "message": pe.Message})
	})
	return err
}

// ---- the conversation ----

// NewPlan is a request to start a plan.
type NewPlan struct {
	Title    string   `json:"title"`
	Priority string   `json:"priority"`
	Repos    []string `json:"repos"`
	// Text is the user's first message, the first product turn's prompt (default: the title).
	Text         string `json:"text"`
	SlackChannel string `json:"slack_channel"`
	SlackTS      string `json:"slack_ts"`
}

// CreatePlan starts a plan at brief and queues its first product turn.
func (s *Store) CreatePlan(ctx context.Context, actor string, n NewPlan) (pipeline.Plan, error) {
	now := s.clock.Now()
	p, err := pipeline.NewPlan(n.Title, n.Priority, n.Repos, s.Repos(), now)
	if err != nil {
		return p, err
	}
	p.SlackChannel, p.SlackTS = strings.TrimSpace(n.SlackChannel), strings.TrimSpace(n.SlackTS)
	text := strings.TrimSpace(n.Text)
	if text == "" {
		text = p.Title
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO plans (title, stage, priority, repos, slack_channel, slack_ts, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			p.Title, string(p.Stage), p.Priority, strings.Join(p.Repos, ","), p.SlackChannel, p.SlackTS, now.UnixNano(), now.UnixNano())
		if err != nil {
			return err
		}
		if p.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := s.planEvent(tx, p.ID, actor, "plan_created", "", p.Stage,
			map[string]any{"title": p.Title, "priority": p.Priority, "repos": p.Repos}); err != nil {
			return err
		}
		_, err = s.queueTurn(tx, actor, p.ID, text, p.SlackTS, "")
		return err
	})
	return p, err
}

// queueTurn adds a message (with its Slack ts, if it came from the plan's thread) and a
// back-to-brief reason to the plan's queued turn, creating one when there is none. Any may be
// empty: approving the brief queues engineering's first turn with no message.
func (s *Store) queueTurn(tx *sql.Tx, actor string, planID int64, text, ts, backToBrief string) (pipeline.PlanTurn, error) {
	now := s.clock.Now()
	q, err := openTurn(tx, planID, pipeline.TurnQueued)
	if err != nil {
		return pipeline.PlanTurn{}, err
	}
	t := pipeline.PlanTurn{PlanID: planID, Messages: []string{}, Status: pipeline.TurnQueued, CreatedAt: now}
	if q != nil {
		t = *q
	}
	if text != "" {
		t.Messages = append(t.Messages, text)
		if ts != "" {
			t.SlackTS = append(t.SlackTS, ts)
		}
	}
	if backToBrief != "" {
		t.BackToBrief = backToBrief
	}
	t.UpdatedAt = now
	if q != nil {
		if err := saveTurn(tx, t); err != nil {
			return t, err
		}
	} else {
		msgs, _ := json.Marshal(t.Messages)
		tss, _ := json.Marshal(nonNil(t.SlackTS))
		res, err := tx.Exec(`INSERT INTO plan_turns (plan_id, messages, slack_ts, back_to_brief, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			planID, string(msgs), string(tss), t.BackToBrief, string(t.Status), now.UnixNano(), now.UnixNano())
		if err != nil {
			return t, err
		}
		if t.ID, err = res.LastInsertId(); err != nil {
			return t, err
		}
	}
	return t, s.planEvent(tx, planID, actor, "plan_turn_queued", "", "",
		map[string]any{"turn": t.ID, "messages": len(t.Messages), "joined": q != nil})
}

// PlanMessage is the user talking to a plan (the API): the message joins the plan's queued turn
// (one is created when none waits). A turn running meanwhile is not disturbed.
func (s *Store) PlanMessage(ctx context.Context, actor string, id int64, text string) (pipeline.PlanTurn, error) {
	return s.PlanSlackMessage(ctx, actor, id, text, "")
}

// PlanSlackMessage is PlanMessage for a reply in the plan's Slack thread: the turn keeps its ts.
func (s *Store) PlanSlackMessage(ctx context.Context, actor string, id int64, text, ts string) (pipeline.PlanTurn, error) {
	var out pipeline.PlanTurn
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := loadPlan(tx, id)
		if err != nil {
			return err
		}
		if !p.Stage.Talking() {
			return errf(pipeline.CodeWrongStage, "plan P%d is %s: there is no conversation to add to", id, p.Stage)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return errf(pipeline.CodeMissingField, "text is required")
		}
		out, err = s.queueTurn(tx, actor, id, text, strings.TrimSpace(ts), "")
		return err
	})
	if err != nil {
		return out, s.refusePlan(ctx, actor, id, "message", err)
	}
	return out, nil
}

// planDocs is every version of a plan's document of a kind, oldest first, with the markdown.
func planDocs(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, id int64, kind string, markdown bool) ([]pipeline.PlanDoc, error) {
	md := `''`
	if markdown {
		md = `markdown`
	}
	query := `SELECT plan_id, kind, version, ` + md + `, sha256, created_at FROM plan_docs WHERE plan_id = ?`
	args := []any{id}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	rows, err := q.Query(query+` ORDER BY kind, version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pipeline.PlanDoc{}
	for rows.Next() {
		var d pipeline.PlanDoc
		var created int64
		if err := rows.Scan(&d.PlanID, &d.Kind, &d.Version, &d.Markdown, &d.SHA256, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromNanos(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// PlanApproved is an approval's effect: the plan, and the items a blueprint's hand-off filed.
type PlanApproved struct {
	Plan  pipeline.Plan   `json:"plan"`
	Items []pipeline.Item `json:"items"`
}

// ApprovePlan is the user approving a plan's brief or blueprint, bound to the sha256 of the
// version they saw (pipeline.ApplyPlanApprove). The brief's approval queues engineering's first
// turn; the blueprint's runs the hand-off in the same transaction: one item per piece, the
// dependencies mapped from piece numbers to the new ids, plan → split. A refusal files nothing.
func (s *Store) ApprovePlan(ctx context.Context, actor string, id int64, kind, sha, words string) (PlanApproved, error) {
	var out PlanApproved
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := loadPlan(tx, id)
		if err != nil {
			return err
		}
		var docs []pipeline.PlanDoc
		if kind == pipeline.DocBrief || kind == pipeline.DocBlueprint {
			if docs, err = planDocs(tx, id, kind, true); err != nil {
				return err
			}
		}
		a, err := pipeline.ApplyPlanApprove(p, kind, sha, docs, s.Repos())
		if err != nil {
			return err
		}
		now := s.clock.Now()
		a.Plan.UpdatedAt = now
		out.Items = []pipeline.Item{}
		if kind == pipeline.DocBlueprint {
			if out.Items, err = s.handOff(tx, actor, a.Plan, a.Pieces); err != nil {
				return err
			}
		}
		if err := savePlan(tx, a.Plan); err != nil {
			return err
		}
		out.Plan = a.Plan
		if err := s.planEvent(tx, id, actor, "plan_approved", p.Stage, a.Plan.Stage,
			map[string]any{"kind": kind, "version": a.Doc.Version, "sha256": a.Doc.SHA256, "words": strings.TrimSpace(words)}); err != nil {
			return err
		}
		if kind == pipeline.DocBrief {
			_, err := s.queueTurn(tx, "loopd", id, "", "", "")
			return err
		}
		ids := make([]int64, len(out.Items))
		for i, it := range out.Items {
			ids[i] = it.ID
		}
		if err := s.planEvent(tx, id, "loopd", "split", a.Plan.Stage, a.Plan.Stage, map[string]any{"items": ids, "blueprint": a.Doc.Version}); err != nil {
			return err
		}
		// A message sent after the blueprint was ready waits in a queued turn that will never
		// run (turns run only at brief and blueprint): it fails, with an event saying why. A
		// running turn keeps its lease; its result lands as ignored.
		return s.failQueuedTurn(tx, a.Plan, "the plan was split before this turn ran")
	})
	if err != nil {
		return out, s.refusePlan(ctx, actor, id, "approve "+kind, err)
	}
	return out, nil
}

// handOff files a split plan's items, returned in piece order. Pieces are inserted dependencies
// first (lowest piece number among the ready ones), so each item's created event names its
// dependencies' ids.
func (s *Store) handOff(tx *sql.Tx, actor string, p pipeline.Plan, pieces []pipeline.Piece) ([]pipeline.Item, error) {
	items, err := pipeline.PlanItems(p, pieces, s.Repos(), s.clock.Now())
	if err != nil {
		return nil, err
	}
	// The existing items a piece waits for, checked before anything is filed, so the refusal names
	// the piece (insertItem would refuse them too, as "depends_on: ...").
	for _, pc := range pieces {
		for _, d := range pc.After {
			var stage pipeline.Stage
			err := tx.QueryRow(`SELECT stage FROM items WHERE id = ?`, d).Scan(&stage)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errf(pipeline.CodeInvalidField, "piece %d waits on #%d, which does not exist", pc.N, d)
			}
			if err != nil {
				return nil, err
			}
			if stage == pipeline.Closed {
				return nil, errf(pipeline.CodeInvalidField, "piece %d waits on #%d, which is closed", pc.N, d)
			}
		}
	}
	ids := map[int]int64{}
	for len(ids) < len(pieces) {
		next := -1
		for i, pc := range pieces {
			if _, done := ids[pc.N]; done {
				continue
			}
			if !slices.ContainsFunc(pc.DependsOn, func(d int) bool { _, ok := ids[d]; return !ok }) {
				next = i
				break
			}
		}
		if next < 0 {
			// ParseBlueprint refuses cycles; this is a guard, not a path.
			return nil, fmt.Errorf("hand-off: the pieces' dependencies loop")
		}
		it := &items[next]
		for _, d := range pieces[next].DependsOn {
			it.DependsOn = append(it.DependsOn, ids[d])
		}
		it.DependsOn = append(it.DependsOn, pieces[next].After...)
		if err := s.insertItem(tx, actor, it); err != nil {
			return nil, err
		}
		ids[pieces[next].N] = it.ID
	}
	return items, nil
}

// failQueuedTurn fails a plan's queued turn, if it has one, when the plan has left the
// conversation (split or dropped) before the turn ran.
func (s *Store) failQueuedTurn(tx *sql.Tx, p pipeline.Plan, why string) error {
	t, err := openTurn(tx, p.ID, pipeline.TurnQueued)
	if err != nil || t == nil {
		return err
	}
	t.Status, t.Error, t.UpdatedAt = pipeline.TurnFailed, why, s.clock.Now()
	if err := saveTurn(tx, *t); err != nil {
		return err
	}
	return s.planEvent(tx, p.ID, "loopd", "plan_turn_failed", p.Stage, p.Stage,
		map[string]any{"turn": t.ID, "messages": t.Messages, "error": why})
}

// DropPlan drops an open plan: a running turn's lease ends (its next heartbeat stops it) and its
// turns fail. Items already filed are untouched.
func (s *Store) DropPlan(ctx context.Context, actor string, id int64, words string) (pipeline.Plan, error) {
	var out pipeline.Plan
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := loadPlan(tx, id)
		if err != nil {
			return err
		}
		np, err := pipeline.ApplyPlanDrop(p)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		np.UpdatedAt = now
		for _, st := range []pipeline.TurnStatus{pipeline.TurnRunning, pipeline.TurnQueued} {
			t, err := openTurn(tx, id, st)
			if err != nil {
				return err
			}
			if t == nil {
				continue
			}
			if _, err := tx.Exec(`UPDATE plan_leases SET ended_at = ?, end_reason = 'dropped', activity = '' WHERE turn_id = ? AND ended_at IS NULL`,
				now.UnixNano(), t.ID); err != nil {
				return err
			}
			t.Status, t.Error, t.UpdatedAt = pipeline.TurnFailed, "the plan was dropped", now
			if err := saveTurn(tx, *t); err != nil {
				return err
			}
		}
		if err := savePlan(tx, np); err != nil {
			return err
		}
		out = np
		return s.planEvent(tx, id, actor, "plan_dropped", p.Stage, np.Stage, map[string]any{"words": strings.TrimSpace(words)})
	})
	if err != nil {
		return out, s.refusePlan(ctx, actor, id, "drop", err)
	}
	return out, nil
}

// PlanTurnResult is a runner finishing the plan turn it holds the lease on
// (pipeline.ApplyTurnResult): the turn is done or failed, new document versions are added, the
// plan's sessions and repos are kept; back_to_brief queues a product turn with the reason.
func (s *Store) PlanTurnResult(ctx context.Context, runnerName string, planID, turnID int64, r pipeline.TurnResult) (pipeline.Plan, pipeline.PlanTurn, error) {
	var (
		outP pipeline.Plan
		outT pipeline.PlanTurn
	)
	actor := "runner:" + runnerName
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		t, err := loadTurn(tx, planID, turnID)
		if err != nil {
			return err
		}
		l, err := scanPlanLease(tx.QueryRow(`SELECT `+planLeaseCols+` FROM plan_leases WHERE turn_id = ? AND ended_at IS NULL`, turnID))
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNoLease, "turn %d of plan P%d has no live lease", turnID, planID)
		}
		if err != nil {
			return err
		}
		if l.Runner != runnerName {
			return errf(CodeNotYourLease, "turn %d of plan P%d is leased by another runner", turnID, planID)
		}
		p, err := loadPlan(tx, planID)
		if err != nil {
			return err
		}
		docs, err := planDocs(tx, planID, "", false)
		if err != nil {
			return err
		}
		latest := map[string]pipeline.PlanDoc{}
		for _, d := range docs {
			latest[d.Kind] = d // oldest first: the last one wins
		}
		now := s.clock.Now()
		o := pipeline.ApplyTurnResult(p, t, r, latest, s.Repos(), now)
		o.Plan.UpdatedAt = now
		if err := savePlan(tx, o.Plan); err != nil {
			return err
		}
		t.UpdatedAt = now
		data := map[string]any{"turn": t.ID, "lease": l.ID, "persona": t.Persona}
		if o.Failed != "" {
			t.Status, t.Error = pipeline.TurnFailed, o.Failed
			if err := endPlanLease(tx, l.ID, now, "failed"); err != nil {
				return err
			}
			if err := saveTurn(tx, t); err != nil {
				return err
			}
			data["error"] = o.Failed
			outP, outT = o.Plan, t
			return s.planEvent(tx, planID, actor, "plan_turn_failed", p.Stage, p.Stage, data)
		}
		for _, d := range o.Docs {
			if _, err := tx.Exec(`INSERT INTO plan_docs (plan_id, kind, version, markdown, sha256, created_at) VALUES (?,?,?,?,?,?)`,
				d.PlanID, d.Kind, d.Version, d.Markdown, d.SHA256, now.UnixNano()); err != nil {
				return err
			}
			if err := s.planEvent(tx, planID, actor, "plan_doc", p.Stage, p.Stage,
				map[string]any{"kind": d.Kind, "version": d.Version, "sha256": d.SHA256, "turn": t.ID}); err != nil {
				return err
			}
		}
		t.Status, t.Reply, t.Ready = pipeline.TurnDone, strings.TrimSpace(r.Reply), o.Ready
		if err := endPlanLease(tx, l.ID, now, "done"); err != nil {
			return err
		}
		if err := saveTurn(tx, t); err != nil {
			return err
		}
		data["ready"] = o.Ready
		if len(o.Ignored) > 0 {
			data["ignored"] = o.Ignored
		}
		if err := s.planEvent(tx, planID, actor, "plan_turn_done", p.Stage, o.Plan.Stage, data); err != nil {
			return err
		}
		outP, outT = o.Plan, t
		if !o.BackToBrief {
			return nil
		}
		why := strings.TrimSpace(r.BackToBrief)
		if err := s.planEvent(tx, planID, actor, "back_to_brief", p.Stage, o.Plan.Stage, map[string]any{"reason": why}); err != nil {
			return err
		}
		_, err = s.queueTurn(tx, "loopd", planID, "", "", why)
		return err
	})
	if err != nil {
		return outP, outT, s.refusePlan(ctx, actor, planID, "turn result", err)
	}
	return outP, outT, nil
}

// ---- claims, heartbeats, the sweep ----

// turnCandidates are the queued turns of plans in a conversation with no turn running.
func turnCandidates(tx *sql.Tx) ([]pipeline.TurnCandidate, error) {
	rows, err := tx.Query(`SELECT t.id, t.plan_id, p.priority, t.created_at FROM plan_turns t JOIN plans p ON p.id = t.plan_id
		WHERE t.status = 'queued' AND p.stage IN ('brief','blueprint')
		AND NOT EXISTS (SELECT 1 FROM plan_turns r WHERE r.plan_id = t.plan_id AND r.status = 'running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.TurnCandidate
	for rows.Next() {
		var c pipeline.TurnCandidate
		var created int64
		if err := rows.Scan(&c.TurnID, &c.PlanID, &c.Priority, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = fromNanos(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// claimTurn starts a queued turn: the persona is the plan's stage's.
func (s *Store) claimTurn(tx *sql.Tx, runnerName, agent string, c pipeline.TurnCandidate) (*Claim, error) {
	now := s.clock.Now()
	p, err := loadPlan(tx, c.PlanID)
	if err != nil {
		return nil, err
	}
	t, err := loadTurn(tx, c.PlanID, c.TurnID)
	if err != nil {
		return nil, err
	}
	t.Status, t.Persona, t.Runner, t.Agent, t.UpdatedAt = pipeline.TurnRunning, pipeline.PersonaFor(p.Stage), runnerName, agent, now
	if err := saveTurn(tx, t); err != nil {
		return nil, err
	}
	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	l := PlanLease{ID: id, TurnID: t.ID, PlanID: p.ID, Runner: runnerName, Agent: agent, ClaimedAt: now, ExpiresAt: now.Add(LeaseTTL)}
	if _, err := tx.Exec(`INSERT INTO plan_leases (id, turn_id, plan_id, runner, agent, claimed_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.TurnID, l.PlanID, l.Runner, l.Agent, now.UnixNano(), l.ExpiresAt.UnixNano()); err != nil {
		return nil, err
	}
	if err := s.planEvent(tx, p.ID, "runner:"+runnerName, "plan_turn_claimed", p.Stage, p.Stage,
		map[string]any{"turn": t.ID, "persona": t.Persona, "runner": runnerName, "agent": agent, "lease": l.ID}); err != nil {
		return nil, err
	}
	return &Claim{Kind: pipeline.WorkPlan, Lease: l.lease(), Plan: &p, Turn: &t}, nil
}

// requeueTurn puts a turn whose run ended early (lease expired, loop stopped) back in the queue;
// when a message already queued the next turn, the two merge (this one's messages first).
func (s *Store) requeueTurn(tx *sql.Tx, t pipeline.PlanTurn) error {
	now := s.clock.Now()
	p, err := loadPlan(tx, t.PlanID)
	if err != nil {
		return err
	}
	if p.Stage != pipeline.PlanBrief && p.Stage != pipeline.PlanBlueprint {
		// The plan left the conversation while the turn ran: there is nothing to run it for.
		t.Status, t.Error, t.UpdatedAt = pipeline.TurnFailed, fmt.Sprintf("its run ended early and the plan is %s", p.Stage), now
		return saveTurn(tx, t)
	}
	q, err := openTurn(tx, t.PlanID, pipeline.TurnQueued)
	if err != nil {
		return err
	}
	if q != nil {
		q.Messages = append(append([]string{}, t.Messages...), q.Messages...)
		q.SlackTS = append(append([]string{}, t.SlackTS...), q.SlackTS...)
		if q.BackToBrief == "" {
			q.BackToBrief = t.BackToBrief
		}
		q.UpdatedAt = now
		t.Status, t.Error, t.UpdatedAt = pipeline.TurnFailed, fmt.Sprintf("its run ended early; its messages joined turn %d", q.ID), now
		if err := saveTurn(tx, t); err != nil {
			return err
		}
		return saveTurn(tx, *q)
	}
	t.Status, t.Persona, t.Runner, t.Agent, t.UpdatedAt = pipeline.TurnQueued, "", "", "", now
	return saveTurn(tx, t)
}

// sweepPlans ends expired and overtime turn leases: an expired turn waits again, an overtime one
// fails (the user replies to try again).
func (s *Store) sweepPlans(tx *sql.Tx, now time.Time, maxRun time.Duration) error {
	live, err := livePlanLeases(tx)
	if err != nil {
		return err
	}
	for _, l := range live {
		v := pipeline.JudgeLease(l.ClaimedAt, l.ExpiresAt, now, maxRun)
		if v == pipeline.LeaseLive {
			continue
		}
		p, err := loadPlan(tx, l.PlanID)
		if err != nil {
			return err
		}
		t, err := loadTurn(tx, l.PlanID, l.TurnID)
		if err != nil {
			return err
		}
		data := map[string]any{"turn": t.ID, "lease": l.ID, "runner": l.Runner, "agent": l.Agent, "note": l.Note}
		if v == pipeline.LeaseExpired {
			if err := endPlanLease(tx, l.ID, now, "expired"); err != nil {
				return err
			}
			if err := s.requeueTurn(tx, t); err != nil {
				return err
			}
			if err := s.planEvent(tx, p.ID, "loopd", "plan_turn_expired", p.Stage, p.Stage, data); err != nil {
				return err
			}
			continue
		}
		if err := endPlanLease(tx, l.ID, now, "overtime"); err != nil {
			return err
		}
		t.Status, t.Error, t.UpdatedAt = pipeline.TurnFailed, fmt.Sprintf("the turn ran past the max run time (%s)", maxRun), now
		if err := saveTurn(tx, t); err != nil {
			return err
		}
		data["error"] = t.Error
		if err := s.planEvent(tx, p.ID, "loopd", "plan_turn_failed", p.Stage, p.Stage, data); err != nil {
			return err
		}
	}
	return nil
}

// planHeartbeat is Heartbeat for a turn's lease. found is false when no turn lease has that id.
func (s *Store) planHeartbeat(tx *sql.Tx, runnerName, leaseID, note, actJSON string) (stop, found bool, err error) {
	l, err := scanPlanLease(tx.QueryRow(`SELECT `+planLeaseCols+` FROM plan_leases WHERE id = ?`, leaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	if l.Runner != runnerName {
		return false, true, errf(CodeNotYourLease, "lease %s is held by another runner", leaseID)
	}
	now := s.clock.Now()
	if _, err := tx.Exec(`UPDATE runners SET last_seen = ? WHERE name = ?`, now.UnixNano(), runnerName); err != nil {
		return false, true, err
	}
	if l.EndedAt != nil {
		return true, true, nil
	}
	ctl, err := s.loadControl(tx)
	if err != nil {
		return false, true, err
	}
	if ctl.Stopped {
		if err := endPlanLease(tx, l.ID, now, "stopped"); err != nil {
			return true, true, err
		}
		t, err := loadTurn(tx, l.PlanID, l.TurnID)
		if err != nil {
			return true, true, err
		}
		if err := s.requeueTurn(tx, t); err != nil {
			return true, true, err
		}
		p, err := loadPlan(tx, l.PlanID)
		if err != nil {
			return true, true, err
		}
		return true, true, s.planEvent(tx, l.PlanID, "loopd", "plan_turn_stopped", p.Stage, p.Stage,
			map[string]any{"turn": t.ID, "lease": l.ID, "runner": l.Runner, "agent": l.Agent, "note": note})
	}
	if note == "" {
		note = l.Note
	}
	if actJSON == "" {
		actJSON = encodedActivity(l.Activity)
	}
	_, err = tx.Exec(`UPDATE plan_leases SET expires_at = ?, note = ?, activity = ? WHERE id = ?`, now.Add(LeaseTTL).UnixNano(), note, actJSON, l.ID)
	return false, true, err
}

// planItemSettled is called when an item reaches done or closed: a split plan whose items are all
// settled, at least one done, is done.
func (s *Store) planItemSettled(tx *sql.Tx, it pipeline.Item) error {
	if it.PlanID == 0 || (it.Stage != pipeline.Done && it.Stage != pipeline.Closed) {
		return nil
	}
	p, err := loadPlan(tx, it.PlanID)
	var pe *pipeline.Error
	if errors.As(err, &pe) && pe.Code == CodeNotFound {
		return nil // an item linked to a plan that does not exist
	}
	if err != nil || p.Stage != pipeline.PlanSplit {
		return err
	}
	rows, err := tx.Query(`SELECT stage FROM items WHERE plan_id = ?`, p.ID)
	if err != nil {
		return err
	}
	var stages []pipeline.Stage
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			rows.Close()
			return err
		}
		stages = append(stages, pipeline.Stage(st))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !pipeline.PlanSettled(stages) {
		return nil
	}
	from := p.Stage
	p.Stage, p.UpdatedAt = pipeline.PlanDone, s.clock.Now()
	if err := savePlan(tx, p); err != nil {
		return err
	}
	return s.planEvent(tx, p.ID, "loopd", "plan_done", from, p.Stage, map[string]any{"item": it.ID})
}

// ---- Slack ----

// PlanByThread finds the plan whose Slack thread is (channel, ts); 0 when there is none.
func (s *Store) PlanByThread(ctx context.Context, channel, ts string) (int64, error) {
	if ts == "" {
		return 0, nil
	}
	var id int64
	err := s.r.QueryRowContext(ctx, `SELECT id FROM plans WHERE slack_channel = ? AND slack_ts = ?`, channel, ts).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetPlanSlack records a plan's Slack thread and the bot's header message in it. Slack state, not
// the plan's: it writes no event.
func (s *Store) SetPlanSlack(ctx context.Context, id int64, channel, ts, header string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE plans SET slack_channel = ?, slack_ts = ?, slack_header = ? WHERE id = ?`, channel, ts, header, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errf(CodeNotFound, "no plan P%d", id)
		}
		return nil
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- reads ----

// PlanRow is one line of the plans list.
type PlanRow struct {
	ID       int64              `json:"id"`
	Title    string             `json:"title"`
	Stage    pipeline.PlanStage `json:"stage"`
	Priority string             `json:"priority"`
	Repos    []string           `json:"repos"`
	// Turn is whose turn it is: "agent" (a turn queued or running), "you" (the conversation waits
	// on the user), "" (not in a conversation).
	Turn      string    `json:"turn,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func whoseTurn(q interface {
	QueryRow(string, ...any) *sql.Row
}, p pipeline.Plan) (string, error) {
	if !p.Stage.Talking() {
		return "", nil
	}
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM plan_turns WHERE plan_id = ? AND status IN ('queued','running')`, p.ID).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "agent", nil
	}
	return "you", nil
}

// ListPlans lists plans, newest first; open leaves out done and dropped ones.
func (s *Store) ListPlans(ctx context.Context, open bool) ([]PlanRow, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := `SELECT ` + planCols + ` FROM plans`
	if open {
		q += ` WHERE stage IN ('brief','blueprint','split')`
	}
	rows, err := tx.Query(q + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	var plans []pipeline.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		plans = append(plans, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []PlanRow{}
	for _, p := range plans {
		turn, err := whoseTurn(tx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, PlanRow{ID: p.ID, Title: p.Title, Stage: p.Stage, Priority: p.Priority, Repos: p.Repos, Turn: turn,
			CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
	}
	return out, nil
}

// PlanItem is an item a plan filed.
type PlanItem struct {
	ID        int64          `json:"id"`
	Title     string         `json:"title"`
	Stage     pipeline.Stage `json:"stage"`
	Piece     int            `json:"piece"`
	DependsOn []int64        `json:"depends_on"`
}

// PlanDetail is a plan with its documents (versions, without the markdown), turns, items and
// history.
type PlanDetail struct {
	Plan   pipeline.Plan       `json:"plan"`
	Turn   string              `json:"turn,omitempty"`
	Docs   []pipeline.PlanDoc  `json:"docs"`
	Turns  []pipeline.PlanTurn `json:"turns"`
	Items  []PlanItem          `json:"items"`
	Events []Event             `json:"events"`
}

// GetPlan returns a plan and everything about it.
func (s *Store) GetPlan(ctx context.Context, id int64) (PlanDetail, error) {
	var d PlanDetail
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if d.Plan, err = loadPlan(tx, id); err != nil {
		return d, err
	}
	if d.Turn, err = whoseTurn(tx, d.Plan); err != nil {
		return d, err
	}
	if d.Docs, err = planDocs(tx, id, "", false); err != nil {
		return d, err
	}
	rows, err := tx.Query(`SELECT `+turnCols+` FROM plan_turns WHERE plan_id = ? ORDER BY id`, id)
	if err != nil {
		return d, err
	}
	d.Turns = []pipeline.PlanTurn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			rows.Close()
			return d, err
		}
		d.Turns = append(d.Turns, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	rows, err = tx.Query(`SELECT id, title, stage, plan_piece, depends_on FROM items WHERE plan_id = ? ORDER BY plan_piece, id`, id)
	if err != nil {
		return d, err
	}
	d.Items = []PlanItem{}
	for rows.Next() {
		var it PlanItem
		var deps string
		if err := rows.Scan(&it.ID, &it.Title, &it.Stage, &it.Piece, &deps); err != nil {
			rows.Close()
			return d, err
		}
		if it.DependsOn, err = splitIDs(deps); err != nil {
			rows.Close()
			return d, err
		}
		d.Items = append(d.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	rows, err = tx.Query(`SELECT `+eventCols+` FROM events WHERE plan_id = ? ORDER BY id`, id)
	if err != nil {
		return d, err
	}
	d.Events, err = scanEvents(rows)
	return d, err
}

// GetPlanDoc returns a version of a plan's document, with its markdown; version 0 is the latest.
func (s *Store) GetPlanDoc(ctx context.Context, id int64, kind string, version int) (pipeline.PlanDoc, error) {
	if kind != pipeline.DocBrief && kind != pipeline.DocBlueprint && kind != pipeline.DocCritique {
		return pipeline.PlanDoc{}, errf(pipeline.CodeInvalidField, "kind must be brief, blueprint or critique, not %q", kind)
	}
	if _, err := loadPlan(s.r, id); err != nil {
		return pipeline.PlanDoc{}, err
	}
	docs, err := planDocs(s.r, id, kind, true)
	if err != nil {
		return pipeline.PlanDoc{}, err
	}
	if len(docs) == 0 {
		return pipeline.PlanDoc{}, errf(CodeNotFound, "plan P%d has no %s yet", id, kind)
	}
	if version == 0 {
		return docs[len(docs)-1], nil
	}
	for _, d := range docs {
		if d.Version == version {
			return d, nil
		}
	}
	return pipeline.PlanDoc{}, errf(CodeNotFound, "plan P%d has no %s v%d (the latest is v%d)", id, kind, version, docs[len(docs)-1].Version)
}
