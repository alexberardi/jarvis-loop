package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
