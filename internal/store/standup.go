package store

import (
	"context"
	"database/sql"
	"time"
)

// KindStandup records a daily standup (docs/decisions/2026-10-18-standup.md): the 09:00 it is for
// (data.for, RFC 3339) and whether it was posted (false: the first run only started the clock, or
// Slack refused it every try).
const KindStandup = "standup"

// RecordStandup records the standup for the 09:00 at, posted or not.
func (s *Store) RecordStandup(ctx context.Context, at time.Time, posted bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return s.event(tx, Event{Actor: "loopd", Kind: KindStandup, Data: map[string]any{"for": at.UTC().Format(time.RFC3339), "posted": posted}})
	})
}

// LastStandup is the 09:00 the newest standup was for (zero before the first).
func (s *Store) LastStandup(ctx context.Context) (time.Time, error) {
	e, ok, err := s.LastEvent(ctx, KindStandup)
	if err != nil || !ok {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, str(e.Data, "for"))
	return t, nil
}
