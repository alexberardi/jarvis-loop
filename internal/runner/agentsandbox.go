package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The agent sandbox (docs/decisions/2026-10-16-agent-sandbox.md): build and review agents run in
// Claude Code's sandbox, which cannot read the harness's credentials or /dev/kvm, and whose
// commands each get a network namespace of their own, so a harness an agent starts dies with its
// command. For a repo with agent_sandbox, the runner brings the item's harness sandbox up itself,
// under the fence (fence.go), before each agent attempt, holds it for the attempt, and tears it
// down after (before verify, which brings up its own). The agent gets the env file the sandbox
// writes (local URLs and sandbox-local credentials, checked for the harness's secrets), loopback
// to the ports it names (through the sandbox's proxy), and named requests (a mobile shot) that the
// runner runs for it under the verify lock.

// MaxAgentRequests is how many requests one agent attempt may make.
const MaxAgentRequests = 10

// AgentRequestTimeout is the time limit of one request.
const AgentRequestTimeout = 20 * time.Minute

// RequestsDir is where the agent drops requests and finds their answers (worktree-relative).
const RequestsDir = ".loop/requests"

// requestPoll is how often the runner looks for new requests and for the sandbox's ready marker.
var requestPoll = 200 * time.Millisecond

// agentSandbox is one attempt's sandbox: the fenced process that holds it up.
type agentSandbox struct {
	cfg     *AgentSandbox
	pid     int
	exited  chan struct{}
	log     *os.File
	secrets []string
	ports   []int
	stop    sync.Once
}

// sandboxSub is s with {sandbox} replaced by the item's sandbox name.
func sandboxSub(s string, ru *run) string { return strings.ReplaceAll(s, "{sandbox}", ru.sandbox) }

// usesAgentSandbox says whether this run brings up the repo's agent sandbox.
func usesAgentSandbox(ru *run) bool {
	return ru.repo.AgentSandbox != nil && ru.repoDir != "" && (ru.stage == StageBuild || ru.stage == StageReview)
}

// sandboxEnv is the environment of the agent sandbox's commands.
func (r *Runner) sandboxEnv(ru *run) []string {
	return append(r.agentEnv(), "LOOP_ITEM="+strconv.FormatInt(ru.item.ID, 10), "LOOP_SANDBOX="+ru.sandbox,
		"LOOP_WORKTREE="+ru.wt, "LOOP_REPO="+ru.repo.Slug, "LOOP_BASE="+ru.repo.Base, "LOOP_MIRROR="+ru.mirror, "LOOP_STAGE="+ru.stage)
}

// trustedFilesChanged is the first of the repo's deny_write paths the worktree changed against
// its base (committed, staged, unstaged or untracked): the sandbox's commands come from those
// files and run outside the agent's sandbox, so they must be the base branch's.
func (r *Runner) trustedFilesChanged(ctx context.Context, ru *run) (string, error) {
	if len(ru.repo.DenyWrite) == 0 {
		return "", nil
	}
	diff, err := r.Git.run(ctx, ru.repoDir, "diff", "--name-status", "--no-renames", "refs/remotes/origin/"+ru.repo.Base, "--")
	if err != nil {
		return "", err
	}
	untracked, err := r.Git.run(ctx, ru.repoDir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(diff), "\n")
	for _, p := range strings.Split(strings.TrimSpace(untracked), "\n") {
		if p != "" {
			lines = append(lines, "A\t"+p)
		}
	}
	return touchesDenyWrite(ru.repo.DenyWrite, slices.DeleteFunc(lines, func(l string) bool { return l == "" })), nil
}

// startAgentSandbox brings the run's sandbox up: Up, under the fence, held by one fenced process
// for the attempt (the fence's pid namespace ends with its first process, so whatever Up leaves
// running lives as long as that process does). It returns once Up has finished and its env file
// passed the checks, and rewrites the run's settings with loopback to the env file's ports.
func (r *Runner) startAgentSandbox(ctx context.Context, ru *run) (*agentSandbox, error) {
	cfg := ru.repo.AgentSandbox
	if p, err := r.trustedFilesChanged(ctx, ru); err != nil {
		return nil, fmt.Errorf("checking the harness files: %w", err)
	} else if p != "" {
		return nil, fmt.Errorf("the worktree changes %s, one of the files the runner trusts (deny_write); the agent sandbox does not run the agent's version of it", p)
	}
	state := filepath.Join(ru.dir, "agent-sandbox")
	ready := filepath.Join(state, "ready")
	os.Remove(ready)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	envFile := filepath.Join(ru.repoDir, sandboxSub(cfg.EnvFile, ru))
	os.Remove(envFile) // an earlier attempt's
	logf, err := os.OpenFile(filepath.Join(ru.dir, "agent-sandbox.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	up := sandboxSub(cfg.Up, ru)
	fmt.Fprintf(logf, "$ %s\n", up)
	script := "(\n" + up + "\n) || exit $?\ntouch " + shellQuote(ready) + "\nexec sleep infinity\n"
	cmd, err := r.fence(ru.repoDir, script, r.sandboxEnv(ru), []string{state})
	if err != nil {
		logf.Close()
		return nil, fmt.Errorf("the agent sandbox cannot run fenced: %w", err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("starting the agent sandbox: %w", err)
	}
	sb := &agentSandbox{cfg: cfg, pid: cmd.Process.Pid, exited: make(chan struct{}), log: logf, secrets: harnessSecrets(cfg.SecretEnvFiles)}
	var state0 *os.ProcessState
	go func() {
		state0, _ = cmd.Process.Wait()
		close(sb.exited)
	}()
	up0 := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		t := time.NewTicker(requestPoll)
		defer t.Stop()
		for {
			if _, err := os.Stat(ready); err == nil {
				close(up0)
				return
			}
			select {
			case <-t.C:
			case <-sb.exited:
				return
			case <-quit:
				return
			}
		}
	}()
	upOrExit := make(chan struct{})
	go func() {
		select {
		case <-up0:
		case <-sb.exited:
		case <-quit:
		}
		close(upOrExit)
	}()
	w := r.keepAlive(ctx, ru, "starting the agent sandbox", upOrExit, cfg.UpTimeout())
	close(quit)
	fail := func(why string) (*agentSandbox, error) {
		r.stopAgentSandbox(context.WithoutCancel(ctx), ru, sb)
		b, _ := os.ReadFile(filepath.Join(ru.dir, "agent-sandbox.log"))
		return nil, fmt.Errorf("the agent sandbox %s: %s", why, clip(redact(tail(string(b), 15), sb.secrets), 800))
	}
	switch {
	case w == waitTimeout:
		return fail(fmt.Sprintf("did not come up within %s", cfg.UpTimeout()))
	case w == waitStopped:
		r.stopAgentSandbox(context.WithoutCancel(ctx), ru, sb)
		return nil, errVerifyStopped
	}
	select {
	case <-up0:
	default:
		<-sb.exited
		code := -1
		if state0 != nil {
			code = state0.ExitCode()
		}
		return fail(fmt.Sprintf("up failed (exit %d)", code))
	}
	body, err := readEnvFile(envFile)
	if err != nil {
		return fail(fmt.Sprintf("env file %s: %v", sandboxSub(cfg.EnvFile, ru), err))
	}
	for _, s := range sb.secrets {
		if strings.Contains(body, s) {
			return fail(fmt.Sprintf("env file %s holds a secret from the harness's env files; agents get only local URLs and sandbox-local credentials", sandboxSub(cfg.EnvFile, ru)))
		}
	}
	sb.ports = loopbackPorts(body, r.Cfg.LoopdAddrs())
	fmt.Fprintf(logf, "[up; env file %s; loopback ports %v]\n", sandboxSub(cfg.EnvFile, ru), sb.ports)
	if err := r.writeSettings(ru, loopbackHosts(sb.ports)); err != nil {
		return fail("settings: " + err.Error())
	}
	if b, err := os.ReadFile(ru.settings); err == nil { // the run's log keeps the item's settings
		os.WriteFile(filepath.Join(state, "settings.json"), b, 0o600)
	}
	r.Log.Info("agent sandbox up", "item", ru.item.ID, "sandbox", ru.sandbox, "ports", fmt.Sprint(sb.ports))
	return sb, nil
}

// stopAgentSandbox runs Down (fenced) and ends the process that held the sandbox up, with
// everything in its pid namespace. It is safe to call more than once and with nil.
func (r *Runner) stopAgentSandbox(ctx context.Context, ru *run, sb *agentSandbox) {
	if sb == nil {
		return
	}
	sb.stop.Do(func() {
		if sb.cfg.Down != "" {
			down := sandboxSub(sb.cfg.Down, ru)
			fmt.Fprintf(sb.log, "$ %s\n", down)
			out, err := r.shellEnv(ctx, ru.repoDir, down, r.sandboxEnv(ru), 5*time.Minute)
			sb.log.WriteString(out)
			if err != nil {
				fmt.Fprintf(sb.log, "[down failed: %v]\n", err)
				r.Log.Warn("agent sandbox down", "item", ru.item.ID, "err", err)
			}
		}
		_ = signalGroup(sb.pid, syscall.SIGTERM)
		select {
		case <-sb.exited:
		case <-time.After(10 * time.Second):
		}
		_ = signalGroup(sb.pid, syscall.SIGKILL)
		<-sb.exited
		sb.log.WriteString("[down]\n")
		sb.log.Close()
		r.Log.Info("agent sandbox down", "item", ru.item.ID, "sandbox", ru.sandbox)
	})
}

// readEnvFile reads the sandbox's env file: a regular file (not a link) of at most 64 KB.
func readEnvFile(p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if fi.Size() > 64<<10 {
		return "", errors.New("larger than 64 KB")
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

// DefaultSecretEnvFiles are the env files whose secrets an agent sandbox's env file must not
// hold: every *.env in the harness's config dir.
func DefaultSecretEnvFiles() []string {
	m, _ := filepath.Glob(filepath.Join(Home(), ".config", "jarvis-harness", "*.env"))
	return m
}

// harnessSecrets are the secret values in files (default DefaultSecretEnvFiles): the values of
// variables named *_KEY, *_TOKEN, *_SECRET or *PASSWORD*, at least 8 characters long.
func harnessSecrets(files []string) []string {
	if files == nil {
		files = DefaultSecretEnvFiles()
	}
	var out []string
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(io.LimitReader(fh, 1<<20))
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "), "=")
			if !ok {
				continue
			}
			k = strings.ToUpper(strings.TrimSpace(k))
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if len(v) >= 8 && (strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_TOKEN") || strings.HasSuffix(k, "_SECRET") || strings.Contains(k, "PASSWORD")) {
				out = append(out, v)
			}
		}
		fh.Close()
	}
	return uniq(out)
}

// redact replaces every secret in s.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

var loopbackRe = regexp.MustCompile(`(?:127\.0\.0\.1|localhost|\[::1\]):(\d{2,5})\b`)

// loopbackPorts are the loopback ports the env file names, without loopd's.
func loopbackPorts(env string, loopd []string) []int {
	var out []int
	for _, m := range loopbackRe.FindAllStringSubmatch(env, -1) {
		p, err := strconv.Atoi(m[1])
		if err != nil || p <= 0 || p > 65535 || slices.Contains(out, p) {
			continue
		}
		bad := false
		for _, a := range loopd {
			if strings.HasSuffix(a, ":"+m[1]) {
				bad = true
			}
		}
		if !bad {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// loopbackHosts are the allowedDomains entries for ports.
func loopbackHosts(ports []int) []string {
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("127.0.0.1:%d", p), fmt.Sprintf("localhost:%d", p))
	}
	return out
}

// AgentRequest is what the agent drops in .loop/requests/<id>.json.
type AgentRequest struct {
	Run  string            `json:"run"`
	Args map[string]string `json:"args,omitempty"`
}

// AgentRequestDone is the runner's answer, in .loop/requests/<id>.done.json.
type AgentRequestDone struct {
	Run      string   `json:"run"`
	ExitCode int      `json:"exit_code"`
	Error    string   `json:"error,omitempty"`
	Log      string   `json:"log"`
	Files    []string `json:"files,omitempty"`
}

var (
	requestID  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)
	requestArg = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
)

// serveRequests answers the agent's requests until ctx ends; the returned func stops it and
// waits for a running request to finish.
func (r *Runner) serveRequests(ctx context.Context, ru *run, sb *agentSandbox) func() {
	dir := filepath.Join(ru.wt, RequestsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Log.Warn("agent requests", "item", ru.item.ID, "err", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := map[string]bool{}
		t := time.NewTicker(requestPoll)
		defer t.Stop()
		for {
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				id, ok := strings.CutSuffix(e.Name(), ".json")
				if !ok || strings.HasSuffix(id, ".done") || seen[id] {
					continue
				}
				seen[id] = true
				res := r.agentRequest(ctx, ru, sb, id, len(seen))
				if ctx.Err() != nil {
					return
				}
				b, _ := json.MarshalIndent(res, "", "  ")
				if err := writeAtomic(filepath.Join(dir, id+".done.json"), append(b, '\n')); err != nil {
					r.Log.Warn("agent request answer", "item", ru.item.ID, "id", id, "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// agentRequest runs one request: a command from the config, never from the agent, under the fence
// and the verify lock (the emulator and the GPU are single); its files land in .loop/evidence/.
func (r *Runner) agentRequest(ctx context.Context, ru *run, sb *agentSandbox, id string, n int) AgentRequestDone {
	res := AgentRequestDone{ExitCode: -1}
	p := filepath.Join(ru.wt, RequestsDir, id+".json")
	if !requestID.MatchString(id) {
		res.Error = "request ids are lowercase letters, digits, - and _ (at most 41)"
		return res
	}
	if n > MaxAgentRequests {
		res.Error = fmt.Sprintf("at most %d requests per run", MaxAgentRequests)
		return res
	}
	raw, err := readEnvFile(p) // a regular file, small
	if err != nil {
		res.Error = "reading the request: " + err.Error()
		return res
	}
	var req AgentRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		res.Error = "the request is not JSON: " + err.Error()
		return res
	}
	res.Run = req.Run
	command, ok := sb.cfg.Requests[req.Run]
	if !ok {
		names := make([]string, 0, len(sb.cfg.Requests))
		for k := range sb.cfg.Requests {
			names = append(names, k)
		}
		sort.Strings(names)
		res.Error = fmt.Sprintf("no request %q; this repo has: %s", req.Run, strings.Join(names, ", "))
		return res
	}
	env := r.sandboxEnv(ru)
	for k, v := range req.Args {
		if !requestArg.MatchString(k) || len(v) > 500 || strings.ContainsAny(v, "\x00\n\r") {
			res.Error = fmt.Sprintf("argument %q: names are lowercase letters, digits and _, values one line of at most 500 characters", k)
			return res
		}
		env = append(env, "LOOP_REQ_"+strings.ToUpper(k)+"="+v)
	}
	if p, err := r.trustedFilesChanged(ctx, ru); err != nil || p != "" {
		res.Error = "the runner does not run requests while the worktree changes the files it trusts (deny_write): " + p
		if err != nil {
			res.Error = "checking the harness files: " + err.Error()
		}
		return res
	}
	head, err := r.Git.run(ctx, ru.repoDir, "rev-parse", "HEAD")
	if err != nil {
		res.Error = "HEAD: " + err.Error()
		return res
	}
	work := filepath.Join(ru.dir, "requests", id)
	evDir := filepath.Join(work, "evidence")
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		res.Error = err.Error()
		return res
	}
	env = append(env, "LOOP_HEAD="+strings.TrimSpace(head), "LOOP_EVIDENCE_DIR="+evDir)
	release, err := r.lockForRequest(ctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer release()
	r.Log.Info("agent request", "item", ru.item.ID, "id", id, "run", req.Run)
	out, err := r.shellEnv(ctx, ru.repoDir, sandboxSub(command, ru), env, AgentRequestTimeout, work)
	os.WriteFile(filepath.Join(work, "output.log"), []byte(out), 0o600)
	res.Log = clip(redact(tail(out, 40), sb.secrets), 4000)
	res.ExitCode = 0
	if err != nil {
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Error = err.Error()
		}
	}
	files, _, err := EvidenceFiles(work)
	if err != nil {
		res.Error = "its files: " + err.Error()
	}
	evOut := filepath.Join(ru.wt, ".loop", "evidence")
	for _, f := range files {
		name := id + "-" + safeName(f.Name)
		b, err := os.ReadFile(f.Path)
		if err == nil {
			err = os.WriteFile(filepath.Join(evOut, name), b, 0o644)
		}
		if err != nil {
			res.Error = "copying " + f.Name + ": " + err.Error()
			continue
		}
		res.Files = append(res.Files, "evidence/"+name)
	}
	return res
}

// lockForRequest takes the machine's verify lock for a request, giving up when ctx ends (the
// agent's attempt is over) or after VerifyLockWait.
func (r *Runner) lockForRequest(ctx context.Context) (func(), error) {
	got := make(chan struct{})
	var f *os.File
	var err error
	go func() {
		f, err = lockFile(r.Cfg.VerifyLock)
		close(got)
	}()
	select {
	case <-got:
		if err != nil {
			return nil, fmt.Errorf("the verify lock: %w", err)
		}
		return func() { closeLock(f) }, nil
	case <-ctx.Done():
	case <-time.After(VerifyLockWait):
	}
	go func() { <-got; closeLock(f) }()
	return nil, errors.New("the verify lock (another verify or request holds the emulator) did not come free in time")
}
