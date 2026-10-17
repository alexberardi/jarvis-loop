package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/harness"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The test binary doubles as the fake claude and as loop-guard (LOOP_TEST_AS), so the runner
// starts real processes in their own group, and the fake runs the real guard as its hook.
func TestMain(m *testing.M) {
	switch os.Getenv("LOOP_TEST_AS") {
	case "claude":
		os.Exit(fakeclaude.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "harness":
		os.Exit(harness.Main(os.Args[1:]))
	case "guard":
		path, err := guard.ConfigPath(os.Args[1:], os.Getenv)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(guard.Hook(path, os.Stdin, os.Stdout, os.Stderr, time.Now))
	}
	os.Exit(m.Run())
}

const (
	runnerToken = "runner-token-0123456789abcdef"
	userToken   = "user-token-0123456789abcdef"
	orchToken   = "orchestrator-token-0123456789"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type rig struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	loopdClk *clock.Fake
	clk      *FakeClock
	r        *Runner
	prs      *fakePRs
	remote   string // the "GitHub" bare repo
	local    string // the user's own clone
	localRef string // its HEAD before the run
	logs     *syncBuffer
	scripts  string // the fake claude's per-item scripts
}

// syncBuffer is the runner's log in the tests: a test may read it while a run writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRig(t *testing.T, scenario string) *rig {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	ctx := context.Background()
	// A "GitHub" remote with one commit on main, and the user's clone of it.
	remote := filepath.Join(root, "github", "demo.git")
	os.MkdirAll(remote, 0o755)
	gitT(t, remote, "init", "--bare", "-q", "-b", "main", ".")
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	gitT(t, seed, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("demo\n"), 0o644)
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "-q", "-m", "init")
	gitT(t, seed, "push", "-q", remote, "main")
	local := filepath.Join(root, "home", "jarvis", "demo")
	gitT(t, root, "clone", "-q", remote, local)

	loopdClk := clock.NewFake(t0)
	st, err := store.Open(filepath.Join(root, "loopd"), loopdClk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := api.ParseTokens(strings.NewReader(runnerToken + "\trunner\tdevbox\n" + userToken + "\tuser\talex\n" + orchToken + "\torchestrator\tloop\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	self, _ := os.Executable()
	verify := false
	cfg := &Config{
		LoopdURL: srv.URL, Token: runnerToken, Name: "devbox", Slots: 1, WorkRoot: filepath.Join(root, "work"),
		Claude: self, Guard: self, Mode: ModeAuto, PollSeconds: 30, UserEmulator: "emulator-5554", VerifyPR: &verify,
		VerifyLock: filepath.Join(root, "verify.lock"),
		// The build tests' runner builds only; the triage and review tests set the stages.
		Stages: []string{StageBuild},
		Repos: map[string]RepoConfig{
			"demo":  {Local: local, Remote: remote, Slug: "alex/demo", Base: "main", Description: "the demo repo"},
			"other": {Remote: remote, Slug: "alex/other", Base: "main", Description: "another repo (the same remote)"},
		},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	clk := NewFakeClock(t0)
	r := &Runner{
		Cfg: cfg, Client: &Client{Base: srv.URL, Token: runnerToken}, Clock: clk,
		Log: slog.New(slog.NewTextHandler(logs, nil)),
		// GORACE: a -race binary sleeps 1 s at exit by default, and every guard call is one.
		Env: append(os.Environ(), "LOOP_TEST_AS=claude", "FAKE_CLAUDE_SCENARIO="+scenario, "LOOP_TOKEN=must-not-leak", "GORACE=atexit_sleep_ms=0",
			"FAKE_CLAUDE_SCRIPTS="+filepath.Join(root, "scripts")),
	}
	os.MkdirAll(filepath.Join(root, "scripts"), 0o755)
	st.SetRepos([]string{"alex/demo", "alex/other"})
	// "GitHub": PRs are a test double; the PR check reads the bare remote.
	prs := &fakePRs{next: 7}
	r.PRs = prs
	r.VerifyPR = func(ctx context.Context, slug string, pr int) (PRInfo, error) {
		spec, ok := prs.byNumber(pr)
		if !ok || slug != "alex/demo" {
			return PRInfo{}, fmt.Errorf("no PR #%d on %s", pr, slug)
		}
		out, _ := exec.Command("git", "-C", remote, "rev-parse", "refs/heads/"+spec.Head).Output()
		return PRInfo{HeadRefName: spec.Head, HeadRefOid: strings.TrimSpace(string(out)), State: "OPEN"}, nil
	}
	return &rig{t: t, ctx: ctx, st: st, loopdClk: loopdClk, clk: clk, r: r, prs: prs, remote: remote, local: local,
		localRef: gitT(t, local, "rev-parse", "HEAD"), logs: logs, scripts: filepath.Join(root, "scripts")}
}

// fakePRs is GitHub's PRs for the tests: it records what the runner asked for.
type fakePRs struct {
	mu        sync.Mutex
	next      int
	open      map[string]int // branch -> PR
	specs     map[int]PRSpec
	bodies    map[int]string
	created   []int
	edited    []int
	comments  map[int][]string // PR -> comment bodies the runner posted
	merged    []MergedPR
	mergedErr map[string]error
}

func (f *fakePRs) View(_ context.Context, slug string, pr int) (PRView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.specs[pr]
	if !ok {
		return PRView{}, fmt.Errorf("no PR #%d", pr)
	}
	return PRView{Number: pr, Title: s.Title, Body: f.bodies[pr], HeadRefName: s.Head, BaseRefName: s.Base, State: "OPEN"}, nil
}

func (f *fakePRs) Comment(_ context.Context, slug string, pr int, bodyFile string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(bodyFile)
	if err != nil {
		return err
	}
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	f.comments[pr] = append(f.comments[pr], string(b))
	return nil
}

func (f *fakePRs) Comments(_ context.Context, slug string, pr int) ([]PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PRComment
	for i, c := range f.comments[pr] {
		out = append(out, PRComment{Author: "alex", At: fmt.Sprintf("2026-10-15T12:0%d:00Z", i), Body: c})
	}
	out = append(out, PRComment{Author: "alex", At: "2026-10-15T12:09:00Z", Body: "Name the test after the case.", Path: "feature.txt", Line: 1})
	return out, nil
}

func (f *fakePRs) MergedPRs(_ context.Context, slug string, n int) ([]MergedPR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.mergedErr[slug]; err != nil {
		return nil, err
	}
	return f.merged, nil
}

func (f *fakePRs) Find(_ context.Context, slug, branch string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[branch], nil
}

func (f *fakePRs) record(pr int, s PRSpec) {
	if f.open == nil {
		f.open, f.specs, f.bodies = map[string]int{}, map[int]PRSpec{}, map[int]string{}
	}
	b, _ := os.ReadFile(s.BodyFile)
	f.open[s.Head], f.specs[pr], f.bodies[pr] = pr, s, string(b)
}

func (f *fakePRs) Create(_ context.Context, s PRSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.open[s.Head]; ok {
		return 0, fmt.Errorf("a PR for %s already exists", s.Head)
	}
	pr := f.next
	f.next++
	f.record(pr, s)
	f.created = append(f.created, pr)
	return pr, nil
}

func (f *fakePRs) Edit(_ context.Context, pr int, s PRSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(pr, s)
	f.edited = append(f.edited, pr)
	return nil
}

func (f *fakePRs) byNumber(pr int) (PRSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.specs[pr]
	return s, ok
}

func (f *fakePRs) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created) + len(f.edited)
}

// toBuild adds an item and walks it through triage by hand, as phase 4 does.
func (g *rig) toBuild(title string) int64 {
	g.t.Helper()
	it, err := g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: title, Ask: "Add a feature file.", Repo: "demo"})
	if err != nil {
		g.t.Fatal(err)
	}
	c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "triager", Stages: []pipeline.Stage{pipeline.Triage}})
	if err != nil || c == nil || c.Item.ID != it.ID {
		g.t.Fatalf("triage claim %v %v", c, err)
	}
	if _, err := g.st.AddEvidence(g.ctx, "devbox", it.ID, "repro", "repro.txt", strings.NewReader("seen")); err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", it.ID, pipeline.Reproduced,
		map[string]string{"area": "demo", "size": "S", "risk": "normal", "resources": ""}, nil); err != nil {
		g.t.Fatal(err)
	}
	return it.ID
}

func (g *rig) item(id int64) store.Detail {
	g.t.Helper()
	d, err := g.st.GetItem(g.ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return d
}

func (g *rig) runOnce() bool {
	g.t.Helper()
	ok, err := g.r.ClaimAndRun(g.ctx)
	if err != nil {
		g.t.Fatal(err)
	}
	return ok
}

// runAsync runs ClaimAndRun in the background; the returned func waits for it.
func (g *rig) runAsync() func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.r.ClaimAndRun(g.ctx)
	}()
	return func() {
		g.t.Helper()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			g.t.Fatalf("the run did not end; log:\n%s", g.logs.String())
		}
	}
}

func (g *rig) lastEvent(id int64, kind string) *store.Event {
	for _, e := range g.item(id).Events {
		if e.Kind == kind {
			e := e
			defer func() {}()
			return &e
		}
	}
	return nil
}

func (g *rig) events(id int64, kind string) []store.Event {
	var out []store.Event
	for _, e := range g.item(id).Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (g *rig) advanced(id int64) store.Event {
	g.t.Helper()
	evs := g.events(id, "advanced")
	if len(evs) < 2 { // the first is triage's reproduced
		g.t.Fatalf("no advance from the runner; log:\n%s", g.logs.String())
	}
	return evs[len(evs)-1]
}

func (g *rig) remoteHead(branch string) string {
	out, err := exec.Command("git", "-C", g.remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// userUntouched checks the user's clone is exactly as before.
func (g *rig) userUntouched() {
	g.t.Helper()
	if h := gitT(g.t, g.local, "rev-parse", "HEAD"); h != g.localRef {
		g.t.Fatalf("the user's clone moved: %s", h)
	}
	if s := gitT(g.t, g.local, "status", "--porcelain"); s != "" {
		g.t.Fatalf("the user's clone changed:\n%s", s)
	}
	if b := gitT(g.t, g.local, "branch", "--list"); strings.Contains(b, "loop/") {
		g.t.Fatalf("a loop branch in the user's clone:\n%s", b)
	}
}

func (g *rig) cleanedUp(id int64) {
	g.t.Helper()
	wt := filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id))
	for _, p := range []string{wt} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			g.t.Fatalf("%s left behind", p)
		}
	}
}

func TestGoodRunOpensPR(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Add a feature")
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s; log:\n%s", d.Item.Stage, g.logs.String())
	}
	branch := fmt.Sprintf("loop/%d-add-a-feature", id)
	head := g.remoteHead(branch) // pushed by the runner, not the agent
	if head == "" || d.Item.HeadSHA != head || d.Item.Branch != branch || d.Item.PR != 7 || d.Item.Repo != "alex/demo" {
		t.Fatalf("item %+v, remote head %s", d.Item, head)
	}
	// The runner opened the PR from the agent's title and body, plus its Verification footer.
	spec, _ := g.prs.byNumber(7)
	if len(g.prs.created) != 1 || spec.Slug != "alex/demo" || spec.Head != branch || spec.Base != "main" || spec.Title != "Add a feature" {
		t.Fatalf("PR %+v (created %v)", spec, g.prs.created)
	}
	body := g.prs.bodies[7]
	for _, want := range []string{"Adds `feature.txt` (1 commits).", "### Verification", "- `after.txt`: tests", "on loop item #",
		"Run: 5 turns, prompt-cache hit 79%", "about $0.42"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, body)
		}
	}
	ev := g.advanced(id)
	if ev.Outcome != "pr_opened" || ev.Data["pr_verified"] != "true" || ev.Data["pr_action"] != "opened" {
		t.Fatalf("outcome %s %v", ev.Outcome, ev.Data)
	}
	paths, _ := ev.Data["changed_paths"].([]any)
	if len(paths) != 1 || paths[0] != "A\tfeature.txt" {
		t.Fatalf("changed paths %v (from git, not the agent)", paths)
	}
	for _, k := range []string{"num_turns", "duration_ms", "total_cost_usd", "run_seconds"} {
		if ev.Data[k] == nil || ev.Data[k] == "" {
			t.Errorf("advance lacks %s: %v", k, ev.Data)
		}
	}
	if ev.Data["permission_mode"] != "auto" || ev.Data["total_cost_usd"] != "0.4200" {
		t.Errorf("data %v", ev.Data)
	}
	// Token use per run, for the digest: the fake reports 1,000 input, 20,000 cache creation and
	// 79,000 cache read tokens.
	for k, want := range map[string]string{"usage_input": "1000", "usage_output": "4000", "usage_cache_creation": "20000",
		"usage_cache_read": "79000", "cache_hit_ratio": "0.79", "session_id": "fake-session-good"} {
		if ev.Data[k] != want {
			t.Errorf("%s = %v, want %s", k, ev.Data[k], want)
		}
	}
	if len(g.events(id, "low_cache_hit")) != 0 {
		t.Error("a 0.79 hit ratio is not low")
	}
	if len(d.Evidence) != 2 || d.Evidence[1].Name != "after.txt" || d.Evidence[1].Label != "tests" {
		t.Fatalf("evidence %+v", d.Evidence)
	}
	if len(g.events(id, "guard_refused")) != 0 {
		t.Fatal("nothing should have been refused")
	}
	if g.remoteHead("main") == head {
		t.Fatal("main moved")
	}
	g.userUntouched()
	g.cleanedUp(id)
	// The run left its log, and the agent never saw the runner's token.
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-*", id), "stream.jsonl"))
	if len(runs) != 1 {
		t.Fatalf("run log %v", runs)
	}
	for _, kv := range g.r.agentEnv() {
		if strings.HasPrefix(kv, "LOOP_TOKEN=") {
			t.Fatal("LOOP_TOKEN reaches the agent")
		}
	}
}

// A runner claims only the stages in its config.
func TestOnlyConfiguredStagesAreClaimed(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if _, err := g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: "still in triage", Repo: "demo"}); err != nil {
		t.Fatal(err)
	}
	if g.runOnce() {
		t.Fatal("a build runner claimed a triage item")
	}
}

func TestGuardRefusalIsReported(t *testing.T) {
	g := newRig(t, fakeclaude.Escape)
	id := g.toBuild("Escape artist")
	g.runOnce()
	refs := g.events(id, "guard_refused")
	if len(refs) != 2 {
		t.Fatalf("guard_refused events %d; log:\n%s", len(refs), g.logs.String())
	}
	if !strings.Contains(fmt.Sprint(refs[1].Data["reason"]), "ready_for_pr") {
		t.Errorf("the push refusal should say who pushes: %v", refs[1].Data["reason"])
	}
	if refs[0].Data["rule"] != guard.RuleWriteOutside || refs[0].Data["tool"] != "Write" || refs[1].Data["rule"] != guard.RuleGitPush {
		t.Fatalf("refusals %v / %v", refs[0].Data, refs[1].Data)
	}
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatal("the write outside the worktree happened")
	}
	if g.remoteHead("main") != g.localRef {
		t.Fatal("main was pushed")
	}
	if g.item(id).Item.Stage != pipeline.Review || g.advanced(id).Data["guard_refusals"] != "2" {
		t.Fatalf("the run should still finish: %+v", g.advanced(id).Data)
	}
}

func TestBadResultsFail(t *testing.T) {
	// The runner's refusals of a result are retried once, then reported as a runner error; the
	// others fail at once.
	refused := map[string]bool{fakeclaude.Lying: true, fakeclaude.Malformed: true, fakeclaude.SymlinkEv: true, fakeclaude.Dirty: true}
	cases := map[string]string{
		fakeclaude.Lying:     "head_sha",
		fakeclaude.Malformed: "not valid JSON",
		fakeclaude.NoResult:  "without a usable .loop/result.json",
		fakeclaude.Exit3:     "code 3",
		fakeclaude.SymlinkEv: "not a regular file",
		fakeclaude.NoArm:     "guard_not_armed",
		fakeclaude.Dirty:     "uncommitted changes (?? notes.txt)",
	}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			g := newRig(t, scenario)
			id := g.toBuild("Bad " + scenario)
			g.runOnce()
			d := g.item(id)
			if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, want) {
				t.Fatalf("stage %s, question %q, want it to mention %q; log:\n%s", d.Item.Stage, d.Item.Question, want, g.logs.String())
			}
			runs := 1
			if refused[scenario] {
				runs = 2
				if !strings.HasPrefix(d.Item.Question, "the build's result couldn't be accepted: ") || !strings.Contains(d.Item.Question, "(again, after one retry)") {
					t.Fatalf("a refused result is a runner error, not a question: %q", d.Item.Question)
				}
			}
			if n := g.starts(id); n != runs {
				t.Fatalf("%d runs, want %d", n, runs)
			}
			if scenario == fakeclaude.Exit3 && !strings.Contains(d.Item.Question, "the tests do not compile") {
				t.Fatalf("the agent's last output belongs in the question: %q", d.Item.Question)
			}
			if g.prs.calls() != 0 || g.remoteHead(fmt.Sprintf("loop/%d-bad-%s", id, Slug(scenario))) != "" {
				t.Fatalf("a rejected result is neither pushed nor opened as a PR")
			}
			g.userUntouched()
			g.cleanedUp(id)
		})
	}
}

// A refused build result gets one retry; the retry's good result is pushed and opened as a PR.
func TestRefusedBuildResultIsRetried(t *testing.T) {
	g := newRig(t, fakeclaude.LyingOnce)
	id := g.toBuild("Lies once")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || g.starts(id) != 2 || g.prs.calls() != 1 {
		t.Fatalf("stage %s %q after %d runs; log:\n%s", d.Item.Stage, d.Item.Question, g.starts(id), g.logs.String())
	}
	ev := g.advanced(id)
	if ev.Data["result_retried"] != "true" || ev.Data["head_sha"] != g.remoteHead(fmt.Sprintf("loop/%d-lies-once", id)) {
		t.Fatalf("advance %+v", ev.Data)
	}
	// The run's numbers cover both attempts.
	dir := g.lastRun(id, StageBuild)
	first, retry := streamResult(t, filepath.Join(dir, "stream-1.jsonl")), streamResult(t, filepath.Join(dir, "stream.jsonl"))
	if ev.Data["total_cost_usd"] != "0.8400" || ev.Data["duration_ms"] != "2468" ||
		ev.Data["num_turns"] != strconv.Itoa(first.NumTurns+retry.NumTurns) || ev.Data["first_session_id"] == nil {
		t.Fatalf("advance %+v; attempts' turns %d + %d", ev.Data, first.NumTurns, retry.NumTurns)
	}
}

// streamResult is the result event of a run's stream file.
func streamResult(t *testing.T, path string) resultEvent {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		var ev struct {
			Type string `json:"type"`
			resultEvent
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "result" {
			return ev.resultEvent
		}
	}
	t.Fatalf("no result event in %s", path)
	return resultEvent{}
}

// An open PR for the branch (a bounced item) is updated, not opened again.
func TestExistingPRIsUpdated(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Bounced")
	branch := fmt.Sprintf("loop/%d-bounced", id)
	g.prs.record(5, PRSpec{Head: branch, BodyFile: os.DevNull})
	g.prs.specs[5] = PRSpec{Head: branch}
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.PR != 5 || len(g.prs.created) != 0 || !slices.Equal(g.prs.edited, []int{5}) {
		t.Fatalf("stage %s pr %d created %v edited %v; log:\n%s", d.Item.Stage, d.Item.PR, g.prs.created, g.prs.edited, g.logs.String())
	}
	if a := g.advanced(id).Data["pr_action"]; a != "updated" {
		t.Fatalf("pr_action %v", a)
	}
	if spec, _ := g.prs.byNumber(5); spec.Title != "Add a feature" || !strings.Contains(g.prs.bodies[5], "### Verification") {
		t.Fatalf("PR 5 not updated: %+v", spec)
	}
}

// A push the runner cannot make fails the item with the reason; no PR is opened.
func TestPushFailureFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	var pushed []string
	g.r.Push = func(ctx context.Context, mirror, branch string) error {
		pushed = append(pushed, branch)
		return fmt.Errorf("rejected: non-fast-forward")
	}
	id := g.toBuild("No push")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "could not push loop/") || !strings.Contains(d.Item.Question, "non-fast-forward") {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	if !slices.Equal(pushed, []string{fmt.Sprintf("loop/%d-no-push", id)}) || g.prs.calls() != 0 {
		t.Fatalf("pushed %v, PR calls %d", pushed, g.prs.calls())
	}
}

// A question leaves the agent's commits in the runner's mirror; the next run continues the branch
// and is told so, and the runner pushes both runs' work.
func TestResumeContinuesTheBranch(t *testing.T) {
	g := newRig(t, fakeclaude.CommitAsk)
	id := g.toBuild("Two runs")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.Question != "Is one feature file enough?" {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	branch := fmt.Sprintf("loop/%d-two-runs", id)
	if g.remoteHead(branch) != "" || g.prs.calls() != 0 {
		t.Fatal("a question pushes nothing")
	}
	mirror := filepath.Join(g.r.Cfg.WorkRoot, "mirrors", "demo.git")
	first := gitT(t, mirror, "rev-parse", "refs/heads/"+branch)
	if _, err := g.st.Answer(g.ctx, "user:alex", id, "Yes."); err != nil {
		t.Fatal(err)
	}
	g.r.Env = append(g.r.Env, "FAKE_CLAUDE_SCENARIO="+fakeclaude.Good)
	g.runOnce()
	d = g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	head := g.remoteHead(branch)
	if head != first || d.Item.HeadSHA != first {
		t.Fatalf("the second run should continue from %s, the remote has %s", first, head)
	}
	briefs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-*", id), "brief.md"))
	b, _ := os.ReadFile(briefs[len(briefs)-1])
	if !strings.Contains(string(b), "this branch already has 1 commit from a previous run") {
		t.Fatalf("the brief does not say the branch has work:\n%s", b)
	}
}

func TestQuestion(t *testing.T) {
	g := newRig(t, fakeclaude.Question)
	id := g.toBuild("Ask me")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.Question != "Should the timer round up or down?" || d.Item.ReturnStage != pipeline.Build {
		t.Fatalf("%+v", d.Item)
	}
}

func TestWrongModePausesTheRunner(t *testing.T) {
	g := newRig(t, fakeclaude.WrongMode)
	id := g.toBuild("Mode check")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, `started in "default", not "auto"`) {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	if g.r.Paused() == "" {
		t.Fatal("the runner should stop claiming")
	}
	g.toBuild("Next one")
	if g.runOnce() {
		t.Fatal("a paused runner claimed")
	}
}

func TestUsageLimit(t *testing.T) {
	g := newRig(t, fakeclaude.UsageLimit)
	id := g.toBuild("Limited")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Lease != nil {
		t.Fatalf("back in the build queue: %s %v", d.Item.Stage, d.Lease)
	}
	ev := g.lastEvent(id, "usage_limit")
	if ev == nil || !strings.Contains(fmt.Sprint(ev.Data["error"]), "rate limit") || ev.Data["reset"] == "" {
		t.Fatalf("usage_limit event %+v", ev)
	}
	ctl, _ := g.st.Control(g.ctx)
	if ctl.ActiveAgents != 0 {
		t.Fatalf("active agents %d", ctl.ActiveAgents)
	}
}

// waitFile polls (real time) for a file the fake agent writes before it hangs.
func waitFile(t *testing.T, p string) {
	t.Helper()
	for i := 0; i < 600; i++ {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", p)
}

func TestHeartbeatsThenStop(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	id := g.toBuild("Hang on")
	wait := g.runAsync()
	g.clk.BlockUntil(2) // heartbeat + time cap armed: the agent is running
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	for i := 0; i < 2; i++ {
		g.loopdClk.Advance(2 * time.Minute)
		g.clk.Advance(60 * time.Second)
		g.clk.BlockUntil(2) // re-armed after the heartbeat
		l := g.item(id).Lease
		if l == nil || !l.ExpiresAt.Equal(g.loopdClk.Now().Add(store.LeaseTTL)) {
			t.Fatalf("heartbeat %d did not extend the lease: %+v", i, l)
		}
		if a := l.Activity; a == nil || a.Phase != "build agent" || a.ElapsedS != int64(60*(i+1)) {
			t.Fatalf("heartbeat %d activity: %+v", i, a)
		}
	}
	// The user stops everything: the next heartbeat says stop.
	stop := true
	if _, err := g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{Stopped: &stop}); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(60 * time.Second)
	wait()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Lease != nil || len(g.events(id, "advanced")) != 1 {
		t.Fatalf("a stopped item returns to its queue without a report: %s", d.Item.Stage)
	}
	branch := fmt.Sprintf("loop/%d-hang-on", id)
	head := g.remoteHead(branch)
	if head == "" {
		t.Fatalf("no WIP push; log:\n%s", g.logs.String())
	}
	if msg := gitT(t, g.remote, "log", "-1", "--format=%s", head); !strings.HasPrefix(msg, "WIP: loop item") {
		t.Fatalf("WIP commit %q", msg)
	}
	if files := gitT(t, g.remote, "show", "--name-only", "--format=", head); files != "wip.txt" {
		t.Fatalf("WIP files %q (never .loop/)", files)
	}
	if g.remoteHead("main") != g.localRef {
		t.Fatal("main moved")
	}
	g.userUntouched()
	g.cleanedUp(id)
}

func TestStopKillsAnAgentThatIgnoresSIGTERM(t *testing.T) {
	g := newRig(t, fakeclaude.IgnoreTerm)
	id := g.toBuild("Stubborn")
	wait := g.runAsync()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	stop := true
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{Stopped: &stop})
	g.clk.Advance(60 * time.Second) // heartbeat → stop → SIGTERM, ignored
	g.clk.BlockUntil(2)             // time cap + the 10 s kill timer
	g.clk.Advance(10 * time.Second) // SIGKILL
	wait()
	if g.remoteHead(fmt.Sprintf("loop/%d-stubborn", id)) == "" {
		t.Fatal("no WIP push after the kill")
	}
	if !strings.Contains(g.logs.String(), "reason=stopped") {
		t.Fatalf("log:\n%s", g.logs.String())
	}
	g.cleanedUp(id)
}

func TestTimeCap(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	one := 1
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{MaxRunMinutes: &one})
	id := g.toBuild("Too slow")
	wait := g.runAsync()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	g.clk.Advance(30 * time.Second) // 1 minute minus the runner's 30 s margin
	wait()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "time cap (1 minutes)") {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	if g.remoteHead(fmt.Sprintf("loop/%d-too-slow", id)) == "" {
		t.Fatal("the work so far should be saved")
	}
}

func TestShutdownSavesWorkWithoutReporting(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	id := g.toBuild("Shut down")
	ctx, cancel := context.WithCancel(g.ctx)
	done := make(chan struct{})
	go func() { defer close(done); g.r.ClaimAndRun(ctx) }()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("no shutdown")
	}
	if len(g.events(id, "advanced")) != 1 || g.remoteHead(fmt.Sprintf("loop/%d-shut-down", id)) == "" {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

func TestUnknownRepoFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.r.Cfg.Repos = map[string]RepoConfig{"other": g.r.Cfg.Repos["demo"]}
	id := g.toBuild("Wrong repo")
	g.runOnce()
	if q := g.item(id).Item.Question; !strings.Contains(q, `no repo "demo"`) {
		t.Fatalf("%q", q)
	}
}

func TestLowCacheHitIsRecorded(t *testing.T) {
	g := newRig(t, fakeclaude.LowCache)
	id := g.toBuild("Cold cache")
	g.runOnce()
	evs := g.events(id, "low_cache_hit")
	if len(evs) != 1 || evs[0].Data["cache_hit_ratio"] != "0.10" || evs[0].Data["num_turns"] != "6" {
		t.Fatalf("low_cache_hit %+v; log:\n%s", evs, g.logs.String())
	}
	if g.item(id).Item.Stage != pipeline.Review {
		t.Fatal("a cold cache is a warning, not a failure")
	}
}

// Two items' agent runs differ only in the prompt's item part, the cwd and the per-run env values
// (golden compare of the rest): the same argv, settings bytes, tool set and system prompt, so the
// prompt cache carries across runs of the stage.
func TestAgentRunsShareTheirPrefix(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	var runs []*run
	var cmds []*exec.Cmd
	for _, title := range []string{"First item", "Second, quite different item"} {
		id := g.toBuild(title)
		cl, err := g.r.Client.Claim(g.ctx, "a-"+title[:5], nil, []string{StageBuild})
		if err != nil || cl == nil || cl.Item.ID != id {
			t.Fatalf("claim %v %v", cl, err)
		}
		ru, err := g.r.newRun(cl, "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.r.prepare(g.ctx, ru); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, ru)
		cmds = append(cmds, g.r.agentCommand(ru))
	}
	a, b := cmds[0], cmds[1]
	if !slices.Equal(a.Args, b.Args) {
		t.Fatalf("argv differs:\n%q\n%q", a.Args, b.Args)
	}
	if a.Dir == b.Dir {
		t.Fatal("each item has its own worktree")
	}
	sa, _ := os.ReadFile(a.Args[slices.Index(a.Args, "--settings")+1])
	sb, _ := os.ReadFile(b.Args[slices.Index(b.Args, "--settings")+1])
	if len(sa) == 0 || !bytes.Equal(sa, sb) {
		t.Fatal("settings differ between items")
	}
	var diff []string
	for i := range a.Env {
		if a.Env[i] != b.Env[i] {
			diff = append(diff, strings.SplitN(a.Env[i], "=", 2)[0])
		}
	}
	if len(a.Env) != len(b.Env) || !slices.Equal(diff, []string{"LOOP_GUARD_CONFIG"}) {
		t.Fatalf("env differs in %v", diff)
	}
	for _, kv := range []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false"} {
		if !slices.Contains(a.Env, kv) {
			t.Errorf("agent env lacks %s", kv)
		}
	}
	ba, _ := os.ReadFile(filepath.Join(runs[0].wt, ".loop", "brief.md"))
	bb, _ := os.ReadFile(filepath.Join(runs[1].wt, ".loop", "brief.md"))
	cut := bytes.Index(ba, []byte("## The item"))
	if cut < 1000 || !bytes.Equal(ba[:cut], bb[:cut]) {
		t.Fatalf("the briefs' static part differs (cut %d)", cut)
	}
	// The item's triage evidence is in the worktree and named in the brief.
	if b, err := os.ReadFile(filepath.Join(runs[0].wt, ".loop", "triage", "1-repro.txt")); err != nil || string(b) != "seen" {
		t.Fatalf("triage evidence %q %v", b, err)
	}
	if !bytes.Contains(ba, []byte("`.loop/triage/1-repro.txt` (repro)")) {
		t.Fatalf("the brief does not list the triage evidence:\n%s", ba[cut:])
	}
	if s := gitT(t, runs[0].wt, "status", "--porcelain"); s != "" {
		t.Fatalf("the runner's files show in git status:\n%s", s)
	}
	for _, ru := range runs {
		g.r.cleanup(g.ctx, ru, g.r.Log)
	}
}

// After the user answers a question, the next run gets the question and the answer in
// .loop/context/answers.md and the brief tells it the answer wins: without it a fresh run hit the
// same wall and asked again (items #34 and #46 asked 3 and 4 times, 2026-10-16).
func TestAnswersReachTheNextRun(t *testing.T) {
	g := newRig(t, fakeclaude.Question)
	id := g.toBuild("Ask me")
	g.runOnce()
	q := g.item(id).Item.Question
	if q == "" {
		t.Fatal("no question")
	}
	if _, err := g.st.Answer(g.ctx, "user:alex", id, "Skip the emulator run;\nlist it under not verified."); err != nil {
		t.Fatal(err)
	}
	cl, err := g.r.Client.Claim(g.ctx, "b", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, _ := g.r.newRun(cl, "b")
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	answers := g.read(filepath.Join(ru.wt, ".loop", "context", "answers.md"))
	for _, want := range []string{"## Answer 1 (", "> " + strings.ReplaceAll(q, "\n", "\n> "), "> Skip the emulator run;\n> list it under not verified."} {
		if !strings.Contains(answers, want) {
			t.Fatalf("answers.md lacks %q:\n%s", want, answers)
		}
	}
	if !strings.Contains(brief, "**The user already answered questions on this item**: `.loop/context/answers.md`") {
		t.Fatalf("brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}
}

// A run on an item nobody answered gets no answers file and no line in the brief.
func TestNoAnswersNoFile(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Quiet")
	cl, _ := g.r.Client.Claim(g.ctx, "b", nil, []string{StageBuild})
	if cl == nil || cl.Item.ID != id {
		t.Fatal("no claim")
	}
	ru, _ := g.r.newRun(cl, "b")
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	if _, err := os.Stat(filepath.Join(ru.wt, ".loop", "context", "answers.md")); err == nil || strings.Contains(brief, "already answered") {
		t.Fatal("answers context without an answer")
	}
}
