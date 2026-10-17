package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// encodeActivity is an activity as stored ("" for nil).
func encodeActivity(a *pipeline.Activity) (string, error) {
	if a == nil {
		return "", nil
	}
	b, err := json.Marshal(a)
	return string(b), err
}

func encodedActivity(a *pipeline.Activity) string {
	s, _ := encodeActivity(a)
	return s
}

// parseActivity reads a stored activity; nil for none (or one that no longer parses).
func parseActivity(s string) *pipeline.Activity {
	if s == "" {
		return nil
	}
	var a pipeline.Activity
	if json.Unmarshal([]byte(s), &a) != nil {
		return nil
	}
	return &a
}

// Working is one live lease as the board's "Working now" card: an item's stage, or a plan turn.
type Working struct {
	// ItemID is the item's (0 for a plan turn); PlanID and Persona are a plan turn's.
	ItemID    int64
	PlanID    int64
	Persona   string
	Title     string
	Stage     string
	Runner    string
	Agent     string
	ClaimedAt time.Time
	Note      string
	Activity  *pipeline.Activity
}

// Working is every live lease, item and plan turn, oldest claim first; id > 0 keeps only that
// item's.
func (s *Store) Working(ctx context.Context, id int64) ([]Working, error) {
	q := `SELECT l.item_id, 0, '', i.title, l.stage, l.runner, l.agent, l.claimed_at, l.note, l.activity
		FROM leases l JOIN items i ON i.id = l.item_id WHERE l.ended_at IS NULL AND (?1 = 0 OR l.item_id = ?1)`
	if id == 0 {
		q += ` UNION ALL SELECT 0, l.plan_id, t.persona, p.title, 'plan', l.runner, l.agent, l.claimed_at, l.note, l.activity
		FROM plan_leases l JOIN plans p ON p.id = l.plan_id JOIN plan_turns t ON t.id = l.turn_id WHERE l.ended_at IS NULL`
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY 8, 1, 2`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Working
	for rows.Next() {
		var w Working
		var claimed int64
		var act string
		if err := rows.Scan(&w.ItemID, &w.PlanID, &w.Persona, &w.Title, &w.Stage, &w.Runner, &w.Agent, &claimed, &w.Note, &act); err != nil {
			return nil, err
		}
		w.ClaimedAt, w.Activity = fromNanos(claimed), parseActivity(act)
		out = append(out, w)
	}
	return out, rows.Err()
}
