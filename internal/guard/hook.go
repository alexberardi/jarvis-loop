package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// ConfigEnv names the guard's config file. The runner sets it in the agent's environment, which
// Claude Code passes to hooks, so the settings file (and the hook command in it) is the same for
// every item and run.
const ConfigEnv = "LOOP_GUARD_CONFIG"

// ConfigPath finds the config: `--config FILE`, else $LOOP_GUARD_CONFIG.
func ConfigPath(args []string, getenv func(string) string) (string, error) {
	switch {
	case len(args) == 2 && args[0] == "--config" && args[1] != "":
		return args[1], nil
	case len(args) == 0 && getenv(ConfigEnv) != "":
		return getenv(ConfigEnv), nil
	}
	return "", fmt.Errorf("usage: loop-guard [--config FILE] (or $%s); reads a Claude Code hook input on stdin", ConfigEnv)
}

// Refusal is one line of the report file the runner turns into guard_refused events.
type Refusal struct {
	At     time.Time `json:"at"`
	Tool   string    `json:"tool"`
	Rule   string    `json:"rule"`
	Reason string    `json:"reason"`
	Target string    `json:"target"`
}

// LoadConfig reads a guard config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Worktree == "" || c.Scratch == "" {
		return nil, fmt.Errorf("%s: worktree and scratch are required", path)
	}
	return &c, nil
}

// Hook runs one hook invocation and returns the exit code. It fails closed: anything it cannot
// read is a block (exit 2), because Claude Code lets a tool call through when a hook errors in
// any other way (onFailure "block" needs Claude Code 2.1.295; see
// docs/decisions/2026-10-15-claude-headless-facts.md). An allowed call prints nothing and exits 0,
// so the normal permission flow still decides; the guard never answers "allow".
func Hook(configPath string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	c, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "loop-guard: cannot load its config (%v); refusing every tool call\n", err)
		return 2
	}
	if c.Armed != "" {
		// Proof for the runner that Claude Code loaded the generated settings.
		_ = os.WriteFile(c.Armed, []byte("armed\n"), 0o600)
	}
	var in Input
	dec := json.NewDecoder(io.LimitReader(stdin, 64<<20))
	if err := dec.Decode(&in); err != nil {
		fmt.Fprintf(stderr, "loop-guard: cannot read the hook input (%v); refusing\n", err)
		return 2
	}
	if in.Event != "" && in.Event != "PreToolUse" {
		return 0
	}
	d := c.Check(in)
	if !d.Deny {
		return 0
	}
	if c.Report != "" {
		if f, err := os.OpenFile(c.Report, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			b, _ := json.Marshal(Refusal{At: now().UTC(), Tool: in.ToolName, Rule: d.Rule, Reason: d.Reason, Target: d.Target})
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": d.Reason}})
	stdout.Write(out)
	fmt.Fprintln(stderr, d.Reason)
	return 2
}
