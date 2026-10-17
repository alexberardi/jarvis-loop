package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	self, _ := os.Executable()
	bin := filepath.Join(g.t.TempDir(), "loop-harness")
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
	// Claude Code's own sandbox (an agent building this) refuses Unix sockets: the broker then
	// listens on loopback TCP, without the caller check.
	if ln, err := net.Listen("unix", filepath.Join(dir, "probe.sock")); err != nil {
		g.t.Logf("no Unix sockets here (%v): the broker uses loopback TCP", err)
		g.r.harnessTCP = true
	} else {
		ln.Close()
	}
	g.t.Cleanup(g.r.CloseHarness)
	return filepath.Join(dir, "alive.lock")
}

// harnessSession opens a session for a build run of item 5 of the demo repo in a fresh worktree,
// as the runner does before the agent starts, and makes it the test's working directory (the
// broker serves only callers inside the session's worktree).
func (g *rig) harnessSession() (*harnessSession, string) {
	g.t.Helper()
	wt := filepath.Join(g.r.Cfg.WorkRoot, "5-build")
	os.MkdirAll(filepath.Join(wt, ".loop", "scratch"), 0o755)
	os.MkdirAll(filepath.Join(wt, ".loop", "evidence"), 0o755)
	ru := &run{item: Item{ID: 5}, stage: StageBuild, repoKey: "demo", repo: g.r.Cfg.Repos["demo"], wt: wt,
		scratch: filepath.Join(wt, ".loop", "scratch"), sandbox: "loop-5"}
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
	code, err := harness.Call(f, harness.Request{Op: op, Cmd: cmd}, &out)
	return code, out.String(), err
}

// The socket's protocol: the run's token and the caller's working directory are checked, an
// unknown op is refused, and output and exit code make the round trip.
func TestHarnessProtocol(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.harnessRepo("")
	s, wt := g.harnessSession()
	if fi, err := os.Stat(g.r.Cfg.HarnessSocket()); !g.r.harnessTCP && (err != nil || fi.Mode().Perm() != 0o600) {
		t.Fatalf("socket %v %v", fi, err)
	}
	sock := s.b.addr
	if fi, err := os.Stat(filepath.Join(wt, harness.FileRel)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("harness.json %v %v", fi, err)
	}
	code, out, err := call(t, harness.OpRun, `echo "hello from $LOOP_SANDBOX in $PWD"; echo to-stderr >&2; exit 7`)
	if err != nil || code != 7 || !strings.Contains(out, "hello from loop-5 in "+wt) || !strings.Contains(out, "to-stderr") {
		t.Fatalf("run: %d %q %v", code, out, err)
	}
	if log := g.read(filepath.Join(wt, ".loop", "scratch", "harness", "1.log")); !strings.Contains(log, "hello from loop-5") || !strings.Contains(log, "[exit 7]") {
		t.Fatalf("run log:\n%s", log)
	}
	if _, _, err := call(t, "reboot", ""); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("unknown op: %v", err)
	}
	if _, _, err := call(t, harness.OpRun, "  "); err == nil {
		t.Fatal("an empty run was accepted")
	}
	f := harness.File{Socket: sock, Token: strings.Repeat("0", 64)}
	if code, err := harness.Call(f, harness.Request{Op: harness.OpRun, Cmd: "true"}, &bytes.Buffer{}); err == nil || code != harness.ExitBroker ||
		!strings.Contains(err.Error(), "no harness session") {
		t.Fatalf("a wrong token: %d %v", code, err)
	}
	// The right token from outside the worktree (another item's agent reading this harness.json).
	t.Chdir(t.TempDir())
	if _, err := harness.Call(harness.File{Socket: sock, Token: s.token}, harness.Request{Op: harness.OpStatus}, &bytes.Buffer{}); g.r.harnessTCP {
		t.Log("not checking the caller's working directory: no Unix sockets here")
	} else if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("a caller outside the worktree: %v", err)
	}
	// After the session ends its token is worthless and harness.json is gone.
	t.Chdir(wt)
	s.end(context.Background())
	if _, err := os.Stat(filepath.Join(wt, harness.FileRel)); !os.IsNotExist(err) {
		t.Fatalf("harness.json left behind: %v", err)
	}
	if _, err := harness.Call(harness.File{Socket: sock, Token: s.token}, harness.Request{Op: harness.OpStatus}, &bytes.Buffer{}); err == nil {
		t.Fatal("an ended session still answers")
	}
	// No up, no down at the end.
	if _, err := os.Stat(filepath.Join(wt, ".loop", "scratch", "down.log")); !os.IsNotExist(err) {
		t.Fatalf("down ran without an up: %v", err)
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
		network, addr := "unix", s.b.addr
		if a, ok := strings.CutPrefix(addr, "tcp:"); ok {
			network, addr = "tcp", a
		}
		c, err := net.Dial(network, addr)
		if err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(c).Encode(harness.Request{Token: s.token, Op: harness.OpRun, Cmd: "echo started; sleep 60"})
		var fr harness.Frame
		if err := json.NewDecoder(c).Decode(&fr); err != nil || !strings.Contains(string(fr.Data), "started") {
			t.Fatalf("first frame %+v %v", fr, err)
		}
		c.Close()
		start = time.Now()
		if code, out, err := call(t, harness.OpRun, "echo next"); code != 0 || err != nil || !strings.Contains(out, "next") || time.Since(start) > 15*time.Second {
			t.Fatalf("%s: the run after a cancelled one: %d %q %v after %s", op, code, out, err, time.Since(start))
		}
	}
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
// starts the harness and runs a command in it; the settings let it reach only the broker's
// socket; when the agent ends the keeper is gone and down ran, before verify.
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
	var s struct {
		Sandbox struct {
			Network map[string]any `json:"network"`
		} `json:"sandbox"`
	}
	json.Unmarshal([]byte(g.read(filepath.Join(g.r.Cfg.WorkRoot, "settings", "demo-build.json"))), &s)
	if got := fmt.Sprint(s.Sandbox.Network["allowUnixSockets"]); got != "["+g.r.Cfg.HarnessSocket()+"]" {
		t.Fatalf("allowUnixSockets %s", got)
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
// on, the broker's socket in allowUnixSockets) and the real guard as its hook, is asked to run one
// Bash command: loop-harness run 'echo ok'. It proves Claude Code's sandbox lets loop-harness reach
// the broker and that the command ran in the runner's fence.
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
	if g.r.harnessTCP {
		t.Fatal("Unix sockets are refused here: run this outside any sandbox")
	}
	_, wt := g.harnessSession()
	self, _ := os.Executable()
	dir := t.TempDir()
	guardBin := filepath.Join(dir, "loop-guard")
	os.WriteFile(guardBin, []byte("#!/bin/sh\nLOOP_TEST_AS=guard exec "+shellQuote(self)+" \"$@\"\n"), 0o755)
	gc, _ := json.Marshal(guard.Config{Item: 5, Worktree: wt, Scratch: filepath.Join(wt, ".loop", "scratch"), Branch: "loop/5-e2e",
		Remote: "origin", Home: Home(), Protected: g.r.Cfg.Protected(), Secrets: g.r.Cfg.Secrets(), Words: []string{g.r.Cfg.UserEmulator},
		LoopdAddrs: g.r.Cfg.LoopdAddrs(), Report: filepath.Join(dir, "refusals.jsonl"), Armed: filepath.Join(dir, "armed")})
	os.WriteFile(filepath.Join(dir, "guard.json"), gc, 0o600)
	settings, err := GenerateSettings(SettingsInput{Mode: ModeAuto, Guard: guardBin, Home: Home(), Protected: g.r.Cfg.Protected(),
		Secrets: g.r.Cfg.Secrets(), LoopdAddrs: g.r.Cfg.LoopdAddrs(), HarnessSocket: g.r.Cfg.HarnessSocket()})
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "settings.json")
	os.WriteFile(sp, settings, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, ClaudeArgs(AgentArgs{Mode: ModeAuto, Settings: sp})...)
	cmd.Dir = wt
	cmd.Env = append(prependPath(scrubEnv(nil), g.r.Cfg.harnessBinDir()), guard.ConfigEnv+"="+filepath.Join(dir, "guard.json"),
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
