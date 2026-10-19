package store

import (
	"context"
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
	claimWith(t, s, pipeline.WorkItem)
	next, _ := s.ScheduledRegression(ctx)
	if next == nil || !next.NotBefore.Equal(r.NotBefore.Add(24*time.Hour)) {
		t.Fatalf("the next night's: %+v", next)
	}
	off := false
	if _, err := s.SetControl(ctx, user, ControlPatch{RegressionNightly: &off}); err != nil {
		t.Fatal(err)
	}
	claimWith(t, s, pipeline.WorkItem)
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
	for clk.Now().Before(t0.Add(toOne + 2*pipeline.OvernightQuiet + 4*time.Hour)) {
		clk.Advance(time.Minute)
		lease := mustReg(t, s, r.ID).lease.ID
		if _, err := s.Heartbeat(ctx, runner, lease, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustReg(t, s, r.ID); got.Status != RegRunning {
		t.Fatalf("after 4 h: %s %s", got.Status, got.Error)
	}
	clk.Advance(2 * time.Hour)
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
