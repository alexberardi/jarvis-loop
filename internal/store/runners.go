package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// RunnerDraining is a runner that claims nothing new and waits for its live runs before a
// restart (docs/decisions/2026-10-18-runner-reload-drain.md).
const RunnerDraining = "draining"

// RunnerState is a runner that said it is in a state other than working.
type RunnerState struct {
	Name  string
	State string
	Since time.Time
}

// SetRunnerState records what runnerName says it is doing: RunnerDraining, or "" (working). Its
// next claim clears it too, so a runner that restarted is no longer shown draining.
func (s *Store) SetRunnerState(ctx context.Context, runnerName, state string) error {
	if state != "" && state != RunnerDraining {
		return &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "state must be \"draining\" or \"\", got " + strconv.Quote(state)}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := s.clock.Now().UnixNano()
		_, err := tx.Exec(`INSERT INTO runners (name, capabilities, last_seen, state, state_at) VALUES (?, '', ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET last_seen = excluded.last_seen, state = excluded.state, state_at = excluded.state_at`,
			runnerName, now, state, now)
		return err
	})
}

// RunnerStates are the runners in a state other than working, by name.
func (s *Store) RunnerStates(ctx context.Context) ([]RunnerState, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT name, state, state_at FROM runners WHERE state != '' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunnerState
	for rows.Next() {
		var r RunnerState
		var at int64
		if err := rows.Scan(&r.Name, &r.State, &at); err != nil {
			return nil, err
		}
		r.Since = fromNanos(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
