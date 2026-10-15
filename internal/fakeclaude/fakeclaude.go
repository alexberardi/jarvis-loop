// Package fakeclaude is a stand-in for `claude -p --output-format stream-json`, for the runner's
// tests and its dry run. It speaks the stream-json shapes the runner reads, runs the hooks from
// the --settings file the way Claude Code does (SessionStart, then PreToolUse before every tool
// call, honouring a deny), and plays a scripted scenario chosen by FAKE_CLAUDE_SCENARIO. It never
// talks to a model, GitHub or loopd.
package fakeclaude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Scenarios.
const (
	Good       = "good"        // commit, push the item branch, evidence, a valid result
	Escape     = "escape"      // try to write outside the worktree and push main first (refused), then good
	Lying      = "lying"       // good, but the result's head_sha is not HEAD
	Malformed  = "malformed"   // push, then a result that is not JSON
	NoResult   = "noresult"    // push, exit 0, no result
	Question   = "question"    // a question result
	Hang       = "hang"        // write a file, then wait forever
	IgnoreTerm = "ignore_term" // like hang, but ignore SIGTERM
	Exit3      = "exit3"       // print something and exit 3
	UsageLimit = "usage_limit" // the plan's session limit
	NoArm      = "noarm"       // run tools without the hooks (settings not applied)
	WrongMode  = "wrong_mode"  // start in default mode whatever was asked
	SymlinkEv  = "symlink_evidence"
	LowCache   = "low_cache" // good, but with a cold prompt cache (10% read over 6 turns)
)

type fake struct {
	out       io.Writer
	cwd       string
	mode      string
	pre, sess string
	runHooks  bool
	n         int
	turns     int
	scenario  string
}

func (f *fake) emit(v any) {
	b, _ := json.Marshal(v)
	f.out.Write(append(b, '\n'))
}

// Main runs the fake with claude's arguments and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := &fake{out: stdout, runHooks: true}
	var settings string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--settings":
			i++
			settings = args[i]
		case "--permission-mode":
			i++
			f.mode = args[i]
		}
	}
	scenario := os.Getenv("FAKE_CLAUDE_SCENARIO")
	if scenario == "" {
		scenario = Good
	}
	f.scenario = scenario
	f.cwd, _ = os.Getwd()
	brief, _ := io.ReadAll(stdin)
	branch := find("Branch: `([^`]+)`", brief)
	slug := find("Repository: `([^`]+)`", brief)
	base := find("base branch `([^`]+)`", brief)
	if b, err := os.ReadFile(settings); err == nil {
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		json.Unmarshal(b, &s)
		if h := s.Hooks["PreToolUse"]; len(h) > 0 && len(h[0].Hooks) > 0 {
			f.pre = h[0].Hooks[0].Command
		}
		if h := s.Hooks["SessionStart"]; len(h) > 0 && len(h[0].Hooks) > 0 {
			f.sess = h[0].Hooks[0].Command
		}
	}
	if scenario == NoArm {
		f.runHooks = false
	}
	if scenario == IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if f.runHooks && f.sess != "" {
		f.hook(f.sess, map[string]any{"hook_event_name": "SessionStart", "source": "startup", "cwd": f.cwd})
	}
	mode := f.mode
	if scenario == WrongMode {
		mode = "default"
	}
	f.emit(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-session-" + scenario, "cwd": f.cwd, "model": "fake-model",
		"permissionMode": mode, "tools": []string{"Bash", "Read", "Edit", "Write"}, "apiKeySource": "none", "claude_code_version": "fake"})

	switch scenario {
	case Hang, IgnoreTerm:
		f.write("wip.txt", "half done\n")
		f.say("working on it")
		for {
			time.Sleep(time.Hour)
		}
	case Exit3:
		f.say("I could not build it: the tests do not compile.")
		return 3
	case UsageLimit:
		f.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "resetsAt": 1792000000}})
		f.emit(map[string]any{"type": "assistant", "error": "rate_limit", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "You've hit your session limit · resets 3:45pm"}}}})
		f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "num_turns": 1, "duration_ms": 900,
			"total_cost_usd": 0.01, "result": "You've hit your session limit · resets 3:45pm"})
		return 1
	case Question:
		f.write(".loop/result.json", `{"outcome":"question","question":"Should the timer round up or down?"}`)
		f.result("Asked a question.")
		return 0
	case WrongMode:
		f.say("hello from manual mode")
		f.result("nothing done")
		return 0
	case Escape:
		outside := filepath.Join(filepath.Dir(f.cwd), "escaped.txt")
		f.write(outside, "I got out\n")
		f.bash("git push origin main")
	}

	// The common path: a change, committed and pushed on the item's branch.
	f.write("feature.txt", "a feature for "+branch+"\n")
	f.bash("git add feature.txt && git -c user.name=fake -c user.email=fake@example.com commit -q -m 'feat: add a feature'")
	if scenario == NoArm {
		f.result("done without hooks")
		return 0
	}
	f.bash("git push -q origin " + branch)
	switch scenario {
	case Malformed:
		f.write(".loop/result.json", `{"outcome": "pr_opened", oops`)
		f.result("done")
		return 0
	case NoResult:
		f.result("done, forgot the result")
		return 0
	}
	f.write(".loop/evidence/after.txt", "go test ./... ok\n")
	if scenario == SymlinkEv {
		f.bash("ln -s /etc/hostname .loop/evidence/host.txt")
	}
	head := strings.TrimSpace(git(f.cwd, "rev-parse", "HEAD"))
	if scenario == Lying {
		head = strings.Repeat("0", 40)
	}
	var paths []string
	for _, l := range strings.Split(git(f.cwd, "diff", "--name-status", "origin/"+base+"...HEAD"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	res, _ := json.Marshal(map[string]any{"outcome": "pr_opened", "repo": slug, "pr": 1, "branch": branch, "head_sha": head,
		"changed_paths": paths, "summary": "Added a feature, test first.", "not_verified": []string{"nothing on a phone"},
		"evidence": []any{map[string]string{"file": "evidence/after.txt", "label": "tests"}}})
	f.write(".loop/result.json", string(res))
	f.say("Opened PR #1.")
	f.result("Opened PR #1.")
	return 0
}

func find(re string, b []byte) string {
	m := regexp.MustCompile(re).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}

func (f *fake) say(text string) {
	f.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
}

// result ends the run. The token counts are made up: a warm cache (79% read) by default, a cold
// one (10%) in the low_cache scenario.
func (f *fake) result(text string) {
	usage := map[string]any{"input_tokens": 1000, "output_tokens": 4000, "cache_creation_input_tokens": 20000, "cache_read_input_tokens": 79000}
	if f.scenario == LowCache {
		usage = map[string]any{"input_tokens": 70000, "output_tokens": 4000, "cache_creation_input_tokens": 20000, "cache_read_input_tokens": 10000}
	}
	f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": f.turns + 1,
		"duration_ms": 1234, "total_cost_usd": 0.42, "result": text, "permission_denials": []any{}, "usage": usage,
		"session_id": "fake-session-" + f.scenario})
}

// hook runs a hook command as Claude Code does (bash, JSON on stdin) and reports a deny.
func (f *fake) hook(command string, input map[string]any) (bool, string) {
	b, _ := json.Marshal(input)
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = f.cwd
	cmd.Stdin = bytes.NewReader(b)
	// The runner's tests run this package from the test binary; the hook is that binary too.
	cmd.Env = append(os.Environ(), "LOOP_TEST_AS=guard")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		var resp struct {
			HSO struct {
				Reason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(out.Bytes(), &resp) == nil && resp.HSO.Reason != "" {
			return true, resp.HSO.Reason
		}
		return true, strings.TrimSpace(errb.String())
	}
	return false, ""
}

// tool plays one tool call: the assistant's tool_use, the PreToolUse hook, the tool, the result.
func (f *fake) tool(name string, input map[string]any, run func() (string, error)) {
	f.n++
	f.turns++
	id := fmt.Sprintf("toolu_%02d", f.n)
	f.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	result, isErr := "", false
	denied, reason := false, ""
	if f.runHooks && f.pre != "" {
		denied, reason = f.hook(f.pre, map[string]any{"hook_event_name": "PreToolUse", "tool_name": name, "tool_input": input,
			"cwd": f.cwd, "permission_mode": f.mode, "tool_use_id": id})
	}
	if denied {
		result, isErr = "PreToolUse:"+name+" hook error: "+reason, true
	} else {
		out, err := run()
		result, isErr = out, err != nil
		if err != nil {
			result = err.Error() + ": " + out
		}
	}
	f.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": result, "is_error": isErr}}}})
}

func (f *fake) write(path, content string) {
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.cwd, p)
	}
	f.tool("Write", map[string]any{"file_path": p, "content": content}, func() (string, error) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return "written", os.WriteFile(p, []byte(content), 0o644)
	})
}

func (f *fake) bash(command string) {
	f.tool("Bash", map[string]any{"command": command, "description": "fake"}, func() (string, error) {
		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = f.cwd
		out, err := cmd.CombinedOutput()
		return string(out), err
	})
}
