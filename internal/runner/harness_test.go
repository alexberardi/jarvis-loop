package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/harness"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// harnessRepo gives the rig's demo repo a harness, with the test binary as loop-harness. Up
// leaves a process holding the returned lock (alive while the keeper is), a
// mark in the keeper's private /tmp, and a secret in the keeper shell's environment; down appends
// to down.log there.
func (g *rig) harnessRepo(timeout string) (lock string) {
	g.t.Helper()
	dir := g.t.TempDir()
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, dir)
	g.r.Env = append(g.r.Env, "HARNESS_TEST_DIR="+dir)
	if err := g.r.ProbeFence(); err != nil {
		g.t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	// The fence sees only what it binds, and its /tmp is private: the test binary (in the host's
	// /tmp, under go test) and the fake loop-harness go in the bound dir.
	self := filepath.Join(dir, "runner.test")
	copyFile(g.t, testBinary(g.t), self)
	bin := filepath.Join(dir, "loop-harness")
	os.WriteFile(bin, []byte("#!/bin/sh\nLOOP_TEST_AS=harness exec "+shellQuote(self)+" \"$@\"\n"), 0o755)
	g.r.Cfg.LoopHarness = bin
	rc := g.r.Cfg.Repos["demo"]
	rc.Harness = &HarnessConfig{
		Up: `export INSTALLED_APP_KEY=up-SECRET; echo "up for $LOOP_SANDBOX"; echo keeper > /tmp/keeper-mark
setsid flock "$HARNESS_TEST_DIR/alive.lock" sleep 1000 </dev/null >/dev/null 2>&1 &
for i in $(seq 100); do flock -n "$HARNESS_TEST_DIR/alive.lock" true || break; sleep 0.05; done`,
		Down:        `echo "down for $LOOP_SANDBOX" >> .loop/scratch/down.log`,
		TimeoutText: timeout,
	}
	if timeout != "" {
		d, _ := time.ParseDuration(timeout)
		rc.Harness.timeout = d
	}
	g.r.Cfg.Repos["demo"] = rc
	g.t.Cleanup(g.r.CloseHarness)
	return filepath.Join(dir, "alive.lock")
}

func testBinary(t *testing.T) string {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// runGuard writes the guard config a build run of item 5 in wt gets, in a dir of its own.
func (g *rig) runGuard(wt string) (dir string) {
	dir = g.t.TempDir()
	gc, _ := json.Marshal(guard.Config{Item: 5, Worktree: wt, Scratch: filepath.Join(wt, ".loop", "scratch"), Branch: "loop/5-e2e",
		Remote: "origin", Home: Home(), Protected: g.r.Cfg.Protected(), Secrets: g.r.Cfg.Secrets(), Words: []string{g.r.Cfg.UserEmulator},
		LoopdAddrs: g.r.Cfg.LoopdAddrs(), Report: filepath.Join(dir, "refusals.jsonl"), Armed: filepath.Join(dir, "armed")})
	os.WriteFile(filepath.Join(dir, "guard.json"), gc, 0o600)
	return dir
}

// harnessSession opens a session for a build run of item 5 of the demo repo in a fresh worktree,
// as the runner does before the agent starts, and makes it the test's working directory.
func (g *rig) harnessSession() (*harnessSession, string) {
	g.t.Helper()
	wt := filepath.Join(g.r.Cfg.WorkRoot, "5-build")
	os.MkdirAll(filepath.Join(wt, ".loop", "scratch"), 0o755)
	os.MkdirAll(filepath.Join(wt, ".loop", "evidence"), 0o755)
	ru := &run{item: Item{ID: 5}, stage: StageBuild, repoKey: "demo", repo: g.r.Cfg.Repos["demo"], wt: wt,
		scratch: filepath.Join(wt, ".loop", "scratch"), sandbox: "loop-5", dir: g.runGuard(wt)}
	s, err := g.r.startHarness(ru)
	if err != nil || s == nil {
		g.t.Fatalf("session %v %v", s, err)
	}
	g.t.Chdir(wt)
	return s, wt
}

// call is loop-harness from the working directory.
func call(t *testing.T, op, cmd string) (int, string, error) {
	t.Helper()
	wd, _ := os.Getwd()
	f, err := harness.Find(wd)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := harness.Call(f, harness.Request{Op: op, Cmd: cmd}, &out, nil)
	return code, out.String(), err
}

// The spool's protocol: the run's token is checked, an unknown op is refused, output and exit
// code make the round trip, and the spool goes with the session.
func TestHarnessProtocol(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("")
	s, wt := g.harnessSession()
	if fi, err := os.Stat(filepath.Join(wt, harness.FileRel)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("harness.json %v %v", fi, err)
	}
	f, err := harness.Find(wt)
	if err != nil || f.Spool != filepath.Join(wt, harness.SpoolRel) {
		t.Fatalf("harness.json %+v %v", f, err)
	}
	code, out, err := call(t, harness.OpRun, `echo "hello from $LOOP_SANDBOX in $PWD"; echo to-stderr >&2; exit 7`)
	if err != nil || code != 7 || !strings.Contains(out, "hello from loop-5 in "+wt) || !strings.Contains(out, "to-stderr") {
		t.Fatalf("run: %d %q %v", code, out, err)
	}
	if log := g.read(filepath.Join(wt, ".loop", "scratch", "harness", "1.log")); !strings.Contains(log, "hello from loop-5") || !strings.Contains(log, "[exit 7]") {
		t.Fatalf("run log:\n%s", log)
	}
	// The client cleans up after itself: nothing is left in the spool.
	for _, d := range []string{"req", "out"} {
		if left, _ := os.ReadDir(filepath.Join(f.Spool, d)); len(left) != 0 {
			t.Fatalf("%s/ keeps %v", d, left)
		}
	}
	if _, _, err := call(t, "reboot", ""); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("unknown op: %v", err)
	}
	if _, _, err := call(t, harness.OpRun, "  "); err == nil {
		t.Fatal("an empty run was accepted")
	}
	bad := harness.File{Spool: f.Spool, Token: strings.Repeat("0", 64)}
	if code, err := harness.Call(bad, harness.Request{Op: harness.OpRun, Cmd: "true"}, &bytes.Buffer{}, nil); err == nil || code != harness.ExitBroker ||
		!strings.Contains(err.Error(), "token") {
		t.Fatalf("a wrong token: %d %v", code, err)
	}
	// Junk in req/ (not a request's name, a directory, a symlink) is left alone or refused.
	os.WriteFile(filepath.Join(f.Spool, "req", "notes.txt"), []byte("hi"), 0o600)
	if code, out, err := call(t, harness.OpRun, "echo still-ok"); code != 0 || err != nil || !strings.Contains(out, "still-ok") {
		t.Fatalf("after junk: %d %q %v", code, out, err)
	}
	// After the session ends harness.json and the spool are gone, and a late request says so.
	s.end(context.Background())
	if _, err := os.Stat(filepath.Join(wt, harness.FileRel)); !os.IsNotExist(err) {
		t.Fatalf("harness.json left behind: %v", err)
	}
	if _, err := os.Stat(f.Spool); !os.IsNotExist(err) {
		t.Fatalf("the spool left behind: %v", err)
	}
	if _, err := harness.Call(f, harness.Request{Op: harness.OpStatus}, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("an ended session still answers")
	}
	// No up, no down at the end.
	if _, err := os.Stat(filepath.Join(wt, ".loop", "scratch", "down.log")); !os.IsNotExist(err) {
		t.Fatalf("down ran without an up: %v", err)
	}
}

// The broker applies the run's guard itself: a request written without the guard hook (the
// agent's own client) cannot run what the agent's Bash could not.
func TestHarnessGuard(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("")
	s, wt := g.harnessSession()
	f, _ := harness.Find(wt)
	for _, cmd := range []string{"adb shell true", "git push origin HEAD", "cd sub && sudo true"} {
		var out bytes.Buffer
		code, err := harness.Call(f, harness.Request{Op: harness.OpRun, Cmd: cmd}, &out, nil)
		if code != harness.ExitBroker || err == nil || !strings.Contains(err.Error(), "loop-guard refused") {
			t.Fatalf("%q: %d %q %v", cmd, code, out.String(), err)
		}
	}
	if code, out, err := call(t, harness.OpRun, "echo ok"); code != 0 || err != nil || out != "ok\n" {
		t.Fatalf("echo ok: %d %q %v", code, out, err)
	}
	if rep := g.read(s.guard.Report); strings.Count(rep, `"tool":"loop-harness run"`) != 3 {
		t.Fatalf("refusals:\n%s", rep)
	}
}

// The client needs nothing but the worktree: in a fence of its own with no network at all (no
// loopback, no Unix socket to the runner) and its own pid namespace, it still reaches the broker.
// (Under an agent's Claude Code sandbox, as when one builds this repo, socket(AF_UNIX) itself is
// refused; the test says so in its log.)
func TestHarnessClientIsolated(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("")
	_, wt := g.harnessSession()
	if err := exec.Command("python3", "-c", "import socket; socket.socket(socket.AF_UNIX)").Run(); err != nil {
		t.Logf("socket(AF_UNIX) is refused here (%v): the spool is the only way", err)
	}
	bwrap := g.r.Cfg.Bwrap
	if bwrap == "" {
		bwrap = DefaultBwrap
	}
	dir := filepath.Dir(g.r.Cfg.LoopHarness)
	cmd := exec.Command(bwrap, "--dev-bind", "/", "/", "--tmpfs", "/tmp", "--ro-bind", dir, dir, "--bind", wt, wt,
		"--unshare-net", "--unshare-pid", "--unshare-ipc", "--proc", "/proc", "--die-with-parent", "--chdir", filepath.Join(wt, ".loop"),
		"--", g.r.Cfg.LoopHarness, "run", "echo ok from $LOOP_SANDBOX; exit 3")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 || string(out) != "ok from loop-5\n" {
		t.Fatalf("isolated client: %v %q", err, out)
	}
}

// The keeper: up's processes and its /tmp survive between runs, which see them; runs get the
// runner's environment, not what up exported; at the session's end the keeper is killed (with
// what it started) and down runs once.
func TestHarnessKeeper(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	lock := g.harnessRepo("")
	s, wt := g.harnessSession()
	if code, _, _ := call(t, harness.OpStatus, ""); code != 1 {
		t.Fatalf("status before up: %d", code)
	}
	code, out, err := call(t, harness.OpUp, "")
	if err != nil || code != 0 || !strings.Contains(out, "up for loop-5") {
		t.Fatalf("up: %d %q %v", code, out, err)
	}
	if held(lock) != true {
		t.Fatal("up's process is not running")
	}
	for i := 0; i < 2; i++ {
		code, out, err := call(t, harness.OpRun, `cat /tmp/keeper-mark; echo "key=[$INSTALLED_APP_KEY]"`)
		if err != nil || code != 0 || !strings.Contains(out, "keeper") || !strings.Contains(out, "key=[]") {
			t.Fatalf("run %d: %d %q %v", i, code, out, err)
		}
	}
	if code, out, _ := call(t, harness.OpStatus, ""); code != 0 || !strings.Contains(out, "up since") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := call(t, harness.OpUp, ""); code != 0 || !strings.Contains(out, "already up") {
		t.Fatalf("second up: %d %q", code, out)
	}
	s.end(context.Background())
	if held(lock) {
		t.Fatal("up's process outlived the session")
	}
	if got := g.read(filepath.Join(wt, ".loop", "scratch", "down.log")); got != "down for loop-5\n" {
		t.Fatalf("down.log %q", got)
	}
	if len(s.fences) != 0 || s.keeper != nil {
		t.Fatalf("fences left: %d", len(s.fences))
	}
}

// The agent's own down: in the keeper, then the keeper ends; a run after it gets a fence of its
// own (no keeper /tmp).
func TestHarnessDown(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	lock := g.harnessRepo("")
	g.harnessSession()
	if code, _, err := call(t, harness.OpUp, ""); code != 0 || err != nil {
		t.Fatalf("up: %d %v", code, err)
	}
	if code, _, err := call(t, harness.OpDown, ""); code != 0 || err != nil {
		t.Fatalf("down: %d %v", code, err)
	}
	if held(lock) {
		t.Fatal("down left up's process running")
	}
	if code, out, _ := call(t, harness.OpRun, "cat /tmp/keeper-mark"); code == 0 {
		t.Fatalf("a run after down still sees the keeper: %q", out)
	}
	if code, _, _ := call(t, harness.OpStatus, ""); code != 1 {
		t.Fatalf("status after down: %d", code)
	}
}

// A command that runs too long is ended (124); the client going away ends its command, and the
// next request is not held up by it.
func TestHarnessTimeoutAndCancel(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("1s")
	s, _ := g.harnessSession()
	start := time.Now()
	code, out, err := call(t, harness.OpRun, "sleep 30")
	if err != nil || code != harness.ExitTimeout || !strings.Contains(out, "timed out after 1s") || time.Since(start) > 15*time.Second {
		t.Fatalf("timeout: %d %q %v after %s", code, out, err, time.Since(start))
	}
	s.cfg.timeout = time.Minute
	for _, op := range []string{harness.OpRun, harness.OpUp} {
		if op == harness.OpUp {
			if code, _, err := call(t, harness.OpUp, ""); code != 0 || err != nil {
				t.Fatalf("up: %d %v", code, err)
			}
		}
		cancel := make(chan struct{})
		w := &signalWriter{want: "started", seen: cancel}
		f, _ := harness.Find(s.wt)
		if code, err := harness.Call(f, harness.Request{Op: harness.OpRun, Cmd: "echo started; sleep 60"}, w, cancel); code != harness.ExitBroker || err == nil {
			t.Fatalf("%s: a cancelled run: %d %v", op, code, err)
		}
		start = time.Now()
		if code, out, err := call(t, harness.OpRun, "echo next"); code != 0 || err != nil || !strings.Contains(out, "next") || time.Since(start) > 15*time.Second {
			t.Fatalf("%s: the run after a cancelled one: %d %q %v after %s", op, code, out, err, time.Since(start))
		}
	}
}

// signalWriter closes seen once want has been written to it.
type signalWriter struct {
	buf  bytes.Buffer
	want string
	seen chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	had := strings.Contains(w.buf.String(), w.want)
	w.buf.Write(p)
	if !had && strings.Contains(w.buf.String(), w.want) {
		close(w.seen)
	}
	return len(p), nil
}

// held reports whether a process holds the lock file.
func held(lock string) bool {
	for i := 0; i < 100; i++ { // the kernel takes a moment to reap a killed namespace
		if exec.Command("flock", "-n", lock, "true").Run() == nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// A build of a repo with a harness: the agent (the fake, through the real guard and its PATH)
// starts the harness and runs a command in it; the settings open no Unix socket; when the agent ends the keeper is gone and down ran, before verify.
func TestHarnessBuildRun(t *testing.T) {
	g := newRig(t, fakeclaude.Harness)
	g.harnessRepo("")
	g.verifyRepo(VerifyStep{Name: "after the harness", Run: `test -f .loop/scratch/down.log && ! test -f .loop/harness.json && flock -n "$HARNESS_TEST_DIR/alive.lock" true && echo clean`})
	id := g.toBuild("Use the harness")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	run := g.lastRun(id, StageBuild)
	if got := g.read(filepath.Join(run, "verify", "verify-1-after-the-harness", "output.log")); got != "clean\n" {
		t.Fatalf("verify output %q", got)
	}
	if st := g.read(filepath.Join(g.r.Cfg.WorkRoot, "settings", "demo-build.json")); strings.Contains(st, "UnixSockets") {
		t.Fatalf("the settings open a Unix socket:\n%s", st)
	}
	if b := g.read(filepath.Join(run, "brief.md")); !strings.Contains(b, "## The harness, through the runner") || !strings.Contains(b, "`loop-harness up`") {
		t.Fatalf("brief:\n%s", b)
	}
	found := false
	for _, e := range g.item(id).Evidence {
		found = found || e.Name == "harness.txt"
	}
	if !found {
		t.Fatalf("the agent's harness run left no evidence: %+v; log:\n%s", g.item(id).Evidence, g.logs.String())
	}
}

// Lease loss (the user stops the loop) and runner shutdown end the session too.
func TestHarnessEndsWithTheRun(t *testing.T) {
	for _, how := range []string{"stopped", "shutdown"} {
		t.Run(how, func(t *testing.T) {
			g := newRig(t, fakeclaude.HarnessHang)
			lock := g.harnessRepo("")
			id := g.toBuild("Hang on")
			ctx, cancel := context.WithCancel(g.ctx)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); g.r.ClaimAndRun(ctx) }()
			wt := filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id))
			g.clk.BlockUntil(2)
			waitFile(t, filepath.Join(wt, "wip.txt"))
			if !held(lock) {
				t.Fatalf("the harness is not up; log:\n%s", g.logs.String())
			}
			if how == "stopped" {
				stop := true
				g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{Stopped: &stop})
				g.clk.Advance(60 * time.Second)
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatalf("the run did not end; log:\n%s", g.logs.String())
			}
			if held(lock) {
				t.Fatal("the harness outlived the run")
			}
			if !strings.Contains(g.logs.String(), "cleaned up") {
				t.Fatalf("log:\n%s", g.logs.String())
			}
		})
	}
}

// The real thing, by hand on the dev box (it needs the installed claude, its login and the API):
//
//	LOOP_E2E_CLAUDE=1 go test ./internal/runner -run TestHarnessRealClaude -v
//
// One headless claude, with the settings the runner generates for a harness repo (its OS sandbox
// on, nothing loosened for the harness) and the real guard as its hook, is asked to run one Bash
// command: loop-harness run 'echo ok'. It proves loop-harness reaches the broker from inside
// Claude Code's sandbox and that the command ran in the runner's fence.
func TestHarnessRealClaude(t *testing.T) {
	if os.Getenv("LOOP_E2E_CLAUDE") == "" {
		t.Skip("set LOOP_E2E_CLAUDE=1 to run a real claude (dev box only)")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("2m")
	s, wt := g.harnessSession()
	dir := filepath.Dir(s.guard.Report)
	guardBin := filepath.Join(dir, "loop-guard")
	os.WriteFile(guardBin, []byte("#!/bin/sh\nLOOP_TEST_AS=guard exec "+shellQuote(testBinary(t))+" \"$@\"\n"), 0o755)
	settings, err := GenerateSettings(SettingsInput{Mode: ModeAuto, Guard: guardBin, Home: Home(), Protected: g.r.Cfg.Protected(),
		Secrets: g.r.Cfg.Secrets(), LoopdAddrs: g.r.Cfg.LoopdAddrs(), Harness: true})
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "settings.json")
	os.WriteFile(sp, settings, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, ClaudeArgs(AgentArgs{Mode: ModeAuto, Settings: sp})...)
	cmd.Dir = wt
	cmd.Env = append(prependPath(ScrubEnv(nil), g.r.Cfg.harnessBinDir()), guard.ConfigEnv+"="+filepath.Join(dir, "guard.json"),
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	cmd.Stdin = strings.NewReader("Run exactly one Bash command, with the sandbox on (do not ask to disable it): loop-harness run 'echo ok'\n" +
		"Then reply with its output and exit code. Do nothing else.\n")
	out, err := cmd.CombinedOutput()
	os.WriteFile(filepath.Join(dir, "stream.jsonl"), out, 0o600)
	log := g.read(filepath.Join(wt, ".loop", "scratch", "harness", "1.log"))
	t.Logf("claude: %v; stream in %s\nharness log:\n%s", err, filepath.Join(dir, "stream.jsonl"), log)
	if !strings.Contains(log, "\nok\n") || !strings.Contains(log, "[exit 0]") {
		t.Fatalf("the harness never ran echo ok; claude's stream (tail):\n%s", out[max(0, len(out)-4000):])
	}
	if _, err := os.Stat(filepath.Join(dir, "armed")); err != nil {
		t.Error("the guard never ran: the settings were not applied")
	}
	if strings.Contains(string(out), `"dangerouslyDisableSandbox":true`) {
		t.Error("claude asked to leave the sandbox: the command must have run inside it")
	}
}

// TestHarnessServe keeps a session open for a client run by hand, from another process (for
// example a Bash command of a Claude Code agent, inside its own OS sandbox):
//
//	LOOP_HARNESS_SERVE=$PWD/.loop/scratch/serve go test ./internal/runner -run TestHarnessServe -v
//
// It writes the session's worktree to $LOOP_HARNESS_SERVE/worktree, then serves until
// $LOOP_HARNESS_SERVE/stop exists (or five minutes pass).
func TestHarnessServe(t *testing.T) {
	dir := os.Getenv("LOOP_HARNESS_SERVE")
	if dir == "" {
		t.Skip("set LOOP_HARNESS_SERVE to a directory to serve a session by hand")
	}
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("2m")
	_, wt := g.harnessSession()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "worktree"), []byte(wt+"\n"), 0o644)
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			break
		}
	}
	t.Logf("harness logs:\n%s", g.read(filepath.Join(wt, ".loop", "scratch", "harness", "1.log")))
}
