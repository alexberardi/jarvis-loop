package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// The permission modes the runner can run an agent in.
const (
	ModeAuto   = "auto"   // the auto mode classifier, with deny rules
	ModeStrict = "strict" // dontAsk with an allowlist, for when auto mode is unavailable
)

// PermissionMode is the Claude Code mode for a runner mode.
func PermissionMode(mode string) string {
	if mode == ModeStrict {
		return "dontAsk"
	}
	return "auto"
}

// SettingsInput is what the generated Claude Code settings depend on: the runner's config and the
// repo, never the item.
type SettingsInput struct {
	Mode string
	// Guard is the loop-guard binary; its config comes from $LOOP_GUARD_CONFIG, so nothing in the
	// settings is per item or per run (see docs/decisions/2026-10-15-prompt-caching.md).
	Guard            string
	Home             string
	Protected        []string
	Secrets          []string
	LoopdAddrs       []string
	AllowedDomains   []string
	ExcludedCommands []string
	// ReadOnly is a triage or review run: no commits (loop-guard checks every spelling too).
	ReadOnly bool
	// HarnessSocket is the runner's harness broker socket, for the build and review runs of a repo
	// with a harness ("" otherwise): the one Unix socket the sandbox lets the agent connect to.
	HarnessSocket string
}

// DefaultDomains are the hosts sandboxed commands may reach: GitHub (Go modules, reading), the Go module
// proxy and checksum DB, the npm registry, and PyPI (the docs and the packages are Python).
var DefaultDomains = []string{
	"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
	"raw.githubusercontent.com", "proxy.golang.org", "sum.golang.org", "registry.npmjs.org",
	"pypi.org", "files.pythonhosted.org",
}

// DenyRules are the permission deny rules for every run. Deny rules are checked before the
// classifier and block in every mode. They match the command text Claude writes, so they are the
// first line; loop-guard re-checks every shell command with its own parser.
func DenyRules(in SettingsInput) []string {
	rules := []string{
		// Merging, approving and repository settings belong to the land stage and the user.
		"Bash(gh pr merge *)", "Bash(gh pr review *)", "Bash(gh repo delete *)", "Bash(gh repo edit *)",
		"Bash(gh repo rename *)", "Bash(gh repo archive *)", "Bash(gh auth *)", "Bash(gh secret *)",
		"Bash(gh ruleset *)", "Bash(gh workflow *)",
	}
	for _, m := range []string{"DELETE", "PUT", "PATCH"} {
		rules = append(rules, "Bash(gh api *-X "+m+"*)", "Bash(gh api *-X"+m+"*)",
			"Bash(gh api *--method "+m+"*)", "Bash(gh api *--method="+m+"*)")
	}
	rules = append(rules,
		// Pushing and opening or editing PRs are the runner's, outside the sandbox, with the user's
		// credentials (docs/decisions/2026-10-15-runner-pushes.md). The agent never pushes.
		"Bash(git push)", "Bash(git push *)", "Bash(gh pr create *)", "Bash(gh pr edit *)",
		// Devices, privileges, services, processes and remote shells.
		"Bash(adb)", "Bash(adb *)", "Bash(sudo *)", "Bash(pkexec *)", "Bash(doas *)", "Bash(systemctl *)",
		"Bash(pkill *)", "Bash(killall *)", "Bash(ssh *)", "Bash(scp *)", "Bash(sftp *)",
	)
	if in.ReadOnly {
		// Triage and review never commit or rewrite history; review reports, build changes.
		for _, sub := range []string{"commit", "commit-tree", "merge", "rebase", "cherry-pick", "revert", "am", "update-ref"} {
			rules = append(rules, "Bash(git "+sub+")", "Bash(git "+sub+" *)")
		}
	}
	rules = append(rules, killRules()...)
	rules = append(rules,
		// Recursive removal of anything absolute or above the worktree (the guard resolves paths).
		"Bash(rm -rf /*)", "Bash(rm -rf ~*)", "Bash(rm -rf ..*)", "Bash(rm -rf $HOME*)",
		"Bash(rm -fr /*)", "Bash(rm -fr ~*)", "Bash(rm -fr ..*)",
	)
	for _, a := range in.LoopdAddrs {
		rules = append(rules, "Bash(curl *"+a+"*)", "Bash(wget *"+a+"*)")
	}
	// No web access and no MCP tools for loop agents.
	rules = append(rules, "WebFetch", "WebSearch", "mcp__*")
	for _, s := range in.Secrets {
		for _, r := range pathRules(s) {
			rules = append(rules, "Read("+r+")")
		}
	}
	for _, p := range in.Protected {
		for _, r := range pathRules(p) {
			rules = append(rules, "Edit("+r+")")
		}
	}
	return uniq(rules)
}

// killRules deny every kill except "kill -0 <pid>" (signal 0 sends nothing: a liveness check,
// which the guard then checks exactly). A rule cannot say "anything but -0", so they spell out
// what kill's first argument can start with: no argument, a signal other than 0 (a digit 1-9, a
// name, -s/-n/-l, --), 0 followed by more digits, a pid, a job, an expansion or a quote.
func killRules() []string {
	rules := []string{"Bash(kill)", "Bash(kill -)", "Bash(kill - *)", "Bash(kill --*)"}
	for c := '1'; c <= '9'; c++ {
		rules = append(rules, "Bash(kill -"+string(c)+"*)")
	}
	for c := '0'; c <= '9'; c++ {
		rules = append(rules, "Bash(kill -0"+string(c)+"*)", "Bash(kill "+string(c)+"*)")
	}
	for c := 'A'; c <= 'Z'; c++ {
		rules = append(rules, "Bash(kill -"+string(c)+"*)", "Bash(kill -"+string(c+'a'-'A')+"*)")
	}
	for _, c := range []string{"%", "$", "`", "\"", "'", "\\", "{", "(", "~", "?", "["} {
		rules = append(rules, "Bash(kill "+c+"*)")
	}
	return rules
}

// pathRules are the Read/Edit rule paths for p, a file or a directory: //abs and //abs/**.
func pathRules(p string) []string {
	p = filepath.Clean(p)
	return []string{"/" + p, "/" + p + "/**"}
}

// AllowRules are the strict mode's allowlist (dontAsk denies everything else that would prompt).
func AllowRules(in SettingsInput) []string {
	rules := []string{
		"Bash(go *)", "Bash(gofmt *)", "Bash(npm *)", "Bash(npx *)", "Bash(git *)",
		"Bash(gh pr view *)", "Bash(gh pr checks *)", "Bash(gh pr list *)",
		"Bash(gh pr comment *)", "Bash(scripts/check)", "Bash(scripts/check *)", "Bash(scripts/sandbox *)",
		"Bash(scripts/fetch-sherpa-libs.sh *)", "Bash(mkdir *)", "Bash(ls *)",
		// The working directory (the worktree, relative so it is the same rule for every item).
		"Edit(./**)",
	}
	if in.HarnessSocket != "" {
		rules = append(rules, "Bash(loop-harness *)")
	}
	return rules
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// GenerateSettings builds the --settings file for a repo's runs of a stage.
func GenerateSettings(in SettingsInput) ([]byte, error) {
	guardCmd := shellQuote(in.Guard)
	hook := []any{map[string]any{"type": "command", "command": guardCmd, "timeout": 30}}
	perms := map[string]any{
		"defaultMode":                  PermissionMode(in.Mode),
		"deny":                         DenyRules(in),
		"disableBypassPermissionsMode": "disable",
	}
	if in.Mode == ModeStrict {
		perms["allow"] = AllowRules(in)
	}
	h := in.Home
	fs := map[string]any{
		// Beyond the worktree (with the scratch dir inside it, .loop/scratch) and $TMPDIR (the
		// defaults): the build caches.
		"allowWrite": []string{filepath.Join(h, ".cache", "go-build"), filepath.Join(h, "go", "pkg", "mod"),
			filepath.Join(h, ".npm"), filepath.Join(h, ".cache", "jarvis-harness")},
		// The mirror's hooks/ and config stay write-denied by Claude Code's own linked-worktree rule.
		"denyWrite": in.Protected,
		"denyRead":  in.Secrets,
	}
	var denied []string
	denied = append(denied, in.LoopdAddrs...)
	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"autoAllowBashIfSandboxed": false,
		"allowUnsandboxedCommands": false,
		"filesystem":               fs,
		"network": map[string]any{
			"allowedDomains":  uniq(append(append([]string{}, DefaultDomains...), in.AllowedDomains...)),
			"deniedDomains":   denied,
			"strictAllowlist": true,
		},
	}
	if in.HarnessSocket != "" {
		// loop-harness talks to the runner's broker (harness.go); every other Unix socket stays
		// closed (never allowAllUnixSockets).
		sandbox["network"].(map[string]any)["allowUnixSockets"] = []string{in.HarnessSocket}
	}
	if len(in.ExcludedCommands) > 0 {
		sandbox["excludedCommands"] = in.ExcludedCommands
	}
	s := map[string]any{
		"permissions": perms,
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": hook}},
			"PreToolUse":   []any{map[string]any{"matcher": "*", "hooks": hook}},
		},
		"sandbox": sandbox,
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// AgentArgs are the per-stage choices for a claude command line. Nothing here is per item.
type AgentArgs struct {
	Mode     string
	Settings string
	Model    string
	// Resume continues an earlier session (planning conversations, later).
	Resume string
}

// ClaudeArgs is the command line for a run. The brief goes on stdin as the prompt, the worktree
// is the cwd, and the per-run guard config is in the environment, so for one stage and repo the
// argv is the same for every item.
func ClaudeArgs(a AgentArgs) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", PermissionMode(a.Mode),
		"--permission-prompts", "none",
		// The generated settings plus the user's own (auto mode's environment); never the
		// repository's project settings, whose hooks would run outside the sandbox from files the
		// agent can edit.
		"--settings", a.Settings,
		"--setting-sources", "user",
		// No MCP servers: none configured is the empty set.
		"--strict-mcp-config",
		// cwd, git status and the memory location go to the first user message, so the system
		// prompt is the same for every worktree.
		"--exclude-dynamic-system-prompt-sections",
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Resume != "" {
		args = append(args, "--resume", a.Resume)
	}
	return args
}
