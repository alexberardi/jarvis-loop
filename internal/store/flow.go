package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The flow panel (docs/decisions/2026-10-18-flow-panel.md): how the loop is doing over a window,
// read from the events and the leases. GET /flow serves it as JSON, /board/flow as a page.

// FlowWindows are the windows the panel offers, and DefaultFlowWindow the one it opens on.
var FlowWindows = []string{"6h", "24h", "7d"}

// DefaultFlowWindow is the window when none is asked for.
const DefaultFlowWindow = "24h"

// ParseFlowWindow is a window's length; an unknown one is refused.
func ParseFlowWindow(w string) (time.Duration, error) {
	switch w {
	case "6h":
		return 6 * time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	}
	return 0, errf(pipeline.CodeInvalidField, "window must be one of %s", strings.Join(FlowWindows, ", "))
}

// FlowStages are the stages the panel times, in pipeline order: the work stages, then the ones
// where an item waits on a person.
var FlowStages = []pipeline.Stage{pipeline.Triage, pipeline.Build, pipeline.Review, pipeline.Land, pipeline.QA, pipeline.NeedsYou, pipeline.Held}

// killedReasons are the lease ends that lose the run's work.
var killedReasons = []string{"expired", "overtime", "stopped", "dropped", "frozen"}

// bounceStage is the stage each send-back outcome is counted against: review's bounce, the
// runner's verify (after a build), land's back to build (red CI, a conflict) or back to review.
var bounceStage = map[string]string{
	string(pipeline.Bounce):       "review",
	string(pipeline.VerifyFailed): "verify",
	string(pipeline.BackToBuild):  "land",
	string(pipeline.BackToReview): "land",
}

// Flow is the panel's numbers for one window ending now.
type Flow struct {
	Window string    `json:"window"`
	Since  time.Time `json:"since"`
	Now    time.Time `json:"now"`
	Hours  float64   `json:"hours"`

	// Done counts the items that reached done in the window, Merged the merges; PerHour each
	// hour's (oldest first; the last may be cut short by now).
	Done    int        `json:"done"`
	Merged  int        `json:"merged"`
	PerHour []FlowHour `json:"per_hour"`

	// Stages is the time an item spent in each stage, one sample per pass that ended in the
	// window (a bounce makes a second pass); Run is the agent runs of the stage that finished in
	// it (claim to advance).
	Stages []FlowStage `json:"stages"`
	// ToMerge and ToDone are end to end: from the item's creation to its merge, or to done.
	ToMerge pipeline.Dist `json:"to_merge"`
	ToDone  pipeline.Dist `json:"to_done"`

	// Waiting is why the open items wait right now (pipeline.WaitReason), most first.
	Waiting      []FlowWait `json:"waiting"`
	WaitingTotal int        `json:"waiting_total"`

	// Bounces are the send-backs in the window by the stage that sent them.
	Bounces []FlowBounce `json:"bounces"`

	// Runners is each runner's slot use; ActiveAgents the loop-wide cap now.
	Runners      []FlowRunner `json:"runners"`
	ActiveAgents int          `json:"active_agents"`
	BusyMinutes  float64      `json:"busy_minutes"`

	// Killed is the runs that ended without a result (expired, overtime, stopped, dropped, frozen) and
	// the agent-minutes they used, all lost.
	Killed      []FlowKilled `json:"killed"`
	KilledRuns  int          `json:"killed_runs"`
	LostMinutes float64      `json:"lost_minutes"`

	// Features is the progress of each plan with an open item or one that moved in the window.
	Features []FlowFeature `json:"features"`
}

// FlowHour is one hour of the window.
type FlowHour struct {
	Start  time.Time `json:"start"`
	Done   int       `json:"done"`
	Merged int       `json:"merged"`
}

// FlowStage is the time in one stage.
type FlowStage struct {
	Stage string        `json:"stage"`
	Time  pipeline.Dist `json:"time"`
	Run   pipeline.Dist `json:"run"`
}

// FlowWait is the open items waiting for one reason.
type FlowWait struct {
	Reason string  `json:"reason"`
	Count  int     `json:"count"`
	Items  []int64 `json:"items"`
}

// FlowBounce is one stage's send-backs: how many, by outcome, and the most common reasons.
type FlowBounce struct {
	Stage     string           `json:"stage"`
	Count     int              `json:"count"`
	ByOutcome map[string]int   `json:"by_outcome"`
	Top       []FlowBounceText `json:"top"`
}

// FlowBounceText is a reason given for a send-back (its first line, cut to bounceTextMax), how
// often, and the items it was given for (latest first).
type FlowBounceText struct {
	Text  string  `json:"text"`
	Count int     `json:"count"`
	Items []int64 `json:"items"`
}

// FlowRunner is one runner's slot use in the window. Slots is what it last reported (0:
// unknown, a runner that predates it); CapacityMinutes is slots × the window for a runner seen
// in it, and IdleMinutes what of that was not busy (nil when Slots is unknown).
type FlowRunner struct {
	Runner          string    `json:"runner"`
	Slots           int       `json:"slots"`
	LastSeen        time.Time `json:"last_seen"`
	Runs            int       `json:"runs"`
	BusyMinutes     float64   `json:"busy_minutes"`
	CapacityMinutes float64   `json:"capacity_minutes"`
	IdleMinutes     *float64  `json:"idle_minutes"`
}

// FlowKilled is the killed runs for one reason.
type FlowKilled struct {
	Reason      string  `json:"reason"`
	Runs        int     `json:"runs"`
	LostMinutes float64 `json:"lost_minutes"`
}

// FlowFeature is a plan's items by stage. Feature tags on items (#198) are not on main yet; a
// plan is the grouping there is.
type FlowFeature struct {
	PlanID  int64          `json:"plan_id"`
	Title   string         `json:"title"`
	Stage   string         `json:"stage"`
	Items   int            `json:"items"`
	Done    int            `json:"done"`
	ByStage map[string]int `json:"by_stage"`
}

const (
	bounceTop     = 3
	bounceTextMax = 100
)

// Flow is the panel for the window (FlowWindows) ending now.
func (s *Store) Flow(ctx context.Context, window string) (Flow, error) {
	d, err := ParseFlowWindow(window)
	if err != nil {
		return Flow{}, err
	}
	now := s.clock.Now()
	f := Flow{Window: window, Since: now.Add(-d), Now: now, Hours: d.Hours()}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Flow{}, err
	}
	defer tx.Rollback()
	cs, err := s.claimState(tx, now)
	if err != nil {
		return Flow{}, err
	}
	f.ActiveAgents = cs.ctl.ActiveAgents
	since, until := f.Since.UnixNano(), now.UnixNano()
	for _, step := range []func(*sql.Tx, int64, int64) error{f.throughput, f.stageTimes, f.bounces, f.runs, f.features} {
		if err := step(tx, since, until); err != nil {
			return Flow{}, err
		}
	}
	if err := f.waiting(tx, cs, now); err != nil {
		return Flow{}, err
	}
	return f, nil
}

// throughput is done and merged, per hour, and end to end.
func (f *Flow) throughput(tx *sql.Tx, since, until int64) error {
	n := int(f.Hours)
	f.PerHour = make([]FlowHour, n)
	for i := range f.PerHour {
		f.PerHour[i].Start = f.Since.Add(time.Duration(i) * time.Hour)
	}
	// A merge into done is one event that counts for both.
	rows, err := tx.Query(`SELECT e.at, e.outcome = 'merged', e.to_stage = 'done' AND e.from_stage != 'done', i.created_at FROM events e JOIN items i ON i.id = e.item_id
		WHERE e.at >= ? AND e.at <= ? AND e.item_id IS NOT NULL
		AND (e.outcome = 'merged' OR (e.to_stage = 'done' AND e.from_stage != 'done'))`, since, until)
	if err != nil {
		return err
	}
	defer rows.Close()
	var toMerge, toDone []time.Duration
	for rows.Next() {
		var at, created int64
		var merged, done bool
		if err := rows.Scan(&at, &merged, &done, &created); err != nil {
			return err
		}
		h := min(int((at-since)/int64(time.Hour)), n-1)
		if merged {
			f.Merged++
			f.PerHour[h].Merged++
			toMerge = append(toMerge, time.Duration(at-created))
		}
		if done {
			f.Done++
			f.PerHour[h].Done++
			toDone = append(toDone, time.Duration(at-created))
		}
	}
	f.ToMerge, f.ToDone = pipeline.DistOf(toMerge), pipeline.DistOf(toDone)
	return rows.Err()
}

// stageTimes is each pass through a stage that ended in the window: from the event that moved the
// item into the stage to the next that moved it out (which may be before the window starts).
func (f *Flow) stageTimes(tx *sql.Tx, since, until int64) error {
	rows, err := tx.Query(`SELECT item_id, at, to_stage FROM events
		WHERE item_id IN (SELECT item_id FROM events WHERE at >= ? AND at <= ? AND item_id IS NOT NULL AND to_stage != from_stage)
		AND to_stage != from_stage AND to_stage != '' ORDER BY item_id, id`, since, until)
	if err != nil {
		return err
	}
	defer rows.Close()
	passes := map[string][]time.Duration{}
	var prevItem, prevAt int64
	prevStage := ""
	for rows.Next() {
		var item, at int64
		var stage string
		if err := rows.Scan(&item, &at, &stage); err != nil {
			return err
		}
		if item == prevItem && prevStage != "" && at >= since {
			passes[prevStage] = append(passes[prevStage], time.Duration(at-prevAt))
		}
		prevItem, prevAt, prevStage = item, at, stage
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range FlowStages {
		f.Stages = append(f.Stages, FlowStage{Stage: string(st), Time: pipeline.DistOf(passes[string(st)])})
	}
	return nil
}

// bounces is the send-backs in the window, by stage, with the reasons given most often.
func (f *Flow) bounces(tx *sql.Tx, since, until int64) error {
	rows, err := tx.Query(`SELECT item_id, outcome, data FROM events WHERE at >= ? AND at <= ? AND item_id IS NOT NULL
		AND kind = 'advanced' AND outcome IN ('bounce', 'verify_failed', 'back_to_build', 'back_to_review') ORDER BY id DESC`, since, until)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string]*FlowBounce{}
	texts := map[string]map[string]*FlowBounceText{}
	for rows.Next() {
		var item int64
		var outcome, raw string
		if err := rows.Scan(&item, &outcome, &raw); err != nil {
			return err
		}
		st := bounceStage[outcome]
		b := by[st]
		if b == nil {
			b = &FlowBounce{Stage: st, ByOutcome: map[string]int{}}
			by[st], texts[st] = b, map[string]*FlowBounceText{}
		}
		b.Count++
		b.ByOutcome[outcome]++
		var data map[string]any
		_ = json.Unmarshal([]byte(raw), &data)
		why := bounceText(str(data, "notes") + str(data, "reason"))
		t := texts[st][why]
		if t == nil {
			t = &FlowBounceText{Text: why}
			texts[st][why] = t
		}
		t.Count++
		if !slices.Contains(t.Items, item) {
			t.Items = append(t.Items, item)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range []string{"review", "verify", "land"} {
		b := by[st]
		if b == nil {
			continue
		}
		var top []FlowBounceText
		for _, t := range texts[st] {
			top = append(top, *t)
		}
		// Most often first; a tie goes to the latest (rows came newest first, so the first item).
		sort.Slice(top, func(i, j int) bool {
			if top[i].Count != top[j].Count {
				return top[i].Count > top[j].Count
			}
			return top[i].Items[0] > top[j].Items[0]
		})
		b.Top = top[:min(len(top), bounceTop)]
		f.Bounces = append(f.Bounces, *b)
	}
	return nil
}

// bounceText is a reason as the panel groups it: the first non-empty line, trimmed and cut.
func bounceText(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > bounceTextMax {
				return string(r[:bounceTextMax-1]) + "…"
			}
			return line
		}
	}
	return "(no reason given)"
}

// runs is the agent runs (item leases, plan turns, orchestrate turns) in the window: how long the
// finished ones took per stage, the busy agent-minutes per runner, and the killed runs.
func (f *Flow) runs(tx *sql.Tx, since, until int64) error {
	rows, err := tx.Query(`SELECT stage, runner, claimed_at, ended_at, end_reason FROM leases WHERE ended_at >= ?1
		UNION ALL SELECT stage, runner, claimed_at, ended_at, end_reason FROM leases WHERE ended_at IS NULL
		UNION ALL SELECT 'plan', runner, claimed_at, ended_at, end_reason FROM plan_leases WHERE ended_at >= ?1
		UNION ALL SELECT 'plan', runner, claimed_at, ended_at, end_reason FROM plan_leases WHERE ended_at IS NULL
		UNION ALL SELECT 'orchestrate', runner, claimed_at, CASE WHEN status = 'running' THEN NULL ELSE updated_at END, 'advanced'
			FROM orch_turns WHERE claimed_at > 0 AND (status = 'running' OR updated_at >= ?1)`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	runs := map[string][]time.Duration{}
	busy := map[string]*FlowRunner{}
	killed := map[string]*FlowKilled{}
	for rows.Next() {
		var stage, runner, reason string
		var claimed int64
		var ended sql.NullInt64
		if err := rows.Scan(&stage, &runner, &claimed, &ended, &reason); err != nil {
			return err
		}
		end := until
		if ended.Valid {
			end = ended.Int64
		}
		r := busy[runner]
		if r == nil {
			r = &FlowRunner{Runner: runner}
			busy[runner] = r
		}
		r.Runs++
		r.BusyMinutes += minutes(max(end, since) - max(claimed, since))
		if !ended.Valid {
			continue
		}
		took := time.Duration(end - claimed)
		switch {
		case reason == "advanced":
			runs[stage] = append(runs[stage], took)
		case slices.Contains(killedReasons, reason):
			k := killed[reason]
			if k == nil {
				k = &FlowKilled{Reason: reason}
				killed[reason] = k
			}
			k.Runs++
			k.LostMinutes += minutes(int64(took))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range f.Stages {
		f.Stages[i].Run = pipeline.DistOf(runs[f.Stages[i].Stage])
	}
	for _, reason := range killedReasons {
		if k := killed[reason]; k != nil {
			k.LostMinutes = round1(k.LostMinutes)
			f.Killed = append(f.Killed, *k)
			f.KilledRuns += k.Runs
			f.LostMinutes += k.LostMinutes
		}
	}
	f.LostMinutes = round1(f.LostMinutes)

	seen, err := tx.Query(`SELECT name, slots, last_seen FROM runners`)
	if err != nil {
		return err
	}
	defer seen.Close()
	for seen.Next() {
		var name string
		var slots int
		var last int64
		if err := seen.Scan(&name, &slots, &last); err != nil {
			return err
		}
		r := busy[name]
		if r == nil {
			if last < since {
				continue
			}
			r = &FlowRunner{Runner: name}
			busy[name] = r
		}
		r.Slots, r.LastSeen = slots, fromNanos(last)
	}
	if err := seen.Err(); err != nil {
		return err
	}
	for _, r := range busy {
		r.BusyMinutes = round1(r.BusyMinutes)
		f.BusyMinutes += r.BusyMinutes
		if r.Slots > 0 {
			if r.LastSeen.UnixNano() >= since {
				r.CapacityMinutes = round1(float64(r.Slots) * minutes(until-since))
			}
			idle := round1(max(r.CapacityMinutes-r.BusyMinutes, 0))
			r.IdleMinutes = &idle
		}
		f.Runners = append(f.Runners, *r)
	}
	f.BusyMinutes = round1(f.BusyMinutes)
	sort.Slice(f.Runners, func(i, j int) bool { return f.Runners[i].Runner < f.Runners[j].Runner })
	return nil
}

func minutes(ns int64) float64 { return float64(ns) / float64(time.Minute) }

func round1(x float64) float64 { return float64(int64(x*10+0.5)) / 10 }

// features is each plan's items by stage, for the plans with an open item or an item that moved
// in the window.
func (f *Flow) features(tx *sql.Tx, since, _ int64) error {
	rows, err := tx.Query(`SELECT p.id, p.title, p.stage, i.stage, COUNT(*), MAX(i.updated_at) FROM items i JOIN plans p ON p.id = i.plan_id
		GROUP BY p.id, i.stage ORDER BY p.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []FlowFeature
	keep := map[int64]bool{}
	for rows.Next() {
		var id, updated int64
		var title, pstage, stage string
		var n int
		if err := rows.Scan(&id, &title, &pstage, &stage, &n, &updated); err != nil {
			return err
		}
		if len(out) == 0 || out[len(out)-1].PlanID != id {
			out = append(out, FlowFeature{PlanID: id, Title: title, Stage: pstage, ByStage: map[string]int{}})
		}
		ft := &out[len(out)-1]
		ft.Items += n
		ft.ByStage[stage] = n
		switch pipeline.Stage(stage) {
		case pipeline.Done:
			ft.Done += n
		case pipeline.Closed:
		default:
			keep[id] = true
		}
		if updated >= since {
			keep[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ft := range out {
		if keep[ft.PlanID] {
			f.Features = append(f.Features, ft)
		}
	}
	return nil
}

// waiting classifies every open item that waits right now, with the claim's own snapshot (cs:
// its pace too) and the builders the runners seen by now offer.
func (f *Flow) waiting(tx *sql.Tx, cs claimState, now time.Time) error {
	items := slices.Clone(cs.waiting)
	rows, err := tx.Query(`SELECT ` + itemCols + ` FROM items WHERE stage IN ('needs_you', 'held', 'qa')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	deps, err := dependencyStages(tx, items)
	if err != nil {
		return err
	}
	flagged, err := unservableMissing(tx)
	if err != nil {
		return err
	}
	slices.SortFunc(items, func(a, b pipeline.Item) int { return int(a.ID - b.ID) })
	builders := pipeline.RunnerBuilders(cs.runners, now)
	by := map[string]*FlowWait{}
	for _, it := range items {
		why := pipeline.WaitReason(cs.ctl, cs.pace, it, deps, cs.live, flagged[it.ID], cs.frozen, cs.holds, builders)
		if why == "" {
			continue
		}
		w := by[why]
		if w == nil {
			w = &FlowWait{Reason: why}
			by[why] = w
		}
		w.Count++
		w.Items = append(w.Items, it.ID)
		f.WaitingTotal++
	}
	for _, w := range by {
		f.Waiting = append(f.Waiting, *w)
	}
	sort.Slice(f.Waiting, func(i, j int) bool {
		if f.Waiting[i].Count != f.Waiting[j].Count {
			return f.Waiting[i].Count > f.Waiting[j].Count
		}
		return f.Waiting[i].Reason < f.Waiting[j].Reason
	})
	return nil
}
