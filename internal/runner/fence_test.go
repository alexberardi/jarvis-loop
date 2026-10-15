package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// fakeHome makes a home with the secrets the fence must hide, and points the rig's commands at it.
func (g *rig) fakeHome() string {
	g.t.Helper()
	home := filepath.Join(g.t.TempDir(), "home")
	for p, body := range map[string]string{
		".config/gh/hosts.yml":                 "github.com:\n  oauth_token: gho_SECRETPUSHTOKEN\n",
		".ssh/id_ed25519":                      "PRIVATE KEY\n",
		".config/jarvis-loop/tokens":           "loopd-token\trunner\tdevbox\n",
		".config/jarvis-harness/slack.env":     "SLACK_BOT_TOKEN=xoxb-SECRET\n",
		".config/jarvis-harness/installed.env": "INSTALLED_APP_ID=app\n",
		".git-credentials":                     "https://u:SECRET@github.com\n",
		".local/share/jarvis-loop/loop.db":     "db\n",
	} {
		f := filepath.Join(home, p)
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			g.t.Fatal(err)
		}
	}
	cfg := filepath.Join(g.t.TempDir(), "runner.json")
	os.WriteFile(cfg, []byte(`{"token":"runner-token-SECRET"}`), 0o600)
	g.r.Cfg.path = cfg
	g.r.Cfg.VerifyMask = append(DefaultVerifyMask(home), cfg)
	var env []string
	for _, kv := range g.r.Env {
		if !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	g.r.Env = append(env, "HOME="+home, "GH_TOKEN=gh-env-SECRET", "GITHUB_TOKEN=github-env-SECRET", "MY_API_KEY=key-SECRET",
		"AWS_SECRET=aws-SECRET", "SSH_AUTH_SOCK=/run/agent.sock", "GIT_ASKPASS=/bin/askpass", "PLAIN_VAR=kept")
	return home
}

// Setup and verify run the agent's code outside the agent's sandbox, so the runner fences them
// with bubblewrap: the credential files are masked, secrets are not in the environment, git has
// no credential helper, and loopback still works (the harness, the emulator and the LLM tap).
func TestFenceHidesSecrets(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	g.fakeHome()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "LOOPBACK-OK") }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	probe := `for f in ~/.config/gh/hosts.yml ~/.ssh/id_ed25519 ~/.config/jarvis-loop/tokens ~/.config/jarvis-harness/slack.env ~/.git-credentials ~/.local/share/jarvis-loop/loop.db ` +
		g.r.Cfg.Path() + `; do echo "file $f: [$(cat "$f" 2>/dev/null)]"; done
echo "ssh dir: [$(ls -A ~/.ssh 2>/dev/null)]"
echo "installed: [$(cat ~/.config/jarvis-harness/installed.env)]"
echo "env: GH=[$GH_TOKEN] GITHUB=[$GITHUB_TOKEN] KEY=[$MY_API_KEY] AWS=[$AWS_SECRET] SOCK=[$SSH_AUTH_SOCK] ASKPASS=[$GIT_ASKPASS] PLAIN=[$PLAIN_VAR] PROMPT=[$GIT_TERMINAL_PROMPT]"
echo "credential helper: [$(git config --get credential.helper)]"
exec 3<>/dev/tcp/127.0.0.1/` + port + ` && printf 'GET / HTTP/1.0\r\nHost: x\r\n\r\n' >&3 && echo "loopback: $(cat <&3 | tail -1)"`
	g.verifyRepo(VerifyStep{Name: "probe", Run: probe})
	rc := g.r.Cfg.Repos["demo"]
	rc.Setup = []string{probe}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Add a feature")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	run := g.lastRun(id, StageBuild)
	for name, out := range map[string]string{
		"verify": g.read(filepath.Join(run, "verify", "verify-1-probe", "output.log")),
		"setup":  g.read(filepath.Join(run, "setup.log")),
	} {
		out = out[strings.Index(out, "\nfile ")+1:] // setup.log starts with the command itself
		for _, bad := range []string{"SECRET", "PRIVATE KEY", "loopd-token", "/run/agent.sock", "/bin/askpass", "id_ed25519\n", "db]"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s saw %q:\n%s", name, bad, out)
			}
		}
		for _, want := range []string{"ssh dir: []", "installed: [INSTALLED_APP_ID=app]", "PLAIN=[kept]", "PROMPT=[0]",
			"credential helper: []", "loopback: LOOPBACK-OK", "file " + g.r.Cfg.Path() + ": []"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, out)
			}
		}
	}
}

// The environment setup and verify get: no tokens, keys, secrets or credential plumbing.
func TestFencedEnv(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "GH_TOKEN=a", "GITHUB_TOKEN=b", "GH_ENTERPRISE_TOKEN=c", "GIT_ASKPASS=d", "SSH_ASKPASS=d2",
		"SSH_AUTH_SOCK=e", "ANTHROPIC_API_KEY=f", "AWS_SECRET=g", "X_SECRET_KEY=h", "LOOP_TOKEN=i", "SLACK_BOT_TOKEN=j",
		"GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "TOKEN_COUNT=7", "KEYBOARD=us", "GIT_TERMINAL_PROMPT=1"}
	got := fencedEnv(in, "/w/fenced.gitconfig")
	want := []string{"PATH=/bin", "HOME=/h", "TOKEN_COUNT=7", "KEYBOARD=us",
		"GIT_CONFIG_GLOBAL=/w/fenced.gitconfig", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("fencedEnv:\n got %v\nwant %v", got, want)
	}
}

// Without a working bubblewrap, setup and verify never run unfenced: the item goes to the user
// with the reason, and nothing is pushed.
func TestFenceFailsClosed(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.r.Cfg.Bwrap = filepath.Join(t.TempDir(), "no-bwrap")
	marker := filepath.Join(t.TempDir(), "ran")
	g.verifyRepo(VerifyStep{Name: "unit tests", Run: "touch " + marker})
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "bubblewrap") || !strings.Contains(d.Item.Question, "verify") {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("verify ran unfenced")
	}
	if g.prs.calls() != 0 || g.remoteHead(ItemBranchName(id, "Add a feature")) != "" {
		t.Fatal("pushed without a verify")
	}

	g2 := newRig(t, fakeclaude.Good)
	g2.r.Cfg.Bwrap = filepath.Join(t.TempDir(), "no-bwrap")
	rc := g2.r.Cfg.Repos["demo"]
	rc.Setup = []string{"touch " + marker}
	g2.r.Cfg.Repos["demo"] = rc
	id = g2.toBuild("Add a feature")
	g2.runOnce()
	if d := g2.item(id); d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "bubblewrap") {
		t.Fatalf("setup: %s %q", d.Item.Stage, d.Item.Question)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("setup ran unfenced")
	}
}

// The default mask list: the credential stores, loopd's files and the runner's own config; the
// harness's installed.env stays readable (accepted: an app client for the local jarvisd only).
func TestDefaultVerifyMask(t *testing.T) {
	m := DefaultVerifyMask("/h")
	for _, want := range []string{"/h/.ssh", "/h/.config/gh", "/h/.config/jarvis-loop", "/h/.config/jarvis-harness/slack.env",
		"/h/.local/share/jarvis-loop", "/h/.gnupg", "/h/.netrc", "/h/.git-credentials", "/h/.config/git/credentials", "/h/.docker/config.json"} {
		if !slices.Contains(m, want) {
			t.Errorf("default mask lacks %s", want)
		}
	}
	if slices.Contains(m, "/h/.config/jarvis-harness") || slices.Contains(m, "/h/.config/jarvis-harness/installed.env") {
		t.Error("installed.env must stay readable")
	}
	c, err := LoadConfig(writeCfg(t, cfgBody, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.VerifyMask, c.Path()) || !slices.Contains(c.VerifyMask, filepath.Join(Home(), ".ssh")) {
		t.Fatalf("config mask %v", c.VerifyMask)
	}
	if c.Bwrap != "/usr/bin/bwrap" {
		t.Fatalf("bwrap %q", c.Bwrap)
	}
}
