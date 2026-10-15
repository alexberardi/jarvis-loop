package runner

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func goldenInput(mode string) SettingsInput {
	return SettingsInput{
		Mode: mode, Guard: "/opt/loop/loop-guard", Home: "/home/u",
		Protected:  []string{"/home/u/.config/jarvis-harness", "/home/u/.ssh", "/home/u/jarvis/jarvis-server"},
		Secrets:    []string{"/home/u/.config/jarvis-harness", "/home/u/.ssh", "/home/u/.config/jarvis-loop/runner.json"},
		LoopdAddrs: []string{"127.0.0.1:7780", "localhost:7780"},
	}
}

func TestSettingsGolden(t *testing.T) {
	for _, mode := range []string{ModeAuto, ModeStrict} {
		got, err := GenerateSettings(goldenInput(mode))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "settings_"+mode+".golden.json")
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err, "(run go test ./internal/runner -run Golden -update)")
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the generated settings; review and rerun with -update.\ngot:\n%s", path, got)
		}
	}
}

// The parts of the settings each boundary relies on, checked by meaning rather than by bytes.
func TestSettingsShape(t *testing.T) {
	for _, mode := range []string{ModeAuto, ModeStrict} {
		in := goldenInput(mode)
		in.ExcludedCommands = []string{"scripts/sandbox *"}
		b, _ := GenerateSettings(in)
		var s struct {
			Permissions struct {
				DefaultMode   string   `json:"defaultMode"`
				Deny          []string `json:"deny"`
				Allow         []string `json:"allow"`
				DisableBypass string   `json:"disableBypassPermissionsMode"`
			} `json:"permissions"`
			Hooks map[string][]struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"hooks"`
			Sandbox struct {
				Enabled, FailIfUnavailable, AutoAllowBashIfSandboxed, AllowUnsandboxedCommands bool
				ExcludedCommands                                                               []string `json:"excludedCommands"`
				Filesystem                                                                     struct {
					AllowWrite, DenyWrite, DenyRead []string
				}
				Network struct {
					AllowedDomains, DeniedDomains []string
					StrictAllowlist               bool
				}
			} `json:"sandbox"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("%s: settings do not have the expected keys only: %v", mode, err)
		}
		p := s.Permissions
		if p.DefaultMode != PermissionMode(mode) || p.DisableBypass != "disable" {
			t.Errorf("%s: mode %q, bypass %q", mode, p.DefaultMode, p.DisableBypass)
		}
		for _, r := range []string{"Bash(gh pr merge *)", "Bash(gh pr review *)", "Bash(gh repo delete *)", "Bash(gh api *-X DELETE*)",
			"Bash(git push *--force*)", "Bash(git push * main)", "Bash(adb *)", "Bash(sudo *)", "Bash(pkexec *)", "Bash(systemctl *)",
			"Bash(kill *)", "Bash(pkill *)", "Bash(killall *)", "Bash(ssh *)", "Bash(scp *)", "Bash(rm -rf /*)",
			"Bash(curl *127.0.0.1:7780*)", "Bash(wget *localhost:7780*)", "Read(//home/u/.ssh/**)", "Edit(//home/u/jarvis/jarvis-server/**)"} {
			if !slices.Contains(p.Deny, r) {
				t.Errorf("%s: deny rule %s missing", mode, r)
			}
		}
		if (mode == ModeStrict) != (len(p.Allow) > 0) || (mode == ModeStrict && !slices.Contains(p.Allow, "Edit(./**)")) {
			t.Errorf("%s: allow rules %v", mode, p.Allow)
		}
		for _, ev := range []string{"PreToolUse", "SessionStart"} {
			hs := s.Hooks[ev]
			if len(hs) != 1 || len(hs[0].Hooks) != 1 || hs[0].Hooks[0].Command != "'/opt/loop/loop-guard'" {
				t.Errorf("%s: %s hook %+v", mode, ev, hs)
			}
		}
		if s.Hooks["PreToolUse"][0].Matcher != "*" {
			t.Errorf("the guard must see every tool")
		}
		sb := s.Sandbox
		if !sb.Enabled || !sb.FailIfUnavailable || sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands || !sb.Network.StrictAllowlist {
			t.Errorf("%s: sandbox switches %+v", mode, sb)
		}
		if !slices.Contains(sb.Filesystem.DenyRead, "/home/u/.ssh") || !slices.Contains(sb.Filesystem.DenyWrite, "/home/u/.ssh") {
			t.Errorf("%s: filesystem %+v", mode, sb.Filesystem)
		}
		if !slices.Contains(sb.Network.DeniedDomains, "127.0.0.1:7780") || !slices.Contains(sb.Network.AllowedDomains, "github.com") {
			t.Errorf("%s: network %+v", mode, sb.Network)
		}
		if !slices.Equal(sb.ExcludedCommands, []string{"scripts/sandbox *"}) {
			t.Errorf("excluded commands %v", sb.ExcludedCommands)
		}
	}
}

func TestClaudeArgs(t *testing.T) {
	got := ClaudeArgs(AgentArgs{Mode: ModeAuto, Settings: "/r/settings.json"})
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "auto", "--permission-prompts", "none",
		"--settings", "/r/settings.json", "--setting-sources", "user", "--strict-mcp-config", "--exclude-dynamic-system-prompt-sections"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if s := ClaudeArgs(AgentArgs{Mode: ModeStrict, Settings: "x", Model: "opus"}); s[5] != "dontAsk" || !slices.Contains(s, "opus") {
		t.Fatalf("strict %q", s)
	}
	// A planning conversation resumes its session.
	res := ClaudeArgs(AgentArgs{Mode: ModeAuto, Settings: "x", Resume: "5f0c-session"})
	if i := slices.Index(res, "--resume"); i < 0 || res[i+1] != "5f0c-session" || !slices.Equal(res[:len(want)], append(want[:9:9], "x", "--setting-sources", "user", "--strict-mcp-config", "--exclude-dynamic-system-prompt-sections")) {
		t.Fatalf("resume %q", res)
	}
	for _, a := range ClaudeArgs(AgentArgs{Mode: ModeAuto, Settings: "x"}) {
		if a == "--bare" || a == "--dangerously-skip-permissions" || a == "bypassPermissions" {
			t.Fatalf("%s must never be passed", a)
		}
	}
}

// Settings carry nothing per item: the same bytes for every item of a repo, so the tool set and
// the system prompt (and their prompt cache) are the same for every run of a stage.
func TestSettingsHaveNoPerItemContent(t *testing.T) {
	a, _ := GenerateSettings(goldenInput(ModeAuto))
	b, _ := GenerateSettings(goldenInput(ModeAuto))
	if !bytes.Equal(a, b) {
		t.Fatal("settings are not deterministic")
	}
	for _, s := range []string{"12-build", "runs/", "loop/12", "guard.json", "LOOP_GUARD_CONFIG"} {
		if bytes.Contains(a, []byte(s)) {
			t.Errorf("settings mention %q, which is per item or per run", s)
		}
	}
}
