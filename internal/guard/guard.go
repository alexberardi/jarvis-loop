// Package guard is the policy behind cmd/loop-guard, the PreToolUse hook every loop agent runs
// under (docs/phases/04-runner-build.md, boundary layer 3). It is a second line of defence that
// does not depend on Claude Code's own rule matching: it refuses writes that resolve (through ..
// and symlinks) outside the item's worktree, writes to the protected prefixes inside it (.git,
// .claude, the repo's deny_write) on every channel — the file tools, a shell redirection, a
// write-shaped command — reads of secrets, shell commands that name the user's checkouts or
// secrets or loopd's address, and the denied command families. It never approves
// anything: a call it does not refuse goes on to the normal permission flow (rules, classifier,
// sandbox).
package guard

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	RuleGitCommit      = "git_commit"
	RuleRmOutside      = "rm_outside"
	RuleUnparseable    = "unparseable"
)

// Config is what the runner writes for one run.
type Config struct {
	Item       int64  `json:"item"`
	Worktree   string `json:"worktree"`
	Scratch    string `json:"scratch"`
	SandboxDir string `json:"sandbox_dir,omitempty"`
	// Branch and Remote are the item's branch and its remote, for the record: agents never push
	// (the runner pushes the branch after the run; docs/decisions/2026-10-15-runner-pushes.md).
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
	// ReadOnly is a triage or review run: it never commits or rewrites git history (a review's
	// worktree is the PR's head; what should change goes into the result).
	ReadOnly bool `json:"read_only,omitempty"`
	// DenyWrite are worktree-relative paths no tool may change (e.g. a harness entry point that
	// runs outside the sandbox).
	DenyWrite []string `json:"deny_write,omitempty"`
	// Generated are worktree-relative globs (path.Match) under DenyWrite that the agent may write
	// anyway: a generator's output, which verify regenerates and compares before it trusts it (#593).
	Generated []string `json:"generated,omitempty"`
	// NewOK are DenyWrite prefixes under which the agent may add a file the base branch lacks;
	// BaseFiles are the base's files under them, which stay refused (#593).
	NewOK     []string `json:"new_ok,omitempty"`
	BaseFiles []string `json:"base_files,omitempty"`
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
	if rel := c.denyWriteRel(p); rel != "" {
		return deny(RuleWriteProtected, p, rel+" is not the agent's to change")
	}
	for _, s := range c.Protected {
		if within(p, realRoot(s)) {
			return deny(RuleWriteProtected, p, "that path is protected")
		}
	}
	return Decision{}
}

// denyPrefixes are the worktree-relative prefixes no tool may change, on any channel.
func (c *Config) denyPrefixes() []string {
	return append([]string{".git", ".claude", ".mcp.json", ".loop/brief.md"}, c.DenyWrite...)
}

// denyWriteRel reports the worktree-relative name of p when it falls under a protected prefix,
// or "" (p is the resolved absolute path).
func (c *Config) denyWriteRel(p string) string {
	wt := realRoot(c.Worktree)
	if !within(p, wt) {
		return ""
	}
	rel, _ := filepath.Rel(wt, p)
	rel = filepath.ToSlash(rel)
	for _, pre := range c.denyPrefixes() {
		if (rel == pre || strings.HasPrefix(rel, pre+"/")) && !c.carvedOut(rel) {
			return rel
		}
	}
	return ""
}

// carvedOut reports whether rel, under a DenyWrite prefix, is one the agent may write anyway: a
// generated file, or a new file under a NewOK prefix (judged against the base, so the agent can
// keep editing a file it added).
func (c *Config) carvedOut(rel string) bool {
	if under(rel, []string{".git", ".claude", ".mcp.json", ".loop"}) {
		return false
	}
	for _, g := range c.Generated {
		if ok, _ := path.Match(g, rel); ok {
			return true
		}
	}
	return under(rel, c.NewOK) && !slices.Contains(c.BaseFiles, rel)
}

func under(rel string, pres []string) bool {
	for _, pre := range pres {
		pre = strings.TrimSuffix(pre, "/")
		if strings.HasPrefix(rel, pre+"/") {
			return true
		}
	}
	return false
}

// checkWriteTarget judges one path a shell command writes: a redirection's target or a
// write-shaped command's destination. The protected prefixes bind every write path (#537), not
// just the file tools. A target the guard cannot resolve (an unexpanded variable or substitution)
// is refused only when its own text names a protected prefix, so `> $TMPDIR/f` stays usable; a
// relative target after cd cannot be resolved at all and is refused, as in checkRm. Writes that
// resolve outside the worktree stay the OS sandbox's to stop, as before.
func (c *Config) checkWriteTarget(t, cwd, target string, movedDir bool) Decision {
	t = strings.TrimSpace(t)
	if t == "" || fdLike(t) {
		return Decision{}
	}
	e := c.expandHome(t)
	if strings.ContainsAny(e, "$`") {
		for _, pre := range c.denyPrefixes() {
			if relMentions(e, pre) {
				return deny(RuleWriteProtected, target, pre+" is not the agent's to change")
			}
		}
		return Decision{}
	}
	if movedDir && !filepath.IsAbs(e) {
		return deny(RuleWriteProtected, target, "a write to a relative path after cd cannot be checked; use an absolute path")
	}
	if rel := c.denyWriteRel(c.resolve(e, cwd)); rel != "" {
		return deny(RuleWriteProtected, target, rel+" is not the agent's to change")
	}
	return Decision{}
}

// fdLike reports a redirection target that is a file descriptor (2>&1) or stdin (-), not a path.
func fdLike(t string) bool {
	if t == "-" {
		return true
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return t != ""
}

// relMentions reports whether t contains the worktree-relative prefix pre on path-component
// boundaries (so `$WT/.git/config` names .git, and `.gitignore` does not).
func relMentions(t, pre string) bool {
	for i := 0; ; {
		j := strings.Index(t[i:], pre)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(pre)
		if (start == 0 || t[start-1] == '/') && (end == len(t) || t[end] == '/') {
			return true
		}
		i = start + 1
	}
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
	sc, err := parseScript(script)
	if err != nil {
		return deny(RuleUnparseable, target, "the command could not be parsed ("+err.Error()+")")
	}
	if d := c.checkProtected(sc, depth, target); d.Deny {
		return d
	}
	subs := sc.subs
	for _, sub := range subs {
		if d := c.checkScript(sub, cwd, target, depth+1); d.Deny {
			return d
		}
	}
	movedDir := false
	for _, sg := range sc.segs {
		for _, w := range sg.writes {
			if d := c.checkWriteTarget(unmark(w), cwd, target, movedDir); d.Deny {
				return d
			}
		}
		raw := sg.words
		// kill -0 is judged on the raw words (which word came from which substitution), and only
		// when nothing wraps it: a wrapper would run /bin/kill, not the shell's builtin.
		if len(raw) > 0 && raw[0] == "kill" {
			if c.killZero(raw[1:], subs, cwd, movedDir) {
				continue
			}
		}
		words := make([]string, len(raw))
		for i, w := range raw {
			words[i] = unmark(w)
		}
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
		case "loop-harness":
			// The runner runs it in the worktree, outside the agent's sandbox (the harness broker):
			// the same rules as any script.
			if len(argv) > 2 && argv[1] == "run" {
				if d := c.checkScript(strings.Join(argv[2:], " "), cwd, target, depth+1); d.Deny {
					return d
				}
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

// checkProtected refuses a script that names a protected path in anything the shell could open or
// run: its words, redirections, here-strings and the here-documents a program reads as input.
// Substitutions are checked as scripts of their own. Text that is only data is not matched, so a
// PR body or a note may mention a path (#95): a here-document that cat or tee writes out, and the
// arguments of echo and printf. That holds only for a top-level script with no pipe and no
// substitution whose every segment is a sink (isSink), so nothing else in the same call can run or
// read what was written; a script with any other command is matched in full. Data written to a
// file can still be read back by a later call, which the guard judges on its own: for the harness
// config, the SSH keys and the loop's own config the OS sandbox also denies that read, but the
// sandbox can read the user's checkouts, so for those the guard is the only check.
func (c *Config) checkProtected(sc script, depth int, target string) Decision {
	plain := depth == 0 && !sc.piped && len(sc.subs) == 0
	for _, sg := range sc.segs {
		plain = plain && isSink(sg)
	}
	var texts []string
	for _, sg := range sc.segs {
		first := ""
		if len(sg.words) > 0 {
			first = sg.words[0]
		}
		words := sg.words
		if plain && (first == "echo" || first == "printf") {
			words = words[:1]
		}
		texts = append(texts, words...)
		texts = append(texts, sg.targets...)
		for _, b := range sg.bodies {
			data := b.quoted || !strings.ContainsAny(b.text, "$`")
			if !(plain && data && (first == "cat" || first == "tee")) {
				texts = append(texts, b.text)
			}
		}
	}
	for _, p := range c.Protected {
		for _, f := range c.pathForms(filepath.Clean(p)) {
			for _, t := range texts {
				if mentions(unmark(t), f) {
					return deny(RuleProtectedPath, target, "the command names "+f+", which is the user's (checkouts, credentials and loopd are off limits)")
				}
			}
		}
	}
	return Decision{}
}

// isSink reports whether a segment only writes data out: echo, printf without -v (which assigns a
// variable; bash also takes it joined, as -vq), cat or tee reading a here-document, or mkdir.
func isSink(sg segment) bool {
	if len(sg.words) == 0 {
		return false
	}
	switch sg.words[0] {
	case "echo", "mkdir":
		return true
	case "printf":
		for _, w := range sg.words[1:] {
			if strings.HasPrefix(w, "-v") {
				return false
			}
		}
		return true
	case "cat", "tee":
		return len(sg.bodies) > 0
	}
	return false
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

var pidRe = regexp.MustCompile(`^[0-9]+$`)

// killZero reports whether kill's arguments are "-0" and then only pids: literal numbers, or
// $(cat <file>) where the file (readable to the agent, not a secret) holds exactly one pid now.
// Signal 0 sends nothing; it only asks whether the process exists. The file is read because the
// substitution's words reach kill unchecked: a file holding "-s 9 1" would send a signal.
func (c *Config) killZero(args, subs []string, cwd string, movedDir bool) bool {
	if len(args) < 2 || args[0] != "-0" {
		return false
	}
	for _, a := range args[1:] {
		if pidRe.MatchString(a) {
			continue
		}
		n, ok := marked(a)
		if !ok || n >= len(subs) {
			return false
		}
		segs, inner, err := parseRaw(subs[n])
		if err != nil || len(inner) > 0 || len(segs) != 1 || len(segs[0]) != 2 || segs[0][0] != "cat" {
			return false
		}
		file := segs[0][1]
		if strings.ContainsAny(file, "$`*?[") || (movedDir && !filepath.IsAbs(c.expandHome(file))) {
			return false
		}
		if c.checkRead(file, cwd).Deny {
			return false
		}
		b, err := readSmall(c.resolve(file, cwd), 64)
		if err != nil || !pidRe.MatchString(strings.TrimSpace(string(b))) {
			return false
		}
	}
	return true
}

func readSmall(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, n+1))
	if err == nil && int64(len(b)) > n {
		return nil, errors.New("too long")
	}
	return b, err
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
	case "tee", "touch", "mkdir", "truncate", "cp", "mv", "install", "ln", "dd", "sed":
		for _, dst := range writeDests(name, argv[1:]) {
			if d := c.checkWriteTarget(dst, cwd, target, movedDir); d.Deny {
				return d
			}
		}
	}
	return Decision{}
}

// writeDests are the paths a write-shaped command changes: a short explicit list, like never and
// checkRm, not a heuristic. Sources are left alone on purpose: protected files are legitimately
// read, run and copied out (cat scripts/sandbox/run, go run ./tools/pipeline/x), so only the
// write-shaped positions may refuse (#537).
func writeDests(name string, args []string) []string {
	switch name {
	case "tee", "touch", "mkdir":
		return nonFlags(args)
	case "truncate":
		var out []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			if (a == "-s" || a == "-r" || a == "--size" || a == "--reference") && i+1 < len(args) {
				i++
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
			out = append(out, a)
		}
		return out
	case "cp", "mv", "install", "ln":
		// -t names the destination directory; otherwise it is the last path argument.
		for i, a := range args {
			if (a == "-t" || a == "--target-directory") && i+1 < len(args) {
				return []string{args[i+1]}
			}
			if v, ok := strings.CutPrefix(a, "--target-directory="); ok {
				return []string{v}
			}
		}
		if ps := nonFlags(args); len(ps) > 1 {
			return ps[len(ps)-1:]
		}
		return nil
	case "dd":
		var out []string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, v)
			}
		}
		return out
	case "sed":
		inPlace := false
		for _, a := range args {
			if a == "--in-place" || strings.HasPrefix(a, "--in-place=") || (strings.HasPrefix(a, "-i") && !strings.HasPrefix(a, "--")) {
				inPlace = true
			}
		}
		if !inPlace {
			return nil
		}
		// The file arguments; without -e or -f the first non-flag argument is the script.
		ps := nonFlags(args)
		for _, a := range args {
			if strings.HasPrefix(a, "-e") || strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "--expression") || strings.HasPrefix(a, "--file") {
				return ps
			}
		}
		if len(ps) > 0 {
			ps = ps[1:]
		}
		return ps
	}
	return nil
}

// nonFlags are the arguments that are not options (everything after -- is kept).
func nonFlags(args []string) []string {
	var out []string
	opts := true
	for _, a := range args {
		switch {
		case opts && a == "--":
			opts = false
		case opts && strings.HasPrefix(a, "-") && a != "-":
		default:
			out = append(out, a)
		}
	}
	return out
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
	if c.ReadOnly && writesHistory(sub, rest) {
		return deny(RuleGitCommit, target, "a triage or review run never commits or rewrites history (git "+sub+"); say what should change in your result instead")
	}
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

// writesHistory reports whether a git subcommand makes commits or moves refs other than HEAD's
// checkout: what a read-only run may not do.
func writesHistory(sub string, rest []string) bool {
	switch sub {
	case "commit", "commit-tree", "merge", "rebase", "cherry-pick", "revert", "am", "update-ref", "filter-branch", "replace", "notes":
		return true
	case "tag":
		for _, a := range rest {
			if a == "-l" || a == "--list" {
				return false
			}
		}
		return len(rest) > 0
	}
	return false
}

func (c *Config) checkPush(args []string, target string) Decision {
	return deny(RuleGitPush, target, "agents never push; commit on the item's branch and write outcome \"ready_for_pr\": the runner pushes it and opens the PR")
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
	case "pr create", "pr edit":
		return deny(RuleGH, target, "the runner opens and updates the PR; write outcome \"ready_for_pr\" with pr_title and pr_body")
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
