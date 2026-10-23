package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/slack"
)

const (
	tokensBody = "user-token-0123456789abcdef\tuser\talex\nrunner-token-0123456789abcdef\trunner\tdevbox\n"
	slackBody  = "SLACK_BOT_TOKEN=xoxb-pretend\nSLACK_APP_TOKEN=xapp-pretend\n"
	orchBody   = "SLACK_BOT_TOKEN=xoxb-orch\nSLACK_APP_TOKEN=xapp-orch\n"
)

// The entrypoint writes each secret that is set at 0600, where loopd's own readers accept it, and
// execs loopd with the paths it wrote (docs/decisions/2026-10-23-loopd-on-fly.md).
func TestRunWritesSecretsAndExecs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "loop")
	t.Setenv("LOOP_TOKENS", tokensBody)
	t.Setenv("LOOP_SLACK_ENV", slackBody)
	t.Setenv("LOOP_ORCHESTRATOR_SLACK_ENV", orchBody)
	var gotPath string
	var gotArgv, gotEnv []string
	err := run([]string{"--data", "/data", "--lan"}, dir, func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != loopdPath || gotArgv[0] != loopdPath {
		t.Errorf("exec %q %v", gotPath, gotArgv)
	}
	// The command line it was given, then the flags for the files it wrote.
	want := []string{loopdPath, "--data", "/data", "--lan",
		"--tokens", filepath.Join(dir, "tokens"), "--slack-env", filepath.Join(dir, "slack.env")}
	if !slices.Equal(gotArgv, want) {
		t.Errorf("argv %v,\n want %v", gotArgv, want)
	}
	// The secrets are out of the environment it hands on.
	for _, s := range secrets {
		if os.Getenv(s.env) != "" {
			t.Errorf("%s is still set", s.env)
		}
		if i := slices.IndexFunc(gotEnv, func(e string) bool { return strings.HasPrefix(e, s.env+"=") }); i >= 0 {
			t.Errorf("%s is still in the exec'd environment", s.env)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, %v", dir, fi.Mode().Perm(), err)
		}
	}
	// loopd's own readers accept the files, mode check and all: that is the point of writing them.
	toks, err := api.LoadTokens(filepath.Join(dir, "tokens"))
	if err != nil {
		t.Fatalf("api.LoadTokens: %v", err)
	}
	if p, ok := toks.Lookup("user-token-0123456789abcdef"); !ok || p.Actor() != "user:alex" {
		t.Errorf("tokens: %+v %v", p, ok)
	}
	for name, want := range map[string]string{"slack.env": "xoxb-pretend", "orchestrator-slack.env": "xoxb-orch"} {
		got, err := slack.LoadEnv(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("slack.LoadEnv %s: %v", name, err)
		}
		if got.Bot != want {
			t.Errorf("%s: bot token is not the one set", name)
		}
	}
}

// Slack is optional: without it loopd starts with no Slack and no --slack-env.
func TestRunWithoutSlack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "loop")
	t.Setenv("LOOP_TOKENS", tokensBody)
	t.Setenv("LOOP_SLACK_ENV", "")
	t.Setenv("LOOP_ORCHESTRATOR_SLACK_ENV", "")
	var gotArgv []string
	if err := run(nil, dir, func(_ string, argv, _ []string) error { gotArgv = argv; return nil }); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(gotArgv, "--slack-env") {
		t.Errorf("argv %v", gotArgv)
	}
	for _, name := range []string{"slack.env", "orchestrator-slack.env"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written: %v", name, err)
		}
	}
}

// Without the tokens it refuses to start, rather than letting loopd fail on a missing file. The
// error names the variable and nothing of any value.
func TestRunNeedsTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "loop")
	t.Setenv("LOOP_SLACK_ENV", slackBody)
	for _, v := range []string{"", "   \n\t "} {
		t.Setenv("LOOP_TOKENS", v)
		err := run(nil, dir, func(string, []string, []string) error {
			t.Error("exec'd loopd without tokens")
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "LOOP_TOKENS") {
			t.Fatalf("error %v, want one naming LOOP_TOKENS", err)
		}
		if strings.Contains(err.Error(), "xoxb") || strings.Contains(err.Error(), v) && v != "" {
			t.Errorf("the error carries a secret: %v", err)
		}
	}
}

// A value that lost its last newline still parses: both readers scan lines.
func TestRunAddsTheMissingNewline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "loop")
	t.Setenv("LOOP_TOKENS", strings.TrimSuffix(tokensBody, "\n"))
	t.Setenv("LOOP_SLACK_ENV", "")
	t.Setenv("LOOP_ORCHESTRATOR_SLACK_ENV", "")
	if err := run(nil, dir, func(string, []string, []string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LoadTokens(filepath.Join(dir, "tokens")); err != nil {
		t.Fatalf("api.LoadTokens: %v", err)
	}
}

// A file left from an earlier start with a loose mode is tightened, not trusted.
func TestRunTightensALooseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no modes")
	}
	dir := filepath.Join(t.TempDir(), "run", "loop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_TOKENS", tokensBody)
	t.Setenv("LOOP_SLACK_ENV", "")
	t.Setenv("LOOP_ORCHESTRATOR_SLACK_ENV", "")
	if err := run(nil, dir, func(string, []string, []string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode %v, %v", path, fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s: mode %v, %v", dir, fi.Mode().Perm(), err)
	}
	if _, err := api.LoadTokens(path); err != nil {
		t.Fatalf("api.LoadTokens: %v", err)
	}
}
