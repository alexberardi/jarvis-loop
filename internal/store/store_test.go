package store

import (
	"context"
	"encoding/json"
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
	_, err := s.Advance(ctx, "other-runner", it.ID, pipeline.Reproduced, nil, nil)
	if code(err) != CodeNotYourLease {
		t.Fatalf("other runner advanced: %v", err)
	}
	f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": "gpu"}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f, nil); code(err) != pipeline.CodeEvidenceRequired {
		t.Fatalf("no evidence: %v", err)
	}
	evidence(t, s, it.ID)
	got, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f, nil)
	if err != nil || got.Stage != pipeline.Build || got.Resources[0] != "gpu" {
		t.Fatalf("advance: %+v %v", got, err)
	}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.PROpened, nil, nil); code(err) != CodeNoLease {
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
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.UsageLimit, map[string]string{"error": "429", "reset": "17:00"}, nil); err != nil {
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
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Question, map[string]string{"question": "which repo?"}, nil); err != nil {
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

func TestHoldRulesPersist(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ctl, _ := s.Control(ctx)
	if len(ctl.HoldRules) != len(pipeline.DefaultHoldRules()) || ctl.HoldRules[0].Name != "auth" {
		t.Fatalf("default hold rules: %+v", ctl.HoldRules)
	}
	rules := []pipeline.HoldRule{{Name: "secrets", Glob: "**/*.env"}, {Name: "auth", Glob: "internal/modules/auth/**"}}
	if _, err := s.SetControl(ctx, user, ControlPatch{HoldRules: &rules}); err != nil {
		t.Fatal(err)
	}
	bad := []pipeline.HoldRule{{Name: "x", Glob: "/etc/**"}}
	if _, err := s.SetControl(ctx, user, ControlPatch{HoldRules: &bad}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("bad rule: %v", err)
	}
	s.Close()
	s, err = Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctl, _ = s.Control(ctx)
	if len(ctl.HoldRules) != 2 || ctl.HoldRules[0].Name != "secrets" {
		t.Fatalf("hold rules after reopen: %+v", ctl.HoldRules)
	}

	// The rules in control are the ones advance uses, and hold_reason is stored.
	it := add(t, s, "x", "")
	claim(t, s, "t")
	evidence(t, s, it.ID)
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil); err != nil {
		t.Fatal(err)
	}
	claim(t, s, "b")
	evidence(t, s, it.ID)
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.PROpened, map[string]string{"repo": "r", "pr": "1", "branch": "b", "head_sha": shaA},
		[]string{"A config/prod.env"}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Risk != pipeline.RiskHold || d.Item.HoldReason != "secrets: A config/prod.env" {
		t.Fatalf("risk %q reason %q", d.Item.Risk, d.Item.HoldReason)
	}
}

// Triage sets the repo of an item that has none, from loopd's configured slugs only.
func TestReproducedRepoUsesConfiguredSlugs(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it, err := s.AddItem(ctx, "slack:U1", NewItem{Title: "from slack, no repo"})
	if err != nil {
		t.Fatal(err)
	}
	f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": "", "repo": "alexberardi/jarvis-loop"}
	claim(t, s, "triager")
	evidence(t, s, it.ID)
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f, nil); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("no configured repos, but the repo was taken: %v", err)
	}
	s.SetRepos([]string{"alexberardi/jarvis-loop"})
	got, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f, nil)
	if err != nil || got.Repo != "alexberardi/jarvis-loop" {
		t.Fatalf("advance: %+v %v", got, err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Repo != "alexberardi/jarvis-loop" {
		t.Fatalf("stored repo %q", d.Item.Repo)
	}
}

// The open list (the runner's triage snapshot) leaves out done and closed items and shows each
// item's repo and PR.
func TestListOpenItems(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "open one", "")
	b := add(t, s, "closed one", "")
	claim(t, s, "triager") // a (oldest first)
	evidence(t, s, a.ID)
	if _, err := s.Advance(ctx, runner, a.ID, pipeline.Reproduced, map[string]string{"area": "x", "size": "S", "risk": "normal", "resources": ""}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "triager-2", Stages: []pipeline.Stage{pipeline.Triage}})
	if err != nil || c == nil || c.Item.ID != b.ID {
		t.Fatalf("claim %+v", c)
	}
	evidence(t, s, b.ID)
	if _, err := s.Advance(ctx, runner, b.ID, pipeline.NotReproducible, map[string]string{"summary": "dup"}, nil); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListItems(ctx, "", "")
	open, err := s.ListItemsFilter(ctx, ListFilter{Open: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(open) != 1 || open[0].ID != a.ID || open[0].Repo != "jarvis-server" || open[0].Stage != "build" {
		t.Fatalf("all %+v\nopen %+v", all, open)
	}
}

// Migration 0004 tags the rules a user set before rules had a repo with jarvis-server's slug: they
// were jarvis-server's hold list. The built-in defaults (”) stay untouched.
func TestHoldRulesMigrationTagsJarvisServer(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	// As a database at version 3 with custom rules would look.
	if _, err := s.w.Exec(`DELETE FROM schema_migrations WHERE version = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE control SET hold_rules = ?`,
		`[{"name":"auth","glob":"internal/modules/auth/**"},{"name":"tests","glob":"**/*_test.go","statuses":["D","R"]},{"name":"mine","glob":"x/**","repo":"o/other"}]`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(dir, clk); err != nil {
		t.Fatal(err)
	}
	ctl, err := s.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []pipeline.HoldRule{
		{Name: "auth", Glob: "internal/modules/auth/**", Repo: pipeline.ServerRepo},
		{Name: "tests", Glob: "**/*_test.go", Statuses: []string{"D", "R"}, Repo: pipeline.ServerRepo},
		{Name: "mine", Glob: "x/**", Repo: "o/other"},
	}
	if fmt.Sprint(ctl.HoldRules) != fmt.Sprint(want) {
		t.Fatalf("migrated rules %+v, want %+v", ctl.HoldRules, want)
	}
	s.Close()

	// A fresh database keeps the built-in defaults (both repos' lists).
	s2, _ := open(t)
	ctl, _ = s2.Control(context.Background())
	if fmt.Sprint(ctl.HoldRules) != fmt.Sprint(pipeline.DefaultHoldRules()) {
		t.Fatalf("fresh rules %+v", ctl.HoldRules)
	}
}

// toDone takes an item through every stage, merged by runnerName.
func toDone(t *testing.T, s *Store, runnerName string) pipeline.Item {
	t.Helper()
	ctx := context.Background()
	it := add(t, s, "ship it", "")
	step := func(agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) {
		t.Helper()
		c, err := s.Claim(ctx, runnerName, pipeline.ClaimRequest{Runner: runnerName, Agent: agent})
		if err != nil || c == nil || c.Item.ID != it.ID {
			t.Fatalf("claim for %s: %+v %v", out, c, err)
		}
		if ev {
			if _, err := s.AddEvidence(ctx, runnerName, it.ID, "log", "run.log", strings.NewReader("ok\n")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Advance(ctx, runnerName, it.ID, out, f, paths); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	step("triager", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil, true)
	step("builder", pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, []string{"M x.go"}, true)
	step("reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)
	step("lander", pipeline.Merged, map[string]string{"merge_sha": "ffffffffffffffffffffffffffffffffffffffff", "head_sha": shaA}, []string{"M x.go"}, false)
	return it
}

// main_ci is recorded by the runner that merged the item, once, on a done item, with a known
// result; anything else is refused.
func TestMainCIEvent(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toDone(t, s, runner)
	good := map[string]any{"result": "red", "urls": []any{"https://github.com/o/r/actions/runs/1"}, "failing": []any{"test"},
		"merge_sha": "ffffffffffffffffffffffffffffffffffffffff", "p1_item": float64(9)}

	if _, err := s.MainCI(ctx, "other-runner", it.ID, good); code(err) != CodeNotYourMerge {
		t.Fatalf("another runner: %v", err)
	}
	for name, bad := range map[string]map[string]any{
		"no result":       {"urls": []any{}},
		"unknown result":  {"result": "purple"},
		"urls not a list": {"result": "green", "urls": "https://x"},
		"url not text":    {"result": "green", "urls": []any{1.0}},
		"unknown key":     {"result": "green", "token": "x"},
		"bad p1":          {"result": "red", "p1_item": "nine"},
	} {
		if _, err := s.MainCI(ctx, runner, it.ID, bad); code(err) != pipeline.CodeInvalidField && code(err) != pipeline.CodeMissingField {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ev, err := s.MainCI(ctx, runner, it.ID, good)
	if err != nil || ev.Kind != "main_ci" || ev.Actor != "runner:"+runner || ev.Data["result"] != "red" {
		t.Fatalf("main_ci: %+v %v", ev, err)
	}
	if _, err := s.MainCI(ctx, runner, it.ID, good); code(err) != CodeAlreadyRecorded {
		t.Fatalf("a second main_ci: %v", err)
	}
	// Not done (or not merged at all): refused.
	notDone := add(t, s, "not merged", "")
	if _, err := s.MainCI(ctx, runner, notDone.ID, good); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("an open item: %v", err)
	}
	if _, err := s.MainCI(ctx, runner, 999, good); code(err) != CodeNotFound {
		t.Fatalf("no item: %v", err)
	}
}

// Changing the active agents (or any other control field) leaves the hold rules on the built-in
// defaults: it used to write them out as if the user had set them, which froze them, so rules
// added to the defaults later (phase 6b's repos) never applied.
func TestControlChangeKeepsLiveDefaultRules(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	n := 2
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &n}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.w.QueryRow(`SELECT hold_rules FROM control WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		t.Fatalf("hold_rules written out on an active-agents change: %.80s…", raw)
	}
	// The defaults the store serves are the live ones.
	s.SetDefaultHoldRules([]pipeline.HoldRule{{Name: "only", Glob: "x/**"}})
	ctl, _ := s.Control(ctx)
	if len(ctl.HoldRules) != 1 || ctl.HoldRules[0].Name != "only" {
		t.Fatalf("rules after a control change: %+v", ctl.HoldRules)
	}
	// Rules the user sets are kept, and stay set through other changes.
	rules := []pipeline.HoldRule{{Name: "mine", Glob: "a/**"}}
	if _, err := s.SetControl(ctx, user, ControlPatch{HoldRules: &rules}); err != nil {
		t.Fatal(err)
	}
	n = 3
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &n}); err != nil {
		t.Fatal(err)
	}
	if ctl, _ = s.Control(ctx); len(ctl.HoldRules) != 1 || ctl.HoldRules[0].Name != "mine" {
		t.Fatalf("user rules after a control change: %+v", ctl.HoldRules)
	}
}

// A database holding a copy of an earlier built-in list (written by a control change before the
// fix above) goes back to the live defaults when loopd opens it; anything else is the user's and
// stays.
func TestOpenResetsACopyOfBuiltInRules(t *testing.T) {
	var phase6 []pipeline.HoldRule
	for _, r := range pipeline.DefaultHoldRules() {
		if r.Repo == pipeline.ServerRepo || r.Repo == pipeline.LoopRepo {
			phase6 = append(phase6, r)
		}
	}
	custom := append(append([]pipeline.HoldRule(nil), phase6...), pipeline.HoldRule{Name: "mine", Glob: "x/**", Repo: pipeline.LoopRepo})
	for name, c := range map[string]struct {
		rules []pipeline.HoldRule
		reset bool
	}{"phase 6 copy": {phase6, true}, "custom": {custom, false}} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			clk := clock.NewFake(t0)
			s, err := Open(dir, clk)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(c.rules)
			if _, err := s.w.Exec(`UPDATE control SET hold_rules = ?`, string(b)); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if s, err = Open(dir, clk); err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctl, _ := s.Control(context.Background())
			want := c.rules
			if c.reset {
				want = pipeline.DefaultHoldRules()
			}
			if fmt.Sprint(ctl.HoldRules) != fmt.Sprint(want) {
				t.Fatalf("rules after reopen: %d rules, want %d", len(ctl.HoldRules), len(want))
			}
		})
	}
}
