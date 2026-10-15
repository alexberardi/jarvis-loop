package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Slack state (docs/phases/03-slack.md). None of it is pipeline state: setting it writes no event.

// SlackThread returns the channel and ts of an item's parent message, empty when it has none.
func (s *Store) SlackThread(ctx context.Context, id int64) (channel, ts string, err error) {
	err = s.r.QueryRowContext(ctx, `SELECT slack_channel, slack_ts FROM items WHERE id = ?`, id).Scan(&channel, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errf(CodeNotFound, "no item %d", id)
	}
	return channel, ts, err
}

// SetSlackThread records an item's parent message.
func (s *Store) SetSlackThread(ctx context.Context, id int64, channel, ts string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE items SET slack_channel = ?, slack_ts = ? WHERE id = ?`, channel, ts, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errf(CodeNotFound, "no item %d", id)
		}
		return nil
	})
}

// ItemByThread finds the item whose parent message is (channel, ts); 0 when there is none.
func (s *Store) ItemByThread(ctx context.Context, channel, ts string) (int64, error) {
	if ts == "" {
		return 0, nil
	}
	var id int64
	err := s.r.QueryRowContext(ctx, `SELECT id FROM items WHERE slack_channel = ? AND slack_ts = ?`, channel, ts).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SlackState is the Slack layer's own row of control.
type SlackState struct {
	// ControlTS is the active-agents message in #loop-digest ("" until it is posted).
	ControlTS string
	// LastDigest is the end of the last digest window (zero before the first).
	LastDigest time.Time
}

// SlackState reads the Slack columns of control.
func (s *Store) SlackState(ctx context.Context) (SlackState, error) {
	var st SlackState
	var last int64
	err := s.r.QueryRowContext(ctx, `SELECT slack_control_ts, last_digest_at FROM control WHERE id = 1`).Scan(&st.ControlTS, &last)
	if last != 0 {
		st.LastDigest = fromNanos(last)
	}
	return st, err
}

// SetSlackControlTS records the active-agents message.
func (s *Store) SetSlackControlTS(ctx context.Context, ts string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE control SET slack_control_ts = ? WHERE id = 1`, ts)
		return err
	})
}

// SetLastDigest records the end of the digest window just reported.
func (s *Store) SetLastDigest(ctx context.Context, t time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE control SET last_digest_at = ? WHERE id = 1`, t.UnixNano())
		return err
	})
}

// Record appends an event that is not a pipeline transition: a refused Slack action, a cancelled
// reply approval, a /loop usage reading. ItemID 0 means no item. Subscribers see it like any other.
func (s *Store) Record(ctx context.Context, e Event) error {
	if e.Kind == "" || e.Actor == "" {
		return fmt.Errorf("record: kind and actor are required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if e.ItemID != 0 {
			if _, err := loadItem(tx, e.ItemID); err != nil {
				return err
			}
		}
		e.ID = 0
		return s.event(tx, e)
	})
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var item sql.NullInt64
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &item, &at, &e.Actor, &e.Kind, &e.From, &e.To, &e.Outcome, &data); err != nil {
			return nil, err
		}
		e.ItemID = item.Int64
		e.At = fromNanos(at)
		if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const eventCols = `id, item_id, at, actor, kind, from_stage, to_stage, outcome, data`

// EventsBetween returns every event with after < at <= upTo, oldest first.
func (s *Store) EventsBetween(ctx context.Context, after, upTo time.Time) ([]Event, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE at > ? AND at <= ? ORDER BY id`,
		after.UnixNano(), upTo.UnixNano())
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// LastEvent returns the newest event of a kind.
func (s *Store) LastEvent(ctx context.Context, kind string) (Event, bool, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind)
	if err != nil {
		return Event{}, false, err
	}
	evs, err := scanEvents(rows)
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}
