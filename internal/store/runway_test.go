package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func runwayEvents(t *testing.T, s *Store) []Event {
	t.Helper()
	rows, err := s.r.Query(`SELECT `+eventCols+` FROM events WHERE kind = ? ORDER BY id`, KindRunwayLow)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := scanEvents(rows)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func setStage(t *testing.T, s *Store, id int64, st pipeline.Stage) {
	t.Helper()
	if _, err := s.w.Exec(`UPDATE items SET stage = ?, head_sha = ? WHERE id = ?`, string(st), shaA, id); err != nil {
		t.Fatal(err)
	}
}

// heldChains is a board whose every chain waits on one held item: it and three items behind it.
func heldChains(t *testing.T, s *Store) pipeline.Item {
	t.Helper()
	ctx := context.Background()
	held := add(t, s, "held", "P2")
	setStage(t, s, held.ID, pipeline.Held)
	if _, err := s.w.Exec(`UPDATE items SET risk = ? WHERE id = ?`, pipeline.RiskHold, held.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		it := add(t, s, "behind", "P2")
		setStage(t, s, it.ID, pipeline.Build)
		if _, err := s.Link(ctx, user, it.ID, []int64{held.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return held
}

func TestRunwayWarnsOncePerDrySpell(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	held := heldChains(t, s)

	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 0 {
		t.Fatalf("warned while the user is active: %+v", evs)
	}
	clk.Advance(pipeline.RunwayAwayAfter)
	for range 5 {
		s.OrchTick(ctx)
		clk.Advance(time.Minute)
	}
	evs := runwayEvents(t, s)
	if len(evs) != 1 {
		t.Fatalf("%d warnings over several sweeps, want 1", len(evs))
	}
	ubs, _ := evs[0].Data["unblockers"].([]any)
	if len(ubs) != 1 {
		t.Fatalf("unblockers = %v, want the held item", evs[0].Data["unblockers"])
	}
	u := ubs[0].(map[string]any)
	if int64(u["item"].(float64)) != held.ID || u["action"] != pipeline.UnblockApprove || u["frees"].(float64) != 4 || u["sha"] != shaA {
		t.Fatalf("unblocker = %v, want approve #%d frees 4 at %s", u, held.ID, shaA)
	}
	if evs[0].Data["hours"].(float64) != 0 {
		t.Fatalf("hours = %v, want 0", evs[0].Data["hours"])
	}

	// It recovers (approved, the merge goes on to land: 3 builds' worth of work), then runs dry again.
	if _, err := s.Approve(ctx, user, held.ID, shaA, "approve"); err != nil {
		t.Fatal(err)
	}
	s.OrchTick(ctx)
	clk.Advance(time.Minute)
	if rep, err := s.Runway(ctx); err != nil || rep.Hours < 3 {
		t.Fatalf("runway = %+v, %v; want at least 3 h", rep, err)
	}
	setStage(t, s, held.ID, pipeline.Held)
	clk.Advance(pipeline.RunwayAwayAfter) // the approval was the user's
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 2 {
		t.Fatalf("%d warnings after a second dry spell, want 2", len(evs))
	}
}

func TestRunwayNoWarningWhileUserActive(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	held := heldChains(t, s)
	// 15:00 in New York, the user acting every 20 minutes.
	clk.Advance(7 * time.Hour)
	for range 6 {
		p := "P2"
		if _, err := s.PatchItem(ctx, user, held.ID, &p, nil); err != nil {
			t.Fatal(err)
		}
		clk.Advance(20 * time.Minute)
		s.OrchTick(ctx)
	}
	if evs := runwayEvents(t, s); len(evs) != 0 {
		t.Fatalf("warned while the user is active: %+v", evs)
	}
	// The orchestrator acting for them is not them.
	p := "P2"
	if _, err := s.PatchItem(WithVia(ctx, orch), user, held.ID, &p, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(pipeline.RunwayAwayAfter - time.Minute) // 49 minutes since the user's last action
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 1 {
		t.Fatalf("%d warnings once the user left, want 1", len(evs))
	}
}

func TestRunwayNoWarningWithPlentyOfWork(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	heldChains(t, s)
	for range 4 {
		it := add(t, s, "free", "P2")
		setStage(t, s, it.ID, pipeline.Build)
	}
	clk.Advance(time.Hour)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 0 {
		t.Fatalf("warned with 4 builds' worth of work: %+v", evs)
	}
}

func TestRunwayOffAndPaused(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	heldChains(t, s)
	zero := 0
	if _, err := s.SetControl(ctx, user, ControlPatch{RunwayWarnHours: &zero}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 0 {
		t.Fatalf("warned with the warning off: %+v", evs)
	}
	three, stop := 3, true
	if _, err := s.SetControl(ctx, user, ControlPatch{RunwayWarnHours: &three, Stopped: &stop}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 0 {
		t.Fatalf("warned with the loop stopped: %+v", evs)
	}
	bad := pipeline.MaxRunwayWarnHours + 1
	if _, err := s.SetControl(ctx, user, ControlPatch{RunwayWarnHours: &bad}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("runway_warn_hours %d: %v, want invalid_field", bad, err)
	}
}

func TestStageDurationsAreRecentMedians(t *testing.T) {
	s, _ := open(t)
	it := add(t, s, "x", "P2")
	for i, m := range []int{10, 30, 50} {
		if _, err := s.w.Exec(`INSERT INTO leases (id, item_id, stage, runner, agent, claimed_at, expires_at, ended_at, end_reason) VALUES (?,?,?,?,?,?,?,?,?)`,
			"l"+string(rune('a'+i)), it.ID, "build", runner, "a", 0, 0, int64(m)*int64(time.Minute), "advanced"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.w.Exec(`INSERT INTO leases (id, item_id, stage, runner, agent, claimed_at, expires_at, ended_at, end_reason) VALUES ('lx',?,?,?,?,0,0,?,'expired')`,
		it.ID, "build", runner, "a", int64(999*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var got map[pipeline.Stage]time.Duration
	if err := s.tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		got, err = stageDurations(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got[pipeline.Build] != 30*time.Minute {
		t.Fatalf("build = %v, want the median 30m", got[pipeline.Build])
	}
	if _, ok := got[pipeline.Review]; ok {
		t.Fatalf("review = %v, want none (no runs)", got[pipeline.Review])
	}
	// Two runs are too few to go by.
	if _, err := s.w.Exec(`DELETE FROM leases WHERE id = 'la'`); err != nil {
		t.Fatal(err)
	}
	if err := s.tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		got, err = stageDurations(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d, ok := got[pipeline.Build]; ok {
		t.Fatalf("build = %v from 2 runs, want none (the default)", d)
	}
}

// An item triaged risk hold is runway itself, but its dependents are not: it stops at land for the
// user.
func TestRunwayHeldRiskDependencyFreesNothing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	x := add(t, s, "risky", "P2")
	claim(t, s, "t")
	evidence(t, s, x.ID)
	if _, err := s.Advance(ctx, runner, x.ID, pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "hold", "resources": ""}, nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, x.ID); d.Item.Stage != pipeline.Build || d.Item.Risk != pipeline.RiskHold {
		t.Fatalf("after triage: stage %s risk %q", d.Item.Stage, d.Item.Risk)
	}
	for range 2 {
		it := add(t, s, "behind", "P2")
		setStage(t, s, it.ID, pipeline.Build)
		if _, err := s.Link(ctx, user, it.ID, []int64{x.ID}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Runway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The risky item's build+review+land at the defaults: 70 minutes.
	if rep.Runnable != 1 || rep.Hours != 1.2 {
		t.Fatalf("runway = %+v; want the risky item only, 1.2 h", rep)
	}
}

// #227: a one-tick blip at the threshold, or a paused tick, does not re-send the same warning; a
// new thing to unblock does.
func TestRunwayWarnsOncePerRealChange(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	heldChains(t, s)
	// Items added now (the user's action), set to a stage later without one.
	var free []int64
	for range 4 {
		it := add(t, s, "free", "P2")
		setStage(t, s, it.ID, pipeline.Done)
		free = append(free, it.ID)
	}
	held2 := add(t, s, "held too", "P2")
	setStage(t, s, held2.ID, pipeline.Done)
	clk.Advance(pipeline.RunwayAwayAfter)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 1 {
		t.Fatalf("%d warnings, want 1", len(evs))
	}

	// A blip: enough work turns up for one tick, then is gone.
	for _, id := range free {
		setStage(t, s, id, pipeline.Build)
	}
	if rep, err := s.Runway(ctx); err != nil || rep.Hours < 3 {
		t.Fatalf("runway = %+v, %v; want at least 3 h", rep, err)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	for _, id := range free {
		setStage(t, s, id, pipeline.Done)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 1 {
		t.Fatalf("%d warnings after a one-tick blip, want 1", len(evs))
	}

	// A paused tick, then the loop runs again.
	stop, run := true, false
	if _, err := s.SetControl(WithVia(ctx, orch), user, ControlPatch{Stopped: &stop}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if _, err := s.SetControl(WithVia(ctx, orch), user, ControlPatch{Stopped: &run}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 1 {
		t.Fatalf("%d warnings after a paused tick, want 1", len(evs))
	}

	// Something new to unblock: a second held item.
	setStage(t, s, held2.ID, pipeline.Held)
	if _, err := s.w.Exec(`UPDATE items SET risk = ? WHERE id = ?`, pipeline.RiskHold, held2.ID); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if evs := runwayEvents(t, s); len(evs) != 2 {
		t.Fatalf("%d warnings after a new unblocker, want 2", len(evs))
	}
}
