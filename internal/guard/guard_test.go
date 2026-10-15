package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture lays out a home with a user checkout and secrets, a work root with the item's worktree
// and scratch dir, and a directory outside everything; plus symlinks that point out of the
// worktree.
type fixture struct {
	home, work, wt, scratch, outside, checkout string
	cfg                                        *Config
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{
		home:    filepath.Join(root, "home"),
		work:    filepath.Join(root, "home", ".cache", "jarvis-loop", "work"),
		outside: filepath.Join(root, "outside"),
	}
	f.wt = filepath.Join(f.work, "12-build")
	f.scratch = filepath.Join(f.work, "12-build.scratch")
	f.checkout = filepath.Join(f.home, "jarvis", "jarvis-server")
	for _, d := range []string{f.wt, f.scratch, f.outside, f.checkout, filepath.Join(f.home, ".ssh"),
		filepath.Join(f.home, ".config", "jarvis-harness"), filepath.Join(f.wt, "internal"), filepath.Join(f.wt, ".loop", "evidence")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink in the worktree that leads out of it, and one that stays inside.
	if err := os.Symlink(f.outside, filepath.Join(f.wt, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.wt, "internal"), filepath.Join(f.wt, "inner")); err != nil {
		t.Fatal(err)
	}
	f.cfg = &Config{
		Item: 12, Worktree: f.wt, Scratch: f.scratch, SandboxDir: filepath.Join(f.wt, ".sandbox", "loop-12"),
		Branch: "loop/12-fix-x", Remote: "origin", Home: f.home,
		Protected: []string{f.checkout, filepath.Join(f.home, ".config", "jarvis-harness"),
			filepath.Join(f.home, ".config", "jarvis-loop"), filepath.Join(f.home, ".ssh")},
		Secrets: []string{filepath.Join(f.home, ".config", "jarvis-harness"), filepath.Join(f.home, ".config", "jarvis-loop"),
			filepath.Join(f.home, ".ssh")},
		Words:      []string{"emulator-5554"},
		LoopdAddrs: []string{"127.0.0.1:7780", "localhost:7780"},
		DenyWrite:  []string{"scripts/sandbox"},
	}
	return f
}

func bash(f fixture, cmd string) Input {
	return Input{Event: "PreToolUse", ToolName: "Bash", Cwd: f.wt, ToolInput: map[string]any{"command": cmd}}
}

func file(tool, key, path string, f fixture) Input {
	return Input{Event: "PreToolUse", ToolName: tool, Cwd: f.wt, ToolInput: map[string]any{key: path}}
}

func TestFileTools(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		in   Input
		rule string // "" = allowed (deferred to the normal permission flow)
	}{
		{"write in worktree", file("Write", "file_path", filepath.Join(f.wt, "internal", "x.go"), f), ""},
		{"edit in worktree", file("Edit", "file_path", filepath.Join(f.wt, "README.md"), f), ""},
		{"write a new dir in worktree", file("Write", "file_path", filepath.Join(f.wt, "new", "deep", "x.go"), f), ""},
		{"result file", file("Write", "file_path", filepath.Join(f.wt, ".loop", "result.json"), f), ""},
		{"evidence file", file("Write", "file_path", filepath.Join(f.wt, ".loop", "evidence", "a.txt"), f), ""},
		{"write in scratch", file("Write", "file_path", filepath.Join(f.scratch, "notes.md"), f), ""},
		{"write in sandbox dir", file("Write", "file_path", filepath.Join(f.wt, ".sandbox", "loop-12", "x"), f), ""},
		{"relative path resolves against cwd", file("Write", "file_path", "internal/y.go", f), ""},
		{"write outside", file("Write", "file_path", filepath.Join(f.outside, "x"), f), RuleWriteOutside},
		{"dotdot escape", file("Write", "file_path", filepath.Join(f.wt, "internal", "..", "..", "x"), f), RuleWriteOutside},
		{"dotdot staying inside", file("Edit", "file_path", filepath.Join(f.wt, "internal", "..", "README.md"), f), ""},
		{"symlink escape", file("Write", "file_path", filepath.Join(f.wt, "escape", "x"), f), RuleWriteOutside},
		{"symlink escape, new subdir", file("Write", "file_path", filepath.Join(f.wt, "escape", "a", "b"), f), RuleWriteOutside},
		{"symlink staying inside", file("Write", "file_path", filepath.Join(f.wt, "inner", "z.go"), f), ""},
		{"sibling with the worktree as prefix", file("Write", "file_path", f.wt+"-evil/x", f), RuleWriteOutside},
		{"user checkout", file("Edit", "file_path", filepath.Join(f.checkout, "main.go"), f), RuleWriteOutside},
		{"multiedit outside", file("MultiEdit", "file_path", filepath.Join(f.home, ".bashrc"), f), RuleWriteOutside},
		{"notebook outside", file("NotebookEdit", "notebook_path", filepath.Join(f.outside, "n.ipynb"), f), RuleWriteOutside},
		{"git dir in worktree", file("Write", "file_path", filepath.Join(f.wt, ".git"), f), RuleWriteProtected},
		{"claude settings in worktree", file("Write", "file_path", filepath.Join(f.wt, ".claude", "settings.json"), f), RuleWriteProtected},
		{"mcp json in worktree", file("Write", "file_path", filepath.Join(f.wt, ".mcp.json"), f), RuleWriteProtected},
		{"configured deny_write", file("Edit", "file_path", filepath.Join(f.wt, "scripts", "sandbox"), f), RuleWriteProtected},
		{"brief is the runner's", file("Write", "file_path", filepath.Join(f.wt, ".loop", "brief.md"), f), RuleWriteProtected},
		{"unknown tool with a file_path is a write", file("FutureWrite", "file_path", filepath.Join(f.outside, "x"), f), RuleWriteOutside},
		{"missing path", Input{ToolName: "Write", Cwd: f.wt, ToolInput: map[string]any{}}, RuleUnparseable},
		{"read in worktree", file("Read", "file_path", filepath.Join(f.wt, "README.md"), f), ""},
		{"read elsewhere is the permission system's", file("Read", "file_path", "/etc/hostname", f), ""},
		{"read a secret", file("Read", "file_path", filepath.Join(f.home, ".ssh", "id_ed25519"), f), RuleReadSecret},
		{"read a secret through a symlink", Input{}, ""}, // filled below
		{"grep in harness creds", file("Grep", "path", filepath.Join(f.home, ".config", "jarvis-harness"), f), RuleReadSecret},
		{"glob pattern into secrets", Input{ToolName: "Glob", Cwd: f.wt, ToolInput: map[string]any{"pattern": filepath.Join(f.home, ".ssh") + "/*"}}, RuleReadSecret},
		{"glob in worktree", Input{ToolName: "Glob", Cwd: f.wt, ToolInput: map[string]any{"pattern": "**/*.go"}}, ""},
		{"todo tool", Input{ToolName: "TodoWrite", Cwd: f.wt, ToolInput: map[string]any{"todos": []any{}}}, ""},
	}
	// A symlink in the worktree to the ssh dir.
	if err := os.Symlink(filepath.Join(f.home, ".ssh"), filepath.Join(f.wt, "keys")); err != nil {
		t.Fatal(err)
	}
	cases[28].in = file("Read", "file_path", filepath.Join(f.wt, "keys", "id_ed25519"), f)
	cases[28].rule = RuleReadSecret
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := f.cfg.Check(c.in)
			if d.Rule != c.rule {
				t.Fatalf("rule = %q (%s), want %q", d.Rule, d.Reason, c.rule)
			}
			if d.Deny != (c.rule != "") {
				t.Fatalf("deny = %v", d.Deny)
			}
			if d.Deny && d.Reason == "" {
				t.Fatal("a refusal needs a reason the agent can read")
			}
		})
	}
}

func TestBashCommands(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		cmd  string
		rule string
	}{
		// Ordinary work.
		{"go test ./...", ""},
		{"CGO_ENABLED=0 go build ./... && go vet ./...", ""},
		{"git status && git diff --stat", ""},
		{"git add -A && git commit -m 'fix: x'", ""},
		{"gh pr view 3 --json state", ""},
		{"gh pr checks 3", ""},
		{"echo 'gh pr merge is not allowed' > notes.txt", ""},
		{"rm -rf build/ .loop/evidence/old", ""},
		{"rm -rf " + f.scratch + "/tmp", ""},
		{"rm -f " + f.outside + "/x", ""}, // not recursive: the sandbox decides
		{"ls ~/go/pkg/mod", ""},

		// Pushing: never. The runner pushes the item's branch after the agent ends.
		{"git push origin loop/12-fix-x", RuleGitPush},
		{"git push -u origin HEAD:loop/12-fix-x", RuleGitPush},
		{"git push origin HEAD:refs/heads/loop/12-fix-x", RuleGitPush},
		{"git push --set-upstream origin loop/12-fix-x", RuleGitPush},
		{"git push --dry-run origin loop/12-fix-x", RuleGitPush},
		{"git commit -m 'git push later' && git log", ""},
		{"git push", RuleGitPush},
		{"git push origin", RuleGitPush},
		{"git push origin main", RuleGitPush},
		{"git push origin HEAD:main", RuleGitPush},
		{"git push origin HEAD:refs/heads/master", RuleGitPush},
		{"git push origin loop/12-fix-x main", RuleGitPush},
		{"git push --force origin loop/12-fix-x", RuleGitPush},
		{"git push -f origin loop/12-fix-x", RuleGitPush},
		{"git push --force-with-lease origin loop/12-fix-x", RuleGitPush},
		{"git push origin +loop/12-fix-x", RuleGitPush},
		{"git push origin loop/13-other", RuleGitPush},
		{"git push upstream loop/12-fix-x", RuleGitPush},
		{"git push https://github.com/x/y.git loop/12-fix-x", RuleGitPush},
		{"git push --mirror origin", RuleGitPush},
		{"git push --all origin", RuleGitPush},
		{"git push origin --delete loop/12-fix-x", RuleGitPush},
		{"git push origin :loop/12-fix-x", RuleGitPush},
		{"git -C . push origin main", RuleGitPush},
		{"git -c push.default=current push origin main", RuleGitPush},
		{"cd sub && git push origin master", RuleGitPush},
		{"echo $(git push origin main)", RuleGitPush},
		{"echo `git push origin main`", RuleGitPush},
		{"bash -c 'git push origin main'", RuleGitPush},
		{"sh -c \"git push --force origin loop/12-fix-x\"", RuleGitPush},
		{"eval git push origin main", RuleGitPush},
		{"FOO=1 timeout 30 git push origin main", RuleGitPush},
		{"/usr/bin/git push origin main", RuleGitPush},
		{"git remote add evil https://github.com/x/y.git", RuleGitRemote},
		{"git remote set-url origin https://github.com/x/y.git", RuleGitRemote},
		{"git remote -v", ""},
		{"git config user.name x", RuleGitConfig},
		{"git config --get user.name", ""},
		{"git worktree add ../x", RuleGitWorktree},

		// gh.
		{"gh pr merge 3 --squash", RuleGH},
		{"gh pr merge --auto 3", RuleGH},
		{"gh pr review 3 --approve", RuleGH},
		{"gh pr review 3 -a", RuleGH},
		{"gh -R alexberardi/jarvis-server pr merge 3", RuleGH},
		{"gh repo delete alexberardi/jarvis-server --yes", RuleGH},
		{"gh repo edit --default-branch x", RuleGH},
		{"gh api -X DELETE repos/alexberardi/jarvis-server", RuleGH},
		{"gh api --method PUT repos/o/r/branches/main/protection", RuleGH},
		{"gh api --method=PATCH repos/o/r", RuleGH},
		{"gh api -XDELETE repos/o/r/git/refs/heads/x", RuleGH},
		{"gh api repos/o/r/merges -f base=main -f head=x", RuleGH},
		{"gh api graphql -f query='mutation { mergePullRequest(input: {}) { clientMutationId } }'", RuleGH},
		{"gh api repos/o/r/pulls/3", ""},
		{"gh auth token", RuleGH},
		{"gh auth status", RuleGH},
		// PRs: the runner opens and updates them.
		{"gh pr create --title 'x' --body 'y' --head loop/12-fix-x", RuleGH},
		{"gh -R o/r pr create --fill", RuleGH},
		{"gh pr edit 3 --body 'x'", RuleGH},
		{"gh pr list --head loop/12-fix-x", ""},
		{"gh secret set X", RuleGH},

		// Never.
		{"sudo apt install x", RuleNever},
		{"pkexec ls", RuleNever},
		{"systemctl --user restart jarvisd", RuleNever},
		{"kill 1234", RuleNever},
		{"kill", RuleNever},
		{"kill -9 1234", RuleNever},
		{"kill -TERM 1234", RuleNever},
		{"kill -s 0 1234", RuleNever},
		{"kill -0", RuleNever},
		{"kill -0 -s 9 1234", RuleNever},
		{"kill -0 1234 && kill 1234", RuleNever},
		{"pkill -f jarvisd", RuleNever},
		{"killall node", RuleNever},
		{"adb devices", RuleNever},
		{"adb -s emulator-5556 shell ls", RuleNever},
		{"ssh prod uptime", RuleNever},
		{"scp a prod:/tmp", RuleNever},
		{"go test ./... | xargs kill", RuleNever},

		// rm -rf outside the worktree.
		{"rm -rf " + f.outside, RuleRmOutside},
		{"rm -rf ../", RuleRmOutside},
		{"rm -rf ..", RuleRmOutside},
		{"rm -Rf escape/", RuleRmOutside},
		{"rm -r -f ~/go", RuleRmOutside},
		{"rm --recursive /", RuleRmOutside},
		{"rm -rf $HOME/x", RuleRmOutside},
		{"rm -rf $SOMEVAR", RuleRmOutside},
		{"rm -rf /*", RuleRmOutside},
		{"cd .. && rm -rf x", RuleRmOutside}, // cd moves the shell, so a relative path is unknowable: refused

		// Protected paths and words, in any spelling the shell would expand.
		{"cat " + f.checkout + "/go.mod", RuleProtectedPath},
		{"ls " + f.checkout, RuleProtectedPath},
		{"cp x ~/jarvis/jarvis-server/", RuleProtectedPath},
		{"cd $HOME/jarvis/jarvis-server && git status", RuleProtectedPath},
		{"cat ${HOME}/.config/jarvis-harness/slack.env", RuleProtectedPath},
		{"cat ~/.config/jarvis-loop/tokens", RuleProtectedPath},
		{"cat ~/.ssh/id_ed25519", RuleProtectedPath},
		{"ls " + f.checkout + "-wt", ""}, // a different directory that shares the prefix
		{"scripts/mobile-shot --serial emulator-5554", RuleProtectedWord},
		{"curl http://127.0.0.1:7780/items", RuleLoopd},
		{"wget -qO- localhost:7780/control", RuleLoopd},
		{"python3 -c 'import urllib.request as u; u.urlopen(\"http://127.0.0.1:7780/\")'", RuleLoopd},

		// Fail closed.
		{"echo 'unterminated", RuleUnparseable},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			d := f.cfg.Check(bash(f, c.cmd))
			if d.Rule != c.rule {
				t.Fatalf("rule = %q (%s), want %q", d.Rule, d.Reason, c.rule)
			}
			if d.Deny != (c.rule != "") {
				t.Fatalf("deny = %v", d.Deny)
			}
		})
	}
}

func TestPushRefusalSaysWhoPushes(t *testing.T) {
	f := newFixture(t)
	d := f.cfg.Check(bash(f, "git push origin loop/12-fix-x"))
	if d.Rule != RuleGitPush || !strings.Contains(d.Reason, "ready_for_pr") {
		t.Fatalf("got %+v", d)
	}
}

// kill -0 sends no signal (a liveness check), so it is allowed with literal pids, or with the pid
// read from a file whose content is a single pid right now. Every other kill stays refused.
func TestKillZero(t *testing.T) {
	f := newFixture(t)
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(f.wt, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent.pid", "4242\n")
	write("two.pid", "1 2\n")
	write("evil.pid", "-s 9 1\n")
	write("empty.pid", "")
	cases := []struct {
		cmd  string
		rule string
	}{
		{"kill -0 4242", ""},
		{"kill -0 4242 4243", ""},
		{"kill -0 $(cat agent.pid)", ""},
		{`kill -0 "$(cat agent.pid)"`, ""},
		{"kill -0 `cat agent.pid`", ""},
		{"kill -0 $(cat agent.pid) 2>/dev/null && echo up || echo down", ""},
		{"test -f agent.pid && kill -0 $(cat " + filepath.Join(f.wt, "agent.pid") + ")", ""},
		{"kill -0 $(cat two.pid)", RuleNever},     // word splitting: more than one pid in the file
		{"kill -0 $(cat evil.pid)", RuleNever},    // options through the substitution
		{"kill -0 $(cat empty.pid)", RuleNever},   // not a pid
		{"kill -0 $(cat missing.pid)", RuleNever}, // nothing to check
		{"kill -0 $(cat agent.pid two.pid)", RuleNever},
		{"kill -0 $(head -1 agent.pid)", RuleNever},
		{"kill -0 $(pgrep jarvisd)", RuleNever},
		{"kill -0 $PID", RuleNever},
		{"kill -0 -1", RuleNever},
		{"kill -0 %1", RuleNever},
		{"kill -0 -- 4242", RuleNever},
		{"kill -00 4242", RuleNever},
		{"/bin/kill -0 4242", RuleNever},
		{"env kill -0 4242", RuleNever},
		{"echo 4242 | xargs kill -0", RuleNever},
		{"FOO=1 kill -0 4242", RuleNever},
		{"kill -0 $(cat ~/.ssh/agent.pid)", RuleProtectedPath},
		{"kill -0 4242; kill -9 4242", RuleNever},
		{"pkill -0 jarvisd", RuleNever},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			d := f.cfg.Check(bash(f, c.cmd))
			if d.Rule != c.rule || d.Deny != (c.rule != "") {
				t.Fatalf("rule = %q (%s), want %q", d.Rule, d.Reason, c.rule)
			}
		})
	}
}

func TestReasonsNameTheRule(t *testing.T) {
	f := newFixture(t)
	d := f.cfg.Check(bash(f, "gh pr merge 3"))
	if !strings.Contains(d.Reason, "merge") || !strings.Contains(d.Reason, "loop-guard") {
		t.Fatalf("reason %q should say what and who", d.Reason)
	}
}

// Triage and review runs are read-only for git history: no commit, merge or rewrite, in any
// spelling; reading, diffing and checking out (to compare against the base) stay allowed. A build
// run commits as before.
func TestReadOnlyRuns(t *testing.T) {
	f := newFixture(t)
	ro := *f.cfg
	ro.ReadOnly = true
	cases := []struct {
		cmd  string
		rule string
	}{
		{"git status && git diff origin/main...HEAD", ""},
		{"git log --oneline -5", ""},
		{"git show HEAD --stat", ""},
		{"git checkout origin/main -- internal/x_test.go && go test ./internal/... ; git checkout HEAD -- internal/x_test.go", ""},
		{"git stash && go test ./... ; git stash pop", ""},
		{"git tag -l", ""},
		{"git commit -m 'fix'", RuleGitCommit},
		{"git add -A && git commit -qm x", RuleGitCommit},
		{"git -c user.name=x commit --amend --no-edit", RuleGitCommit},
		{"git -C . commit -m x", RuleGitCommit},
		{"bash -c 'git commit -m x'", RuleGitCommit},
		{"echo $(git commit -m x)", RuleGitCommit},
		{"/usr/bin/git commit -m x", RuleGitCommit},
		{"git commit-tree HEAD^{tree} -m x", RuleGitCommit},
		{"git merge origin/main", RuleGitCommit},
		{"git rebase origin/main", RuleGitCommit},
		{"git cherry-pick abc123", RuleGitCommit},
		{"git revert HEAD", RuleGitCommit},
		{"git am < x.patch", RuleGitCommit},
		{"git tag v1", RuleGitCommit},
		{"git update-ref refs/heads/x HEAD", RuleGitCommit},
		{"git push origin HEAD", RuleGitPush},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			d := ro.Check(bash(f, c.cmd))
			if d.Rule != c.rule || d.Deny != (c.rule != "") {
				t.Fatalf("rule = %q (%s), want %q", d.Rule, d.Reason, c.rule)
			}
			if c.rule == RuleGitCommit && !strings.Contains(d.Reason, "never commits") {
				t.Fatalf("reason %q should say why", d.Reason)
			}
		})
	}
	// The same commands in a build run: commits are the build's job.
	for _, cmd := range []string{"git commit -m x", "git merge origin/main", "git rebase origin/main"} {
		if d := f.cfg.Check(bash(f, cmd)); d.Deny {
			t.Errorf("build run refused %q: %+v", cmd, d)
		}
	}
}
