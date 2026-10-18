package store

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The runway warning (#205, pipeline/runway.go): once a minute loopd works out how much the loop
// can do without the user; below the control's runway_warn_hours, with the user away, it records
// one runway_low event per dry spell (the Slack layer posts it, with the buttons).

// KindRunwayLow is the dry-spell warning: data.hours, data.threshold, data.runnable (a count) and
// data.unblockers ([{item, action, frees, sha}], most freed first, at most MaxRunwayUnblockers).
const KindRunwayLow = "runway_low"

// MaxRunwayUnblockers caps the unblockers a warning names.
const MaxRunwayUnblockers = 5

// runwayRecentRuns is how many recent runs of a stage its duration is the median of.
const runwayRecentRuns = 20

// RunwayUnblocker is an unblocker with what its button needs: the commit an approval or a
// trusted-file OK is for.
type RunwayUnblocker struct {
	pipeline.Unblocker
	SHA string `json:"sha,omitempty"`
}

// RunwayReport is the runway as the board, the orchestrator's snapshot and the warning show it.
type RunwayReport struct {
	// Hours is the runnable work in agent-hours, to a tenth.
	Hours      float64           `json:"hours"`
	Runnable   int               `json:"runnable"`
	Threshold  int               `json:"warn_below_hours"`
	Unblockers []RunwayUnblocker `json:"unblockers"`
}

// Runway is the runway now.
func (s *Store) Runway(ctx context.Context) (RunwayReport, error) {
	var out RunwayReport
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, _, err = s.runway(tx)
		return err
	})
	return out, err
}

// runway computes the report, and whether the loop runs at all (not stopped, some agents).
func (s *Store) runway(tx *sql.Tx) (RunwayReport, bool, error) {
	var out RunwayReport
	ctl, err := s.loadControl(tx)
	if err != nil {
		return out, false, err
	}
	out.Threshold = ctl.RunwayWarnHours
	rows, err := tx.Query(`SELECT ` + itemCols + ` FROM items WHERE stage NOT IN ('done','closed')`)
	if err != nil {
		return out, false, err
	}
	var open []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return out, false, err
		}
		open = append(open, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, false, err
	}
	deps, err := dependencyStages(tx, open)
	if err != nil {
		return out, false, err
	}
	flagged, err := unservableMissing(tx)
	if err != nil {
		return out, false, err
	}
	claimed := map[int64]bool{}
	lrows, err := tx.Query(`SELECT item_id FROM leases WHERE ended_at IS NULL`)
	if err != nil {
		return out, false, err
	}
	for lrows.Next() {
		var id int64
		if err := lrows.Scan(&id); err != nil {
			lrows.Close()
			return out, false, err
		}
		claimed[id] = true
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return out, false, err
	}
	nodes := map[int64]pipeline.RunwayNode{}
	for id, st := range deps {
		nodes[id] = pipeline.RunwayNode{Stage: st}
	}
	sha := map[int64]string{}
	for _, it := range open {
		nodes[it.ID] = pipeline.RunwayNode{Stage: it.Stage, Claimed: claimed[it.ID], DependsOn: it.DependsOn, Priority: it.Priority,
			Unservable: len(flagged[it.ID]) > 0 && !claimed[it.ID], Trust: it.Trust != nil && it.Trust.By == "" && it.Stage == pipeline.NeedsYou}
		switch {
		case it.Stage == pipeline.Held:
			sha[it.ID] = it.HeadSHA
		case it.Trust != nil:
			sha[it.ID] = it.Trust.HeadSHA
		}
	}
	durations, err := stageDurations(tx)
	if err != nil {
		return out, false, err
	}
	r := pipeline.ComputeRunway(nodes, durations)
	out.Hours, out.Runnable = math.Round(r.Hours*10)/10, len(r.Runnable)
	out.Unblockers = []RunwayUnblocker{}
	for _, u := range r.Unblockers {
		ru := RunwayUnblocker{Unblocker: u}
		if u.Action != pipeline.UnblockAnswer {
			ru.SHA = sha[u.ItemID]
		}
		out.Unblockers = append(out.Unblockers, ru)
	}
	return out, !ctl.Stopped && ctl.ActiveAgents > 0, nil
}

// stageDurations is each work stage's median run time over its recent runs that finished (the
// result advanced or queued the item); a stage with none is missing.
func stageDurations(tx *sql.Tx) (map[pipeline.Stage]time.Duration, error) {
	out := map[pipeline.Stage]time.Duration{}
	for _, st := range []pipeline.Stage{pipeline.Triage, pipeline.Build, pipeline.Review, pipeline.Land} {
		rows, err := tx.Query(`SELECT ended_at - claimed_at FROM leases WHERE stage = ? AND end_reason IN ('advanced','queued')
			ORDER BY ended_at DESC LIMIT ?`, string(st), runwayRecentRuns)
		if err != nil {
			return nil, err
		}
		var ds []int64
		for rows.Next() {
			var d int64
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return nil, err
			}
			ds = append(ds, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(ds) > 0 {
			slices.Sort(ds)
			out[st] = time.Duration(ds[len(ds)/2])
		}
	}
	return out, nil
}

// lastUserAction is the newest action of the user's since since (theirs in Slack or the CLI, not
// one the orchestrator took for them, or a message of the approver's to the orchestrator), or
// zero when there is none.
func lastUserAction(tx *sql.Tx, since time.Time) (time.Time, error) {
	var at sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(at) FROM events WHERE at >= ? AND (
		(actor LIKE 'slack:%' OR actor LIKE 'user:%') AND json_extract(data, '$.via') IS NULL
		OR kind = ? AND json_extract(data, '$.approver') = 1)`, since.UnixNano(), KindOrchMessage).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return fromNanos(at.Int64), nil
}

// runwayCheck records the dry-spell warning when it is due (pipeline.RunwayWarn), and keeps
// orch_state's flag of one per dry spell.
func (s *Store) runwayCheck(tx *sql.Tx, now time.Time) error {
	rep, running, err := s.runway(tx)
	if err != nil {
		return err
	}
	var warned bool
	if err := tx.QueryRow(`SELECT runway_warned FROM orch_state WHERE id = 1`).Scan(&warned); err != nil {
		return err
	}
	threshold := rep.Threshold
	if !running {
		threshold = 0 // the user paused the loop: nothing runs dry
	}
	r := pipeline.Runway{Hours: rep.Hours}
	for _, u := range rep.Unblockers {
		r.Unblockers = append(r.Unblockers, u.Unblocker)
	}
	away := false
	if send, _ := pipeline.RunwayWarn(r, threshold, true, warned); send {
		last, err := lastUserAction(tx, now.Add(-pipeline.RunwayAwayAfter))
		if err != nil {
			return err
		}
		away = pipeline.UserAway(last, now, s.orchLoc())
	}
	send, keep := pipeline.RunwayWarn(r, threshold, away, warned)
	if keep != warned {
		if _, err := tx.Exec(`UPDATE orch_state SET runway_warned = ? WHERE id = 1`, keep); err != nil {
			return err
		}
	}
	if !send {
		return nil
	}
	var ubs []map[string]any
	for _, u := range rep.Unblockers[:min(len(rep.Unblockers), MaxRunwayUnblockers)] {
		ub := map[string]any{"item": u.ItemID, "action": u.Action, "frees": u.Frees}
		if u.SHA != "" {
			ub["sha"] = u.SHA
		}
		ubs = append(ubs, ub)
	}
	return s.event(tx, Event{Actor: "loopd", Kind: KindRunwayLow, Data: map[string]any{
		"hours": rep.Hours, "threshold": rep.Threshold, "runnable": rep.Runnable, "unblockers": ubs}})
}
