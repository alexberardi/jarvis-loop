package store

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

var regKinds = []string{pipeline.WorkItem, pipeline.WorkRegression}

func claimReg(t *testing.T, s *Store, caps ...string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: "a1", Capabilities: caps, Kinds: regKinds})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func queueReg(t *testing.T, s *Store, suite string) Regression {
	t.Helper()
	r, err := s.QueueRegression(context.Background(), "slack:U1", suite, "slack")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegressionOneAtATime(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r := queueReg(t, s, "web")
	if r.Status != RegQueued || r.Suite != "web" {
		t.Fatalf("queued %+v", r)
	}
	if _, err := s.QueueRegression(ctx, "slack:U1", "mobile", "slack"); code(err) != CodeRegressionOpen {
		t.Fatalf("a second run while one is queued: %v, want %s", err, CodeRegressionOpen)
	}
	c := claimReg(t, s)
	if c == nil || c.Kind != pipeline.WorkRegression || c.Reg == nil || c.Reg.ID != r.ID || c.Stage != pipeline.RegressionStage {
		t.Fatalf("claim %+v", c)
	}
	if _, err := s.QueueRegression(ctx, "slack:U1", "all", "board"); code(err) != CodeRegressionOpen {
		t.Fatalf("a second run while one runs: %v, want %s", err, CodeRegressionOpen)
	}
	if _, err := s.QueueRegression(ctx, "slack:U1", "voice", "slack"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("suite voice: %v", err)
	}
	if _, err := s.RegressionResult(ctx, runner, r.ID, RegressionReport{Tests: []RegressionReportTest{{Suite: "web", Title: "a", Status: "passed"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueRegression(ctx, "slack:U1", "all", "board"); err != nil {
		t.Fatalf("after the run ended: %v", err)
	}
}

func TestRegressionCountsAsAnAgent(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	n := 1
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &n}); err != nil {
		t.Fatal(err)
	}
	add(t, s, "an item", "P1")
	queueReg(t, s, "web")
	if c := claimReg(t, s); c == nil || c.Reg == nil {
		t.Fatalf("the run did not go first: %+v", c)
	}
	if c := claimReg(t, s); c != nil {
		t.Fatalf("the item took a second slot while the run holds the only one: %+v", c)
	}
}

func TestRegressionMobileNeedsTheEmulator(t *testing.T) {
	s, _ := open(t)
	queueReg(t, s, "mobile")
	if c := claimReg(t, s); c != nil {
		t.Fatalf("a runner without the emulator got %+v", c)
	}
	if c := claimReg(t, s, "emulator"); c == nil || c.Reg == nil {
		t.Fatalf("the emulator runner got %+v", c)
	}
}

func TestRegressionLeaseExpiresBackToTheQueue(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	r := queueReg(t, s, "web")
	c := claimReg(t, s)
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, "e2e"); err != nil || stop {
		t.Fatalf("heartbeat: %v %v", stop, err)
	}
	clk.Advance(LeaseTTL + time.Second)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRegression(ctx, r.ID)
	if err != nil || got.Status != RegQueued || got.Runner != "" {
		t.Fatalf("after expiry: %+v %v", got, err)
	}
	if c := claimReg(t, s); c == nil || c.Reg == nil || c.Reg.ID != r.ID {
		t.Fatalf("not claimable again: %+v", c)
	}
	// Its heartbeats keep the lease, but the run goes past the max: it ends with an error.
	c = &Claim{Lease: Lease{ID: mustOpen(t, s).lease.ID}}
	for end := clk.Now().Add(pipeline.RegressionMaxRun + time.Minute); clk.Now().Before(end); {
		clk.Advance(LeaseTTL / 2)
		if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil {
			break
		}
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetRegression(ctx, r.ID); got.Status != RegError || !strings.Contains(got.Error, "went past") {
		t.Fatalf("overtime: %+v", got)
	}
}

func mustOpen(t *testing.T, s *Store) *Regression {
	t.Helper()
	r, err := openRegression(s.r)
	if err != nil || r == nil {
		t.Fatalf("no open run: %v", err)
	}
	return r
}

func TestRegressionStoppedLoopRequeues(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r := queueReg(t, s, "web")
	c := claimReg(t, s)
	yes := true
	if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &yes}); err != nil {
		t.Fatal(err)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat while stopped: stop %v err %v", stop, err)
	}
	if got, _ := s.GetRegression(ctx, r.ID); got.Status != RegQueued {
		t.Fatalf("status %q, want queued", got.Status)
	}
}

func reportRun(t *testing.T, s *Store, id int64) Regression {
	t.Helper()
	ctx := context.Background()
	if _, err := s.AddRegressionFile(ctx, runner, id, "login-failed.png", strings.NewReader("\x89PNG\r\n\x1a\nfake")); err != nil {
		t.Fatal(err)
	}
	got, err := s.RegressionResult(ctx, runner, id, RegressionReport{E2ERun: "20261018-1", ServerHead: shaA, Tests: []RegressionReportTest{
		{Suite: "web", Title: "inbox › deletes an item", File: "tests/inbox.spec.ts", Line: 3, Status: "passed", MS: 500},
		{Suite: "web", Title: "login › refuses a bad password", File: "tests/login.spec.ts", Line: 9, Status: "failed",
			Error: "expect(locator).toHaveText(\"Wrong password\")", MS: 900, Screenshot: "login-failed.png", Repo: "o/server"},
		{Suite: "mobile", Title: "Biometric login", Status: "skipped"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRegressionResult(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	var kinds []string
	s.Subscribe(func(e Event) { kinds = append(kinds, e.Kind) })
	r := queueReg(t, s, "web")
	claimReg(t, s)
	got := reportRun(t, s, r.ID)
	if got.Status != RegFailed || got.Passed != 1 || got.Failed != 1 || got.Skipped != 1 || got.FinishedAt == nil {
		t.Fatalf("result %+v", got)
	}
	full, err := s.GetRegression(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Tests) != 3 || full.Tests[1].Screenshot != 1 || full.Tests[0].Screenshot != 0 || !full.Tests[1].Failed() || full.Tests[2].Failed() {
		t.Fatalf("tests %+v", full.Tests)
	}
	f, rc, err := s.OpenRegressionFile(ctx, r.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if f.ContentType != "image/png" || !bytes.HasSuffix(b, []byte("fake")) {
		t.Fatalf("file %+v %q", f, b)
	}
	if want := []string{KindRegQueued, KindRegClaimed, KindRegDone}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	// The lease is gone: a second report or a file is refused.
	if _, err := s.AddRegressionFile(ctx, runner, r.ID, "x.png", strings.NewReader("x")); code(err) != CodeNoLease {
		t.Fatalf("file after the end: %v", err)
	}
}

// A suite that did not run fails the run even when every test that did run passed: otherwise a
// mobile suite that never started would be reported green.
func TestRegressionSuiteErrorFailsTheRun(t *testing.T) {
	s, _ := open(t)
	var done []Event
	s.Subscribe(func(e Event) {
		if e.Kind == KindRegDone {
			done = append(done, e)
		}
	})
	r := queueReg(t, s, "all")
	claimReg(t, s, "emulator")
	got, err := s.RegressionResult(context.Background(), runner, r.ID, RegressionReport{Error: "mobile: the emulator did not boot",
		Tests: []RegressionReportTest{{Suite: "web", Title: "Login", Status: "passed"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RegFailed || got.Passed != 1 || got.Failed != 0 || got.Error != "mobile: the emulator did not boot" {
		t.Fatalf("result %+v", got)
	}
	if len(done) != 1 || done[0].Data["status"] != RegFailed {
		t.Fatalf("done events %+v", done)
	}
}

func TestRegressionNoReportIsAnError(t *testing.T) {
	s, _ := open(t)
	r := queueReg(t, s, "web")
	claimReg(t, s)
	got, err := s.RegressionResult(context.Background(), runner, r.ID, RegressionReport{Error: "scripts/e2e exited 2 with no report.json"})
	if err != nil || got.Status != RegError || got.Error == "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRegressionOtherRunner(t *testing.T) {
	s, _ := open(t)
	r := queueReg(t, s, "web")
	claimReg(t, s)
	if _, err := s.RegressionResult(context.Background(), "mac", r.ID, RegressionReport{}); code(err) != CodeNotYourLease {
		t.Fatalf("another runner's report: %v", err)
	}
}

func TestFileRegressionFailureDeduplicates(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.SetRepos([]string{"o/server", "o/mobile"})
	r := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r.ID)

	if _, _, err := s.FileRegressionFailure(ctx, "slack:U1", r.ID, 1); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("filing a pass: %v", err)
	}
	it, existing, err := s.FileRegressionFailure(ctx, "slack:U1", r.ID, 2)
	if err != nil || existing {
		t.Fatalf("file it: %v existing %v", err, existing)
	}
	if it.Title != "E2E failing: login › refuses a bad password" || it.Repo != "o/server" || it.Priority != "P2" ||
		!strings.Contains(it.Ask, "Wrong password") || !strings.Contains(it.Ask, "/board/regressions/1") {
		t.Fatalf("item %+v", it)
	}
	d, err := s.GetItem(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Evidence) != 1 || d.Evidence[0].ContentType != "image/png" || d.Evidence[0].Name != "login-failed.png" {
		t.Fatalf("evidence %+v", d.Evidence)
	}
	if _, f, err := s.OpenEvidence(ctx, it.ID, 1); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}

	// Again, and from a later run of the same test: the same item.
	again, existing, err := s.FileRegressionFailure(ctx, "slack:U1", r.ID, 2)
	if err != nil || !existing || again.ID != it.ID {
		t.Fatalf("second press: %v existing %v item %d", err, existing, again.ID)
	}
	r2 := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r2.ID)
	full, _ := s.GetRegression(ctx, r2.ID)
	if full.Tests[1].ItemID != it.ID {
		t.Fatalf("the later run's test does not show the open item: %+v", full.Tests[1])
	}
	later, existing, err := s.FileRegressionFailure(ctx, "slack:U1", r2.ID, 2)
	if err != nil || !existing || later.ID != it.ID {
		t.Fatalf("a later run: %v existing %v item %d", err, existing, later.ID)
	}

	// Once that item is closed, the test is filed anew.
	if _, err := s.Drop(ctx, user, it.ID, "fixed elsewhere"); err != nil {
		t.Fatal(err)
	}
	fresh, existing, err := s.FileRegressionFailure(ctx, "slack:U1", r2.ID, 2)
	if err != nil || existing || fresh.ID == it.ID {
		t.Fatalf("after the item closed: %v existing %v item %d", err, existing, fresh.ID)
	}
}

// A run's events change no item, so they leave the orchestrator's sweep fingerprint alone: a run
// must not start an orchestrate turn.
func TestRegressionEventsLeaveTheOrchFingerprint(t *testing.T) {
	s, _ := open(t)
	add(t, s, "x", "P3")
	before, err := orchFingerprint(s.r)
	if err != nil {
		t.Fatal(err)
	}
	r := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r.ID)
	if after, err := orchFingerprint(s.r); err != nil || after != before {
		t.Fatalf("fingerprint %s → %s (%v)", before, after, err)
	}
}
