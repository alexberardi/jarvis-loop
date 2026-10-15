package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const (
	user   = "user:alex"
	runner = "devbox"
	shaA   = "0123456789abcdef0123456789abcdef01234567"
)

func open(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	s, err := Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clk
}

func code(err error) string {
	var pe *pipeline.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

func add(t *testing.T, s *Store, title, pri string) pipeline.Item {
	t.Helper()
	it, err := s.AddItem(context.Background(), user, NewItem{Title: title, Repo: "jarvis-server", Priority: pri})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestMigrationsFromEmptyAndReopen(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(context.Background())
	if err != nil || v != len(migrationFiles(t)) {
		t.Fatalf("schema version %d (%v), want %d", v, err, len(migrationFiles(t)))
	}
	if _, err := s.AddItem(context.Background(), user, NewItem{Title: "survives"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening applies nothing twice and keeps the data.
	s, err = Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListItems(context.Background(), "", "")
	if err != nil || len(rows) != 1 || rows[0].Title != "survives" {
		t.Fatalf("after reopen: %v %+v", err, rows)
	}
	var mode string
	if err := s.w.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q (%v), want wal", mode, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("data dir must be private: %v %v", fi.Mode(), err)
	}
}

func migrationFiles(t *testing.T) []string {
	t.Helper()
	ents, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestItemRoundTrip(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it, err := s.AddItem(ctx, user, NewItem{Title: "  wake word misses  ", Ask: "fix it", Repo: "jarvis-server", Source: "finding"})
	if err != nil {
		t.Fatal(err)
	}
	if it.ID == 0 || it.Title != "wake word misses" || it.Priority != "P3" || it.Stage != pipeline.Triage || !it.CreatedAt.Equal(t0) {
		t.Fatalf("added: %+v", it)
	}
	scout, err := s.AddItem(ctx, "runner:devbox", NewItem{Title: "scout idea", Source: "scout"})
	if err != nil || scout.Priority != "P4" {
		t.Fatalf("scout: %+v %v", scout, err)
	}
	if _, err := s.AddItem(ctx, user, NewItem{Title: "x", Priority: "P7"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("bad priority: %v", err)
	}

	clk.Advance(time.Hour)
	p1 := "P1"
	if _, err := s.PatchItem(ctx, user, it.ID, &p1, nil); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetItem(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Item.Priority != "P1" || d.Item.Ask != "fix it" || d.Item.Source != "finding" || len(d.Events) != 2 {
		t.Fatalf("detail: %+v", d)
	}
	if d.Events[0].Kind != "created" || d.Events[1].Kind != "patched" || d.Events[1].Actor != user || !d.Events[1].At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("events: %+v", d.Events)
	}

	rows, err := s.ListItems(ctx, "", "P1")
	if err != nil || len(rows) != 1 || rows[0].ID != it.ID || rows[0].Display != "intake" || rows[0].AgeSeconds != 3600 {
		t.Fatalf("board P1: %+v %v", rows, err)
	}
	rows, _ = s.ListItems(ctx, "triage", "")
	if len(rows) != 2 || rows[0].ID != it.ID {
		t.Fatalf("board triage, P1 first: %+v", rows)
	}
	if _, err := s.GetItem(ctx, 999); code(err) != CodeNotFound {
		t.Fatalf("missing item: %v", err)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	s, _ := open(t)
	it := add(t, s, "x", "")
	if _, err := s.w.Exec("UPDATE events SET kind = 'forged' WHERE item_id = ?", it.ID); err == nil {
		t.Fatal("UPDATE on events must fail")
	}
	if _, err := s.w.Exec("DELETE FROM events WHERE item_id = ?", it.ID); err == nil {
		t.Fatal("DELETE on events must fail")
	}
	d, _ := s.GetItem(context.Background(), it.ID)
	if len(d.Events) != 1 || d.Events[0].Kind != "created" {
		t.Fatalf("events changed: %+v", d.Events)
	}
}

func claim(t *testing.T, s *Store, agent string, caps ...string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: agent, Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func evidence(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.AddEvidence(context.Background(), runner, id, "log", "run.log", strings.NewReader("ok\n")); err != nil {
		t.Fatal(err)
	}
}

func TestClaimAdvanceAndLeaseRules(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")

	c := claim(t, s, "agent-1")
	if c == nil || c.Item.ID != it.ID || c.Stage != pipeline.Triage || !c.Lease.ExpiresAt.Equal(t0.Add(LeaseTTL)) {
		t.Fatalf("claim: %+v", c)
	}
	if again := claim(t, s, "agent-2"); again != nil {
		t.Fatalf("a claimed item was handed out again: %+v", again)
	}
	rows, _ := s.ListItems(ctx, "", "")
	if rows[0].Display != "triage" || rows[0].ClaimedBy != "devbox/agent-1" {
		t.Fatalf("board while claimed: %+v", rows[0])
	}

	// Only the lease holder advances, and evidence is per lease.
	_, err := s.Advance(ctx, "other-runner", it.ID, pipeline.Reproduced, nil)
	if code(err) != CodeNotYourLease {
		t.Fatalf("other runner advanced: %v", err)
	}
	f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": "gpu"}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f); code(err) != pipeline.CodeEvidenceRequired {
		t.Fatalf("no evidence: %v", err)
	}
	evidence(t, s, it.ID)
	got, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f)
	if err != nil || got.Stage != pipeline.Build || got.Resources[0] != "gpu" {
		t.Fatalf("advance: %+v %v", got, err)
	}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.PROpened, nil); code(err) != CodeNoLease {
		t.Fatalf("advance without a lease: %v", err)
	}
	if _, err := s.AddEvidence(ctx, runner, it.ID, "", "x", strings.NewReader("x")); code(err) != CodeNoLease {
		t.Fatalf("evidence without a lease: %v", err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	kinds := []string{}
	for _, e := range d.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "created,claimed,refused,refused,evidence,advanced,refused,refused" {
		t.Fatalf("events: %v", kinds)
	}

	// The build lease: a runner without gpu cannot take it.
	if c := claim(t, s, "agent-2"); c != nil {
		t.Fatalf("no-gpu runner got a gpu item: %+v", c)
	}
	c = claim(t, s, "agent-2", "gpu")
	if c == nil || c.Stage != pipeline.Build {
		t.Fatalf("gpu claim: %+v", c)
	}

	// Heartbeats keep it alive past the TTL.
	clk.Advance(4 * time.Minute)
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, "building"); err != nil || stop {
		t.Fatalf("heartbeat: %v %v", stop, err)
	}
	if _, err := s.Heartbeat(ctx, "other-runner", c.Lease.ID, ""); code(err) != CodeNotYourLease {
		t.Fatalf("other runner's heartbeat: %v", err)
	}
	clk.Advance(4 * time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, it.ID); d.Lease == nil {
		t.Fatal("heartbeat did not extend the lease")
	}

	// No heartbeat within the TTL: expired, back to waiting in its stage.
	clk.Advance(LeaseTTL)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetItem(ctx, it.ID)
	if d.Lease != nil || d.Item.Stage != pipeline.Build || d.Events[len(d.Events)-1].Kind != "lease_expired" {
		t.Fatalf("after expiry: lease %+v stage %s last %s", d.Lease, d.Item.Stage, d.Events[len(d.Events)-1].Kind)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat on an expired lease must say stop: %v %v", stop, err)
	}
}

func TestMaxRunTime(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	c := claim(t, s, "a")
	for range 11 { // 44 minutes of heartbeats
		clk.Advance(4 * time.Minute)
		if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, "still reproducing"); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(time.Minute)
	stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, "")
	if err != nil || !stop {
		t.Fatalf("heartbeat past max run: stop=%v %v", stop, err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.ReturnStage != pipeline.Triage ||
		d.Item.Question != "run exceeded 45 minutes; last note: still reproducing" {
		t.Fatalf("overtime item: %+v", d.Item)
	}
}

func TestStopAndUsageLimit(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	c := claim(t, s, "a")

	stopped := true
	if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &stopped}); err != nil {
		t.Fatal(err)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("stopped: heartbeat stop=%v %v", stop, err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Lease != nil || d.Item.Stage != pipeline.Triage {
		t.Fatalf("stop must return the item to its queue: %+v", d)
	}
	if c := claim(t, s, "a"); c != nil {
		t.Fatal("claim while stopped")
	}
	resume := false
	if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &resume}); err != nil {
		t.Fatal(err)
	}
	if c = claim(t, s, "a"); c == nil {
		t.Fatal("no claim after resume")
	}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.UsageLimit, map[string]string{"error": "429", "reset": "17:00"}); err != nil {
		t.Fatal(err)
	}
	ctl, _ := s.Control(ctx)
	if ctl.ActiveAgents != 0 {
		t.Fatalf("usage_limit must set active agents to 0, got %d", ctl.ActiveAgents)
	}
	if c := claim(t, s, "a"); c != nil {
		t.Fatal("claim at active agents 0")
	}
	d, _ = s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.Triage || d.Lease != nil {
		t.Fatalf("usage_limit item: %+v", d)
	}
	n := 7
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &n}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("active agents 7: %v", err)
	}
	if _, err := s.SetControl(ctx, user, ControlPatch{Limits: map[string]int{"gpu": 1}}); err != nil {
		t.Fatal(err)
	}
	ctl, _ = s.Control(ctx)
	if ctl.Limits["gpu"] != 1 || ctl.Limits["voice"] != 1 {
		t.Fatalf("limits merge: %+v", ctl.Limits)
	}
}

func TestEvidenceStorage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	claim(t, s, "a")
	ev, err := s.AddEvidence(ctx, runner, it.ID, "before", "../../etc/pass wd.png", strings.NewReader("\x89PNG\r\n\x1a\nrest"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.N != 1 || ev.Name != "etc_pass_wd.png" || ev.Size != 12 || ev.ContentType != "image/png" || len(ev.SHA256) != 64 || ev.Label != "before" {
		t.Fatalf("evidence: %+v", ev)
	}
	want := filepath.Join(s.dir, "evidence", fmt.Sprint(it.ID), "1-etc_pass_wd.png")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("not stored at %s: %v", want, err)
	}
	ev2, err := s.AddEvidence(ctx, runner, it.ID, "", "", strings.NewReader("hello"))
	if err != nil || ev2.N != 2 || ev2.Name != "file" {
		t.Fatalf("2nd: %+v %v", ev2, err)
	}
	got, f, err := s.OpenEvidence(ctx, it.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello" || got.N != 2 {
		t.Fatalf("read back %q %+v", b, got)
	}
	if _, _, err := s.OpenEvidence(ctx, it.ID, 3); code(err) != CodeNotFound {
		t.Fatalf("missing evidence: %v", err)
	}

	big := io.LimitReader(zeros{}, MaxEvidenceBytes+1)
	if _, err := s.AddEvidence(ctx, runner, it.ID, "", "big.bin", big); code(err) != CodeTooLarge {
		t.Fatalf("over 20 MB: %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(s.dir, "evidence", fmt.Sprint(it.ID)))
	if len(left) != 2 {
		t.Fatalf("a refused upload left files behind: %v", left)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Many runners claiming at once never get the same item, and never more than active agents.
func TestConcurrentClaims(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	six := 6
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &six, Limits: map[string]int{"triage": 6}}); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		add(t, s, fmt.Sprintf("item %d", i), "")
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  = map[int64]int{}
		errs []error
	)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: fmt.Sprintf("agent-%d", i)})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if c != nil {
				got[c.Item.ID]++
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("claim errors: %v", errs)
	}
	if len(got) != 6 {
		t.Fatalf("granted %d distinct items, want 6 (active agents)", len(got))
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("item %d handed out %d times", id, n)
		}
	}
}

func TestHumanActionsAndRefusals(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.Answer(ctx, user, it.ID, "x"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("answer at triage: %v", err)
	}
	if _, err := s.Approve(ctx, user, it.ID, shaA, ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("approve at triage: %v", err)
	}
	claim(t, s, "a")
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Question, map[string]string{"question": "which repo?"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Answer(ctx, user, it.ID, "jarvis-server")
	if err != nil || got.Stage != pipeline.Triage {
		t.Fatalf("answer: %+v %v", got, err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	refused := 0
	for _, e := range d.Events {
		if e.Kind == "refused" {
			refused++
			if e.Data["code"] != pipeline.CodeWrongStage {
				t.Fatalf("refused event data: %+v", e.Data)
			}
		}
	}
	if refused != 2 {
		t.Fatalf("want 2 refused events, got %d", refused)
	}
}
