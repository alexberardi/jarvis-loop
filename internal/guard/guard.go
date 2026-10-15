// Package guard is the policy behind cmd/loop-guard, the PreToolUse hook every loop agent runs
// under (docs/phases/04-runner-build.md, boundary layer 3). It is a second line of defence that
// does not depend on Claude Code's own rule matching: it refuses writes that resolve (through ..
// and symlinks) outside the item's worktree, reads of secrets, shell commands that name the user's
// checkouts or secrets or loopd's address, and the denied command families. It never approves
// anything: a call it does not refuse goes on to the normal permission flow (rules, classifier,
// sandbox).
package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Rule names: stable, recorded on the guard_refused event.
const (
	RuleWriteOutside   = "write_outside"
	RuleWriteProtected = "write_protected"
	RuleReadSecret     = "read_secret"
	RuleProtectedPath  = "protected_path"
	RuleProtectedWord  = "protected_word"
	RuleLoopd          = "loopd_address"
	RuleNever          = "denied_command"
	RuleGH             = "gh"
	RuleGitPush        = "git_push"
	RuleGitRemote      = "git_remote"
	RuleGitConfig      = "git_config"
	RuleGitWorktree    = "git_worktree"
	RuleRmOutside      = "rm_outside"
	RuleUnparseable    = "unparseable"
)

// Config is what the runner writes for one run.
type Config struct {
	Item       int64  `json:"item"`
	Worktree   string `json:"worktree"`
	Scratch    string `json:"scratch"`
	SandboxDir string `json:"sandbox_dir,omitempty"`
	// Branch is the only branch the agent may push, to Remote.
	Branch string `json:"branch"`
	Remote string `json:"remote"`
	Home   string `json:"home"`
	// Protected paths may not be named by a shell command nor written (the user's checkouts,
	// credential dirs, loopd's tokens and data).
	Protected []string `json:"protected"`
	// Secrets may not be read by the file tools either.
	Secrets []string `json:"secrets"`
	// Words may not appear in a shell command at all (the user's emulator serial).
	Words []string `json:"words"`
	// LoopdAddrs are host:port spellings of loopd's address.
	LoopdAddrs []string `json:"loopd_addrs"`
	// DenyWrite are worktree-relative paths no tool may change (e.g. a harness entry point that
	// runs outside the sandbox).
	DenyWrite []string `json:"deny_write,omitempty"`
	// Report is the JSONL file refusals are appended to; Armed is touched on every invocation.
	Report string `json:"report"`
	Armed  string `json:"armed"`
}

// Input is the hook's stdin (the fields the guard uses).
type Input struct {
	Event      string         `json:"hook_event_name"`
	ToolName   string         `json:"tool_name"`
	ToolInput  map[string]any `json:"tool_input"`
	Cwd        string         `json:"cwd"`
	Scratchpad string         `json:"scratchpad_dir"`
}

// Decision is the guard's verdict. Deny false means "no objection".
type Decision struct {
	Deny   bool   `json:"deny"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Target is what was refused (a path, or the command), for the report.
	Target string `json:"target,omitempty"`
}

func deny(rule, target, why string) Decision {
	return Decision{Deny: true, Rule: rule, Target: target,
		Reason: "loop-guard refused this (" + rule + "): " + why + ". Do not retry it another way; if the item needs it, finish with outcome \"question\" and say why."}
}

var (
	writeTools = []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}
	readTools  = []string{"Read", "Grep", "Glob", "LS", "NotebookRead"}
)

// Check decides one tool call.
func (c *Config) Check(in Input) Decision {
	ti := in.ToolInput
	if ti == nil {
		ti = map[string]any{}
	}
	if cmd, ok := ti["command"].(string); ok {
		return c.checkShell(cmd, in.Cwd)
	}
	isRead := slices.Contains(readTools, in.ToolName)
	if isRead {
		for _, k := range []string{"file_path", "path", "notebook_path"} {
			if p, ok := ti[k].(string); ok && p != "" {
				if d := c.checkRead(p, in.Cwd); d.Deny {
					return d
				}
			}
		}
		if pat, ok := ti["pattern"].(string); ok && in.ToolName == "Glob" {
			if pre := staticPrefix(pat); pre != "" && (filepath.IsAbs(pre) || strings.HasPrefix(pre, "~")) {
				if d := c.checkRead(pre, in.Cwd); d.Deny {
					return d
				}
			}
		}
		return Decision{}
	}
	var path string
	hasPath := false
	for _, k := range []string{"file_path", "notebook_path"} {
		if p, ok := ti[k].(string); ok {
			path, hasPath = p, true
			break
		}
	}
	if slices.Contains(writeTools, in.ToolName) || hasPath {
		if strings.TrimSpace(path) == "" {
			return deny(RuleUnparseable, in.ToolName, "a file tool call without a path")
		}
		return c.checkWrite(path, in.Cwd, in.Scratchpad)
	}
	return Decision{}
}

// ---- paths ----

// resolve makes p absolute (against cwd, expanding ~), cleans it, and resolves symlinks in the
// longest prefix that exists, so a path through a symlink, or a new file under one, is judged by
// where it really lands.
func (c *Config) resolve(p, cwd string) string {
	p = c.expandHome(p)
	if !filepath.IsAbs(p) {
		base := cwd
		if base == "" {
			base = c.Worktree
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

func (c *Config) expandHome(p string) string {
	for _, pre := range []string{"~", "$HOME", "${HOME}"} {
		if p == pre {
			return c.Home
		}
		if strings.HasPrefix(p, pre+"/") {
			return filepath.Join(c.Home, p[len(pre)+1:])
		}
	}
	return p
}

// realRoot resolves a configured root (which may itself sit behind a symlink).
func realRoot(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func within(p, root string) bool {
	if root == "" {
		return false
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

func (c *Config) writeRoots(scratchpad string) []string {
	roots := []string{realRoot(c.Worktree), realRoot(c.Scratch)}
	if c.SandboxDir != "" {
		roots = append(roots, realRoot(c.SandboxDir))
	}
	// Claude Code's own per-session scratchpad, which lives under the temp dir.
	if scratchpad != "" && within(realRoot(scratchpad), realRoot(os.TempDir())) {
		roots = append(roots, realRoot(scratchpad))
	}
	return roots
}

func (c *Config) checkWrite(path, cwd, scratchpad string) Decision {
	p := c.resolve(path, cwd)
	ok := false
	for _, r := range c.writeRoots(scratchpad) {
		ok = ok || within(p, r)
	}
	if !ok {
		return deny(RuleWriteOutside, p, "writes are limited to this item's worktree, its sandbox dir and the scratch dir "+c.Scratch)
	}
	wt := realRoot(c.Worktree)
	if within(p, wt) {
		rel, _ := filepath.Rel(wt, p)
		rel = filepath.ToSlash(rel)
		for _, pre := range append([]string{".git", ".claude", ".mcp.json", ".loop/brief.md"}, c.DenyWrite...) {
			if rel == pre || strings.HasPrefix(rel, pre+"/") {
				return deny(RuleWriteProtected, p, rel+" is not the agent's to change")
			}
		}
	}
	for _, s := range c.Protected {
		if within(p, realRoot(s)) {
			return deny(RuleWriteProtected, p, "that path is protected")
		}
	}
	return Decision{}
}

func (c *Config) checkRead(path, cwd string) Decision {
	p := c.resolve(path, cwd)
	for _, s := range c.Secrets {
		if within(p, realRoot(s)) {
			return deny(RuleReadSecret, p, "that is a credential store")
		}
	}
	return Decision{}
}

// staticPrefix is the part of a glob before its first wildcard, cut back to a directory.
func staticPrefix(pat string) string {
	i := strings.IndexAny(pat, "*?[{")
	if i < 0 {
		return pat
	}
	pre := pat[:i]
	if j := strings.LastIndex(pre, "/"); j >= 0 {
		return pre[:j+1]
	}
	return ""
}

// ---- shell ----

// pathForms are the spellings of p a shell command could use.
func (c *Config) pathForms(p string) []string {
	forms := []string{p}
	if r := realRoot(p); r != p {
		forms = append(forms, r)
	}
	if c.Home != "" && within(p, c.Home) {
		rel := strings.TrimPrefix(p, c.Home)
		forms = append(forms, "~"+rel, "$HOME"+rel, "${HOME}"+rel)
	}
	return forms
}

// mentions reports whether s contains form as a whole path (not a prefix of a longer name).
func mentions(s, form string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], form)
		if j < 0 {
			return false
		}
		end := i + j + len(form)
		if end == len(s) || strings.ContainsRune("/ \t\n'\"`;|&)<>:,=", rune(s[end])) {
			return true
		}
		i = i + j + 1
	}
}

func (c *Config) checkShell(cmd, cwd string) Decision {
	target := cmd
	if len(target) > 300 {
		target = target[:300] + "…"
	}
	for _, p := range c.Protected {
		for _, f := range c.pathForms(filepath.Clean(p)) {
			if mentions(cmd, f) {
				return deny(RuleProtectedPath, target, "the command names "+f+", which is the user's (checkouts, credentials and loopd are off limits)")
			}
		}
	}
	for _, w := range c.Words {
		if w != "" && strings.Contains(cmd, w) {
			return deny(RuleProtectedWord, target, w+" is the user's own; use the sandbox name you were given")
		}
	}
	for _, a := range c.LoopdAddrs {
		if a != "" && strings.Contains(cmd, a) {
			return deny(RuleLoopd, target, "loopd is the runner's to talk to, not the agent's")
		}
	}
	return c.checkScript(cmd, cwd, target, 0)
}

func (c *Config) checkScript(script, cwd, target string, depth int) Decision {
	if depth > 4 {
		return deny(RuleUnparseable, target, "too many nested shells")
	}
	segs, subs, err := parse(script)
	if err != nil {
		return deny(RuleUnparseable, target, "the command could not be parsed ("+err.Error()+")")
	}
	for _, sub := range subs {
		if d := c.checkScript(sub, cwd, target, depth+1); d.Deny {
			return d
		}
	}
	movedDir := false
	for _, words := range segs {
		argv := stripWrappers(words)
		if len(argv) == 0 {
			continue
		}
		name := filepath.Base(argv[0])
		switch name {
		case "bash", "sh", "zsh", "dash", "ksh":
			for i := 1; i+1 < len(argv); i++ {
				if strings.HasPrefix(argv[i], "-") && strings.Contains(argv[i], "c") && !strings.HasPrefix(argv[i], "--") {
					if d := c.checkScript(argv[i+1], cwd, target, depth+1); d.Deny {
						return d
					}
					break
				}
			}
		case "eval":
			if d := c.checkScript(strings.Join(argv[1:], " "), cwd, target, depth+1); d.Deny {
				return d
			}
		case "cd", "pushd", "popd":
			movedDir = true
		}
		if d := c.checkArgv(name, argv, cwd, target, movedDir); d.Deny {
			return d
		}
	}
	return Decision{}
}

var never = map[string]string{
	"sudo": "no privilege escalation", "pkexec": "no privilege escalation", "doas": "no privilege escalation",
	"su": "no privilege escalation", "run0": "no privilege escalation",
	"systemctl": "services are not the agent's", "service": "services are not the agent's", "systemd-run": "services are not the agent's",
	"kill": "stopping processes is the runner's job", "pkill": "stopping processes is the runner's job",
	"killall": "stopping processes is the runner's job", "skill": "stopping processes is the runner's job",
	"adb": "emulator work goes through the harness scripts, which pass -s",
	"ssh": "no remote shells", "scp": "no remote copies", "sftp": "no remote copies",
}

func (c *Config) checkArgv(name string, argv []string, cwd, target string, movedDir bool) Decision {
	if why, ok := never[name]; ok {
		return deny(RuleNever, target, name+": "+why)
	}
	switch name {
	case "git":
		return c.checkGit(argv[1:], target)
	case "gh":
		return checkGH(argv[1:], target)
	case "rm", "rmdir":
		return c.checkRm(argv[1:], cwd, target, movedDir)
	}
	return Decision{}
}

// ---- git ----

func (c *Config) checkGit(args []string, target string) Decision {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "--exec-path" {
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return Decision{}
	}
	sub, rest := args[i], args[i+1:]
	switch sub {
	case "push":
		return c.checkPush(rest, target)
	case "remote":
		if len(rest) > 0 && slices.Contains([]string{"add", "set-url", "rename", "remove", "rm", "set-branches", "set-head"}, rest[0]) {
			return deny(RuleGitRemote, target, "remotes are set up by the runner")
		}
	case "config":
		for _, a := range rest {
			if slices.Contains([]string{"--get", "--get-all", "--get-regexp", "--list", "-l", "get", "list"}, a) {
				return Decision{}
			}
		}
		return deny(RuleGitConfig, target, "git config is the runner's; read it with --get")
	case "worktree":
		if len(rest) > 0 && rest[0] != "list" {
			return deny(RuleGitWorktree, target, "worktrees are created and removed by the runner")
		}
	}
	return Decision{}
}

func (c *Config) checkPush(args []string, target string) Decision {
	if c.Branch == "" {
		return deny(RuleGitPush, target, "this run may not push")
	}
	want := "git push " + c.Remote + " " + c.Branch
	var pos []string
	for _, a := range args {
		switch {
		case a == "-u" || a == "--set-upstream" || a == "-q" || a == "--quiet" || a == "-v" || a == "--verbose" || a == "--no-verify" || a == "--porcelain":
		case strings.HasPrefix(a, "-"):
			return deny(RuleGitPush, target, "push flag "+a+" is not allowed; push with: "+want)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		return deny(RuleGitPush, target, "name the remote and the branch: "+want)
	}
	if pos[0] != c.Remote {
		return deny(RuleGitPush, target, "push only to "+c.Remote+": "+want)
	}
	for _, ref := range pos[1:] {
		src, dst, hasColon := strings.Cut(ref, ":")
		if !hasColon {
			dst = src
		}
		if strings.HasPrefix(ref, "+") || src == "" {
			return deny(RuleGitPush, target, "no force pushes or deletes: "+want)
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if dst != c.Branch {
			return deny(RuleGitPush, target, "only this item's branch "+c.Branch+" may be pushed: "+want)
		}
	}
	return Decision{}
}

// ---- gh ----

func checkGH(args []string, target string) Decision {
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-R" || a == "--repo" || a == "--hostname" {
			i++
			continue
		}
		words = append(words, a)
	}
	first := func(n int) string {
		var w []string
		for _, a := range words {
			if strings.HasPrefix(a, "-") {
				continue
			}
			w = append(w, a)
			if len(w) == n {
				break
			}
		}
		return strings.Join(w, " ")
	}
	switch first(1) {
	case "auth", "secret", "variable", "ruleset", "workflow", "release":
		return deny(RuleGH, target, "gh "+first(1)+" is not for loop agents")
	case "api":
		lower := strings.ToLower(strings.Join(words, " "))
		for i, a := range words {
			m := ""
			switch {
			case (a == "-X" || a == "--method") && i+1 < len(words):
				m = words[i+1]
			case strings.HasPrefix(a, "-X"):
				m = a[2:]
			case strings.HasPrefix(a, "--method="):
				m = a[len("--method="):]
			}
			if m = strings.ToUpper(m); m == "DELETE" || m == "PUT" || m == "PATCH" {
				return deny(RuleGH, target, "gh api "+m+" changes repository settings")
			}
		}
		if strings.Contains(lower, "merge") || strings.Contains(lower, "mutation") {
			return deny(RuleGH, target, "merging, or GraphQL mutations, through gh api")
		}
	}
	switch first(2) {
	case "pr merge":
		return deny(RuleGH, target, "merging is the land stage's job (never gh pr merge, never --auto)")
	case "pr review":
		return deny(RuleGH, target, "the builder never reviews or approves")
	case "repo delete", "repo edit", "repo rename", "repo archive", "repo sync":
		return deny(RuleGH, target, "repository settings are not the agent's")
	}
	return Decision{}
}

// ---- rm ----

func (c *Config) checkRm(args []string, cwd, target string, movedDir bool) Decision {
	recursive := false
	var paths []string
	opts := true
	for _, a := range args {
		switch {
		case opts && a == "--":
			opts = false
		case opts && (a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR"))):
			recursive = true
		case opts && strings.HasPrefix(a, "-"):
		default:
			paths = append(paths, a)
		}
	}
	if !recursive {
		return Decision{} // single files: the sandbox's write boundary decides
	}
	roots := c.writeRoots("")
	for _, p := range paths {
		e := c.expandHome(p)
		if strings.ContainsAny(e, "$`") {
			return deny(RuleRmOutside, target, "rm -r with an unexpanded variable ("+p+") cannot be checked")
		}
		if movedDir && !filepath.IsAbs(e) {
			return deny(RuleRmOutside, target, "rm -r with a relative path after cd cannot be checked; use an absolute path")
		}
		if pre := staticPrefix(e); pre != e {
			if pre == "" {
				pre = "."
			}
			e = pre
		}
		r := c.resolve(e, cwd)
		ok := false
		for _, root := range roots {
			ok = ok || within(r, root)
		}
		if !ok {
			return deny(RuleRmOutside, target, "rm -r outside the worktree ("+r+")")
		}
	}
	return Decision{}
}

// stripWrappers drops leading assignments, block keywords and the wrappers that run their
// argument as the command.
func stripWrappers(w []string) []string {
	for len(w) > 0 {
		a := w[0]
		switch {
		case isAssignment(a):
			w = w[1:]
		case slices.Contains([]string{"!", "{", "}", "if", "then", "else", "elif", "do", "while", "until", "time", "nohup", "command", "builtin", "exec", "noglob", "setsid"}, a):
			w = w[1:]
		case a == "timeout":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			if len(w) > 0 {
				w = w[1:] // the duration
			}
		case a == "nice" || a == "stdbuf" || a == "ionice" || a == "env" || a == "xargs" || a == "watch" || a == "flock":
			w = w[1:]
			for len(w) > 0 && (strings.HasPrefix(w[0], "-") || isAssignment(w[0])) {
				if (w[0] == "-n" || w[0] == "-c" || w[0] == "-u") && len(w) > 1 {
					w = w[1:]
				}
				w = w[1:]
			}
		default:
			return w
		}
	}
	return w
}

func isAssignment(a string) bool {
	k, _, ok := strings.Cut(a, "=")
	if !ok || k == "" {
		return false
	}
	for i, r := range k {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
