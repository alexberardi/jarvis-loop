// Package sandbox is the loop's own harness (docs/phases/04b-loop-harness.md): the real loopd
// wiring (store, API, Slack bot and Socket Mode client), the real runner and the real loop-guard,
// run together against a fake Slack (internal/slackfake), a fake claude (internal/fakeclaude) and
// a fake GitHub, on temp data. Scenarios (scenarios/*.json) drive it and check what happened,
// including what must never happen; transcripts show the Slack threads as they would have looked.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/loopd"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/runner"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The sandbox's names. The repo is a scratch repo the sandbox creates; the slug only ever names
// the fake GitHub.
const (
	RepoKey  = "demo"
	RepoSlug = "sandbox/demo"
	// LoopKey and LoopSlug are a second scratch repo, standing in for jarvis-loop: triage picks
	// it for items added without a repo.
	LoopKey  = "jarvis-loop"
	LoopSlug = "sandbox/jarvis-loop"
	// MobileKey and MobileSlug are a third scratch repo, standing in for jarvis-node-mobile (group
	// mobile, aliases "mobile" and "node app"): routing picks it, and its verify steps are the
	// runner's (one in its worktree, one in a checkout of demo as jarvis-server's mobile-shot runs).
	MobileKey  = "jarvis-node-mobile"
	MobileSlug = "sandbox/jarvis-node-mobile"
	RunnerName = "sandbox"
	HandRunner = "hand"
	UserName   = "sandbox"
	// Other is the Slack member who is not the approver.
	Other = "U0SANDBOXOTHER"
)

// Start is where the fake clock starts: 2026-10-15 13:00 UTC, 09:00 in New York, three hours
// before the next digest boundary.
var Start = time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)

// Binaries are the programs the runner starts: the fake claude and loop-guard. Env is added to
// the agent's environment (the tests use it to make their own binary act as both).
type Binaries struct {
	Claude string
	Guard  string
	Env    []string
}

// Options configure a stack.
type Options struct {
	Dir string
	Bin Binaries
	// RealClock runs on the wall clock (the long-lived `up` sandbox, polled by runner.Run).
	// Otherwise the stack runs on a fake clock that only scenario steps move, and the runner only
	// claims when a step says so.
	RealClock bool
	Log       io.Writer
	// Stages are the stages the runner claims (default: triage, build and review, as a real
	// runner).
	Stages []string
}

// Stack is one sandboxed loop: loopd (store, API on a loopback port, Slack layer), a runner, a
// fake Slack, a fake GitHub and its remote.
type Stack struct {
	Dir      string
	Clock    *runner.FakeClock // nil on the real clock
	Store    *store.Store
	Slack    *slackfake.Server
	GitHub   *GitHub
	Runner   *runner.Runner
	Bot      *slack.Bot
	LoopdURL string
	Remote   string
	MainSHA  string // the remote's main when the stack started
	// LoopRemote is the second repo's remote (LoopSlug), MobileRemote the third's.
	LoopRemote   string
	MobileRemote string

	// Secrets are every token the stack holds: none may appear in a Slack message.
	Secrets []string
	user    *apiClient
	hand    *runner.Client
	scripts string
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	bg     sync.WaitGroup
	srv    *http.Server

	runsMu   sync.Mutex
	inflight int
	runsDone chan struct{} // closed and replaced whenever a run ends
}

func randToken(prefix string) string {
	b := make([]byte, 16)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"-c", "user.name=sandbox", "-c", "user.email=sandbox@localhost"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// makeRemote creates a scratch repo: a bare remote with one commit on main.
func makeRemote(dir, name string) (string, string, error) {
	remote := filepath.Join(dir, "github", name+".git")
	seed := filepath.Join(dir, "github", name+"-seed")
	for _, d := range []string{remote, seed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", err
		}
	}
	if _, err := git(remote, "init", "--bare", "-q", "-b", "main", "."); err != nil {
		return "", "", err
	}
	if _, err := git(seed, "init", "-q", "-b", "main", "."); err != nil {
		return "", "", err
	}
	files := map[string]string{
		"README.md": "# " + name + "\n\nThe loop sandbox's scratch repository.\n",
		"CLAUDE.md": "# " + name + "\n\nA scratch repository for the loop's harness. Nothing here is real.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(body), 0o644); err != nil {
			return "", "", err
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "init"}, {"push", "-q", remote, "main"}} {
		if _, err := git(seed, args...); err != nil {
			return "", "", err
		}
	}
	sha, err := git(remote, "rev-parse", "main")
	os.RemoveAll(seed)
	return remote, sha, err
}

// StartStack builds and starts a stack under opts.Dir.
func StartStack(opts Options) (st *Stack, err error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}
	log := slog.New(slog.NewTextHandler(logw, nil))
	s := &Stack{Dir: dir, log: log, runsDone: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	var clk clock.Clock = clock.Real{}
	var rclk runner.Clock = runner.RealClock{}
	if !opts.RealClock {
		s.Clock = runner.NewFakeClock(Start)
		clk, rclk = s.Clock, s.Clock
	}

	// Tokens, as loopd reads them: a 0600 file.
	userTok, runnerTok, handTok := randToken("sbx-user-"), randToken("sbx-runner-"), randToken("sbx-hand-")
	slackToks := slack.Tokens{Bot: randToken("xoxb-sandbox-"), App: randToken("xapp-sandbox-")}
	s.Secrets = []string{userTok, runnerTok, handTok, slackToks.Bot, slackToks.App}
	tokPath := filepath.Join(dir, "tokens")
	tokText := fmt.Sprintf("%s\tuser\t%s\n%s\trunner\t%s\n%s\trunner\t%s\n", userTok, UserName, runnerTok, RunnerName, handTok, HandRunner)
	if err := os.WriteFile(tokPath, []byte(tokText), 0o600); err != nil {
		return nil, err
	}
	toks, err := api.LoadTokens(tokPath)
	if err != nil {
		return nil, err
	}

	// loopd: the store, the API on a free loopback port, and the Slack layer on the fake.
	if s.Store, err = store.Open(filepath.Join(dir, "loopd"), clk); err != nil {
		return nil, err
	}
	// loopd's --repos, and the built-in hold rules pointed at the scratch repos: jarvis-server's
	// at demo, jarvis-loop's at the stand-in for jarvis-loop.
	s.Store.SetRepos(Repos)
	s.Store.SetDefaultHoldRules(SandboxHoldRules())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.LoopdURL = "http://" + ln.Addr().String()
	s.srv = &http.Server{Handler: loopd.Handler(s.Store, toks, log.With("component", "api")), ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	if opts.RealClock {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-t.C:
					s.Store.Sweep(s.ctx)
				}
			}
		}()
	}
	s.Slack = slackfake.New()
	tick := time.Second
	if !opts.RealClock {
		tick = 20 * time.Millisecond // a moved fake clock takes effect at once
	}
	if s.Bot, err = loopd.StartSlack(s.ctx, &s.bg, s.Store, loopd.Slack{BaseURL: s.Slack.BaseURL(), Tokens: slackToks,
		Repos: Repos, Aliases: Aliases, Clock: clk, Log: log, TickEvery: tick}); err != nil {
		return nil, err
	}
	if err := s.Slack.WaitConnected(10 * time.Second); err != nil {
		return nil, err
	}

	// The fake GitHub and its remotes.
	if s.Remote, s.MainSHA, err = makeRemote(dir, "demo"); err != nil {
		return nil, err
	}
	if s.LoopRemote, _, err = makeRemote(dir, "jarvis-loop"); err != nil {
		return nil, err
	}
	if s.MobileRemote, _, err = makeRemote(dir, "jarvis-node-mobile"); err != nil {
		return nil, err
	}
	s.GitHub = NewGitHub(map[string]string{RepoSlug: s.Remote, LoopSlug: s.LoopRemote, MobileSlug: s.MobileRemote})

	// The runner, from a 0600 config file as loop-runner reads it.
	s.scripts = filepath.Join(dir, "scripts")
	if err := os.MkdirAll(s.scripts, 0o755); err != nil {
		return nil, err
	}
	verify := true
	cfg := runner.Config{
		LoopdURL: s.LoopdURL, Token: runnerTok, Name: RunnerName, Capabilities: []string{}, Slots: 1,
		WorkRoot: filepath.Join(dir, "work"), Claude: opts.Bin.Claude, Guard: opts.Bin.Guard, Mode: runner.ModeAuto,
		PollSeconds: 2, UserEmulator: "emulator-5554", VerifyPR: &verify,
		LoopdTokens: tokPath, LoopdData: filepath.Join(dir, "loopd"), Stages: opts.Stages,
		VerifyLock: filepath.Join(dir, "verify.lock"),
		Repos: map[string]runner.RepoConfig{
			RepoKey: {Remote: s.Remote, Slug: RepoSlug, Base: "main", Group: "server", Description: "the sandbox's demo app"},
			LoopKey: {Remote: s.LoopRemote, Slug: LoopSlug, Base: "main", Group: "loop", Description: "the loop itself (loopd, the runner, the Slack bot)"},
			MobileKey: {Remote: s.MobileRemote, Slug: MobileSlug, Base: "main", Group: "mobile", Aliases: []string{"mobile", "node app"},
				Description: "the household's phone app (screens, chat, pairing)", Verify: MobileVerify(dir)},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := filepath.Join(dir, "runner.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return nil, err
	}
	rcfg, err := runner.LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	// The runner pushes the item's branch to the scratch remote with git, as it would to GitHub,
	// and opens the PR through the fake (the runner's PR client), with its PR check on.
	s.Runner = &runner.Runner{Cfg: rcfg, Client: &runner.Client{Base: s.LoopdURL, Token: runnerTok}, Clock: rclk,
		Log: log.With("component", "runner"), Env: agentEnv(s.scripts, opts.Bin.Env), PRs: s.GitHub,
		VerifyPR: s.GitHub.Verify, Land: s.GitHub, Close: s.GitHub, HeartbeatEvery: 60 * time.Second}
	s.user = &apiClient{base: s.LoopdURL, token: userTok}
	s.hand = &runner.Client{Base: s.LoopdURL, Token: handTok}
	if opts.RealClock {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.Runner.Run(s.ctx) }()
	}
	return s, nil
}

// Repos are the sandbox loopd's --repos, and Aliases its --repos-file aliases.
var (
	Repos   = []string{RepoSlug, LoopSlug, MobileSlug}
	Aliases = map[string]string{"mobile": MobileSlug, "node app": MobileSlug}
)

// VerifyFailFile is the file (under the stack dir) whose presence makes the mobile repo's verify
// fail, as a scenario's "verify" step sets it. It is in the runner's work root, which the fence
// keeps readable (the rest of the stack dir may be under a /tmp the fence makes private).
const VerifyFailFile = "work/verify-fail"

// MobileVerify are the stand-in node-mobile's verify steps: unit tests in the worktree; "mobile
// shots" in a checkout of demo (as jarvis-server's mobile-shot runs against the runner's mirror at
// LOOP_HEAD), writing a screenshot and index.json as evidence, failing when the scenario says so;
// and a step that only runs when web/ changed.
func MobileVerify(dir string) []runner.VerifyStep {
	fail := filepath.Join(dir, VerifyFailFile)
	return []runner.VerifyStep{
		{Name: "unit tests", Run: `test -f README.md && echo "unit tests: 12 passed at $LOOP_HEAD"`},
		{Name: "mobile shots", Repo: RepoKey, Run: `set -e
git --git-dir="$LOOP_MIRROR" cat-file -e "$LOOP_HEAD^{commit}"
echo "built $LOOP_HEAD from the runner's mirror; shooting in sandbox $LOOP_SANDBOX from $(basename "$PWD")"
printf 'PNG

' > "$LOOP_EVIDENCE_DIR/home-light.png"
printf '{"screens":["home"],"head":"%s"}
' "$LOOP_HEAD" > "$LOOP_EVIDENCE_DIR/index.json"
if [ -e ` + shellQuote(fail) + ` ]; then echo "FAIL: the home screen is blank (home-light.png)"; exit 1; fi
echo "shots: 1 screen, light"`},
		{Name: "web screenshots", Run: "echo should not run; exit 1", WhenChanged: []string{"web/**"}},
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// SandboxHoldRules are the built-in hold rules with their repos swapped for the sandbox's: the
// rules for jarvis-server apply to demo, jarvis-loop's to the sandbox's jarvis-loop, and
// jarvis-node-mobile's to its stand-in.
func SandboxHoldRules() []pipeline.HoldRule {
	swap := map[string]string{pipeline.ServerRepo: RepoSlug, pipeline.LoopRepo: LoopSlug, pipeline.NodeMobileRepo: MobileSlug}
	var out []pipeline.HoldRule
	for _, r := range pipeline.DefaultHoldRules() {
		if to, ok := swap[r.Repo]; ok {
			r.Repo = to
		}
		out = append(out, r)
	}
	return out
}

// agentEnv is the environment the fake claude (and the runner's helper commands) start with: the
// runner's own minus anything that names real credentials, plus the fake's script dir.
func agentEnv(scripts string, extra []string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GH_TOKEN" || k == "GITHUB_TOKEN" || k == "ANTHROPIC_API_KEY" || strings.HasPrefix(k, "SLACK_") ||
			strings.HasPrefix(k, "LOOP_") || strings.HasPrefix(k, "FAKE_CLAUDE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, append([]string{"FAKE_CLAUDE_SCRIPTS=" + scripts, "GIT_TERMINAL_PROMPT=0"}, extra...)...)
}

// Close stops everything the stack started: a running agent is stopped by the runner's own
// shutdown path (its process group is signalled), then the servers.
func (s *Stack) Close() {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.WaitRuns(2 * time.Minute)
		if s.Runner != nil {
			s.Runner.Wait()
		}
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
	}
	if s.srv != nil {
		s.srv.Close()
	}
	if s.Slack != nil {
		s.Slack.Close()
	}
	if s.Store != nil {
		s.Store.Close()
	}
}

// Poll has the runner claim once, as its poll loop would, and work the item in the background.
// It returns once the claim is decided: the item claimed (it reports which), or nothing to claim.
func (s *Stack) Poll(timeout time.Duration) (int64, error) {
	before, err := s.lastEventID()
	if err != nil {
		return 0, err
	}
	s.runsMu.Lock()
	s.inflight++
	s.runsMu.Unlock()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		defer s.runEnded()
		// As Run does each poll: close the PRs of dropped items, then claim.
		if err := s.Runner.CloseDropped(s.ctx); err != nil {
			s.log.Warn("close dropped PRs", "err", err)
		}
		if _, err := s.Runner.ClaimAndRun(s.ctx); err != nil {
			s.log.Warn("claim", "err", err)
		}
	}()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id, ok := s.claimedSince(before); ok {
			return id, nil
		}
		select {
		case <-ended:
			if id, ok := s.claimedSince(before); ok {
				return id, nil
			}
			return 0, nil
		case <-time.After(10 * time.Millisecond):
		}
	}
	return 0, errors.New("the runner neither claimed nor gave up")
}

func (s *Stack) runEnded() {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	s.inflight--
	close(s.runsDone)
	s.runsDone = make(chan struct{})
}

// Inflight is how many runner claims are still working, plus the land script's watches of
// main's CI after a merge (which outlive their claim).
func (s *Stack) Inflight() int {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	return s.inflight + s.Runner.Watching()
}

// WaitRuns waits until no runner claim is working and no main-CI watch is running.
func (s *Stack) WaitRuns(timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		s.runsMu.Lock()
		n, ch := s.inflight, s.runsDone
		s.runsMu.Unlock()
		w := s.Runner.Watching()
		if n == 0 && w == 0 {
			return nil
		}
		select {
		case <-ch:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			return fmt.Errorf("%d runner run(s) still working, %d main-CI watch(es)", n, w)
		}
	}
}

// Advance moves the fake clock by d in steps of at most 30 s, letting the runner re-arm its
// timers (heartbeats every 60 s, the time cap) between steps, as the wall clock would.
func (s *Stack) Advance(d time.Duration) error {
	if s.Clock == nil {
		return errors.New("this sandbox runs on the real clock")
	}
	const step = 30 * time.Second
	for d > 0 {
		n := min(d, step)
		s.Clock.Advance(n)
		d -= n
		s.settleTimers()
	}
	return nil
}

// settleTimers waits briefly until each working run has its two timers (heartbeat and time cap,
// or heartbeat and kill) armed again, or no run is working.
func (s *Stack) settleTimers() {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n := s.Inflight()
		if n == 0 || s.Clock.Armed() >= 2*n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func farFuture() time.Time { return time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC) }

// Events returns every event loopd recorded, in order.
func (s *Stack) Events() ([]store.Event, error) {
	return s.Store.EventsBetween(context.Background(), time.Unix(0, 0), farFuture())
}

func (s *Stack) lastEventID() (int64, error) {
	evs, err := s.Events()
	if err != nil || len(evs) == 0 {
		return 0, err
	}
	return evs[len(evs)-1].ID, nil
}

func (s *Stack) claimedSince(id int64) (int64, bool) {
	evs, _ := s.Events()
	for _, e := range evs {
		if e.ID > id && e.Kind == "claimed" && e.Actor == "runner:"+RunnerName {
			return e.ItemID, true
		}
	}
	return 0, false
}

// SetScript sets how the fake claude plays item id's next runs of a stage ("" or "build": its
// build runs); repo is the repo a triage run picks for an item that has none.
func (s *Stack) SetScript(id int64, stage, play, file, repo string) error {
	b, _ := json.Marshal(map[string]string{"play": play, "file": file, "repo": repo})
	name := fmt.Sprintf("%d.json", id)
	if stage != "" && stage != runner.StageBuild {
		name = fmt.Sprintf("%d-%s.json", id, stage)
	}
	return os.WriteFile(filepath.Join(s.scripts, name), b, 0o644)
}

// AgentState is what the fake claude reported for an item's runs so far ("started", "hanging").
func (s *Stack) AgentState(id int64) []string {
	b, _ := os.ReadFile(filepath.Join(s.scripts, fmt.Sprintf("%d.state", id)))
	return strings.Fields(string(b))
}

// Thread is an item's Slack thread ts ("" before its parent is posted).
func (s *Stack) Thread(id int64) string {
	_, ts, _ := s.Store.SlackThread(context.Background(), id)
	return ts
}

// ControlTS is the active-agents message's ts.
func (s *Stack) ControlTS() string {
	st, _ := s.Store.SlackState(context.Background())
	return st.ControlTS
}

// ItemBranch is the branch the runner uses for an item.
func ItemBranch(id int64, title string) string {
	return fmt.Sprintf("loop/%d-%s", id, runner.Slug(title))
}

// apiClient calls loopd's API with the user's token, as the `loop` CLI does.
type apiClient struct {
	base, token string
}

func (c *apiClient) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
