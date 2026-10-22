package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Event kinds of the unservable flag (docs/decisions/2026-10-17-unservable-items.md).
const (
	KindUnservable = "unservable" // data.missing: the resources no runner offers
	KindServable   = "servable"   // the flag cleared: a runner offers them now, or the item moved on
)

func runnersSeen(tx *sql.Tx) ([]pipeline.RunnerSeen, error) {
	rows, err := tx.Query(`SELECT name, capabilities, only_resources, poll_seconds, stages, last_seen, builders, kinds FROM runners`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.RunnerSeen
	for rows.Next() {
		var r pipeline.RunnerSeen
		var caps, only, stages, builders, kinds string
		var seen int64
		if err := rows.Scan(&r.Name, &caps, &only, &r.PollSeconds, &stages, &seen, &builders, &kinds); err != nil {
			return nil, err
		}
		r.Capabilities, r.OnlyResources, r.LastSeen, r.Builders, r.Kinds = splitList(caps), splitList(only), fromNanos(seen), splitList(builders), splitList(kinds)
		for _, st := range splitList(stages) {
			r.Stages = append(r.Stages, pipeline.Stage(st))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// checkUnservable flags each item in an agent stage (land needs no resources, pipeline.Item.Needs)
// whose resources no runner seen lately offers (an unservable event, once per missing set) and
// clears the flag of the others (servable, when a runner offers them now). It runs on every claim
// (a runner's capabilities may have changed), on every advance (triage sets the resources) and
// once a minute (a runner may have gone quiet).
func (s *Store) checkUnservable(tx *sql.Tx, now time.Time) error {
	runners, err := runnersSeen(tx)
	if err != nil {
		return err
	}
	flagged := map[int64]string{}
	rows, err := tx.Query(`SELECT item_id, missing FROM unservable`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			rows.Close()
			return err
		}
		flagged[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type cand struct {
		id    int64
		stage string
		res   []string
	}
	var items []cand
	rows, err = tx.Query(`SELECT id, stage, resources FROM items WHERE stage IN ('triage','build','review') AND resources != ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c cand
		var res string
		if err := rows.Scan(&c.id, &c.stage, &res); err != nil {
			rows.Close()
			return err
		}
		c.res = splitList(res)
		items = append(items, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range items {
		missing := pipeline.Unservable(c.res, runners, now)
		was, ok := flagged[c.id]
		delete(flagged, c.id)
		if len(missing) == 0 {
			if ok {
				if err := s.clearUnservable(tx, c.id, c.stage, was); err != nil {
					return err
				}
			}
			continue
		}
		m := strings.Join(missing, ",")
		if ok && was == m {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO unservable (item_id, missing, since) VALUES (?,?,?)
			ON CONFLICT (item_id) DO UPDATE SET missing = excluded.missing, since = excluded.since`, c.id, m, now.UnixNano()); err != nil {
			return err
		}
		if err := s.event(tx, Event{ItemID: c.id, Actor: "loopd", Kind: KindUnservable, From: c.stage, To: c.stage,
			Data: map[string]any{"missing": missing}}); err != nil {
			return err
		}
	}
	// What is left moved out of the work stages or lost its resources: no runner came for it, so
	// the flag just goes (it is set again if the item comes back still unservable).
	for id := range flagged {
		if _, err := tx.Exec(`DELETE FROM unservable WHERE item_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) clearUnservable(tx *sql.Tx, id int64, stage, was string) error {
	if _, err := tx.Exec(`DELETE FROM unservable WHERE item_id = ?`, id); err != nil {
		return err
	}
	return s.event(tx, Event{ItemID: id, Actor: "loopd", Kind: KindServable, From: stage, To: stage,
		Data: map[string]any{"missing": splitList(was)}})
}

// unservableMissing is each flagged item's missing resources.
func unservableMissing(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) (map[int64][]string, error) {
	rows, err := q.Query(`SELECT item_id, missing FROM unservable`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = splitList(m)
	}
	return out, rows.Err()
}
