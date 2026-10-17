package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const cfgAgentSandbox = `{"loopd_url":"http://127.0.0.1:7780","token":"runner-token-0123456789","name":"devbox",
 "repos":{"jarvis-server":{"remote":"https://github.com/alexberardi/jarvis-server.git","slug":"alexberardi/jarvis-server",
  "agent_sandbox":{"up":"scripts/sandbox up --name {sandbox}","down":"scripts/sandbox down --name {sandbox} --purge",
   "env_file":".sandbox/{sandbox}/sandbox.env","up_timeout":"5m",
   "requests":{"mobile-shot":"scripts/mobile-shot --sandbox {sandbox} --screens \"$LOOP_REQ_SCREENS\""}}}}}`

// A repo opts in to the agent sandbox with up and env_file; the rest is optional and checked.
func TestConfigAgentSandbox(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, cfgAgentSandbox, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Repos["jarvis-server"].AgentSandbox
	if a == nil || a.EnvFile != ".sandbox/{sandbox}/sandbox.env" || a.UpTimeout().String() != "5m0s" || len(a.Requests) != 1 {
		t.Fatalf("agent sandbox %+v", a)
	}
	if (AgentSandbox{}).UpTimeout() != DefaultUpTimeout {
		t.Fatal("default up timeout")
	}
	for name, body := range map[string]string{
		"no up":             strings.Replace(cfgAgentSandbox, `"up":"scripts/sandbox up --name {sandbox}",`, ``, 1),
		"no env file":       strings.Replace(cfgAgentSandbox, `"env_file":".sandbox/{sandbox}/sandbox.env",`, ``, 1),
		"absolute env file": strings.Replace(cfgAgentSandbox, `".sandbox/{sandbox}/sandbox.env"`, `"/tmp/sandbox.env"`, 1),
		"env file above":    strings.Replace(cfgAgentSandbox, `".sandbox/{sandbox}/sandbox.env"`, `"../x/sandbox.env"`, 1),
		"bad up timeout":    strings.Replace(cfgAgentSandbox, `"5m"`, `"soon"`, 1),
		"bad request name":  strings.Replace(cfgAgentSandbox, `"mobile-shot":`, `"Mobile Shot":`, 1),
		"empty request":     strings.Replace(cfgAgentSandbox, `"scripts/mobile-shot --sandbox {sandbox} --screens \"$LOOP_REQ_SCREENS\""`, `" "`, 1),
	} {
		if _, err := LoadConfig(writeCfg(t, body, 0o600)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// freePort is a loopback port nothing listens on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// sandboxRepo gives demo an agent sandbox whose up starts a server on port in the background and
// exits (the fence's pid namespace must not take the server with it), writes an env file, and,
// when leak is set, puts the prod key in it.
func (g *rig) sandboxRepo(home, port string, leak bool) {
	g.t.Helper()
	prodEnv := shellQuote(filepath.Join(home, ".config", "jarvis-harness", "prod-background.env"))
	extra := ""
	if leak {
		extra = `grep '^PROD_APP_KEY=' ` + prodEnv + ` | sed 's/^/export /' >> "$d/sandbox.env"`
	}
	rc := g.r.Cfg.Repos["demo"]
	rc.AgentSandbox = &AgentSandbox{
		Up: `set -e
d=.sandbox/{sandbox}; mkdir -p "$d/www"; printf "*\n" > "$d/.gitignore"
echo HELLO-FROM-SANDBOX > "$d/www/hello.txt"
grep -q '^PROD_APP_KEY=' ` + prodEnv + ` && echo "up: the harness's prod env is readable"
python3 -m http.server ` + port + ` --bind 127.0.0.1 --directory "$d/www" >/dev/null 2>&1 &
for i in $(seq 100); do (exec 3<>/dev/tcp/127.0.0.1/` + port + `) 2>/dev/null && break; sleep 0.05; done
printf 'export SBX_URL=http://127.0.0.1:` + port + `\nexport SBX_TOKEN=sandbox-local-token\n' > "$d/sandbox.env"
` + extra,
		Down:    `echo "down ran for $LOOP_SANDBOX"`,
		EnvFile: ".sandbox/{sandbox}/sandbox.env",
		Requests: map[string]string{"mobile-shot": `printf 'PNG' > "$LOOP_EVIDENCE_DIR/home.png"
echo "shot $LOOP_REQ_SCREENS at $LOOP_HEAD in $LOOP_SANDBOX"
cat ` + prodEnv},
		SecretEnvFiles: []string{filepath.Join(home, ".config", "jarvis-harness", "prod-background.env")},
	}
	g.r.Cfg.Repos["demo"] = rc
}

func (g *rig) evidenceBody(id int64, name string) string {
	g.t.Helper()
	for _, ev := range g.item(id).Evidence {
		if ev.Name == name {
			_, f, err := g.st.OpenEvidence(g.ctx, id, ev.N)
			if err != nil {
				g.t.Fatal(err)
			}
			defer f.Close()
			b, _ := io.ReadAll(f)
			return string(b)
		}
	}
	g.t.Fatalf("no evidence %s; log:\n%s", name, g.logs.String())
	return ""
}

// A repo with agent_sandbox: the runner brings the item's sandbox up under the fence before the
// build agent (with the harness's prod env readable to up), keeps it serving for the whole run
// although up exited, gives the agent loopback to its port and an env file without the prod key,
// runs the agent's mobile-shot request (output redacted, PNG in its evidence), and takes the
// sandbox down after. The prod key reaches nothing the agent sees.
func TestAgentSandbox(t *testing.T) {
	g := newRig(t, fakeclaude.UseSandbox)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	home := g.fakeHome()
	port := freePort(t)
	g.sandboxRepo(home, port, false)
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if got := g.evidenceBody(id, "sandbox-reply.txt"); got != "HELLO-FROM-SANDBOX\n" {
		t.Errorf("the agent's request to the sandbox got %q", got)
	}
	if got := g.evidenceBody(id, "shot-1-home.png"); got != "PNG" {
		t.Errorf("shot %q", got)
	}
	var done AgentRequestDone
	if err := json.Unmarshal([]byte(g.evidenceBody(id, "shot-1-done.json")), &done); err != nil {
		t.Fatal(err)
	}
	head := g.remoteHead(ItemBranchName(id, "Add a feature"))
	if done.ExitCode != 0 || done.Error != "" || !slices.Equal(done.Files, []string{"evidence/shot-1-home.png"}) ||
		!strings.Contains(done.Log, "shot home at "+head+" in loop-"+strconv.FormatInt(id, 10)) || !strings.Contains(done.Log, "PROD_APP_KEY=[redacted]") {
		t.Errorf("the shot's answer %+v (head %s)", done, head)
	}
	run := g.lastRun(id, StageBuild)
	log := g.read(filepath.Join(run, "agent-sandbox.log"))
	for _, want := range []string{"up: the harness's prod env is readable", "loopback ports [" + port + "]", "down ran for loop-", "[down]"} {
		if !strings.Contains(log, want) {
			t.Errorf("agent-sandbox.log lacks %q:\n%s", want, log)
		}
	}
	// The agent's settings: loopback to the sandbox's port, the harness dir unreadable, loopd denied.
	b, err := os.ReadFile(filepath.Join(run, "agent-sandbox", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead []string `json:"denyRead"`
			} `json:"filesystem"`
			Network struct {
				Allowed []string `json:"allowedDomains"`
				Denied  []string `json:"deniedDomains"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	json.Unmarshal(b, &s)
	if !slices.Contains(s.Sandbox.Network.Allowed, "127.0.0.1:"+port) || !slices.Contains(s.Sandbox.Network.Allowed, "localhost:"+port) ||
		!slices.Contains(s.Sandbox.Filesystem.DenyRead, filepath.Join(Home(), ".config", "jarvis-harness")) || len(s.Sandbox.Network.Denied) == 0 {
		t.Errorf("settings %+v", s.Sandbox)
	}
	if a := g.read(filepath.Join(run, "argv.json")); !strings.Contains(a, fmt.Sprintf("demo-build-loop-%d.json", id)) {
		t.Errorf("argv %s", a)
	}
	// Nothing the agent saw holds the prod key: its brief, settings, transcript and evidence.
	seen := g.read(filepath.Join(run, "brief.md")) + string(b) + g.read(filepath.Join(run, "stream.jsonl"))
	for _, ev := range g.item(id).Evidence {
		seen += g.evidenceBody(id, ev.Name)
	}
	if strings.Contains(seen, prodKey) || strings.Contains(seen, "SECRET") {
		t.Error("the prod key reached the agent")
	}
	if !strings.Contains(g.read(filepath.Join(run, "brief.md")), fmt.Sprintf("`.sandbox/loop-%d/sandbox.env` holds its local URLs", id)) {
		t.Error("the brief does not name the env file")
	}
	// Down: the server is gone.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Error("the sandbox still serves after the run")
	}
	g.cleanedUp(id)
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, "settings", fmt.Sprintf("demo-build-loop-%d.json", id))); err == nil {
		t.Error("the item's settings file was left behind")
	}
}

// An env file that holds a secret from the harness's env files fails the run before the agent
// starts, without the secret in the message; the sandbox is taken down.
func TestAgentSandboxRefusesASecretInTheEnvFile(t *testing.T) {
	g := newRig(t, fakeclaude.UseSandbox)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	home := g.fakeHome()
	port := freePort(t)
	g.sandboxRepo(home, port, true)
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "holds a secret") || strings.Contains(d.Item.Question, prodKey) {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	run := g.lastRun(id, StageBuild)
	if _, err := os.Stat(filepath.Join(run, "stream.jsonl")); err == nil {
		t.Error("the agent ran")
	}
	if !strings.Contains(g.read(filepath.Join(run, "agent-sandbox.log")), "down ran") {
		t.Error("down did not run")
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Error("the sandbox still serves")
	}
}

// An up that fails fails the run with its log's tail; the agent never starts.
func TestAgentSandboxUpFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	rc := g.r.Cfg.Repos["demo"]
	rc.AgentSandbox = &AgentSandbox{Up: "echo no free port offset; exit 3", Down: "echo down ran", EnvFile: "sandbox.env", SecretEnvFiles: []string{}}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "up failed (exit 3)") || !strings.Contains(d.Item.Question, "no free port offset") {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	if _, err := os.Stat(filepath.Join(g.lastRun(id, StageBuild), "stream.jsonl")); err == nil {
		t.Error("the agent ran")
	}
}

// The env file's loopback ports, without loopd's.
func TestLoopbackPorts(t *testing.T) {
	env := "export SBX_AUTH=http://127.0.0.1:10701\nexport SBX_MQTT=127.0.0.1:11884\nX=http://localhost:10701/x\nY=http://[::1]:10799\nL=http://127.0.0.1:7780\nZ=http://example.com:80\n"
	if got := loopbackPorts(env, []string{"127.0.0.1:7780", "localhost:7780"}); !slices.Equal(got, []int{10701, 10799, 11884}) {
		t.Fatalf("ports %v", got)
	}
}
