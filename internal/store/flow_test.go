package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// flowFixture writes items, events and leases straight into the tables, at the times given (hours
// after t0), so the flow panel's numbers can be worked out by hand.
type flowFixture struct {
	t *testing.T
	s *Store
}

func h(x float64) int64 { return t0.Add(time.Duration(x * float64(time.Hour))).UnixNano() }

func (f flowFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.s.w.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f flowFixture) item(id int64, stage string, created float64, plan int64, deps string) {
	f.t.Helper()
	f.exec(`INSERT INTO items (id, title, source, priority, stage, created_at, updated_at, plan_id, depends_on) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, fmt.Sprintf("item %d", id), "user", "P3", stage, h(created), h(created), plan, deps)
}

// ev is an event; kind created when from is "".
func (f flowFixture) ev(item int64, at float64, from, to, outcome, data string) {
	f.t.Helper()
	kind := "advanced"
	if from == "" {
		kind = "created"
	}
	if data == "" {
		data = "{}"
	}
	f.exec(`INSERT INTO events (item_id, at, actor, kind, from_stage, to_stage, outcome, data) VALUES (?,?,?,?,?,?,?,?)`,
		item, h(at), "test", kind, from, to, outcome, data)
}

// lease is a run; end < 0 for one still live.
func (f flowFixture) lease(id string, item int64, stage, runner string, claimed, end float64, reason string) {
	f.t.Helper()
	var ended any
	if end >= 0 {
		ended = h(end)
	}
	f.exec(`INSERT INTO leases (id, item_id, stage, runner, agent, claimed_at, expires_at, ended_at, end_reason) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, item, stage, runner, runner+"-1", h(claimed), h(claimed+1), ended, reason)
}

func TestFlow(t *testing.T) {
	s, clk := open(t)
	f := flowFixture{t, s}
	clk.Advance(30 * time.Hour) // now = t0+30h; the 24h window starts at t0+6h

	f.exec(`INSERT INTO plans (id, title, stage, priority, created_at, updated_at) VALUES (1, 'Voice', 'split', 'P2', ?, ?), (2, 'Old', 'done', 'P3', ?, ?)`,
		h(0), h(0), h(0), h(0))
	// 1: through every stage with one review bounce, merged into done at +14h.
	f.item(1, "done", 5, 1, "")
	f.ev(1, 5, "", "triage", "", "")
	f.ev(1, 7, "triage", "build", "reproduced", "")
	f.ev(1, 9, "build", "review", "pr_opened", "")
	f.ev(1, 10, "review", "build", "bounce", `{"notes":"add a test\nfor the empty case"}`)
	f.ev(1, 11, "build", "build", "", "") // a note, not a move: no pass ends
	f.ev(1, 12, "build", "review", "pr_opened", "")
	f.ev(1, 13, "review", "land", "approved", "")
	f.ev(1, 14, "land", "done", "merged", "")
	// 2: its triage pass ended before the window; its build pass (7h) in it; it asks the user.
	f.item(2, "needs_you", 0, 0, "")
	f.ev(2, 0, "", "triage", "", "")
	f.ev(2, 1, "triage", "build", "reproduced", "")
	f.ev(2, 8, "build", "needs_you", "question", "")
	// 3 waits on 2; 4 on the triage limit (5 holds it); 6 is held after a 1h triage.
	f.item(3, "build", 25, 1, "2")
	f.item(4, "triage", 25, 0, "")
	f.item(5, "triage", 25, 0, "")
	f.item(6, "held", 25, 0, "")
	f.ev(6, 25, "", "triage", "", "")
	f.ev(6, 26, "triage", "held", "hold", "")
	// 7: 10h in land, merged into qa.
	f.item(7, "qa", 10, 0, "")
	f.ev(7, 10, "", "land", "", "")
	f.ev(7, 20, "land", "qa", "merged", "")
	// 8, 9: ready builds, each failed the runner's verify for the same reason.
	f.item(8, "build", 2, 0, "")
	f.item(9, "build", 2, 0, "")
	f.ev(8, 15, "build", "build", "verify_failed", `{"reason":"  check: FAIL  \nlog follows"}`)
	f.ev(9, 16, "build", "build", "verify_failed", `{"reason":"check: FAIL"}`)
	// 10: an old plan's done item, out of the window.
	f.item(10, "done", 0, 2, "")
	// A land send-back (back to build on a conflict) counts against land.
	f.ev(9, 17, "land", "build", "back_to_build", `{"reason":"conflict"}`)

	f.lease("L0", 2, "build", "devbox", 1, 2, "advanced") // ended before the window
	f.lease("L1", 1, "build", "devbox", 7, 8, "advanced")
	f.lease("L2", 1, "build", "mac", 3, 7, "advanced") // 4h run, 1h of it in the window
	f.lease("L3", 2, "review", "devbox", 20, 20.5, "expired")
	f.lease("L4", 2, "review", "devbox", 21, 21.75, "overtime")
	f.lease("L5", 5, "triage", "devbox", 29, -1, "")
	f.exec(`INSERT INTO orch_turns (status, runner, claimed_at, created_at, updated_at) VALUES ('done', 'devbox', ?, ?, ?)`, h(10), h(10), h(10.5))
	f.exec(`INSERT INTO runners (name, capabilities, last_seen, slots) VALUES ('devbox', '', ?, 2), ('mac', 'macos', ?, 0), ('old', '', ?, 1)`,
		h(30), h(29), h(0))

	got, err := s.Flow(context.Background(), "24h")
	if err != nil {
		t.Fatal(err)
	}
	if got.Window != "24h" || !got.Since.Equal(t0.Add(6*time.Hour)) || got.Hours != 24 {
		t.Fatalf("window: %s %v %v", got.Window, got.Since, got.Hours)
	}
	if got.Done != 1 || got.Merged != 2 {
		t.Errorf("done %d merged %d, want 1 and 2", got.Done, got.Merged)
	}
	if len(got.PerHour) != 24 || got.PerHour[8] != (FlowHour{Start: t0.Add(14 * time.Hour), Done: 1, Merged: 1}) || got.PerHour[14].Merged != 1 {
		t.Errorf("per hour: %+v", got.PerHour)
	}
	hr := func(x int64) int64 { return x * 3600 }
	dist := func(n int, med, p90 int64) pipeline.Dist {
		return pipeline.Dist{Count: n, MedianSeconds: med, P90Seconds: p90}
	}
	if want := dist(2, hr(9), hr(10)); got.ToMerge != want {
		t.Errorf("to merge %+v, want %+v", got.ToMerge, want)
	}
	if want := dist(1, hr(9), hr(9)); got.ToDone != want {
		t.Errorf("to done %+v, want %+v", got.ToDone, want)
	}
	wantStages := []FlowStage{
		{Stage: "triage", Time: dist(2, hr(1), hr(2))},
		{Stage: "build", Time: dist(3, hr(2), hr(7)), Run: dist(2, hr(1), hr(4))},
		{Stage: "review", Time: dist(2, hr(1), hr(1))},
		{Stage: "land", Time: dist(2, hr(1), hr(10))},
		{Stage: "qa"}, {Stage: "needs_you"}, {Stage: "held"},
	}
	if !reflect.DeepEqual(got.Stages, wantStages) {
		t.Errorf("stages:\n got %+v\nwant %+v", got.Stages, wantStages)
	}

	wantWait := []FlowWait{
		{Reason: "ready", Count: 2, Items: []int64{8, 9}},
		{Reason: "dependencies", Count: 1, Items: []int64{3}},
		{Reason: "held for approval", Count: 1, Items: []int64{6}},
		{Reason: "limit triage:1", Count: 1, Items: []int64{4}},
		{Reason: "needs you", Count: 1, Items: []int64{2}},
		{Reason: "qa", Count: 1, Items: []int64{7}},
	}
	if !reflect.DeepEqual(got.Waiting, wantWait) || got.WaitingTotal != 7 {
		t.Errorf("waiting (%d):\n got %+v\nwant %+v", got.WaitingTotal, got.Waiting, wantWait)
	}

	wantBounces := []FlowBounce{
		{Stage: "review", Count: 1, ByOutcome: map[string]int{"bounce": 1}, Top: []FlowBounceText{{Text: "add a test", Count: 1, Items: []int64{1}}}},
		{Stage: "verify", Count: 2, ByOutcome: map[string]int{"verify_failed": 2}, Top: []FlowBounceText{{Text: "check: FAIL", Count: 2, Items: []int64{9, 8}}}},
		{Stage: "land", Count: 1, ByOutcome: map[string]int{"back_to_build": 1}, Top: []FlowBounceText{{Text: "conflict", Count: 1, Items: []int64{9}}}},
	}
	if !reflect.DeepEqual(got.Bounces, wantBounces) {
		t.Errorf("bounces:\n got %+v\nwant %+v", got.Bounces, wantBounces)
	}

	idle := 2*24*60 - 225.0
	wantRunners := []FlowRunner{
		{Runner: "devbox", Slots: 2, LastSeen: t0.Add(30 * time.Hour), Runs: 5, BusyMinutes: 225, CapacityMinutes: 2880, IdleMinutes: &idle},
		{Runner: "mac", LastSeen: t0.Add(29 * time.Hour), Runs: 1, BusyMinutes: 60},
	}
	if !reflect.DeepEqual(got.Runners, wantRunners) || got.BusyMinutes != 285 || got.ActiveAgents != 4 {
		t.Errorf("runners (busy %v, agents %d):\n got %+v\nwant %+v", got.BusyMinutes, got.ActiveAgents, got.Runners, wantRunners)
	}
	wantKilled := []FlowKilled{{Reason: "expired", Runs: 1, LostMinutes: 30}, {Reason: "overtime", Runs: 1, LostMinutes: 45}}
	if !reflect.DeepEqual(got.Killed, wantKilled) || got.KilledRuns != 2 || got.LostMinutes != 75 {
		t.Errorf("killed (%d, %v min): %+v", got.KilledRuns, got.LostMinutes, got.Killed)
	}

	wantFeatures := []FlowFeature{{PlanID: 1, Title: "Voice", Stage: "split", Items: 2, Done: 1, ByStage: map[string]int{"done": 1, "build": 1}}}
	if !reflect.DeepEqual(got.Features, wantFeatures) {
		t.Errorf("features: %+v", got.Features)
	}

	// The 6h window (from +24h) sees none of the morning's work; the last run is still busy.
	six, err := s.Flow(context.Background(), "6h")
	if err != nil {
		t.Fatal(err)
	}
	if six.Done != 0 || six.Merged != 0 || len(six.PerHour) != 6 || six.Stages[0].Time != dist(1, hr(1), hr(1)) || six.BusyMinutes != 60 || len(six.Bounces) != 0 {
		t.Errorf("6h: %+v", six)
	}
	if _, err := s.Flow(context.Background(), "1y"); code(err) != pipeline.CodeInvalidField {
		t.Errorf("1y: %v", err)
	}
}

// A frozen repo's items wait on the freeze, its fix does not; a land run the freeze stopped lost
// its work, a run whose advance was queued did not.
func TestFlowFrozenRepo(t *testing.T) {
	s, clk := open(t)
	f := flowFixture{t, s}
	clk.Advance(30 * time.Hour)
	f.item(1, "build", 25, 0, "")
	f.item(2, "build", 25, 0, "")
	f.item(3, "land", 25, 0, "")
	f.item(4, "build", 25, 0, "")
	f.exec(`UPDATE items SET repo = 'o/r' WHERE id IN (1, 2, 3)`)
	f.exec(`INSERT INTO repo_freezes (repo, item_id, since, red_since) VALUES ('o/r', 1, ?, ?)`, h(26), h(26))
	f.lease("L1", 3, "land", "devbox", 27, 27.5, "frozen")
	f.lease("L2", 2, "build", "devbox", 26, 27, "queued")

	got, err := s.Flow(context.Background(), "24h")
	if err != nil {
		t.Fatal(err)
	}
	wantWait := []FlowWait{
		{Reason: "frozen repo", Count: 2, Items: []int64{2, 3}},
		{Reason: "ready", Count: 2, Items: []int64{1, 4}},
	}
	if !reflect.DeepEqual(got.Waiting, wantWait) {
		t.Errorf("waiting:\n got %+v\nwant %+v", got.Waiting, wantWait)
	}
	wantKilled := []FlowKilled{{Reason: "frozen", Runs: 1, LostMinutes: 30}}
	if !reflect.DeepEqual(got.Killed, wantKilled) || got.LostMinutes != 30 {
		t.Errorf("killed (%v min): %+v", got.LostMinutes, got.Killed)
	}
}

// The last free slot held for a Mac's item (#192): the Mac's item is ready, the others have no
// free slot.
func TestFlowHeldSlot(t *testing.T) {
	s, clk := open(t)
	f := flowFixture{t, s}
	clk.Advance(30 * time.Hour)
	f.item(1, "build", 25, 0, "")
	f.item(2, "build", 25, 0, "")
	f.item(3, "build", 26, 0, "")
	f.exec(`UPDATE items SET resources = 'macos' WHERE id = 2`)
	f.exec(`UPDATE control SET active_agents = 2 WHERE id = 1`)
	f.exec(`INSERT INTO runners (name, capabilities, only_resources, poll_seconds, stages, last_seen, slots) VALUES ('mbp', 'macos', 'macos', 20, 'build', ?, 1)`, h(30))
	f.lease("L1", 3, "build", "devbox", 29, -1, "")

	got, err := s.Flow(context.Background(), "24h")
	if err != nil {
		t.Fatal(err)
	}
	wantWait := []FlowWait{
		{Reason: "no free slot", Count: 1, Items: []int64{1}},
		{Reason: "ready", Count: 1, Items: []int64{2}},
	}
	if !reflect.DeepEqual(got.Waiting, wantWait) {
		t.Errorf("waiting:\n got %+v\nwant %+v", got.Waiting, wantWait)
	}
}

func TestBounceText(t *testing.T) {
	long := strings.Repeat("x", 150)
	for in, want := range map[string]string{
		"":                      "(no reason given)",
		"\n  \n second line\n":  "second line",
		"one line":              "one line",
		long:                    strings.Repeat("x", bounceTextMax-1) + "…",
		"  padded  \nand more ": "padded",
	} {
		if got := bounceText(in); got != want {
			t.Errorf("bounceText(%q) = %q, want %q", in, got, want)
		}
	}
}

// The window queries use the indexes, not a scan of events or leases (the 7d window on a
// database of 100k events must stay fast).
func TestFlowQueriesUseIndexes(t *testing.T) {
	s, _ := open(t)
	for _, c := range []struct{ q, want string }{
		{`SELECT item_id FROM events WHERE at >= 1 AND at <= 2 AND item_id IS NOT NULL AND to_stage != from_stage`, "USING INDEX events_at"},
		{`SELECT e.at FROM events e JOIN items i ON i.id = e.item_id WHERE e.at >= 1 AND e.at <= 2 AND e.item_id IS NOT NULL AND e.outcome = 'merged'`, "USING INDEX events_at"},
		{`SELECT stage FROM leases WHERE ended_at >= 1`, "USING INDEX leases_ended"},
		{`SELECT runner FROM plan_leases WHERE ended_at >= 1`, "USING INDEX plan_leases_ended"},
	} {
		rows, err := s.r.Query(`EXPLAIN QUERY PLAN ` + c.q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if p := strings.Join(plan, "; "); !strings.Contains(p, c.want) {
			t.Errorf("%s\nplan: %s (want %s)", c.q, p, c.want)
		}
	}
}

// BenchmarkFlow7d is the 7d window over 100k events (2k items, 50 events each) and 20k leases:
// go test -run '^$' -bench Flow7d ./internal/store/
func BenchmarkFlow7d(b *testing.B) {
	clk := clock.NewFake(t0.Add(60 * 24 * time.Hour))
	s, err := Open(b.TempDir(), clk)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	tx, err := s.w.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stages := []string{"triage", "build", "review", "land"}
	for i := int64(1); i <= 2000; i++ {
		start := h(float64(i) * 0.7) // spread over 58 days
		if _, err := tx.Exec(`INSERT INTO items (id, title, source, priority, stage, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			i, "x", "user", "P3", "done", start, start); err != nil {
			b.Fatal(err)
		}
		for j := int64(0); j < 50; j++ {
			from, to := stages[j%4], stages[(j+1)%4]
			at := start + j*int64(10*time.Minute)
			if _, err := tx.Exec(`INSERT INTO events (item_id, at, actor, kind, from_stage, to_stage, outcome, data) VALUES (?,?,?,?,?,?,?,?)`,
				i, at, "test", "advanced", from, to, "bounce", `{"notes":"x"}`); err != nil {
				b.Fatal(err)
			}
			if j%5 == 0 {
				if _, err := tx.Exec(`INSERT INTO leases (id, item_id, stage, runner, agent, claimed_at, expires_at, ended_at, end_reason) VALUES (?,?,?,?,?,?,?,?,?)`,
					fmt.Sprintf("l%d-%d", i, j), i, from, "devbox", "a", at, at, at+int64(5*time.Minute), "advanced"); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Flow(context.Background(), "7d"); err != nil {
			b.Fatal(err)
		}
	}
}

// Under claude's usage pace the panel says which items wait on it, with the claim's own pace, and
// a P4 size-M build stops waiting once a runner says it has qwen (runners.builders).
func TestFlowUsagePaced(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.SetControl(ctx, user, ControlPatch{Budgets: map[string]pipeline.Budget{"claude": {Per5h: 10000}}}); err != nil {
		t.Fatal(err)
	}
	big, small, low := add(t, s, "P2 M", "P2"), add(t, s, "P2 S", "P2"), add(t, s, "P4 M", "P4")
	sizeM := claudeRun(3000)
	sizeM["size"] = "M"
	triageWith(t, s, big, sizeM)
	triageWith(t, s, small, claudeRun(3000))
	triageWith(t, s, low, sizeM) // 9000 of 10000: slow
	waits := func() map[string][]int64 {
		t.Helper()
		f, err := s.Flow(ctx, "6h")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]int64{}
		for _, w := range f.Waiting {
			out[w.Reason] = w.Items
		}
		return out
	}
	want := map[string][]int64{pipeline.WaitPaced: {big.ID, low.ID}, pipeline.WaitReady: {small.ID}}
	if got := waits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("slow, no qwen runner: %v, want %v", got, want)
	}
	if c := claimReq(t, s, pipeline.ClaimRequest{Builders: []string{pipeline.BuilderQwen}, Stages: []pipeline.Stage{pipeline.Triage}}); c != nil {
		t.Fatalf("triage claim %+v, want none", c)
	}
	want = map[string][]int64{pipeline.WaitPaced: {big.ID}, pipeline.WaitReady: {small.ID, low.ID}}
	if got := waits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("slow, a qwen runner: %v, want %v", got, want)
	}
}
