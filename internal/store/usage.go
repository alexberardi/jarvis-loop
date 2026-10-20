package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The usage ledger and claim pacing (docs/decisions/2026-10-18-usage-awareness.md).

// KindUsagePace is the event (no item) of a change of claude's pace: slow, paused (a limit hit or
// the budget), back to normal.
const KindUsagePace = "usage_pace"

// usageZone is where the CLI's reset texts ("resets 3:45pm") and the days are read: the user's.
func usageZone() *time.Location {
	loc, err := time.LoadLocation(OrchDayZone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func fieldInt(f map[string]string, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(f[k]), 10, 64)
	return n
}

// usageRunFrom reads a run's usage from its advance fields; false when the run reported none.
func usageRunFrom(itemID int64, stage string, f map[string]string, now time.Time) (pipeline.UsageRun, bool) {
	r := pipeline.UsageRun{At: now, Started: now.Add(-time.Duration(fieldInt(f, "run_seconds")) * time.Second), ItemID: itemID, Stage: stage, Model: f["model"]}
	switch {
	case f["qwen_tokens_input"] != "":
		r.Provider, r.Input, r.Output, r.Requests = pipeline.ProviderQwen, fieldInt(f, "qwen_tokens_input"), fieldInt(f, "qwen_tokens_output"), fieldInt(f, "qwen_requests")
	case f["usage_input"] != "":
		r.Provider = pipeline.ProviderClaude
		r.Input, r.Output = fieldInt(f, "usage_input"), fieldInt(f, "usage_output")
		r.CacheCreation, r.CacheRead = fieldInt(f, "usage_cache_creation"), fieldInt(f, "usage_cache_read")
		r.CostUSD, _ = strconv.ParseFloat(f["total_cost_usd"], 64)
		r.Warning = f["rate_limit_status"] == "allowed_warning"
	default:
		return r, false
	}
	return r, true
}

func insertUsageRun(tx *sql.Tx, r pipeline.UsageRun) error {
	warn := 0
	if r.Warning {
		warn = 1
	}
	_, err := tx.Exec(`INSERT INTO usage_runs (at, started, item_id, stage, provider, model, input, output, cache_creation, cache_read, cost_usd, requests, warning)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.At.UnixNano(), r.Started.UnixNano(), r.ItemID, r.Stage, r.Provider, r.Model,
		r.Input, r.Output, r.CacheCreation, r.CacheRead, r.CostUSD, r.Requests, warn)
	return err
}

// usageRuns are the runs since since (all providers when provider is ""), oldest first.
func usageRuns(q querier, provider string, since time.Time) ([]pipeline.UsageRun, error) {
	rows, err := q.Query(`SELECT at, started, COALESCE(item_id, 0), stage, provider, model, input, output, cache_creation, cache_read, cost_usd, requests, warning
		FROM usage_runs WHERE at >= ? AND (? = '' OR provider = ?) ORDER BY at, id`, since.UnixNano(), provider, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.UsageRun
	for rows.Next() {
		var r pipeline.UsageRun
		var at, started int64
		var warn int
		if err := rows.Scan(&at, &started, &r.ItemID, &r.Stage, &r.Provider, &r.Model, &r.Input, &r.Output, &r.CacheCreation, &r.CacheRead, &r.CostUSD, &r.Requests, &warn); err != nil {
			return nil, err
		}
		r.At, r.Started, r.Warning = time.Unix(0, at).UTC(), time.Unix(0, started).UTC(), warn != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// usageHits are the provider's latest limit hits, oldest first.
func usageHits(q querier, provider string) ([]pipeline.UsageHit, error) {
	rows, err := q.Query(`SELECT at, provider, limit_window, reset_at, burn, COALESCE(item_id, 0), error FROM usage_hits
		WHERE provider = ? ORDER BY at DESC, id DESC LIMIT 20`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.UsageHit
	for rows.Next() {
		var h pipeline.UsageHit
		var at, reset int64
		if err := rows.Scan(&at, &h.Provider, &h.Window, &reset, &h.Burn, &h.ItemID, &h.Error); err != nil {
			return nil, err
		}
		h.At = time.Unix(0, at).UTC()
		if reset != 0 {
			h.ResetAt = time.Unix(0, reset).UTC()
		}
		out = append(out, h)
	}
	slices.Reverse(out)
	return out, rows.Err()
}

func budgetOf(ctl pipeline.Control, provider string) pipeline.Budget {
	if b, ok := ctl.Budgets[provider]; ok {
		return b
	}
	return pipeline.Budget{}
}

// usageState is the provider's usage now.
func usageState(q querier, ctl pipeline.Control, provider string, now time.Time) (pipeline.UsageState, error) {
	runs, err := usageRuns(q, provider, now.Add(-pipeline.WeekLen-pipeline.Session))
	if err != nil {
		return pipeline.UsageState{}, err
	}
	hits, err := usageHits(q, provider)
	if err != nil {
		return pipeline.UsageState{}, err
	}
	return pipeline.ComputeUsage(provider, budgetOf(ctl, provider), runs, hits, now), nil
}

// recordHit records a usage limit a run reported (its usage already in the ledger): the window,
// the reset read from the CLI's text, and the window's burn now, which is the learned limit.
func recordHit(tx *sql.Tx, ctl pipeline.Control, itemID int64, f map[string]string, now time.Time) (pipeline.UsageHit, error) {
	h := pipeline.UsageHit{At: now, Provider: pipeline.ProviderClaude, ItemID: itemID, Error: f["error"]}
	if w := f["limit_window"]; w == pipeline.Window5h || w == pipeline.WindowWeek {
		h.Window = w
	} else {
		h.Window = pipeline.LimitWindow(f["error"], "")
	}
	if t, ok := pipeline.ParseReset(f["reset"], now, usageZone()); ok && t.After(now) {
		h.ResetAt = t
	}
	st, err := usageState(tx, ctl, h.Provider, now)
	if err != nil {
		return h, err
	}
	h.Burn = st.Burn5h
	if h.Window == pipeline.WindowWeek {
		h.Burn = st.BurnWeek
	}
	var reset int64
	if !h.ResetAt.IsZero() {
		reset = h.ResetAt.UnixNano()
	}
	_, err = tx.Exec(`INSERT INTO usage_hits (at, provider, limit_window, reset_at, burn, item_id, error) VALUES (?,?,?,?,?,?,?)`,
		now.UnixNano(), h.Provider, h.Window, reset, h.Burn, itemID, h.Error)
	return h, err
}

// paceNow is claude's pace now and the usage it comes from, recording nothing (a read-only tx may
// call it).
func paceNow(tx *sql.Tx, ctl pipeline.Control, now time.Time) (pipeline.Pace, pipeline.UsageState, error) {
	st, err := usageState(tx, ctl, pipeline.ProviderClaude, now)
	if err != nil {
		return pipeline.Pace{}, st, err
	}
	return pipeline.PaceFor(ctl, budgetOf(ctl, pipeline.ProviderClaude), st), st, nil
}

// pace is claude's pace now. It records a usage_pace event when the level changed since the last
// one (or, when slow, the number at once did); extra goes on that event (a hit's item and error).
func (s *Store) pace(tx *sql.Tx, ctl pipeline.Control, now time.Time, extra map[string]any) (pipeline.Pace, error) {
	p, st, err := paceNow(tx, ctl, now)
	if err != nil {
		return p, err
	}
	var last struct{ Level, Agents int }
	err = tx.QueryRow(`SELECT level, agents FROM usage_pace WHERE provider = ?`, pipeline.ProviderClaude).Scan(&last.Level, &last.Agents)
	if err != nil && err != sql.ErrNoRows {
		return p, err
	}
	if last.Level == p.Level && (p.Level != pipeline.PaceSlow || last.Agents == p.Agents) && extra == nil {
		return p, nil
	}
	if _, err := tx.Exec(`INSERT INTO usage_pace (provider, level, agents) VALUES (?,?,?)
		ON CONFLICT (provider) DO UPDATE SET level = excluded.level, agents = excluded.agents`, pipeline.ProviderClaude, p.Level, p.Agents); err != nil {
		return p, err
	}
	data := map[string]any{"provider": pipeline.ProviderClaude, "level": p.Level, "from_level": last.Level, "agents": p.Agents,
		"active_agents": ctl.ActiveAgents, "window": p.Window, "percent": p.Percent, "hit": p.Hit, "about": p.Describe(usageZone())}
	if p.Lifted {
		data["lifted"] = true
	}
	if !p.Until.IsZero() {
		data["until"] = p.Until.Format(time.RFC3339)
	}
	if t, ok := pipeline.Forecast(st); ok {
		data["forecast"] = t.Format(time.RFC3339)
	}
	for k, v := range extra {
		data[k] = v
	}
	return p, s.event(tx, Event{Actor: "loopd", Kind: KindUsagePace, Data: data})
}

// UsageItem is one item's usage in the report.
type UsageItem struct {
	ItemID  int64   `json:"item_id"`
	Title   string  `json:"title"`
	Burn    int64   `json:"burn"`
	CostUSD float64 `json:"cost_usd"`
	Runs    int     `json:"runs"`
}

// UsageTotal is a group's usage (a day, a stage, a provider).
type UsageTotal struct {
	Key      string  `json:"key"`
	Provider string  `json:"provider"`
	Burn     int64   `json:"burn"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Runs     int     `json:"runs"`
}

// UsageReport is the usage view (GET /usage, the board, `loop usage`).
type UsageReport struct {
	Now      time.Time           `json:"now"`
	Claude   pipeline.UsageState `json:"claude"`
	Pace     pipeline.Pace       `json:"pace"`
	Budget   pipeline.Budget     `json:"budget"`
	Forecast *time.Time          `json:"forecast,omitempty"`
	// Days are the last 7 days (in the user's zone), per provider, newest first.
	Days []UsageTotal `json:"days"`
	// Stages are this week's usage per stage and provider.
	Stages []UsageTotal `json:"stages"`
	// Items are this week's most expensive items (claude's burn).
	Items []UsageItem         `json:"items"`
	Hits  []pipeline.UsageHit `json:"hits"`
}

// Usage reports the usage now: claude's windows and pace, the last 7 days, the week per stage and
// its most expensive items, and the limits hit.
func (s *Store) Usage(ctx context.Context) (UsageReport, error) {
	now := s.clock.Now()
	rep := UsageReport{Now: now}
	ctl, err := s.loadControl(s.r)
	if err != nil {
		return rep, err
	}
	if rep.Claude, err = usageState(s.r, ctl, pipeline.ProviderClaude, now); err != nil {
		return rep, err
	}
	rep.Budget = budgetOf(ctl, pipeline.ProviderClaude)
	rep.Pace = pipeline.PaceFor(ctl, rep.Budget, rep.Claude)
	if t, ok := pipeline.Forecast(rep.Claude); ok {
		rep.Forecast = &t
	}
	if rep.Hits, err = usageHits(s.r, pipeline.ProviderClaude); err != nil {
		return rep, err
	}
	slices.Reverse(rep.Hits)
	if len(rep.Hits) > 5 {
		rep.Hits = rep.Hits[:5]
	}
	loc := usageZone()
	t := now.In(loc)
	firstDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -6)
	runs, err := usageRuns(s.r, "", firstDay)
	if err != nil {
		return rep, err
	}
	add := func(m map[[2]string]*UsageTotal, key string, r pipeline.UsageRun) {
		k := [2]string{key, r.Provider}
		g := m[k]
		if g == nil {
			g = &UsageTotal{Key: key, Provider: r.Provider}
			m[k] = g
		}
		g.Burn += r.Burn()
		g.Tokens += r.Input + r.Output + r.CacheCreation + r.CacheRead
		g.CostUSD += r.CostUSD
		g.Runs++
	}
	days, stages := map[[2]string]*UsageTotal{}, map[[2]string]*UsageTotal{}
	items := map[int64]*UsageItem{}
	for _, r := range runs {
		add(days, r.At.In(loc).Format(time.DateOnly), r)
		if r.At.Before(now.Add(-pipeline.WeekLen)) {
			continue
		}
		add(stages, r.Stage, r)
		if r.Provider != pipeline.ProviderClaude || r.ItemID == 0 {
			continue
		}
		it := items[r.ItemID]
		if it == nil {
			it = &UsageItem{ItemID: r.ItemID}
			items[r.ItemID] = it
		}
		it.Burn += r.Burn()
		it.CostUSD += r.CostUSD
		it.Runs++
	}
	rep.Days = sortedTotals(days, func(a, b UsageTotal) bool { return a.Key > b.Key })
	rep.Stages = sortedTotals(stages, func(a, b UsageTotal) bool { return a.Burn > b.Burn })
	for _, it := range items {
		rep.Items = append(rep.Items, *it)
	}
	sort.Slice(rep.Items, func(i, j int) bool {
		if rep.Items[i].Burn != rep.Items[j].Burn {
			return rep.Items[i].Burn > rep.Items[j].Burn
		}
		return rep.Items[i].ItemID < rep.Items[j].ItemID
	})
	if len(rep.Items) > 5 {
		rep.Items = rep.Items[:5]
	}
	for i := range rep.Items {
		_ = s.r.QueryRow(`SELECT title FROM items WHERE id = ?`, rep.Items[i].ItemID).Scan(&rep.Items[i].Title)
	}
	return rep, nil
}

func sortedTotals(m map[[2]string]*UsageTotal, less func(a, b UsageTotal) bool) []UsageTotal {
	out := make([]UsageTotal, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if less(out[i], out[j]) != less(out[j], out[i]) {
			return less(out[i], out[j])
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}
