package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// t0 is 08:00 in New York; tonight's 01:00 is 17 hours on.
const toOne = 17 * time.Hour

var nightCaps = []string{"emulator", "gpu", "voice"}

func claimWith(t *testing.T, s *Store, kinds ...string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: nightCaps, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestScheduleRegression(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RegScheduled || r.Suite != pipeline.SuiteOvernight || r.NotBefore == nil || !r.NotBefore.Equal(t0.Add(toOne)) {
		t.Fatalf("scheduled %+v (not before %v)", r, r.NotBefore)
	}
	if _, err := s.ScheduleRegression(ctx, "slack:U1", "board"); code(err) != CodeRegressionOpen {
		t.Fatalf("a second schedule: %v, want %s", err, CodeRegressionOpen)
	}
	// A run by hand in the day is not held up by tonight's.
	if _, err := s.QueueRegression(ctx, "slack:U1", "web", "slack"); err != nil {
		t.Fatalf("a manual run while one is scheduled: %v", err)
	}
	// The overnight suite is only scheduled, never queued by hand.
	if _, err := s.QueueRegression(ctx, "slack:U1", pipeline.SuiteOvernight, "slack"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("overnight by hand: %v", err)
	}
	if _, err := s.CancelScheduledRegression(ctx, "slack:U1", "slack"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ScheduledRegression(ctx); got != nil {
		t.Fatalf("still scheduled after cancel: %+v", got)
	}
	if _, err := s.CancelScheduledRegression(ctx, "slack:U1", "slack"); code(err) != CodeNotFound {
		t.Fatalf("cancel with none scheduled: %v", err)
	}
}

func TestOvernightGate(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	// Quiet all evening, but not before 01:00.
	clk.Advance(toOne - time.Hour)
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed before the set time: %+v", c)
	}
	clk.Advance(time.Hour - time.Minute)
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed at 00:59: %+v", c)
	}
	// 00:59: an item comes in; the queue is busy, and the quiet clock starts again once it is gone.
	it := add(t, s, "late work", "P3")
	clk.Advance(time.Minute)
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed with an item waiting: %+v", c)
	}
	if got, _ := s.ScheduledRegression(ctx); got == nil || got.QuietSince != nil {
		t.Fatalf("quiet with an item waiting: %+v", got)
	}
	if _, err := s.Drop(ctx, user, it.ID, "not tonight"); err != nil {
		t.Fatal(err)
	}
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed the moment the queue went quiet: %+v", c)
	}
	clk.Advance(pipeline.OvernightQuiet - time.Second)
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed after %s quiet: %+v", pipeline.OvernightQuiet-time.Second, c)
	}
	clk.Advance(time.Second)
	c := claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	if c == nil || c.Reg == nil || c.Reg.ID != r.ID || c.Reg.Status != RegRunning {
		t.Fatalf("not claimed at 01:10 after 10 quiet minutes: %+v", c)
	}
	// While it runs: an item in triage is held back.
	add(t, s, "an idea at 01:15", "P1")
	if c := claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed during the overnight run: %+v", c)
	}
	// The run reports; its report waits for 08:30.
	done, err := s.RegressionResult(ctx, runner, r.ID, RegressionReport{Tests: []RegressionReportTest{
		{Suite: "web", Title: "a", Status: "passed"},
		{Suite: "voice", Title: "b", Status: TestFlaky, Error: "first try: no reply"},
		{Suite: "pipeline", Title: "c", Status: "failed", Recording: "c.wav"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != RegFailed || done.Passed != 1 || done.Flaky != 1 || done.Failed != 1 {
		t.Fatalf("finished %+v", done)
	}
	morning := t0.Add(toOne + 7*time.Hour + 30*time.Minute)
	if done.ReportAt == nil || !done.ReportAt.Equal(morning) {
		t.Fatalf("report at %v, want %v", done.ReportAt, morning)
	}
	if due, _ := s.DueRegressionReports(ctx); len(due) != 0 {
		t.Fatalf("due at night: %+v", due)
	}
	if c := claimWith(t, s, pipeline.WorkItem); c == nil || c.Item.Title != "an idea at 01:15" {
		t.Fatalf("the held item was not claimed after the run: %+v", c)
	}
	clk.Advance(morning.Sub(clk.Now()))
	due, err := s.DueRegressionReports(ctx)
	if err != nil || len(due) != 1 || due[0].Run.ID != r.ID || len(due[0].Run.Tests) != 3 || due[0].Done["status"] != RegFailed {
		t.Fatalf("due in the morning: %+v %v", due, err)
	}
	if err := s.MarkRegressionReported(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueRegressionReports(ctx); len(due) != 0 {
		t.Fatalf("due again after it was posted: %+v", due)
	}
}

func TestOvernightLiveLeaseKeepsItBusy(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	if _, err := s.ScheduleRegression(ctx, "slack:U1", "slack"); err != nil {
		t.Fatal(err)
	}
	add(t, s, "work", "P3")
	clk.Advance(toOne)
	held := claimWith(t, s, pipeline.WorkItem)
	if held == nil || held.Item.ID == 0 {
		t.Fatalf("the item was not claimed: %+v", held)
	}
	// Its triage lease is live for hours: no overnight run.
	for i := 0; i < 6; i++ {
		clk.Advance(5 * time.Minute)
		if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
			t.Fatalf("claimed with a triage lease live: %+v", c)
		}
		if _, err := s.Heartbeat(ctx, runner, held.Lease.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOvernightNightly(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	on, at := true, "02:30"
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionNightly: &on, RegressionAt: &at}); err != nil {
		t.Fatal(err)
	}
	bad := "2am"
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionAt: &bad}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("regression_at 2am: %v", err)
	}
	if c := claimWith(t, s, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed in the day: %+v", c)
	}
	r, err := s.ScheduledRegression(ctx)
	if err != nil || r == nil || r.Via != "nightly" || !r.NotBefore.Equal(t0.Add(toOne+90*time.Minute)) {
		t.Fatalf("nightly schedule %+v %v", r, err)
	}
	clk.Advance(toOne + 90*time.Minute)
	if c := claimWith(t, s, pipeline.WorkRegression); c == nil || c.Reg == nil || c.Reg.ID != r.ID {
		t.Fatalf("not claimed at 02:30, quiet all day: %+v", c)
	}
	// Claimed: the next poll schedules the next night's.
	claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	next, _ := s.ScheduledRegression(ctx)
	if next == nil || !next.NotBefore.Equal(r.NotBefore.Add(24*time.Hour)) {
		t.Fatalf("the next night's: %+v", next)
	}
	off := false
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionNightly: &off}); err != nil {
		t.Fatal(err)
	}
	claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	if got, _ := s.ScheduledRegression(ctx); got != nil {
		t.Fatalf("the nightly run stays scheduled with the setting off: %+v", got)
	}
	ctl, _ := s.Control(ctx)
	if ctl.RegressionNightly || ctl.RegressionAt != "02:30" {
		t.Fatalf("control %+v", ctl)
	}
}

func TestOvernightOvertimeCap(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r, _ := s.ScheduleRegression(ctx, "slack:U1", "slack")
	clk.Advance(toOne + pipeline.OvernightQuiet)
	claimWith(t, s, pipeline.WorkRegression)
	clk.Advance(pipeline.OvernightQuiet)
	if c := claimWith(t, s, pipeline.WorkRegression); c == nil || c.Reg == nil {
		t.Fatalf("not claimed: %+v", c)
	}
	// Past the manual runs' 3 h it runs on (heartbeats); past 6 h it ends.
	beatUntil := func(d time.Duration) {
		for clk.Now().Before(t0.Add(toOne + 2*pipeline.OvernightQuiet + d)) {
			clk.Advance(time.Minute)
			lease := mustReg(t, s, r.ID).lease.ID
			if _, err := s.Heartbeat(ctx, runner, lease, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	beatUntil(4 * time.Hour)
	if got := mustReg(t, s, r.ID); got.Status != RegRunning {
		t.Fatalf("after 4 h: %s %s", got.Status, got.Error)
	}
	// Still heartbeating at the cap: overtime, not expired (#592).
	beatUntil(6*time.Hour - time.Minute)
	clk.Advance(2 * time.Minute)
	claimWith(t, s, pipeline.WorkItem)
	got := mustReg(t, s, r.ID)
	if got.Status != RegError || got.ReportAt == nil {
		t.Fatalf("after 6 h: %+v", got)
	}
}

func mustReg(t *testing.T, s *Store, id int64) Regression {
	t.Helper()
	r, err := s.GetRegression(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Nightly is once a night by set time: a cancelled night is skipped, not scheduled again, and a run
// that waited into the afternoon does not take the next night's place.
func TestOvernightNightlyOncePerSetTime(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	on := true
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionNightly: &on}); err != nil {
		t.Fatal(err)
	}
	claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	r1, _ := s.ScheduledRegression(ctx)
	if r1 == nil || !r1.NotBefore.Equal(t0.Add(toOne)) {
		t.Fatalf("tonight's: %+v", r1)
	}
	if _, err := s.CancelScheduledRegression(ctx, "slack:U1", "slack"); err != nil {
		t.Fatal(err)
	}
	claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	r2, _ := s.ScheduledRegression(ctx)
	if r2 == nil || !r2.NotBefore.Equal(r1.NotBefore.Add(24*time.Hour)) {
		t.Fatalf("after cancelling tonight's (%v), the next poll scheduled %+v, want the next night", r1.NotBefore, r2)
	}
	// R2 waits until 14:00 the day after its set time, then runs.
	clk.Advance(toOne + 24*time.Hour + 13*time.Hour)
	if c := claimWith(t, s, pipeline.WorkRegression); c == nil || c.Reg == nil || c.Reg.ID != r2.ID {
		t.Fatalf("R2 not claimed in the afternoon: %+v", c)
	}
	claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	r3, _ := s.ScheduledRegression(ctx)
	if r3 == nil || !r3.NotBefore.Equal(r2.NotBefore.Add(24*time.Hour)) {
		t.Fatalf("after a late run (set %v), the next is %+v, want the very next night", r2.NotBefore, r3)
	}
}

// A limit hit pauses claude: the build it holds back cannot be claimed, so the queue is quiet for
// the overnight run, whose lanes are no claude runs (#317).
func TestOvernightStartsWhilePaused(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	hit, other := add(t, s, "hits the limit", "P2"), add(t, s, "waits", "P2")
	triageWith(t, s, hit, claudeRun(4000))
	triageWith(t, s, other, claudeRun(1000))
	if c := claim(t, s, "agent-1"); c == nil || c.Item.ID != hit.ID {
		t.Fatalf("build claim %+v", c)
	}
	f := claudeRun(3000)
	f["error"], f["reset"], f["limit_window"] = "rate limit: rejected", t0.Add(toOne+6*time.Hour).Format(time.RFC3339), "5h"
	if _, err := s.Advance(ctx, runner, hit.ID, pipeline.UsageLimit, f, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(toOne)
	if c := claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression); c != nil {
		t.Fatalf("claimed at 01:00 while paused: %+v", c)
	}
	if got, _ := s.ScheduledRegression(ctx); got == nil || got.QuietSince == nil {
		t.Fatalf("the paused queue is not quiet: %+v", got)
	}
	clk.Advance(pipeline.OvernightQuiet)
	c := claimWith(t, s, pipeline.WorkItem, pipeline.WorkRegression)
	if c == nil || c.Reg == nil || c.Reg.ID != r.ID {
		t.Fatalf("not claimed at 01:10 with claude paused: %+v", c)
	}
}

func TestOvernightUnstartedNote(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	it := add(t, s, "late work", "P3")
	clk.Advance(toOne + 6*time.Hour - time.Minute)
	claimWith(t, s, pipeline.WorkRegression) // a runner that could take it
	if u, err := s.UnstartedOvernight(ctx); err != nil || u != nil {
		t.Fatalf("a note at 06:59: %+v %v", u, err)
	}
	clk.Advance(time.Minute)
	u, err := s.UnstartedOvernight(ctx)
	want := fmt.Sprintf("the queue is busy: #%d waits in triage", it.ID)
	if err != nil || u == nil || u.Run.ID != r.ID || u.Why != want {
		t.Fatalf("at 07:00: %+v %v, want why %q", u, err, want)
	}
	if err := s.MarkUnstartedNoted(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UnstartedOvernight(ctx); err != nil || u != nil {
		t.Fatalf("a second note: %+v %v", u, err)
	}
}

// #416: a run carried over to the next night gets a note on every night it misses while a runner
// can take it, once each, and a regression_missed event per night.
func TestOvernightUnstartedNoteEveryNight(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	add(t, s, "late work", "P3")
	clk.Advance(toOne + 6*time.Hour)
	for night := 1; night <= 3; night++ {
		claimWith(t, s, pipeline.WorkRegression) // a runner that could take it, seen every night
		u, err := s.UnstartedOvernight(ctx)
		if err != nil || u == nil || u.Run.ID != r.ID {
			t.Fatalf("night %d: no note at 07:00 (%+v %v)", night, u, err)
		}
		if err := s.MarkUnstartedNoted(ctx, u); err != nil {
			t.Fatal(err)
		}
		if u, err := s.UnstartedOvernight(ctx); err != nil || u != nil {
			t.Fatalf("night %d: a second note: %+v %v", night, u, err)
		}
		clk.Advance(24*time.Hour - time.Minute)
		if u, err := s.UnstartedOvernight(ctx); err != nil || u != nil {
			t.Fatalf("night %d: a note at 06:59 the next day: %+v %v", night, u, err)
		}
		clk.Advance(time.Minute)
	}
	if n := countEvents(t, s, KindRegMissed); n != 3 {
		t.Fatalf("%d regression_missed events, want 3", n)
	}
}

// #505: when no runner seen lately can take the overnight run (none claims regression runs with
// every resource), it expires at 07:00 after its set time without a note: cancelled by loopd, via
// expired, and no regression_missed.
func TestOvernightUnstartedNoRunnerTakesIt(t *testing.T) {
	for _, seen := range []bool{false, true} {
		s, clk := open(t)
		ctx := context.Background()
		r, err := s.ScheduleRegression(ctx, "slack:U1", "slack")
		if err != nil {
			t.Fatal(err)
		}
		clk.Advance(toOne)
		for i := 0; seen && i < 40; i++ {
			c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: []string{"gpu", "emulator"}, Kinds: []string{pipeline.WorkItem, pipeline.WorkRegression}})
			if err != nil || c != nil {
				t.Fatalf("claim: %+v %v", c, err)
			}
			clk.Advance(9 * time.Minute)
		}
		clk.Advance(toOne + 6*time.Hour - clk.Now().Sub(t0))
		if u, err := s.UnstartedOvernight(ctx); err != nil || u != nil {
			t.Fatalf("seen=%v: a note at 07:00 with no runner to take it: %+v %v", seen, u, err)
		}
		if got := mustReg(t, s, r.ID); got.Status != RegCancelled {
			t.Fatalf("seen=%v: R%d is %s at 07:00, want cancelled", seen, r.ID, got.Status)
		}
		if sched, _ := s.ScheduledRegression(ctx); sched != nil {
			t.Fatalf("seen=%v: still scheduled: %+v", seen, sched)
		}
		var via string
		if err := s.w.QueryRow(`SELECT json_extract(data, '$.via') FROM events WHERE kind = ? AND actor = 'loopd'`, KindRegCancelled).Scan(&via); err != nil || via != "expired" {
			t.Fatalf("seen=%v: regression_cancelled by loopd via %q (%v), want expired", seen, via, err)
		}
		if n := countEvents(t, s, KindRegMissed); n != 0 {
			t.Fatalf("seen=%v: %d regression_missed events, want none", seen, n)
		}
	}
}

// #505: with regression_nightly on, a night no runner seen can take the overnight run is not
// scheduled; it is once a runner that can take it polls.
func TestOvernightNightlyNeedsATaker(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	on := true
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionNightly: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: []string{"gpu", "emulator"}, Kinds: []string{pipeline.WorkItem, pipeline.WorkRegression}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.ScheduledRegression(ctx); r != nil {
		t.Fatalf("scheduled with no runner to take it: %+v", r)
	}
	if n := countEvents(t, s, KindRegScheduled); n != 0 {
		t.Fatalf("%d regression_scheduled events, want none", n)
	}
	claimWith(t, s, pipeline.WorkRegression)
	if r, _ := s.ScheduledRegression(ctx); r == nil || r.Via != "nightly" {
		t.Fatalf("not scheduled once a runner can take it: %+v", r)
	}
}

// One runner claims in two lanes under one name: the land lane (or a paused runner) claims [land]
// with no kinds, and must not make the 07:00 note say the runner does not take regression runs.
func TestOvernightUnstartedLandLaneKeepsKinds(t *testing.T) {
	for _, configured := range []bool{false, true} {
		s, clk := open(t)
		ctx := context.Background()
		if _, err := s.ScheduleRegression(ctx, "slack:U1", "slack"); err != nil {
			t.Fatal(err)
		}
		add(t, s, "late work", "P3")
		clk.Advance(toOne + 6*time.Hour)
		agent := pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: []string{"emulator", "gpu", "voice"}, Stages: []pipeline.Stage{pipeline.Build}, Kinds: []string{pipeline.WorkItem, pipeline.WorkRegression}}
		land := pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: []string{"emulator", "gpu", "voice"}, Stages: []pipeline.Stage{pipeline.Land}}
		if configured {
			agent.RunnerKinds = []string{pipeline.WorkItem, pipeline.WorkRegression}
			land.RunnerKinds = agent.RunnerKinds
		}
		if _, err := s.Claim(ctx, runner, agent); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(ctx, runner, land); err != nil {
			t.Fatal(err)
		}
		u, err := s.UnstartedOvernight(ctx)
		if err != nil || u == nil || strings.Contains(u.Why, "no runner takes it") {
			t.Fatalf("configured=%v: note %+v %v; the runner takes regression runs", configured, u, err)
		}
	}
}
