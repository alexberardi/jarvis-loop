package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, c *Config) string {
	t.Helper()
	dir := t.TempDir()
	c.Report = filepath.Join(dir, "refusals.jsonl")
	c.Armed = filepath.Join(dir, "armed")
	b, _ := json.Marshal(c)
	p := filepath.Join(dir, "guard.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var at = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func TestHookDenies(t *testing.T) {
	f := newFixture(t)
	p := writeConfig(t, f.cfg)
	in := `{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"` + f.wt + `","tool_input":{"command":"gh pr merge 3"}}`
	var out, errb bytes.Buffer
	code := Hook(p, strings.NewReader(in), &out, &errb, func() time.Time { return at })
	if code != 2 {
		t.Fatalf("exit %d, want 2 (a block that JSON cannot undo)", code)
	}
	var resp struct {
		HSO struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	if resp.HSO.Event != "PreToolUse" || resp.HSO.Decision != "deny" || !strings.Contains(resp.HSO.Reason, "merge") {
		t.Fatalf("got %+v", resp)
	}
	if !strings.Contains(errb.String(), "merge") {
		t.Fatalf("stderr %q should carry the reason", errb.String())
	}
	b, err := os.ReadFile(f.cfg.Report)
	if err != nil {
		t.Fatal(err)
	}
	var r Refusal
	if err := json.Unmarshal(bytes.TrimSpace(b), &r); err != nil {
		t.Fatal(err)
	}
	if r.Rule != RuleGH || r.Tool != "Bash" || !r.At.Equal(at) || r.Target != "gh pr merge 3" {
		t.Fatalf("report %+v", r)
	}
	if _, err := os.Stat(f.cfg.Armed); err != nil {
		t.Fatal("not armed:", err)
	}
}

func TestHookDefers(t *testing.T) {
	f := newFixture(t)
	p := writeConfig(t, f.cfg)
	for _, in := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"` + f.wt + `","tool_input":{"command":"go test ./..."}}`,
		`{"hook_event_name":"SessionStart","source":"startup"}`,
	} {
		var out, errb bytes.Buffer
		if code := Hook(p, strings.NewReader(in), &out, &errb, time.Now); code != 0 || out.Len() != 0 {
			t.Fatalf("%s: exit %d, stdout %q: an allowed call prints nothing (never \"allow\", which would skip the classifier)", in, code, out.String())
		}
	}
	if _, err := os.Stat(f.cfg.Armed); err != nil {
		t.Fatal("not armed:", err)
	}
	if _, err := os.Stat(f.cfg.Report); !os.IsNotExist(err) {
		t.Fatal("nothing was refused, so no report", err)
	}
}

func TestHookFailsClosed(t *testing.T) {
	f := newFixture(t)
	p := writeConfig(t, f.cfg)
	cases := map[string]struct{ cfg, in string }{
		"missing config":  {filepath.Join(t.TempDir(), "nope.json"), `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`},
		"bad stdin":       {p, `{not json`},
		"config not json": {"", `{"hook_event_name":"PreToolUse"}`},
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	for name, c := range cases {
		cfg := c.cfg
		if cfg == "" {
			cfg = bad
		}
		var out, errb bytes.Buffer
		if code := Hook(cfg, strings.NewReader(c.in), &out, &errb, time.Now); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestConfigPath(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if p, err := ConfigPath([]string{"--config", "/a.json"}, env("")); err != nil || p != "/a.json" {
		t.Fatal(p, err)
	}
	if p, err := ConfigPath(nil, env("/run/guard.json")); err != nil || p != "/run/guard.json" {
		t.Fatal(p, err)
	}
	for _, args := range [][]string{nil, {"--config"}, {"x"}} {
		if _, err := ConfigPath(args, env("")); err == nil {
			t.Errorf("%q with no env should fail (and the hook then refuses)", args)
		}
	}
}
