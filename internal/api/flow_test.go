package api

import (
	"testing"
	"time"
)

// GET /flow is the flow panel's numbers for the orchestrator and Claude sessions: any reading
// token, a window of 6h, 24h (the default) or 7d. A claim's slots reach the runner's row.
func TestFlow(t *testing.T) {
	e := newEnv(t)
	tri := e.addItem("triaged now")
	e.addItem("waits for the triage limit")
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a1", "stages": []string{"triage"}, "slots": 3}, 200, nil)
	e.refused(runnerTok, "POST", "/claim", map[string]any{"agent": "a2", "slots": -1}, 400, "invalid_field")
	e.clock.Advance(2 * time.Minute)

	type flow struct {
		Window       string `json:"window"`
		Hours        float64
		WaitingTotal int `json:"waiting_total"`
		Waiting      []struct {
			Reason string  `json:"reason"`
			Items  []int64 `json:"items"`
		} `json:"waiting"`
		Runners []struct {
			Runner      string   `json:"runner"`
			Slots       int      `json:"slots"`
			BusyMinutes float64  `json:"busy_minutes"`
			IdleMinutes *float64 `json:"idle_minutes"`
		} `json:"runners"`
		PerHour []struct{} `json:"per_hour"`
	}
	var f flow
	e.do(orchTok, "GET", "/flow", nil, 200, &f)
	if f.Window != "24h" || len(f.PerHour) != 24 || f.WaitingTotal != 1 || f.Waiting[0].Reason != "limit triage:1" || f.Waiting[0].Items[0] != tri+1 {
		t.Fatalf("flow %+v", f)
	}
	if len(f.Runners) != 1 || f.Runners[0].Runner != "devbox" || f.Runners[0].Slots != 3 || f.Runners[0].BusyMinutes != 2 ||
		f.Runners[0].IdleMinutes == nil || *f.Runners[0].IdleMinutes != 3*24*60-2 {
		t.Fatalf("runners %+v", f.Runners)
	}
	e.do(runnerTok, "GET", "/flow?window=7d", nil, 200, &f)
	if f.Window != "7d" || len(f.PerHour) != 168 {
		t.Fatalf("7d: %s %d", f.Window, len(f.PerHour))
	}
	e.do(userTok, "GET", "/flow?window=6h", nil, 200, &f)
	e.refused(userTok, "GET", "/flow?window=1y", nil, 400, "invalid_field")
	e.refused("", "GET", "/flow", nil, 401, "unauthorized")
}
