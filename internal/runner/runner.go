package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/guard"
)

// Runner claims build items and runs one agent per item.
type Runner struct {
	Cfg    *Config
	Client *Client
	Clock  Clock
	Git    Git
	Log    *slog.Logger
	// Env is the environment the agent starts with (default: the runner's own), minus loopd's
	// and Slack's secrets.
	Env []string
	// PRs opens or updates the item's PR after the runner pushed its branch (GH, or DryRunPRs).
	PRs PRs
	// Push pushes the item's branch from the mirror; nil means Git.PushBranch.
	Push func(ctx context.Context, mirror, branch string) error
	// VerifyPR checks the PR on GitHub; nil skips the check (the dry run, tests).
	VerifyPR func(ctx context.Context, slug string, pr int) (PRInfo, error)
	// HeartbeatEvery and KillGrace default to 60 s and 10 s.
	HeartbeatEvery time.Duration
	KillGrace      time.Duration

	paused   atomic.Value // string: why the runner stopped claiming
	mirrorMu sync.Map     // mirror path -> *sync.Mutex
}

// PRInfo is what GitHub says about a PR.
type PRInfo struct {
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	State       string `json:"state"`
}

// Stages this runner claims (phase 4: build only).
var Stages = []string{"build"}

func (r *Runner) heartbeatEvery() time.Duration {
	if r.HeartbeatEvery > 0 {
		return r.HeartbeatEvery
	}
	return 60 * time.Second
}

func (r *Runner) killGrace() time.Duration {
	if r.KillGrace > 0 {
		return r.KillGrace
	}
	return 10 * time.Second
}

// Paused says why the runner stopped claiming ("" when it has not).
func (r *Runner) Paused() string {
	s, _ := r.paused.Load().(string)
	return s
}

// Run works items until ctx ends, in up to Cfg.Slots parallel slots.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < r.Cfg.Slots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				claimed, err := r.ClaimAndRun(ctx)
				if err != nil {
					r.Log.Warn("claim", "err", err)
				}
				if claimed && err == nil {
					continue
				}
				t := r.Clock.NewTimer(time.Duration(r.Cfg.PollSeconds) * time.Second)
				select {
				case <-ctx.Done():
				case <-t.C():
				}
				t.Stop()
			}
		}()
	}
	wg.Wait()
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ClaimAndRun claims one build item and works it to the end. It reports whether it claimed one.
func (r *Runner) ClaimAndRun(ctx context.Context) (bool, error) {
	if why := r.Paused(); why != "" {
		return false, nil
	}
	agent := r.Cfg.Name + "-" + randHex(4)
	cl, err := r.Client.Claim(ctx, agent, r.Cfg.Capabilities, Stages)
	if err != nil || cl == nil {
		return false, err
	}
	r.work(ctx, cl, agent)
	return true, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is a branch-safe form of a title.
func Slug(title string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "item"
	}
	return s
}

// run is one item's run: where things are.
type run struct {
	item     Item
	lease    string
	agent    string
	repoKey  string
	repo     RepoConfig
	branch   string
	wt       string
	scratch  string
	dir      string // the run's own dir: settings, guard config, logs (never writable by the agent)
	mirror   string
	sandbox  string
	settings string
	started  time.Time

	reported int // refusals already sent
	refusals int
}

func (r *Runner) lockMirror(path string) func() {
	m, _ := r.mirrorMu.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// newRun works out where an item's run happens.
func (r *Runner) newRun(cl *Claim, agent string) (*run, error) {
	it := cl.Item
	ru := &run{item: it, lease: cl.Lease.ID, agent: agent, started: r.Clock.Now()}
	key, repo, ok := r.Cfg.Repo(it.Repo)
	if !ok {
		return ru, fmt.Errorf("runner %s has no repo %q configured", r.Cfg.Name, it.Repo)
	}
	ru.repoKey, ru.repo = key, repo
	ru.branch = it.Branch
	if !strings.HasPrefix(ru.branch, "loop/") {
		ru.branch = fmt.Sprintf("loop/%d-%s", it.ID, Slug(it.Title))
	}
	ru.wt = filepath.Join(r.Cfg.WorkRoot, fmt.Sprintf("%d-build", it.ID))
	// Inside the worktree (ignored by git), so the agent needs no extra directory: an --add-dir
	// would differ per item.
	ru.scratch = filepath.Join(ru.wt, ".loop", "scratch")
	ru.dir = filepath.Join(r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-%s", it.ID, ru.started.Format("20060102T150405Z")))
	ru.mirror = filepath.Join(r.Cfg.WorkRoot, "mirrors", Slug(key)+".git")
	ru.settings = filepath.Join(r.Cfg.WorkRoot, "settings", Slug(key)+"-build.json")
	ru.sandbox = fmt.Sprintf("loop-%d", it.ID)
	return ru, nil
}

func (r *Runner) work(ctx context.Context, cl *Claim, agent string) {
	log := r.Log.With("item", cl.Item.ID, "agent", agent)
	ru, err := r.newRun(cl, agent)
	if err != nil {
		r.advance(ctx, ru, "failed", map[string]string{"error": err.Error()}, nil)
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	defer r.cleanup(cleanupCtx, ru, log)

	brief, err := r.prepare(ctx, ru)
	if err != nil {
		log.Error("prepare", "err", err)
		r.advance(ctx, ru, "failed", map[string]string{"error": "the runner could not prepare the run: " + clip(err.Error(), 1500)}, nil)
		return
	}
	log.Info("agent starting", "worktree", ru.wt, "branch", ru.branch, "mode", r.Cfg.Mode)
	out := r.runAgent(ctx, ru, brief)
	log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	// Reporting and saving work must finish even when the runner is shutting down.
	r.report(cleanupCtx, ru, out, log)
}

// prepare makes the worktree, the brief, the guard config and the settings.
func (r *Runner) prepare(ctx context.Context, ru *run) (string, error) {
	os.RemoveAll(ru.dir)
	if err := os.MkdirAll(ru.dir, 0o700); err != nil {
		return "", err
	}
	unlock := r.lockMirror(ru.mirror)
	err := r.Git.EnsureMirror(ctx, ru.mirror, ru.repo)
	ahead := 0
	if err == nil {
		ahead, err = r.Git.AddWorktree(ctx, ru.mirror, ru.wt, ru.branch, ru.repo.Base)
	}
	unlock()
	if err != nil {
		return "", err
	}
	for _, d := range []string{filepath.Join(ru.wt, ".loop", "evidence"), ru.scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	info := RunInfo{Slug: ru.repo.Slug, Base: ru.repo.Base, Branch: ru.branch, Worktree: ru.wt, Scratch: ru.scratch, Sandbox: ru.sandbox, Resumed: ahead}
	info.Triage, info.TriageErr = r.fetchTriage(ctx, ru)
	brief := BuildBrief(ru.item, info)
	for _, p := range []string{filepath.Join(ru.wt, ".loop", "brief.md"), filepath.Join(ru.dir, "brief.md")} {
		if err := os.WriteFile(p, []byte(brief), 0o644); err != nil {
			return "", err
		}
	}
	for _, c := range ru.repo.Prepare {
		if out, err := r.shell(ctx, ru.wt, c, 20*time.Minute); err != nil {
			return "", fmt.Errorf("prepare %q: %v: %s", c, err, clip(out, 500))
		}
	}
	home := Home()
	gc := guard.Config{
		Item: ru.item.ID, Worktree: ru.wt, Scratch: ru.scratch, SandboxDir: filepath.Join(ru.wt, ".sandbox", ru.sandbox),
		Branch: ru.branch, Remote: "origin", Home: home,
		Protected: r.Cfg.Protected(), Secrets: r.Cfg.Secrets(), Words: []string{r.Cfg.UserEmulator},
		LoopdAddrs: r.Cfg.LoopdAddrs(), DenyWrite: ru.repo.DenyWrite,
		Report: filepath.Join(ru.dir, "refusals.jsonl"), Armed: filepath.Join(ru.dir, "armed"),
	}
	b, _ := json.MarshalIndent(gc, "", "  ")
	if err := os.WriteFile(filepath.Join(ru.dir, "guard.json"), b, 0o600); err != nil {
		return "", err
	}
	// The same bytes for every item of the repo (see docs/decisions/2026-10-15-prompt-caching.md);
	// written atomically, as parallel slots share the file.
	settings, err := GenerateSettings(SettingsInput{
		Mode: r.Cfg.Mode, Guard: r.Cfg.Guard, Home: home, Protected: r.Cfg.Protected(), Secrets: r.Cfg.Secrets(),
		LoopdAddrs: r.Cfg.LoopdAddrs(), AllowedDomains: r.Cfg.AllowedDomains, ExcludedCommands: ru.repo.ExcludedCommands,
	})
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ru.settings, settings); err != nil {
		return "", err
	}
	if _, err := os.Stat(r.Cfg.Guard); err != nil {
		return "", fmt.Errorf("the guard binary %s: %w", r.Cfg.Guard, err)
	}
	return brief, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + "." + randHex(4)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Runner) shell(ctx context.Context, dir, command string, limit time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
	cmd.Env = r.agentEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// agentEnv is the environment for the agent and the runner's helper commands: no loopd or Slack
// secret.
func (r *Runner) agentEnv() []string { return scrubEnv(r.Env) }

// scrubEnv is src (nil: the runner's own environment) without loopd's and Slack's secrets.
func scrubEnv(src []string) []string {
	if src == nil {
		src = os.Environ()
	}
	var out []string
	for _, kv := range src {
		k, _, _ := strings.Cut(kv, "=")
		if k == "LOOP_TOKEN" || k == "LOOP_URL" || strings.HasPrefix(k, "SLACK_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Why a run ended.
const (
	endExited       = "exited"
	endStopped      = "stopped"
	endTimeCap      = "time_cap"
	endShutdown     = "shutdown"
	endModeMismatch = "mode_mismatch"
	endNotArmed     = "guard_not_armed"
	endStartFailed  = "start_failed"
)

type outcome struct {
	reason   string
	detail   string
	exitCode int
	stats    streamStats
}

type streamStats struct {
	initMode    string
	sessionID   string
	lastText    string
	toolResults int
	limit       *LimitHit
	result      *resultEvent
}

type resultEvent struct {
	Subtype   string   `json:"subtype"`
	IsError   bool     `json:"is_error"`
	NumTurns  int      `json:"num_turns"`
	Duration  int64    `json:"duration_ms"`
	CostUSD   float64  `json:"total_cost_usd"`
	Result    string   `json:"result"`
	Errors    []string `json:"errors"`
	Denials   []any    `json:"permission_denials"`
	ErrStatus *int     `json:"api_error_status"`
	Usage     struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// CacheHitRatio is cache reads over all input tokens (reads, writes and uncached input).
func CacheHitRatio(input, creation, read int64) float64 {
	total := input + creation + read
	if total == 0 {
		return 0
	}
	return float64(read) / float64(total)
}

// handle reads one stream-json line; it returns a reason to end the run early, or "".
func (r *Runner) handle(ru *run, st *streamStats, line []byte) (string, string) {
	if hit, ok := MatchUsageLimit(line); ok && st.limit == nil {
		st.limit = &hit
	}
	var ev struct {
		Type           string `json:"type"`
		Subtype        string `json:"subtype"`
		PermissionMode string `json:"permissionMode"`
		SessionID      string `json:"session_id"`
		Message        *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return "", ""
	}
	switch {
	case ev.Type == "system" && ev.Subtype == "init":
		st.initMode = ev.PermissionMode
		st.sessionID = ev.SessionID
		if want := PermissionMode(r.Cfg.Mode); ev.PermissionMode != want {
			return endModeMismatch, fmt.Sprintf("the session started in %q, not %q", ev.PermissionMode, want)
		}
	case ev.Type == "assistant" && ev.Message != nil:
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					st.lastText = clip(strings.TrimSpace(b.Text), 1500)
				}
			}
		}
	case ev.Type == "user" && ev.Message != nil:
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type != "tool_result" {
					continue
				}
				st.toolResults++
				if st.toolResults == 1 {
					if _, err := os.Stat(filepath.Join(ru.dir, "armed")); err != nil {
						return endNotArmed, "a tool ran but loop-guard never did: the generated settings were not applied"
					}
				}
			}
		}
	case ev.Type == "result":
		var res resultEvent
		if json.Unmarshal(line, &res) == nil {
			st.result = &res
			if res.Result != "" {
				st.lastText = clip(strings.TrimSpace(res.Result), 1500)
			}
		}
	}
	return "", ""
}

// agentCommand is the claude command for a run: the same argv for every item of a stage; the
// worktree as cwd; the per-run guard config in the environment.
func (r *Runner) agentCommand(ru *run) *exec.Cmd {
	cmd := exec.Command(r.Cfg.Claude, ClaudeArgs(AgentArgs{Mode: r.Cfg.Mode, Settings: ru.settings, Model: r.Cfg.Model})...)
	cmd.Dir = ru.wt
	cmd.Env = append(r.agentEnv(),
		guard.ConfigEnv+"="+filepath.Join(ru.dir, "guard.json"),
		// Loop agents keep no auto memory (a per-worktree memory dir would be new every run) and
		// fetch no claude.ai connectors (no MCP tools at all).
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
	)
	return cmd
}

// runAgent runs claude for one item, in its own process group, with heartbeats and the time cap.
func (r *Runner) runAgent(ctx context.Context, ru *run, brief string) outcome {
	out := outcome{reason: endExited, exitCode: -1}
	stdin, err := os.Open(filepath.Join(ru.dir, "brief.md"))
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	defer stdin.Close()
	stderr, err := os.OpenFile(filepath.Join(ru.dir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	defer stderr.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	cmd := r.agentCommand(ru)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, pw, stderr
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	pw.Close()
	pid := cmd.Process.Pid

	lines := make(chan []byte, 64)
	go func() {
		defer close(lines)
		defer pr.Close()
		logf, _ := os.OpenFile(filepath.Join(ru.dir, "stream.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if logf != nil {
			defer logf.Close()
		}
		br := bufio.NewReaderSize(pr, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if logf != nil {
					logf.Write(line)
				}
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan *os.ProcessState, 1)
	go func() {
		ps, _ := cmd.Process.Wait()
		exited <- ps
	}()

	maxRun := 45 * time.Minute
	if ctl, err := r.Client.Control(ctx); err == nil && ctl.MaxRunMinutes > 0 {
		maxRun = time.Duration(ctl.MaxRunMinutes) * time.Minute
	}
	// Stop a little before loopd's own cap, so the run is saved and reported by the runner.
	capAt := maxRun - 30*time.Second
	if capAt <= 0 {
		capAt = maxRun
	}
	hb := r.Clock.NewTimer(r.heartbeatEvery())
	capT := r.Clock.NewTimer(capAt)
	var killT Timer
	defer func() {
		hb.Stop()
		capT.Stop()
		if killT != nil {
			killT.Stop()
		}
	}()
	terminating := false
	terminate := func(reason, detail string) {
		if terminating {
			return
		}
		terminating = true
		out.reason, out.detail = reason, detail
		_ = signalGroup(pid, syscall.SIGTERM)
		killT = r.Clock.NewTimer(r.killGrace())
	}
	done := ctx.Done()
	for {
		var killC <-chan time.Time
		if killT != nil {
			killC = killT.C()
		}
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			if why, detail := r.handle(ru, &out.stats, line); why != "" {
				terminate(why, detail)
			}
		case ps := <-exited:
			if ps != nil {
				out.exitCode = ps.ExitCode()
			}
			// Whatever the agent left running in its group goes with it.
			_ = signalGroup(pid, syscall.SIGKILL)
			drain := time.NewTimer(5 * time.Second)
		drainLoop:
			for lines != nil {
				select {
				case line, ok := <-lines:
					if !ok {
						break drainLoop
					}
					if why, detail := r.handle(ru, &out.stats, line); why != "" && out.reason == endExited {
						out.reason, out.detail = why, detail
					}
				case <-drain.C:
					break drainLoop
				}
			}
			drain.Stop()
			return out
		case <-hb.C():
			r.flushRefusals(ctx, ru)
			stop, err := r.Client.Heartbeat(ctx, ru.lease, fmt.Sprintf("agent running: %d tool calls", out.stats.toolResults))
			var ae *APIError
			if err == nil && stop {
				terminate(endStopped, "loopd asked to stop")
			} else if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
				terminate(endStopped, "the lease is gone: "+ae.Error())
			} else {
				if err != nil {
					r.Log.Warn("heartbeat", "item", ru.item.ID, "err", err)
				}
				hb = r.Clock.NewTimer(r.heartbeatEvery())
			}
		case <-capT.C():
			terminate(endTimeCap, fmt.Sprintf("the run hit the time cap (%d minutes)", int(maxRun.Minutes())))
		case <-killC:
			_ = signalGroup(pid, syscall.SIGKILL)
			killT = nil
		case <-done:
			done = nil
			terminate(endShutdown, "the runner is shutting down")
		}
	}
}

// flushRefusals sends new guard refusals to loopd as guard_refused events.
func (r *Runner) flushRefusals(ctx context.Context, ru *run) {
	b, err := os.ReadFile(filepath.Join(ru.dir, "refusals.jsonl"))
	if err != nil {
		return
	}
	// Only whole lines: the guard may be appending one right now.
	lines := strings.Split(string(b), "\n")
	lines = lines[:len(lines)-1]
	for n, line := range lines {
		if n < ru.reported {
			continue
		}
		ru.reported = n + 1
		var ref guard.Refusal
		if json.Unmarshal([]byte(line), &ref) != nil {
			continue
		}
		ru.refusals++
		if ru.refusals > 25 {
			continue // the rest are in the run's log; 25 events are enough to see a pattern
		}
		err := r.Client.Event(ctx, ru.item.ID, "guard_refused", map[string]any{
			"rule": ref.Rule, "tool": ref.Tool, "reason": clip(ref.Reason, 300), "target": clip(ref.Target, 300), "at": ref.At})
		if err != nil {
			r.Log.Warn("guard_refused event", "item", ru.item.ID, "err", err)
		}
	}
}

func (r *Runner) advance(ctx context.Context, ru *run, outcome string, fields map[string]string, paths []string) {
	r.flushRefusals(ctx, ru)
	if fields == nil {
		fields = map[string]string{}
	}
	fields["runner_agent"] = ru.agent
	if err := r.Client.Advance(ctx, ru.item.ID, outcome, fields, paths); err != nil {
		r.Log.Error("advance", "item", ru.item.ID, "outcome", outcome, "err", err)
		return
	}
	r.Log.Info("reported", "item", ru.item.ID, "outcome", outcome)
}

func (out outcome) lastOutput() string {
	if out.stats.lastText == "" {
		return "(no output)"
	}
	return out.stats.lastText
}

func (r *Runner) metrics(ru *run, out outcome, f map[string]string) map[string]string {
	f["run_seconds"] = strconv.Itoa(int(r.Clock.Now().Sub(ru.started).Seconds()))
	f["exit_code"] = strconv.Itoa(out.exitCode)
	if out.stats.initMode != "" {
		f["permission_mode"] = out.stats.initMode
	}
	if out.stats.sessionID != "" {
		f["session_id"] = out.stats.sessionID
	}
	if res := out.stats.result; res != nil {
		u := res.Usage
		f["num_turns"] = strconv.Itoa(res.NumTurns)
		f["duration_ms"] = strconv.FormatInt(res.Duration, 10)
		f["total_cost_usd"] = strconv.FormatFloat(res.CostUSD, 'f', 4, 64)
		f["usage_input"] = strconv.FormatInt(u.Input, 10)
		f["usage_output"] = strconv.FormatInt(u.Output, 10)
		f["usage_cache_creation"] = strconv.FormatInt(u.CacheCreation, 10)
		f["usage_cache_read"] = strconv.FormatInt(u.CacheRead, 10)
		f["cache_hit_ratio"] = strconv.FormatFloat(CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead), 'f', 2, 64)
	}
	if ru.refusals > 0 {
		f["guard_refusals"] = strconv.Itoa(ru.refusals)
	}
	return f
}

func (r *Runner) saveWIP(ctx context.Context, ru *run, why string, log *slog.Logger) {
	pushed, err := r.Git.SaveWIP(ctx, ru.wt, ru.branch, ru.repo.Base, fmt.Sprintf("WIP: loop item #%d (%s)", ru.item.ID, why))
	if err != nil {
		log.Error("WIP push", "err", err)
		return
	}
	log.Info("WIP saved", "pushed", pushed, "branch", ru.branch)
}

// report turns how the run ended into loopd calls.
// LowCacheTurns and LowCacheRatio: a run longer than this many turns with a cache hit ratio
// below this is recorded as low_cache_hit (a warning for the digest; nothing changes).
const (
	LowCacheTurns = 5
	LowCacheRatio = 0.5
)

func (r *Runner) report(ctx context.Context, ru *run, out outcome, log *slog.Logger) {
	r.flushRefusals(ctx, ru)
	if res := out.stats.result; res != nil && res.NumTurns > LowCacheTurns {
		u := res.Usage
		if ratio := CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead); ratio < LowCacheRatio {
			log.Warn("low prompt-cache hit ratio", "ratio", ratio, "turns", res.NumTurns)
			if err := r.Client.Event(ctx, ru.item.ID, "low_cache_hit", map[string]any{
				"cache_hit_ratio": strconv.FormatFloat(ratio, 'f', 2, 64), "num_turns": strconv.Itoa(res.NumTurns),
				"usage_input": u.Input, "usage_cache_creation": u.CacheCreation, "usage_cache_read": u.CacheRead,
				"session_id": out.stats.sessionID}); err != nil {
				log.Warn("low_cache_hit event", "err", err)
			}
		}
	}
	switch out.reason {
	case endStopped, endShutdown:
		// The item returns to its queue (loopd ended the lease, or it will expire); keep the work.
		r.flushRefusals(ctx, ru)
		r.saveWIP(ctx, ru, out.reason, log)
		return
	case endTimeCap:
		r.saveWIP(ctx, ru, "time cap", log)
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": out.detail + "; work so far is on " + ru.branch + ". Last output: " + out.lastOutput()}), nil)
		return
	case endModeMismatch:
		r.paused.Store(out.detail)
		log.Error("pausing: the agent did not get the permission mode it was started with", "detail", out.detail)
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": "boundary check failed: " + out.detail +
			". Auto mode may be unavailable; the runner stopped claiming until it is restarted (use mode \"strict\" to run without it)."}), nil)
		return
	case endNotArmed, endStartFailed:
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": "boundary check failed (" + out.reason + "): " + out.detail}), nil)
		return
	}
	if hit := out.stats.limit; hit != nil {
		f := map[string]string{"error": hit.Error}
		if hit.Reset != "" {
			f["reset"] = hit.Reset
		}
		r.advance(ctx, ru, "usage_limit", r.metrics(ru, out, f), nil)
		return
	}
	fail := func(msg string) {
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": clip(msg, 1200) + ". Last output: " + out.lastOutput()}), nil)
	}
	raw, err := readResult(filepath.Join(ru.wt, ".loop", "result.json"))
	if err != nil {
		fail(fmt.Sprintf("the agent exited (code %d) without a usable .loop/result.json (%v)", out.exitCode, err))
		return
	}
	unlock := r.lockMirror(ru.mirror)
	facts, ferr := r.Git.Facts(ctx, ru.mirror, ru.wt, ru.repo.Base, ru.branch)
	unlock()
	if ferr != nil {
		fail("the runner could not read git: " + ferr.Error())
		return
	}
	v, err := ValidateResult(raw, Expect{Stage: "build"}, facts, filepath.Join(ru.wt, ".loop"))
	if err != nil {
		fail("result rejected: " + err.Error())
		return
	}
	if v.Outcome == "question" {
		r.advance(ctx, ru, "question", r.metrics(ru, out, map[string]string{"question": clip(v.Question, 2000)}), nil)
		return
	}
	files, err := EvidenceFiles(filepath.Join(ru.wt, ".loop"))
	if err != nil {
		fail("evidence: " + err.Error())
		return
	}
	pr, err := r.publish(ctx, ru, v, files, out)
	if err != nil {
		fail(err.Error())
		return
	}
	prVerified := "skipped"
	if r.VerifyPR != nil {
		info, err := r.VerifyPR(ctx, ru.repo.Slug, pr)
		if err != nil {
			fail(fmt.Sprintf("could not check PR #%d on GitHub: %v", pr, err))
			return
		}
		if info.HeadRefName != ru.branch || info.HeadRefOid != facts.HeadSHA || info.State != "OPEN" {
			fail(fmt.Sprintf("PR #%d is %s %s at %s, not %s at %s", pr, info.State, info.HeadRefName, info.HeadRefOid, ru.branch, facts.HeadSHA))
			return
		}
		prVerified = "true"
	}
	labels := evidenceLabels(v)
	for _, f := range files {
		if err := r.Client.Upload(ctx, ru.item.ID, f.Path, f.Name, labels[f.Name]); err != nil {
			fail("uploading evidence " + f.Name + ": " + err.Error())
			return
		}
	}
	fields := map[string]string{
		"repo": ru.repo.Slug, "pr": strconv.Itoa(pr), "branch": ru.branch, "head_sha": facts.HeadSHA,
		"summary": clip(v.Summary, 2000), "not_verified": clip(strings.Join(v.NotVerified, "; "), 1000), "pr_verified": prVerified,
	}
	if v.NeedsReview {
		fields["needs_review"] = "true"
	}
	r.advance(ctx, ru, "pr_opened", r.metrics(ru, out, fields), facts.ChangedPaths)
}

func evidenceLabels(v Valid) map[string]string {
	labels := map[string]string{}
	for _, e := range v.Evidence {
		labels[filepath.Base(e.File)] = e.Label
	}
	return labels
}

// publish pushes the item's branch (only it, never forced) and opens its PR, or updates the open
// one, with the runner's own credentials; it returns the PR number. The agent did neither.
func (r *Runner) publish(ctx context.Context, ru *run, v Valid, files []EvidenceFile, out outcome) (int, error) {
	if r.PRs == nil {
		return 0, errors.New("the runner has no way to open PRs (configuration)")
	}
	push := r.Push
	if push == nil {
		push = r.Git.PushBranch
	}
	unlock := r.lockMirror(ru.mirror)
	err := push(ctx, ru.mirror, ru.branch)
	remote := ""
	if err == nil {
		remote, err = r.Git.RemoteHead(ctx, ru.mirror, ru.branch)
	}
	unlock()
	if err != nil {
		return 0, fmt.Errorf("the runner could not push %s: %v", ru.branch, err)
	}
	if remote != v.HeadSHA {
		return 0, fmt.Errorf("after the push, %s on the remote is %q, not HEAD %s", ru.branch, remote, v.HeadSHA)
	}
	st := RunStats{Seconds: int(r.Clock.Now().Sub(ru.started).Seconds())}
	if res := out.stats.result; res != nil {
		u := res.Usage
		st = RunStats{Turns: res.NumTurns, CacheHit: CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead), Seconds: st.Seconds,
			CostUSD: res.CostUSD, HasResult: true}
	}
	body := filepath.Join(ru.dir, "pr-body.md")
	if err := os.WriteFile(body, []byte(PRBody(v.PRBody, ru.item.ID, files, evidenceLabels(v), st)), 0o600); err != nil {
		return 0, err
	}
	spec := PRSpec{Slug: ru.repo.Slug, Head: ru.branch, Base: ru.repo.Base, Title: v.PRTitle, BodyFile: body}
	pr, err := r.PRs.Find(ctx, ru.repo.Slug, ru.branch)
	if err != nil {
		return 0, fmt.Errorf("could not look for an open PR for %s: %v", ru.branch, err)
	}
	if pr > 0 {
		if err := r.PRs.Edit(ctx, pr, spec); err != nil {
			return 0, fmt.Errorf("could not update PR #%d: %v", pr, err)
		}
		return pr, nil
	}
	pr, err = r.PRs.Create(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("could not open the PR for %s: %v", ru.branch, err)
	}
	if pr <= 0 {
		return 0, fmt.Errorf("opening the PR for %s returned no number", ru.branch)
	}
	return pr, nil
}

// fetchTriage copies the item's evidence so far (triage's notes and repro first) into
// .loop/triage/ for the agent to read. A failure is reported in the brief, not fatal.
func (r *Runner) fetchTriage(ctx context.Context, ru *run) ([]TriageFile, string) {
	evs, err := r.Client.Evidence(ctx, ru.item.ID)
	if err != nil {
		return nil, err.Error()
	}
	if len(evs) == 0 {
		return nil, ""
	}
	dir := filepath.Join(ru.wt, ".loop", "triage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err.Error()
	}
	var out []TriageFile
	for _, e := range evs {
		name := fmt.Sprintf("%d-%s", e.N, safeName(e.Name))
		if err := r.Client.DownloadEvidence(ctx, ru.item.ID, e.N, filepath.Join(dir, name)); err != nil {
			return out, fmt.Sprintf("evidence %d (%s): %v", e.N, e.Name, err)
		}
		out = append(out, TriageFile{Name: name, Label: e.Label})
	}
	return out, ""
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeName is a file name with no path in it.
func safeName(n string) string {
	n = strings.Trim(unsafeName.ReplaceAllString(filepath.Base(n), "_"), "._")
	if n == "" {
		n = "file"
	}
	if len(n) > 80 {
		n = n[:80]
	}
	return n
}

func readResult(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("result.json is not a regular file")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// cleanup removes what the runner made for this item: the harness sandbox (with the harness
// files restored from the base branch first, so no agent edit runs with the runner's rights), the
// worktree and the scratch dir. The run dir stays as the run's log. It never touches anything
// else.
func (r *Runner) cleanup(ctx context.Context, ru *run, log *slog.Logger) {
	if _, err := os.Stat(ru.wt); err == nil && len(ru.repo.Cleanup) > 0 {
		restored := true
		if len(ru.repo.CleanupRestore) > 0 {
			args := append([]string{"checkout", "refs/remotes/origin/" + ru.repo.Base, "--"}, ru.repo.CleanupRestore...)
			if _, err := r.Git.run(ctx, ru.wt, args...); err != nil {
				log.Error("cleanup: restoring the harness files failed; not running cleanup commands", "err", err)
				restored = false
			}
		}
		for _, c := range ru.repo.Cleanup {
			if !restored {
				break
			}
			c = strings.ReplaceAll(c, "{sandbox}", ru.sandbox)
			if out, err := r.shell(ctx, ru.wt, c, 5*time.Minute); err != nil {
				log.Warn("cleanup command", "cmd", c, "err", err, "out", clip(out, 300))
			}
		}
	}
	unlock := r.lockMirror(ru.mirror)
	r.Git.RemoveWorktree(ctx, ru.mirror, ru.wt)
	unlock()
	log.Info("cleaned up", "worktree", ru.wt)
}

// GHVerify checks a PR with the gh CLI (the runner's own GitHub credentials).
func GHVerify(ctx context.Context, slug string, pr int) (PRInfo, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", "headRefName,headRefOid,state")
	out, err := cmd.Output()
	if err != nil {
		return PRInfo{}, err
	}
	var info PRInfo
	return info, json.Unmarshal(out, &info)
}
