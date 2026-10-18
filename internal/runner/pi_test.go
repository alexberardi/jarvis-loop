package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/harness"
)

// fakeSSH is an ssh that refuses a command line without the runner's fixed options, logs each
// call, and runs the remote script with sh in <dir>/home, the "Pi".
const fakeSSH = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_PI/calls.log"
for want in "-F /dev/null" "-o IdentitiesOnly=yes" "-o IdentityAgent=none" "-o BatchMode=yes" "-o StrictHostKeyChecking=yes" "-o ClearAllForwardings=yes" "-o ForwardAgent=no" "-p 22 -l pi -- pi.test"; do
  case " $* " in *" $want "*) ;; *) echo "fake ssh: missing $want" >&2; exit 254;; esac
done
[ -z "$SSH_AUTH_SOCK" ] || { echo "fake ssh: an agent socket leaked" >&2; exit 254; }
for last; do :; done
cd "$FAKE_PI/home" && exec sh -c "$last"
`

// piRig gives the rig's runner a test Pi (the fake ssh above) and the loop-harness binary, and
// returns the Pi's fake home.
func (g *rig) piRig() (home string) {
	g.t.Helper()
	dir := g.t.TempDir()
	home = filepath.Join(dir, "home")
	os.MkdirAll(home, 0o755)
	g.t.Setenv("FAKE_PI", dir)
	g.t.Setenv("SSH_AUTH_SOCK", "/run/user/1000/agent.sock")
	ssh := filepath.Join(dir, "ssh")
	os.WriteFile(ssh, []byte(fakeSSH), 0o755)
	key, known := filepath.Join(dir, "id"), filepath.Join(dir, "known_hosts")
	os.WriteFile(key, []byte("key"), 0o600)
	os.WriteFile(known, []byte("pi.test ssh-ed25519 AAAA\n"), 0o644)
	g.r.Cfg.Capabilities = append(g.r.Cfg.Capabilities, ResourcePi)
	g.r.Cfg.Pi = &PiConfig{Host: "pi.test", User: "pi", Key: key, KnownHosts: known, SSH: ssh,
		Restore: `echo restored >> restore.log`}
	if err := g.r.Cfg.Pi.normalize(g.r.Cfg.Capabilities); err != nil {
		g.t.Fatal(err)
	}
	// The broker checks the client binary exists; these tests call it in-process.
	bin := filepath.Join(dir, "loop-harness")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	g.r.Cfg.LoopHarness = bin
	g.t.Cleanup(g.r.CloseHarness)
	return home
}

// piSession opens a build session of item id with the given resources, in a worktree of its own.
func (g *rig) piSession(id int64, resources ...string) (*harnessSession, string) {
	g.t.Helper()
	wt := filepath.Join(g.r.Cfg.WorkRoot, filepathBase(id))
	os.MkdirAll(filepath.Join(wt, ".loop", "scratch"), 0o755)
	ru := &run{item: Item{ID: id, Resources: resources}, stage: StageBuild, repoKey: "demo", repo: g.r.Cfg.Repos["demo"], wt: wt,
		scratch: filepath.Join(wt, ".loop", "scratch"), sandbox: "loop-x", dir: g.runGuard(wt)}
	s, err := g.r.startHarness(ru)
	if err != nil {
		g.t.Fatal(err)
	}
	return s, wt
}

func filepathBase(id int64) string { return "pi-" + string(rune('0'+id)) + "-build" }

func piCall(t *testing.T, wt string, req harness.Request) (int, string, error) {
	t.Helper()
	f, err := harness.Find(wt)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := harness.Call(f, req, &out, nil)
	return code, out.String(), err
}

// ssh's command line is fixed by the runner: no config file, only the loop's key, a pinned host
// key, no agent and no forwarding, the host after "--".
func TestPiSSHArgs(t *testing.T) {
	p := &PiConfig{Host: "jarvis-pi4.local", User: "pi", Port: 22, Key: "/k/id", KnownHosts: "/k/known"}
	got := p.sshArgs("uname -a")
	want := []string{"-F", "/dev/null", "-i", "/k/id", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=/k/known", "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		"-p", "22", "-l", "pi", "--", "jarvis-pi4.local", "uname -a"}
	if !slices.Equal(got, want) {
		t.Fatalf("ssh args:\n got %q\nwant %q", got, want)
	}
}

// The config refuses a Pi it cannot use safely.
func TestPiConfig(t *testing.T) {
	dir := t.TempDir()
	key, loose, known := filepath.Join(dir, "id"), filepath.Join(dir, "loose"), filepath.Join(dir, "known")
	os.WriteFile(key, []byte("k"), 0o600)
	os.WriteFile(loose, []byte("k"), 0o644)
	os.WriteFile(known, []byte("h"), 0o644)
	good := func() *PiConfig {
		return &PiConfig{Host: "jarvis-pi4.local", User: "pi", Key: key, KnownHosts: known, Restore: "sudo systemctl start jarvis-node"}
	}
	caps := []string{"gpu", "pi"}
	if p := good(); p.normalize(caps) != nil || p.Port != 22 || p.SSH != "ssh" || p.Timeout() != DefaultVerifyTimeout {
		t.Fatalf("a good config: %+v", p)
	}
	for name, c := range map[string]struct {
		mut  func(*PiConfig)
		caps []string
		want string
	}{
		"no capability":     {func(*PiConfig) {}, []string{"gpu"}, "capabilities"},
		"an option as host": {func(p *PiConfig) { p.Host = "-oProxyCommand=sh" }, caps, "host"},
		"two hosts":         {func(p *PiConfig) { p.Host = "a b" }, caps, "host"},
		"a bad user":        {func(p *PiConfig) { p.User = "pi;id" }, caps, "user"},
		"a readable key":    {func(p *PiConfig) { p.Key = loose }, caps, "0600"},
		"no key":            {func(p *PiConfig) { p.Key = filepath.Join(dir, "none") }, caps, "pi.key"},
		"no known_hosts":    {func(p *PiConfig) { p.KnownHosts = filepath.Join(dir, "none") }, caps, "known_hosts"},
		"no restore":        {func(p *PiConfig) { p.Restore = " " }, caps, "restore"},
		"a bad timeout":     {func(p *PiConfig) { p.TimeoutText = "soon" }, caps, "timeout"},
		"a bad port":        {func(p *PiConfig) { p.Port = 70000 }, caps, "port"},
	} {
		p := good()
		c.mut(p)
		if err := p.normalize(c.caps); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The key and known_hosts are secrets to agents and verify steps.
	cfg := &Config{Pi: good()}
	cfg.Pi.normalize(caps)
	if s := cfg.Secrets(); !slices.Contains(s, key) || !slices.Contains(s, known) {
		t.Fatalf("secrets %v", s)
	}
}

// An item that needs the Pi: the Pi is prepared once (its normal state, a fresh scratch dir),
// commands run in the item's scratch dir there, files go both ways inside it and the worktree,
// paths out of either are refused, and at the end the Pi is put back and freed.
func TestPiSession(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	home := g.piRig()
	os.MkdirAll(filepath.Join(home, PiScratch, "old"), 0o755) // left by a crashed runner
	s, wt := g.piSession(5, ResourcePi)

	code, out, err := piCall(t, wt, harness.Request{Op: harness.OpPi, Cmd: `pwd; echo hi > made.txt; exit 3`})
	if err != nil || code != 3 || !strings.Contains(out, "preparing the Pi") || !strings.HasSuffix(strings.TrimSpace(out), "/home/"+PiScratch+"/5") {
		t.Fatalf("pi: %d %q %v", code, out, err)
	}
	if _, err := os.Stat(filepath.Join(home, PiScratch, "old")); !os.IsNotExist(err) {
		t.Fatal("a crashed run's scratch survived the preparation")
	}
	if code, out, _ := piCall(t, wt, harness.Request{Op: harness.OpPi, Cmd: "true"}); code != 0 || strings.Contains(out, "preparing") {
		t.Fatalf("prepared twice: %q", out)
	}

	os.WriteFile(filepath.Join(wt, "agent.bin"), []byte("ELF"), 0o755)
	if code, out, err := piCall(t, wt, harness.Request{Op: harness.OpPiPut, Src: "agent.bin", Dst: "bin/jarvis-agent"}); code != 0 || err != nil {
		t.Fatalf("pi-put: %d %q %v", code, out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, PiScratch, "5", "bin", "jarvis-agent")); string(b) != "ELF" {
		t.Fatalf("on the Pi: %q", b)
	}
	if code, out, err := piCall(t, wt, harness.Request{Op: harness.OpPiGet, Src: "made.txt", Dst: ".loop/evidence/made.txt"}); code != 0 || err != nil {
		t.Fatalf("pi-get: %d %q %v", code, out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, ".loop", "evidence", "made.txt")); string(b) != "hi\n" {
		t.Fatalf("fetched %q", b)
	}
	// A missing file: cat's message goes to the output, no file is left in the worktree.
	if code, out, _ := piCall(t, wt, harness.Request{Op: harness.OpPiGet, Src: "nope.txt", Dst: "nope.txt"}); code == 0 || !strings.Contains(out, "nope.txt") {
		t.Fatalf("pi-get of a missing file: %d %q", code, out)
	}
	if left, _ := filepath.Glob(filepath.Join(wt, "nope.txt*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
	// Out of the scratch dir on the Pi, or out of the worktree here.
	os.Symlink("/etc", filepath.Join(wt, "etc-link"))
	for _, req := range []harness.Request{
		{Op: harness.OpPiPut, Src: "agent.bin", Dst: "../escape"},
		{Op: harness.OpPiPut, Src: "agent.bin", Dst: "/tmp/x"},
		{Op: harness.OpPiPut, Src: "../../outside", Dst: "x"},
		{Op: harness.OpPiPut, Src: "etc-link/hostname", Dst: "x"},
		{Op: harness.OpPiGet, Src: "../5/made.txt", Dst: "x"},
		{Op: harness.OpPiGet, Src: "made.txt", Dst: "../outside.txt"},
		{Op: harness.OpPi, Cmd: "  "},
		{Op: harness.OpPi, Cmd: "adb -s emulator-5554 shell"}, // the guard, without the hook
	} {
		if code, _, err := piCall(t, wt, req); code != harness.ExitBroker || err == nil {
			t.Errorf("%+v: %d %v", req, code, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, PiScratch, "escape")); !os.IsNotExist(err) {
		t.Fatal("pi-put wrote outside the scratch dir")
	}

	// The end: the item's scratch dir goes, the restore runs again, the Pi is free.
	s.end(context.Background())
	if b, _ := os.ReadFile(filepath.Join(home, "restore.log")); string(b) != "restored\nrestored\n" {
		t.Fatalf("restores: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, PiScratch, "5")); !os.IsNotExist(err) {
		t.Fatal("the item's scratch dir survived the run")
	}
	if g.r.broker().piHolder != 0 {
		t.Fatal("the Pi is still held")
	}
	if log := g.read(filepath.Join(home, "..", "calls.log")); strings.Contains(log, "agent.sock") {
		t.Fatalf("the agent socket reached ssh:\n%s", log)
	}
}

// A failed preparation does not keep the Pi: the item can try again, and others get it after.
func TestPiPrepareFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	home := g.piRig()
	g.r.Cfg.Pi.Restore = "test -e ok"
	a, wta := g.piSession(5, ResourcePi)
	if _, _, err := piCall(t, wta, harness.Request{Op: harness.OpPi, Cmd: "true"}); err == nil || !strings.Contains(err.Error(), "preparing the Pi failed") {
		t.Fatalf("a failing restore: %v", err)
	}
	os.WriteFile(filepath.Join(home, "ok"), nil, 0o644)
	if code, out, err := piCall(t, wta, harness.Request{Op: harness.OpPi, Cmd: "true"}); code != 0 || err != nil || !strings.Contains(out, "preparing") {
		t.Fatalf("the retry: %d %q %v", code, out, err)
	}
	a.end(context.Background())
	_, wtb := g.piSession(6, ResourcePi)
	if code, _, err := piCall(t, wtb, harness.Request{Op: harness.OpPi, Cmd: "true"}); code != 0 || err != nil {
		t.Fatalf("the next item: %d %v", code, err)
	}
	os.Remove(filepath.Join(home, "ok"))
	c, wtc := g.piSession(7, ResourcePi)
	piCall(t, wtc, harness.Request{Op: harness.OpPi, Cmd: "true"})
	c.end(context.Background())
	if g.r.broker().piHolder != 6 {
		t.Fatalf("holder %d, want 6 (item 7 never got it)", g.r.broker().piHolder)
	}
}

// One item at a time: a second item is refused while the first holds the Pi, and gets it once
// the first run ends. A session that never used the Pi neither takes it nor restores it.
func TestPiOneItemAtATime(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	home := g.piRig()
	idle, _ := g.piSession(4, ResourcePi)
	a, wta := g.piSession(5, ResourcePi)
	_, wtb := g.piSession(6, ResourcePi)
	if code, _, err := piCall(t, wta, harness.Request{Op: harness.OpPi, Cmd: "true"}); code != 0 || err != nil {
		t.Fatalf("first: %d %v", code, err)
	}
	if _, _, err := piCall(t, wtb, harness.Request{Op: harness.OpPi, Cmd: "true"}); err == nil || !strings.Contains(err.Error(), "in use by item #5") {
		t.Fatalf("second while held: %v", err)
	}
	idle.end(context.Background())
	if b, _ := os.ReadFile(filepath.Join(home, "restore.log")); string(b) != "restored\n" {
		t.Fatalf("an idle session restored the Pi: %q", b)
	}
	a.end(context.Background())
	if code, _, err := piCall(t, wtb, harness.Request{Op: harness.OpPi, Cmd: "true"}); code != 0 || err != nil {
		t.Fatalf("second after release: %d %v", code, err)
	}
}

// Only an item that needs the Pi gets it; a Pi-only session has no harness up/run/down; a repo
// with a harness and an item without the resource has no Pi.
func TestPiOnlyForItsItems(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.piRig()
	if s, _ := g.piSession(5); s != nil {
		t.Fatal("a session for an item that needs nothing (and a repo with no harness)")
	}
	_, wt := g.piSession(6, ResourcePi)
	for _, op := range []string{harness.OpUp, harness.OpRun, harness.OpDown} {
		if _, _, err := piCall(t, wt, harness.Request{Op: op, Cmd: "true"}); err == nil || !strings.Contains(err.Error(), "no harness") {
			t.Errorf("%s in a Pi-only session: %v", op, err)
		}
	}
	g.harnessRepo("")
	_, wt = g.piSession(7)
	if _, _, err := piCall(t, wt, harness.Request{Op: harness.OpPi, Cmd: "true"}); err == nil || !strings.Contains(err.Error(), "no test Pi") {
		t.Fatalf("pi without the resource: %v", err)
	}
}

// The agent's brief says how to use the Pi, its own ssh stays denied, and the guard lets the
// Pi's commands through loop-harness (they run on the Pi) but not ssh itself.
func TestPiBriefAndGuard(t *testing.T) {
	p := &PiConfig{Restore: "sudo systemctl start jarvis-node"}
	b := Brief(StageBuild, Item{ID: 5, Title: "t"}, RunInfo{Pi: p, HarnessBin: "/w/bin/loop-harness"})
	for _, want := range []string{"## The test Pi", "loop-harness pi '<command>'", "pi-put", "pi-get", "sudo systemctl start jarvis-node"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if !slices.Contains(DenyRules(SettingsInput{Harness: true}), "Bash(ssh *)") {
		t.Fatal("ssh is no longer denied")
	}
	gc := &guard.Config{Item: 5, Worktree: "/w", Home: "/home/u", Words: []string{"emulator-5554"}}
	check := func(cmd string) bool {
		return gc.Check(guard.Input{Event: "PreToolUse", ToolName: "Bash", ToolInput: map[string]any{"command": cmd}, Cwd: "/w"}).Deny
	}
	if check(`loop-harness pi 'sudo systemctl stop jarvis-node && ./bin/jarvis-agent run --profile pi'`) {
		t.Fatal("a Pi command was refused")
	}
	for _, cmd := range []string{"ssh pi@jarvis-pi4.local true", "scp x pi@jarvis-pi4.local:", "loop-harness run 'ssh pi@x true'",
		"loop-harness pi 'adb -s emulator-5554 shell'"} {
		if !check(cmd) {
			t.Errorf("%q was let through", cmd)
		}
	}
}
