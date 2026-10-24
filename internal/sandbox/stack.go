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
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	"github.com/alexberardi/jarvis-loop/internal/web"
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
	// ExtraKey and ExtraSlug are a fourth scratch repo (Options.ExtraRepo), on the fake GitHub
	// only: neither loopd nor the runner knows it until a reload adds it.
	ExtraKey   = "extra"
	ExtraSlug  = "sandbox/extra"
	RunnerName = "sandbox"
	HandRunner = "hand"
	UserName   = "sandbox"
	// OrchName is the orchestrator token's name (docs/phases/08-orchestrator.md).
	OrchName = "loop"
	// Other is the Slack member who is not the approver.
	Other = "U0SANDBOXOTHER"
	// OrchApp is the @orchestrator Slack app's name in the fake, OrchUser its bot user.
	OrchApp  = "orchestrator"
	OrchUser = "U0SANDBOXORCH"
	// SlackWorkspace is loopd's --slack-workspace in the sandbox, for the board's thread links.
	SlackWorkspace = "https://sandbox.slack.com"
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
	// ExtraRepo makes ExtraSlug's remote on the fake GitHub, for a reload to add.
	ExtraRepo bool
	// Slots are the runner's agent slots (default 1).
	Slots int
	// Capabilities are the runner's (default none).
	Capabilities []string
	// Qwen gives the runner a qwen block against a fake jarvisd: its builds with no builder set
	// may then go to qwen (docs/decisions/2026-10-19-qwen-routing.md).
	Qwen bool
	// PipelineVerify gives the demo repo (jarvis-server's stand-in) a pipeline-regressions verify
	// step (PipelineVerifyStep).
	PipelineVerify bool
	// SlowPrepare gives the demo repo a Prepare command that waits for ReleasePrepare (a slow
	// fetch or lib download; #453).
	SlowPrepare bool
	// GoBroken makes the runner's Go probe fail (#591): it runs no go proof command and claims
	// no Go repo's work.
	GoBroken bool
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
	// BoardURL is loopd's board listener (--board-addr), on its own loopback port.
	BoardURL string
	// RunnerURL is loopd's listener for remote runners (--runner-addr): runner tokens only.
	RunnerURL string
	Remote    string
	MainSHA   string // the remote's main when the stack started
	// LoopRemote is the second repo's remote (LoopSlug), MobileRemote the third's.
	LoopRemote   string
	MobileRemote string
	ExtraRemote  string // ExtraSlug's, with Options.ExtraRepo

	runnerCfg   string // the runner's config file, which reload_runner rewrites
	LoopMainSHA string // LoopRemote's main when the stack started

	// Secrets are every token the stack holds: none may appear in a Slack message.
	Secrets []string
	user    *apiClient
	hand    *runner.Client
	handAPI *apiClient // the hand runner's raw API, for plan turns (the runner has no client for them yet)
	orch    *apiClient // the orchestrator's token, for its actions (its runner turns are piece 3)
	scripts string
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	bg     sync.WaitGroup
	srv    *http.Server
	qwen   *fakeQwen
	board  *http.Server
	// runners is the remote runners' listener; handRemote the hand runner on it, as a Mac would be.
	runners    *http.Server
	handRemote *runner.Client
	// boardTok is the board token (kind board) the board expectation signs in with.
	boardTok string

	runsMu   sync.Mutex
	inflight int
	runsDone chan struct{} // closed and replaced whenever a run ends
	// agentRuns and landRuns are the claims working in the runner's agent slots and in its land
	// lane, which Poll keeps to as Run does.
	agentRuns, landRuns int
}

// pollRun is one Poll's run: the lane counter it holds once its claim is known.
type pollRun struct {
	ended bool
	lane  *int
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
	s.boardTok = randToken("sbx-board-")
	orchTok := randToken("sbx-orch-")
	slackToks := slack.Tokens{Bot: randToken("xoxb-sandbox-"), App: randToken("xapp-sandbox-")}
	orchSlack := slack.Tokens{Bot: randToken("xoxb-sandbox-orch-"), App: randToken("xapp-sandbox-orch-")}
	s.Secrets = []string{userTok, runnerTok, handTok, slackToks.Bot, slackToks.App, s.boardTok, orchTok, orchSlack.Bot, orchSlack.App}
	tokPath := filepath.Join(dir, "tokens")
	tokText := fmt.Sprintf("%s\tuser\t%s\n%s\trunner\t%s\n%s\trunner\t%s\n%s\tboard\tphone\n%s\torchestrator\t%s\n",
		userTok, UserName, runnerTok, RunnerName, handTok, HandRunner, s.boardTok, orchTok, OrchName)
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
	s.Store.SetAliases(Aliases)
	s.Store.SetDefaultHoldRules(SandboxHoldRules())
	s.Store.SetLoopRepo(LoopSlug)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.LoopdURL = "http://" + ln.Addr().String()
	boardCfg := web.BoardConfig{SlackWorkspace: SlackWorkspace, Clock: clk}
	s.srv = &http.Server{Handler: loopd.Handler(s.Store, toks, log.With("component", "api"), boardCfg), ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	bln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.BoardURL = "http://" + bln.Addr().String()
	s.board = &http.Server{Handler: loopd.BoardHandler(s.Store, toks, log.With("component", "board"), boardCfg), ReadHeaderTimeout: 10 * time.Second}
	go s.board.Serve(bln)
	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.RunnerURL = "http://" + rln.Addr().String()
	s.runners = &http.Server{Handler: loopd.RunnerHandler(s.Store, toks, log.With("component", "runners")), ReadHeaderTimeout: 10 * time.Second}
	go s.runners.Serve(rln)
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
	s.Slack.AddApp(slackfake.App{Name: OrchApp, BotToken: orchSlack.Bot, AppToken: orchSlack.App, BotUser: OrchUser})
	tick := time.Second
	if !opts.RealClock {
		tick = 20 * time.Millisecond // a moved fake clock takes effect at once
	}
	if s.Bot, err = loopd.StartSlack(s.ctx, &s.bg, s.Store, loopd.Slack{BaseURL: s.Slack.BaseURL(), Tokens: slackToks,
		Repos: Repos, Aliases: Aliases, Clock: clk, Log: log, TickEvery: tick, Orchestrator: &orchSlack}); err != nil {
		return nil, err
	}
	if err := s.Slack.WaitConnected(10 * time.Second); err != nil {
		return nil, err
	}
	if err := s.Slack.WaitAppConnected(OrchApp, 10*time.Second); err != nil {
		return nil, err
	}

	// The fake GitHub and its remotes.
	if s.Remote, s.MainSHA, err = makeRemote(dir, "demo"); err != nil {
		return nil, err
	}
	if s.LoopRemote, s.LoopMainSHA, err = makeRemote(dir, "jarvis-loop"); err != nil {
		return nil, err
	}
	if s.MobileRemote, _, err = makeRemote(dir, "jarvis-node-mobile"); err != nil {
		return nil, err
	}
	remotes := map[string]string{RepoSlug: s.Remote, LoopSlug: s.LoopRemote, MobileSlug: s.MobileRemote}
	if opts.ExtraRepo {
		if s.ExtraRemote, _, err = makeRemote(dir, "extra"); err != nil {
			return nil, err
		}
		remotes[ExtraSlug] = s.ExtraRemote
	}
	s.GitHub = NewGitHub(remotes)

	// The runner, from a 0600 config file as loop-runner reads it.
	s.scripts = filepath.Join(dir, "scripts")
	if err := os.MkdirAll(s.scripts, 0o755); err != nil {
		return nil, err
	}
	// The masked stand-in exists (the fence only masks what is there); its value is never shown.
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, VerifyMaskedFile), []byte("PROD_APP_ID=sandbox-prod-app\n"), 0o600); err != nil {
		return nil, err
	}
	verify := true
	slots := max(opts.Slots, 1)
	cfg := runner.Config{
		LoopdURL: s.LoopdURL, Token: runnerTok, OrchestratorToken: orchTok, Name: RunnerName, Capabilities: append([]string{}, opts.Capabilities...), Slots: slots,
		WorkRoot: filepath.Join(dir, "work"), Claude: opts.Bin.Claude, Guard: opts.Bin.Guard, Mode: runner.ModeAuto,
		PollSeconds: 2, UserEmulator: "emulator-5554", VerifyPR: &verify,
		LoopdTokens: tokPath, LoopdData: filepath.Join(dir, "loopd"), Stages: opts.Stages,
		VerifyLock: filepath.Join(dir, "verify.lock"),
		VerifyMask: append(runner.DefaultVerifyMask(runner.Home()), filepath.Join(dir, VerifyMaskedFile)),
		Repos: map[string]runner.RepoConfig{
			RepoKey: {Remote: s.Remote, Slug: RepoSlug, Base: "main", Group: "server", Description: "the sandbox's demo app"},
			LoopKey: {Remote: s.LoopRemote, Slug: LoopSlug, Base: "main", Group: "loop", Description: "the loop itself (loopd, the runner, the Slack bot)",
				QAAfterDeploy: true},
			MobileKey: {Remote: s.MobileRemote, Slug: MobileSlug, Base: "main", Group: "mobile", Aliases: []string{"mobile", "node app"},
				Description: "the household's phone app (screens, chat, pairing)", Verify: MobileVerify(dir), QA: true,
				DenyWrite: []string{MobileTrustedFile}},
		},
		// The demo repo stands in for jarvis-server (its scripts/e2e from a scenario's e2e step).
		// The overnight run's pipeline scenarios: the stand-in the "e2e" step commits with outcome
		// overnight.
		Regression: &runner.RegressionConfig{Repo: RepoKey, MobileRepo: MobileKey, PipelineCommand: "scripts/pipeline-scenarios"},
	}
	if opts.PipelineVerify {
		rc := cfg.Repos[RepoKey]
		if rc.Verify, err = PipelineVerifyStep(dir); err != nil {
			return nil, err
		}
		cfg.Repos[RepoKey] = rc
	}
	if opts.SlowPrepare {
		rc := cfg.Repos[RepoKey]
		rc.Prepare = []string{"touch .loop/scratch/preparing; while [ ! -e " + shellQuote(filepath.Join(cfg.WorkRoot, PrepareRelease)) + " ]; do sleep 0.05; done"}
		cfg.Repos[RepoKey] = rc
	}
	if opts.Qwen {
		if s.qwen, err = startFakeQwen(); err != nil {
			return nil, err
		}
		if cfg.Qwen, err = s.qwen.config(dir, opts.Bin.Claude); err != nil {
			return nil, err
		}
		// The fake opencode runs in the verify fence: it writes its run state (agent_running,
		// release) where the fake claude does.
		cfg.VerifyWritable = append(cfg.VerifyWritable, s.scripts)
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := filepath.Join(dir, "runner.json")
	s.runnerCfg = cfgPath
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
	if opts.GoBroken {
		s.Runner.GoProbe = func(context.Context) string { return `go: no such tool "vet"` }
	}
	s.user = &apiClient{base: s.LoopdURL, token: userTok}
	s.hand = &runner.Client{Base: s.LoopdURL, Token: handTok}
	s.handAPI = &apiClient{base: s.LoopdURL, token: handTok}
	s.handRemote = &runner.Client{Base: s.RunnerURL, Token: handTok}
	s.orch = &apiClient{base: s.LoopdURL, token: orchTok}
	if opts.RealClock {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.Runner.Run(s.ctx) }()
	}
	return s, nil
}

// Deploy is the runner restarting on a loop build (docs/decisions/2026-10-16-qa-after-deploy.md):
// "base", jarvis-loop's main when the stack started, or "main", its main now. As on its start, it
// reports deployed for the qa items awaiting a deploy whose merge that build contains.
func (s *Stack) Deploy(build string) error {
	rev := s.LoopMainSHA
	switch build {
	case "base":
	case "main":
		out, err := exec.Command("git", "-C", s.LoopRemote, "rev-parse", "refs/heads/main").Output()
		if err != nil {
			return fmt.Errorf("jarvis-loop's main: %w", err)
		}
		rev = strings.TrimSpace(string(out))
	default:
		return fmt.Errorf("deploy build must be base or main, not %q", build)
	}
	s.Runner.Revision = rev
	return s.Runner.ReportDeployed(s.ctx)
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

// VerifyMaskedFile is a stand-in for the harness's prod env files: in the runner's verify_mask, so
// fenced steps cannot read it. VerifyMaskedMarker's presence (a scenario's "verify" step with outcome
// "masked") makes "mobile shots" source it, as jarvis-server's sandbox sources prod-llm.env.
const (
	VerifyMaskedFile   = "work/prod-llm.env"
	VerifyMaskedMarker = "work/verify-masked"
)

// MobileTrustedFile is the stand-in node-mobile's deny_write path: a harness file the runner
// trusts, whose change verify runs only on the user's trust OK (scenario 56).
const MobileTrustedFile = "tools/shoot"

// MobileVerify are the stand-in node-mobile's verify steps: unit tests in the worktree; "mobile
// shots" in a checkout of demo (as jarvis-server's mobile-shot runs against the runner's mirror at
// LOOP_HEAD), writing a screenshot and index.json as evidence, failing when the scenario says so;
// and a step that only runs when web/ changed.
func MobileVerify(dir string) []runner.VerifyStep {
	fail := filepath.Join(dir, VerifyFailFile)
	masked, marker := filepath.Join(dir, VerifyMaskedFile), filepath.Join(dir, VerifyMaskedMarker)
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
if [ -e ` + shellQuote(marker) + ` ]; then set -u; . ` + shellQuote(masked) + `; echo "app $PROD_APP_ID"; fi
if [ -e ` + shellQuote(fail) + ` ]; then echo "FAIL: the home screen is blank (home-light.png)"; exit 1; fi
echo "shots: 1 screen, light"`},
		{Name: "web screenshots", Run: "echo should not run; exit 1", WhenChanged: []string{"web/**"}},
	}
}

// PipelineVerifyMode is the file (under the stack dir) that says how the stand-in pipeline
// scenarios go on the change, as a scenario's "verify" step with outcome flaky or regressed sets
// it; absent, the change fails only what fails on main too.
const PipelineVerifyMode = "work/pipeline-verify-mode"

// pipelineStandIn stands in for tools/pipeline run --json in a pipeline-regressions step: on
// both sides "timer correction" passes and "computer control off" fails every run (failing on
// main too); "who am I" passes on the base, and on the change fails every run when the mode is
// flaky (but passes the confirm round) or regressed (every round).
const pipelineStandIn = `#!/usr/bin/env bash
set -euo pipefail
mode=$(cat "$1" 2>/dev/null || true)
n=$LOOP_PIPELINE_REPEAT
echo "pipeline: $LOOP_PIPELINE_SIDE, $n runs, filter '$LOOP_PIPELINE_FILTER', sandbox $LOOP_PIPELINE_SANDBOX"
who=$n
if [ "$LOOP_PIPELINE_SIDE" = head ]; then
  case "$mode" in
  regressed) who=0 ;;
  flaky) if [ -z "$LOOP_PIPELINE_FILTER" ]; then who=0; fi ;;
  esac
fi
all="{\"name\": \"timer correction\", \"passed\": $n, \"runs\": $n}, {\"name\": \"computer control off\", \"passed\": 0, \"runs\": $n, \"failures\": [\"run 1: no computer tool call\"]}, {\"name\": \"who am I\", \"passed\": $who, \"runs\": $n}"
if [ -n "$LOOP_PIPELINE_FILTER" ]; then
  out=""
  for sc in "timer correction" "computer control off" "who am I"; do
    if [[ "$sc" =~ $LOOP_PIPELINE_FILTER ]]; then
      p=$n; [ "$sc" = "computer control off" ] && p=0; [ "$sc" = "who am I" ] && p=$who
      out="$out${out:+, }{\"name\": \"$sc\", \"passed\": $p, \"runs\": $n}"
    fi
  done
  all=$out
fi
echo "[$all]" > "$LOOP_PIPELINE_OUT"
exit 1
`

// PipelineVerifyStep is the demo repo's pipeline-regressions step: the stand-in above, written to
// the runner's work root (readable in the fence), as jarvis-server's would run tools/pipeline.
func PipelineVerifyStep(dir string) ([]runner.VerifyStep, error) {
	script := filepath.Join(dir, "work", "pipeline-standin")
	if err := os.WriteFile(script, []byte(pipelineStandIn), 0o755); err != nil {
		return nil, err
	}
	return []runner.VerifyStep{{Name: "pipeline scenarios", Pipeline: &runner.PipelineVerify{},
		Run: shellQuote(script) + " " + shellQuote(filepath.Join(dir, PipelineVerifyMode))}}, nil
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
// runner's own minus anything that names real credentials, plus the fake's script dir. The
// Anthropic variables (and the Bedrock/Vertex switches) go with the runner's own rule,
// runner.ScrubEnv, so the harness has no second list of names to keep in step with it.
func agentEnv(scripts string, extra []string) []string {
	var out []string
	for _, kv := range runner.ScrubEnv(os.Environ()) {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GH_TOKEN" || k == "GITHUB_TOKEN" || strings.HasPrefix(k, "SLACK_") ||
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
	if s.board != nil {
		s.board.Close()
	}
	if s.runners != nil {
		s.runners.Close()
	}
	s.qwen.Close()
	if s.Slack != nil {
		s.Slack.Close()
	}
	if s.Store != nil {
		s.Store.Close()
	}
}

// Claimed is what one poll of the runner claimed: an item, or a plan's turn (zero: nothing).
type Claimed struct {
	Item, Plan int64
	Orch       int64  // an orchestrate turn's id
	Reg        int64  // a regression run's id
	Stage      string // the item's stage when claimed ("" for a plan turn)
}

// Poll has the runner claim once, as its poll loop would, and work the item (or plan turn) in the
// background. It returns once the claim is decided: what it claimed, or nothing. It keeps to the
// runner's lanes as Run does: with every agent slot working it claims only land, with every land
// lane working only agent stages, and with both, nothing; with room in both, it claims once in
// every stage (Run's lanes would each claim; the scenario sees the one loopd hands out first).
func (s *Stack) Poll(timeout time.Duration) (Claimed, error) {
	before, err := s.lastEventID()
	if err != nil {
		return Claimed{}, err
	}
	agentLane, landLane := s.Runner.Lanes()
	stages := s.Runner.Config().Stages
	run := &pollRun{}
	s.runsMu.Lock()
	switch agentFull, landFull := s.agentRuns >= s.Runner.Config().Slots, s.landRuns >= s.Runner.Config().LandSlots; {
	case agentFull && landFull:
		stages = nil
	case agentFull:
		stages = landLane
	case landFull:
		stages = agentLane
	}
	s.inflight++
	s.runsMu.Unlock()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		defer s.runEnded(run)
		// As Run does each poll: close the PRs of dropped items, report the deploys, check the held
		// PRs and the due proofs (the land lane's runner), then claim.
		if err := s.Runner.CloseDropped(s.ctx); err != nil {
			s.log.Warn("close dropped PRs", "err", err)
		}
		if err := s.Runner.CheckWaitPRs(s.ctx); err != nil {
			s.log.Warn("check waited-on PRs", "err", err)
		}
		if err := s.Runner.CheckHeld(s.ctx); err != nil {
			s.log.Warn("check held PRs", "err", err)
		}
		if err := s.Runner.ReportDeployed(s.ctx); err != nil {
			s.log.Warn("report deployed", "err", err)
		}
		if len(landLane) > 0 {
			if err := s.Runner.CheckProofs(s.ctx); err != nil {
				s.log.Warn("check proofs", "err", err)
			}
		}
		if stages == nil {
			return
		}
		if _, err := s.Runner.ClaimAndRunIn(s.ctx, stages); err != nil {
			s.log.Warn("claim", "err", err)
		}
	}()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, ok := s.claimedSince(before); ok {
			s.runsMu.Lock()
			if !run.ended {
				run.lane = &s.agentRuns
				if c.Stage == runner.StageLand {
					run.lane = &s.landRuns
				}
				*run.lane++
			}
			s.runsMu.Unlock()
			return c, nil
		}
		select {
		case <-ended:
			if c, ok := s.claimedSince(before); ok {
				return c, nil
			}
			return Claimed{}, nil
		case <-time.After(10 * time.Millisecond):
		}
	}
	return Claimed{}, errors.New("the runner neither claimed nor gave up")
}

func (s *Stack) runEnded(run *pollRun) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	run.ended = true
	if run.lane != nil {
		*run.lane--
	}
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
// timers (heartbeats every 60 s, the time cap) between steps, as the wall clock would. It settles
// before the first step too: a run still on its way to a timer (a land run between reading CI and
// arming its poll) would otherwise arm it after the move and sleep past the scenario's end.
func (s *Stack) Advance(d time.Duration) error {
	if s.Clock == nil {
		return errors.New("this sandbox runs on the real clock")
	}
	const step = 30 * time.Second
	s.settleTimers()
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

func (s *Stack) claimedSince(id int64) (Claimed, bool) {
	evs, _ := s.Events()
	for _, e := range evs {
		if e.ID <= id || e.Actor != "runner:"+RunnerName {
			continue
		}
		switch e.Kind {
		case "claimed":
			return Claimed{Item: e.ItemID, Stage: e.To}, true
		case "plan_turn_claimed":
			return Claimed{Plan: e.PlanID}, true
		case store.KindOrchClaimed:
			n, _ := e.Data["turn"].(float64)
			return Claimed{Orch: int64(n)}, true
		case store.KindRegClaimed:
			n, _ := e.Data["regression"].(float64)
			return Claimed{Reg: int64(n)}, true
		}
	}
	return Claimed{}, false
}

// SetScript sets how the fake claude plays item id's next runs of a stage ("" or "build": its
// build runs); repo is the repo a triage run picks for an item that has none; proof is the proof a
// build states (for "proof": the one the orchestrator's proofs play writes for the item).
func (s *Stack) SetScript(id int64, stage, play, file, repo string, proof json.RawMessage) error {
	m := map[string]any{"play": play, "file": file, "repo": repo}
	if len(proof) > 0 {
		m["proof"] = proof
	}
	b, _ := json.Marshal(m)
	name := fmt.Sprintf("%d.json", id)
	if stage != "" && stage != runner.StageBuild {
		name = fmt.Sprintf("%d-%s.json", id, stage)
	}
	return os.WriteFile(filepath.Join(s.scripts, name), b, 0o644)
}

// Gemini turns on the runner's Gemini second opinion (off by default), with the fake agy (the
// fake claude through a link named agy, unfenced) playing play (fakeagy's plays; "" for ok).
func (s *Stack) Gemini(play string, blocking bool) error {
	agy := filepath.Join(s.Dir, "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(agy), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(s.Runner.Cfg.Claude, agy); err != nil && !os.IsExist(err) {
		return err
	}
	b, _ := json.Marshal(map[string]string{"play": play})
	if err := os.WriteFile(filepath.Join(s.scripts, "agy.json"), b, 0o644); err != nil {
		return err
	}
	off := false
	s.Runner.Cfg.Gemini = &runner.GeminiConfig{Agy: agy, Model: runner.DefaultGeminiModel, Blocking: blocking, Fence: &off,
		Home: filepath.Join(s.Dir, "gemini")}
	return nil
}

// AgentState is what the fake claude reported for an item's runs so far ("started", "hanging").
func (s *Stack) AgentState(id int64) []string {
	b, _ := os.ReadFile(filepath.Join(s.scripts, fmt.Sprintf("%d.state", id)))
	return strings.Fields(string(b))
}

// PlanThread is a plan's thread ts in #plan ("" before it has one).
func (s *Stack) PlanThread(id int64) string {
	d, err := s.Store.GetPlan(context.Background(), id)
	if err != nil || d.Plan.SlackChannel != slack.ChannelPlan {
		return ""
	}
	return d.Plan.SlackTS
}

// PlanScript sets how the fake claude plays a plan's next turns of a persona; repo is the repo the
// product agent names (repos), if any.
func (s *Stack) PlanScript(id int64, persona, play, repo string) error {
	b, _ := json.Marshal(map[string]string{"play": play, "repo": repo})
	return os.WriteFile(filepath.Join(s.scripts, fmt.Sprintf("P%d-%s.json", id, persona)), b, 0o644)
}

// PlanAgentState is what the fake claude reported for a plan's turns so far ("started",
// "waiting").
func (s *Stack) PlanAgentState(id int64) []string {
	b, _ := os.ReadFile(filepath.Join(s.scripts, fmt.Sprintf("P%d.state", id)))
	return strings.Fields(string(b))
}

// ReloadRunner rewrites the runner's config file from its current config (slots > 0 sets the
// slots, addRepo ExtraKey adds the extra repo, invalid truncates the file) and reloads it, as
// SIGHUP does.
func (s *Stack) ReloadRunner(slots int, addRepo string, invalid bool) error {
	c := *s.Runner.Config()
	c.Repos = maps.Clone(c.Repos)
	if slots > 0 {
		c.Slots = slots
	}
	switch addRepo {
	case "":
	case ExtraKey:
		if s.ExtraRemote == "" {
			return fmt.Errorf("add_repo %q needs the scenario's extra_repo", addRepo)
		}
		c.Repos[ExtraKey] = runner.RepoConfig{Remote: s.ExtraRemote, Slug: ExtraSlug, Base: "main", Group: "package",
			Description: "a repo a reload added"}
	default:
		return fmt.Errorf("add_repo %q: only %q", addRepo, ExtraKey)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if invalid {
		b = b[:len(b)/2]
	}
	if err := os.WriteFile(s.runnerCfg, b, 0o600); err != nil {
		return err
	}
	return s.Runner.Reload()
}

// ReloadLoopd writes loopd's --repos-file and reads the repos again, as SIGHUP does.
func (s *Stack) ReloadLoopd(text string) error {
	p := filepath.Join(s.Dir, "repos-file")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		return err
	}
	return loopd.ReloadRepos(loopd.RepoSource{Flag: Repos, File: p}, s.Store, s.Bot, s.log)
}

// ReleasePlan lets a plan's waiting fake agent (play "wait") finish its turn.
func (s *Stack) ReleasePlan(id int64) error {
	return os.WriteFile(filepath.Join(s.scripts, fmt.Sprintf("P%d.release", id)), nil, 0o644)
}

// PrepareRelease is the file (under the runner's work root, which the fence shows read-only; the
// rest of a stack under the host's /tmp is behind its private /tmp) that lets a SlowPrepare
// command end.
const PrepareRelease = "prepare-release"

// ReleasePrepare lets the demo repo's slow Prepare commands end, now and from then on.
func (s *Stack) ReleasePrepare() error {
	return os.WriteFile(filepath.Join(s.Runner.Config().WorkRoot, PrepareRelease), nil, 0o644)
}

// Preparing says whether the item's build is in its slow Prepare command.
func (s *Stack) Preparing(id int64) bool {
	_, err := os.Stat(filepath.Join(s.Runner.Config().WorkRoot, fmt.Sprintf("%d-build", id), ".loop", "scratch", "preparing"))
	return err == nil
}

// Release lets an item's waiting build (play "wait" or "wait_release") go on.
func (s *Stack) Release(id int64) error {
	return os.WriteFile(filepath.Join(s.scripts, fmt.Sprintf("%d.release", id)), nil, 0o644)
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
	return c.doHeader(method, path, nil, in, out)
}

func (c *apiClient) doHeader(method, path string, hdr map[string]string, in, out any) error {
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
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// runnerListener reads GET /items on the remote runners' listener with as's token ("user",
// "orchestrator", "board", "none" or "hand"); with refused (a regexp) it must be refused.
func (s *Stack) runnerListener(as, refused string) error {
	tok := map[string]string{"user": s.user.token, "orchestrator": s.orch.token, "board": s.boardTok, "hand": s.handRemote.Token, "none": ""}
	t, ok := tok[as]
	if !ok {
		return fmt.Errorf("runner_listener as %q: want user, orchestrator, board, none or hand", as)
	}
	err := (&apiClient{base: s.RunnerURL, token: t}).do("GET", "/items", nil, nil)
	if refused == "" {
		if err != nil {
			return fmt.Errorf("runner listener as %s: %w", as, err)
		}
		return nil
	}
	if err == nil {
		return fmt.Errorf("runner listener as %s: not refused, want /%s/", as, refused)
	}
	re, rerr := regexp.Compile(refused)
	if rerr != nil {
		return rerr
	}
	if !re.MatchString(err.Error()) {
		return fmt.Errorf("runner listener as %s: refused with %q, want /%s/", as, err, refused)
	}
	return nil
}
