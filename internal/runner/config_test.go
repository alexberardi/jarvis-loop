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
