package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// PRClosedStates are what a pr_closed event may report: the runner closed the PR, or found it
// already closed or merged (closed by hand), which is a no-op on GitHub.
var PRClosedStates = []string{"closed", "already_closed", "already_merged"}

// Drop closes an open item at the user's word (docs/decisions/2026-10-15-drop.md). A live lease
// ends in the same transaction, as overtime ends one: the runner's next heartbeat stops the run,
// and its report finds no lease. The runner closes the item's PR later (PRClosed).
func (s *Store) Drop(ctx context.Context, actor string, id int64, words string) (pipeline.Item, error) {
	var out pipeline.Item
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		tr, err := pipeline.ApplyDrop(it, words)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		l, err := scanLease(tx.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE item_id = ? AND ended_at IS NULL`, id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := endLease(tx, l.ID, now, "dropped"); err != nil {
				return err
			}
			tr.Data["lease"], tr.Data["runner"], tr.Data["agent"] = l.ID, l.Runner, l.Agent
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = tr.Item
		if err := s.event(tx, transitionEvent(id, actor, tr)); err != nil {
			return err
		}
		return s.dependencyClosed(tx, id)
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, "drop", err)
	}
	return out, nil
}

// PRClosed records that a runner closed a dropped item's PR (kind pr_closed), once. Any runner may
// record it: a dropped item has no lease, and closing is idempotent on GitHub.
func (s *Store) PRClosed(ctx context.Context, runnerName string, id int64, data map[string]any) (Event, error) {
	var out Event
	if err := validPRClosed(data); err != nil {
		return out, err
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		if it.Stage != pipeline.Closed || it.PR == 0 {
			return errf(pipeline.CodeWrongStage, "item %d is %s with no PR to close", id, it.Stage)
		}
		if pr := int(data["pr"].(float64)); pr != it.PR {
			return errf(pipeline.CodeInvalidField, "item %d's PR is #%d, not #%d", id, it.PR, pr)
		}
		var dropped, closed int
		if err := tx.QueryRow(`SELECT COUNT(*) FILTER (WHERE kind = 'dropped'), COUNT(*) FILTER (WHERE kind = 'pr_closed')
			FROM events WHERE item_id = ?`, id).Scan(&dropped, &closed); err != nil {
			return err
		}
		if dropped == 0 {
			return errf(pipeline.CodeWrongStage, "item %d was not dropped", id)
		}
		if closed > 0 {
			return errf(CodeAlreadyRecorded, "item %d's PR is already closed", id)
		}
		out = Event{ItemID: id, Actor: "runner:" + runnerName, Kind: "pr_closed", From: string(pipeline.Closed), To: string(pipeline.Closed), Data: data}
		return s.event(tx, out)
	})
	if err != nil {
		return out, err
	}
	out.At = s.clock.Now()
	return out, nil
}

func validPRClosed(data map[string]any) error {
	for k := range data {
		if k != "pr" && k != "state" {
			return errf(pipeline.CodeInvalidField, "pr_closed data may hold pr and state, not %q", k)
		}
	}
	if f, ok := data["pr"].(float64); !ok || f < 1 || f != float64(int64(f)) {
		return errf(pipeline.CodeMissingField, "pr_closed needs the PR number")
	}
	st, _ := data["state"].(string)
	if !slices.Contains(PRClosedStates, st) {
		return errf(pipeline.CodeInvalidField, "state must be one of %s", strings.Join(PRClosedStates, ", "))
	}
	return nil
}
