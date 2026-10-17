package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

const cfgBody = `{"loopd_url":"http://127.0.0.1:7780","token":"runner-token-0123456789","name":"devbox",
 "capabilities":["gpu"],"repos":{"jarvis-server":{"local":"~/jarvis/jarvis-server","remote":"https://github.com/alexberardi/jarvis-server.git","slug":"alexberardi/jarvis-server"}}}`

func TestConfig(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, cfgBody, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeAuto || c.Slots != 1 || c.Repos["jarvis-server"].Base != "main" || !*c.VerifyPR ||
		c.WorkRoot != filepath.Join(Home(), ".cache", "jarvis-loop", "work") || c.UserEmulator != "emulator-5554" {
		t.Fatalf("defaults %+v", c)
	}
	if !slices.Contains(c.Protected(), filepath.Join(Home(), "jarvis", "jarvis-server")) || !slices.Contains(c.Secrets(), c.Path()) {
		t.Fatalf("protected %v secrets %v", c.Protected(), c.Secrets())
	}
	if got := c.LoopdAddrs(); !slices.Equal(got, []string{"127.0.0.1:7780", "localhost:7780", "[::1]:7780", "0.0.0.0:7780"}) {
		t.Fatalf("addrs %v", got)
	}
	// Plan turns are opt-in: not in the default stages; 15 minutes a turn.
	if slices.Contains(c.Stages, StagePlan) || c.PlanTurnMinutes != 15 {
		t.Fatalf("stages %v, plan_turn_minutes %d", c.Stages, c.PlanTurnMinutes)
	}
	p, err := LoadConfig(writeCfg(t, strings.Replace(cfgBody, `"name"`, `"stages":["build","plan"],"plan_turn_minutes":5,"name"`, 1), 0o600))
	if err != nil || !slices.Equal(p.Stages, []string{"build", "plan"}) || p.PlanTurnMinutes != 5 {
		t.Fatalf("plan stage: %+v %v", p, err)
	}
}

func TestConfigRefusals(t *testing.T) {
	if _, err := LoadConfig(writeCfg(t, cfgBody, 0o644)); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("a readable config must be refused: %v", err)
	}
	for name, body := range map[string]string{
		"unknown field": strings.Replace(cfgBody, `"name"`, `"nmae"`, 1),
		"short token":   strings.Replace(cfgBody, "runner-token-0123456789", "x", 1),
		"bad mode":      strings.Replace(cfgBody, `"name"`, `"mode":"bypass","name"`, 1),
		"no slug":       strings.Replace(cfgBody, `,"slug":"alexberardi/jarvis-server"`, ``, 1),
		"bad plan turn": strings.Replace(cfgBody, `"name"`, `"plan_turn_minutes":-1,"name"`, 1),
		"bad url":       strings.Replace(cfgBody, "http://127.0.0.1:7780", "127.0.0.1:7780", 1),
	} {
		if _, err := LoadConfig(writeCfg(t, body, 0o600)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// loopd stores the PR's repo (owner/name) on the item at pr_opened, so an item bounced back to
// build carries the slug, not the config key.
func TestRepoBySlugOrKey(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, cfgBody, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jarvis-server", "alexberardi/jarvis-server"} {
		if key, r, ok := c.Repo(name); !ok || key != "jarvis-server" || r.Slug != "alexberardi/jarvis-server" {
			t.Errorf("%s: %q %+v %v", name, key, r, ok)
		}
	}
	if _, _, ok := c.Repo("evil/fork"); ok {
		t.Error("an unknown repo matched")
	}
}

// The repos of phase 6b (docs/phases/06b-all-repos.md): group, aliases, setup and verify steps.
const cfgAllRepos = `{"loopd_url":"http://127.0.0.1:7780","token":"runner-token-0123456789","name":"devbox",
 "repos":{
  "jarvis-server":{"remote":"https://github.com/alexberardi/jarvis-server.git","slug":"alexberardi/jarvis-server","group":"server",
   "verify":[{"name":"pipeline scenarios","run":"scripts/sandbox up --name loop-$LOOP_ITEM && go run ./tools/pipeline run"},
             {"name":"admin screenshots","run":"scripts/screenshots --sandbox loop-$LOOP_ITEM","when_changed":["web/**"],"timeout":"15m"}]},
  "jarvis-node-mobile":{"remote":"https://github.com/alexberardi/jarvis-node-mobile.git","slug":"alexberardi/jarvis-node-mobile",
   "group":"mobile","aliases":["mobile","node app"],"description":"the mobile app",
   "setup":["mise exec node@22 -- npm ci --ignore-scripts"],
   "verify":[{"name":"mobile shots","repo":"jarvis-server","run":"scripts/mobile-shot --sandbox loop-$LOOP_ITEM --ref $LOOP_HEAD"}]}}}`

func TestConfigAllRepos(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, cfgAllRepos, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Repos["jarvis-node-mobile"]
	if m.Group != "mobile" || !slices.Equal(m.Aliases, []string{"mobile", "node app"}) || len(m.Setup) != 1 || len(m.Verify) != 1 {
		t.Fatalf("node-mobile %+v", m)
	}
	if v := m.Verify[0]; v.Repo != "jarvis-server" || v.Timeout() != DefaultVerifyTimeout {
		t.Fatalf("verify %+v (timeout %s)", v, v.Timeout())
	}
	if v := c.Repos["jarvis-server"].Verify[1]; v.Timeout().String() != "15m0s" || !slices.Equal(v.WhenChanged, []string{"web/**"}) {
		t.Fatalf("screenshots %+v", v)
	}
	if c.VerifyLock != filepath.Join(Home(), ".cache", "jarvis-loop", "verify.lock") {
		t.Fatalf("verify lock %q", c.VerifyLock)
	}
	// A repo is found by an alias too (the words the user types).
	if key, _, ok := c.Repo("node app"); !ok || key != "jarvis-node-mobile" {
		t.Fatalf("alias: %q %v", key, ok)
	}
	for name, body := range map[string]string{
		"unknown group":          strings.Replace(cfgAllRepos, `"group":"mobile"`, `"group":"phone"`, 1),
		"verify without run":     strings.Replace(cfgAllRepos, `,"run":"scripts/mobile-shot --sandbox loop-$LOOP_ITEM --ref $LOOP_HEAD"`, ``, 1),
		"verify without name":    strings.Replace(cfgAllRepos, `"name":"mobile shots",`, ``, 1),
		"verify in unknown repo": strings.Replace(cfgAllRepos, `"repo":"jarvis-server","run"`, `"repo":"nowhere","run"`, 1),
		"bad verify timeout":     strings.Replace(cfgAllRepos, `"timeout":"15m"`, `"timeout":"soon"`, 1),
		"bad when_changed glob":  strings.Replace(cfgAllRepos, `"web/**"`, `"/abs/**"`, 1),
		"alias names two repos":  strings.Replace(cfgAllRepos, `"group":"server",`, `"group":"server","aliases":["mobile"],`, 1),
		"alias is another's key": strings.Replace(cfgAllRepos, `"node app"]`, `"node app","jarvis-server"]`, 1),
		"empty setup command":    strings.Replace(cfgAllRepos, `"mise exec node@22 -- npm ci --ignore-scripts"`, `" "`, 1),
	} {
		if _, err := LoadConfig(writeCfg(t, body, 0o600)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A repo's harness (harness.go): up and down are required, the timeout defaults to 20 minutes.
func TestConfigHarness(t *testing.T) {
	withHarness := func(h string) string {
		return strings.Replace(cfgBody, `"slug":"alexberardi/jarvis-server"`, `"slug":"alexberardi/jarvis-server","harness":`+h, 1)
	}
	c, err := LoadConfig(writeCfg(t, withHarness(`{"up":"scripts/sandbox up --name $LOOP_SANDBOX","down":"scripts/sandbox down --name $LOOP_SANDBOX --purge"}`), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Repos["jarvis-server"].Harness; h == nil || h.Timeout() != 20*time.Minute || !strings.HasPrefix(h.Up, "scripts/sandbox up") {
		t.Fatalf("harness %+v", h)
	}
	if c.LoopHarness == "" || filepath.Base(c.LoopHarness) != "loop-harness" {
		t.Fatalf("loop_harness default %q", c.LoopHarness)
	}
	c, err = LoadConfig(writeCfg(t, withHarness(`{"up":"u","down":"d","timeout":"90s"}`), 0o600))
	if err != nil || c.Repos["jarvis-server"].Harness.Timeout() != 90*time.Second {
		t.Fatalf("timeout: %v", err)
	}
	for name, h := range map[string]string{
		"no down":      `{"up":"u"}`,
		"no up":        `{"down":"d"}`,
		"bad timeout":  `{"up":"u","down":"d","timeout":"soon"}`,
		"unknown key":  `{"up":"u","down":"d","stop":"x"}`,
		"zero timeout": `{"up":"u","down":"d","timeout":"0s"}`,
	} {
		if _, err := LoadConfig(writeCfg(t, withHarness(h), 0o600)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// An item added with a repo's bare name (--repo jarvis-server) resolves by the name part of the
// slug, whatever the config key; a name two owners share resolves to neither.
func TestRepoByBareName(t *testing.T) {
	c := &Config{Repos: map[string]RepoConfig{
		"alexberardi-jarvis-server": {Slug: "alexberardi/jarvis-server"},
		"mine":                      {Slug: "alexberardi/fork"},
		"theirs":                    {Slug: "someone/fork"},
	}}
	for _, name := range []string{"jarvis-server", "Jarvis-Server", " jarvis-server "} {
		if key, _, ok := c.Repo(name); !ok || key != "alexberardi-jarvis-server" {
			t.Errorf("%q: %q %v", name, key, ok)
		}
	}
	if key, _, ok := c.Repo("fork"); ok {
		t.Errorf("an ambiguous name resolved to %q", key)
	}
	if key, _, ok := c.Repo("someone/fork"); !ok || key != "theirs" {
		t.Errorf("slug: %q %v", key, ok)
	}
}
