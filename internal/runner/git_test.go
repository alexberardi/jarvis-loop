package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRig is a "GitHub" bare repo with main, and the runner's mirror of it.
type gitRig struct {
	t                    *testing.T
	ctx                  context.Context
	g                    Git
	remote, mirror, seed string
}

func newGitRig(t *testing.T) gitRig {
	t.Helper()
	root := t.TempDir()
	r := gitRig{t: t, ctx: context.Background(), remote: filepath.Join(root, "remote.git"), mirror: filepath.Join(root, "mirror.git"),
		seed: filepath.Join(root, "seed")}
	os.MkdirAll(r.remote, 0o755)
	os.MkdirAll(r.seed, 0o755)
	gitT(t, r.remote, "init", "--bare", "-q", "-b", "main", ".")
	gitT(t, r.seed, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(r.seed, "a.txt"), []byte("a\n"), 0o644)
	gitT(t, r.seed, "add", ".")
	gitT(t, r.seed, "commit", "-q", "-m", "init")
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// commitIn makes a commit in a worktree.
func (r gitRig) commitIn(wt, file string) string {
	os.WriteFile(filepath.Join(wt, file), []byte(file+"\n"), 0o644)
	gitT(r.t, wt, "add", file)
	gitT(r.t, wt, "commit", "-q", "-m", "add "+file)
	return gitT(r.t, wt, "rev-parse", "HEAD")
}

func TestPushBranch(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	head := r.commitIn(wt, "b.txt")
	for _, bad := range []string{"main", "master", "loop/1-x:main", "+loop/1-x", "loop/1-x main", "-f", "loop/../main", "refs/heads/main", "loop/2-not-there"} {
		if err := r.g.PushBranch(r.ctx, r.mirror, bad); err == nil {
			t.Errorf("pushed %q", bad)
		}
	}
	if s := gitT(t, r.remote, "branch", "--list"); strings.TrimSpace(s) != "main" && strings.TrimSpace(s) != "* main" {
		t.Fatalf("the remote got branches: %q", s)
	}
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.g.RemoteHead(r.ctx, r.mirror, "loop/1-x"); err != nil || got != head {
		t.Fatalf("remote head %q %v, want %s", got, err, head)
	}
	if got, err := r.g.RemoteHead(r.ctx, r.mirror, "loop/9-none"); err != nil || got != "" {
		t.Fatalf("a branch the remote lacks: %q %v", got, err)
	}
	// Someone else moved the remote branch: the runner's push is refused, never forced.
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", "-b", "loop/1-x", r.remote, other)
	theirs := r.commitIn(other, "theirs.txt")
	gitT(t, other, "push", "-q", "origin", "loop/1-x")
	r.commitIn(wt, "c.txt")
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err == nil {
		t.Fatal("a non-fast-forward push went through")
	}
	if h := gitT(t, r.remote, "rev-parse", "refs/heads/loop/1-x"); h != theirs {
		t.Fatalf("the remote branch was overwritten: %s", h)
	}
}

func TestAddWorktreeResumes(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	// Fresh: from the base, nothing ahead.
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil || n != 0 {
		t.Fatalf("fresh: %d %v", n, err)
	}
	// An earlier run committed but never pushed (a question): the mirror's branch is continued.
	first := r.commitIn(wt, "b.txt")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil || n != 1 || gitT(t, wt, "rev-parse", "HEAD") != first {
		t.Fatalf("local resume: %d %v", n, err)
	}
	// Pushed, then the remote moved on (a WIP push from elsewhere): the remote, which contains the
	// mirror's branch, wins.
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", "-b", "loop/1-x", r.remote, other)
	ahead := r.commitIn(other, "c.txt")
	gitT(t, other, "push", "-q", "origin", "loop/1-x")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil || n != 2 || gitT(t, wt, "rev-parse", "HEAD") != ahead {
		t.Fatalf("remote resume: %d %v", n, err)
	}
	// Diverged: refused, so neither side's commits are lost.
	r.commitIn(wt, "local.txt")
	r.commitIn(other, "remote.txt")
	gitT(t, other, "push", "-q", "origin", "loop/1-x")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged: %v", err)
	}
}

// rebaseRig is #155: the PR's head was pushed (prev), main moved on, and a build rebased the
// branch in the mirror (the new head waited on the user's Trust, unpushed).
func rebaseRig(t *testing.T) (r gitRig, wt, prev, rebased string) {
	r = newGitRig(t)
	wt = filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	prev = r.commitIn(wt, "b.txt")
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err != nil {
		t.Fatal(err)
	}
	r.commitIn(r.seed, "main2.txt")
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	gitT(t, wt, "rebase", "-q", "origin/main")
	rebased = gitT(t, wt, "rev-parse", "HEAD")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	return r, wt, prev, rebased
}

func TestAddWorktreeContinuesARecordedRebase(t *testing.T) {
	r, wt, prev, rebased := rebaseRig(t)
	// Not recorded (no Trust for the rebased head, or the remote is not the PR's head): refused.
	for name, rec := range map[string]Recorded{
		"nothing recorded":    {},
		"rebase not recorded": {Prev: prev, Heads: []string{prev}},
		"remote not the PR's": {Prev: rebased, Heads: []string{rebased}},
		"no previous head":    {Heads: []string{rebased}},
	} {
		if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", rec); err == nil || !strings.Contains(err.Error(), "diverged") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The mirror's head is the one the user trusted and the remote is the PR's previous head: the
	// run continues the rebased branch.
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{Prev: prev, Heads: []string{"", rebased}}); err != nil || n != 1 ||
		gitT(t, wt, "rev-parse", "HEAD") != rebased {
		t.Fatalf("recorded rebase: %d %v", n, err)
	}
	if !r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", prev) {
		t.Fatal("the rebase over the PR's head is not a rewrite")
	}
	if r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", rebased) || r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", "") {
		t.Fatal("a rewrite of a head the remote is not at")
	}
	if rd, err := r.g.RangeDiff(r.ctx, r.mirror, "main", prev, rebased); err != nil || !strings.Contains(rd, "1:") || !strings.Contains(rd, "add b.txt") {
		t.Fatalf("range-diff %q %v", rd, err)
	}
	// A plain push is refused (not a fast-forward); the lease pinned to the PR's head goes through.
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err == nil {
		t.Fatal("a rewrite went through a plain push")
	}
	if err := r.g.ForcePushBranch(r.ctx, r.mirror, "loop/1-x", prev); err != nil {
		t.Fatal(err)
	}
	if h := gitT(t, r.remote, "rev-parse", "refs/heads/loop/1-x"); h != rebased {
		t.Fatalf("remote %s, want the rebased %s", h, rebased)
	}
	if r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", prev) {
		t.Fatal("still a rewrite after the push")
	}
}

func TestForcePushBranchHoldsTheLease(t *testing.T) {
	r, wt, prev, _ := rebaseRig(t)
	// Someone else pushed over the PR's head: the lease pinned to it fails, nothing is lost.
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", "-b", "loop/1-x", r.remote, other)
	theirs := r.commitIn(other, "theirs.txt")
	gitT(t, other, "push", "-q", "origin", "loop/1-x")
	if err := r.g.ForcePushBranch(r.ctx, r.mirror, "loop/1-x", prev); err == nil {
		t.Fatal("a stale lease went through")
	}
	if h := gitT(t, r.remote, "rev-parse", "refs/heads/loop/1-x"); h != theirs {
		t.Fatalf("the remote branch was overwritten: %s", h)
	}
	// The remote has a commit the loop never recorded: still diverged, even with the rebase trusted.
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	rebased := gitT(t, r.mirror, "rev-parse", "refs/heads/loop/1-x")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{Prev: prev, Heads: []string{rebased}}); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("foreign commit: %v", err)
	}
	if r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", prev) {
		t.Fatal("a foreign remote head counted as the PR's")
	}
	for _, bad := range []string{"main", "loop/1-x:main", "+loop/1-x", "-f"} {
		if err := r.g.ForcePushBranch(r.ctx, r.mirror, bad, prev); err == nil {
			t.Errorf("force-pushed %q", bad)
		}
	}
	if err := r.g.ForcePushBranch(r.ctx, r.mirror, "loop/1-x", ""); err == nil {
		t.Error("a forced push with no lease")
	}
}

func TestFactsSeeUncommittedWork(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{})
	r.commitIn(wt, "b.txt")
	os.MkdirAll(filepath.Join(wt, ".loop", "evidence"), 0o755)
	os.WriteFile(filepath.Join(wt, ".loop", "result.json"), []byte("{}"), 0o644)
	f, err := r.g.Facts(r.ctx, r.mirror, wt, "main", "loop/1-x")
	if err != nil || len(f.Dirty) != 0 || len(f.ChangedPaths) != 1 {
		t.Fatalf("clean (the runner's .loop/ is not work): %+v %v", f, err)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new\n"), 0o644)
	f, _ = r.g.Facts(r.ctx, r.mirror, wt, "main", "loop/1-x")
	if strings.Join(f.Dirty, "|") != "M a.txt|?? new.txt" {
		t.Fatalf("dirty %q", f.Dirty)
	}
}

// A worktree's config may name commands git runs by itself (core.fsmonitor, hooks); the runner's
// git, which runs unfenced after the agent, runs none of them.
func TestGitRunsNoPlantedCommand(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{})
	ran := filepath.Join(t.TempDir(), "ran")
	planted := filepath.Join(t.TempDir(), "planted.sh")
	os.WriteFile(planted, []byte("#!/bin/sh\ntouch "+ran+"\n"), 0o755)
	gitT(t, wt, "config", "core.fsmonitor", planted)
	os.MkdirAll(filepath.Join(r.mirror, "hooks"), 0o755)
	os.WriteFile(filepath.Join(r.mirror, "hooks", "pre-commit"), []byte("#!/bin/sh\ntouch "+ran+"\n"), 0o755)
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b\n"), 0o644)
	if _, err := r.g.Facts(r.ctx, r.mirror, wt, "main", "loop/1-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.g.run(r.ctx, wt, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.g.run(r.ctx, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatalf("a planted command ran: %v", err)
	}
}

// An agent's own git in its worktree (switch -c, commit, config) holds the mirror's config.lock
// for a moment; the runner's git call that meets it waits and retries instead of failing (#31's
// land: "could not lock config file config: File exists").
func TestGitRetriesALockHeldByAnotherProcess(t *testing.T) {
	r := newGitRig(t)
	lock := filepath.Join(r.mirror, "config.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(400 * time.Millisecond); os.Remove(lock) }()
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote + "/", Base: "main"}); err != nil {
		t.Fatalf("refresh while another git held config.lock: %v", err)
	}
	if got := gitT(t, r.mirror, "config", "remote.origin.url"); got != r.remote+"/" {
		t.Errorf("remote.origin.url %q, want %q", got, r.remote+"/")
	}
}

// A lock that never goes away still fails, with git's message, once the retries run out.
func TestGitGivesUpOnAStuckLock(t *testing.T) {
	r := newGitRig(t)
	os.WriteFile(filepath.Join(r.mirror, "config.lock"), nil, 0o644)
	start := time.Now()
	err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote + "/", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "could not lock config file") {
		t.Fatalf("err %v, want git's lock error", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("gave up after %v; the retries should be bounded", d)
	}
}

// A refresh writes the mirror's config only when a value changed: every write takes
// config.lock, which a parallel run's refresh then fails on.
func TestEnsureMirrorLeavesConfigAlone(t *testing.T) {
	r := newGitRig(t)
	cfg := filepath.Join(r.mirror, "config")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(cfg, old, old)
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(cfg); !fi.ModTime().Equal(old) {
		t.Errorf("config rewritten on a refresh with the same remote (mtime %v)", fi.ModTime())
	}
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote + "/", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, r.mirror, "config", "remote.origin.url"); got != r.remote+"/" {
		t.Errorf("remote.origin.url %q after a change, want %q", got, r.remote+"/")
	}
}

// The mirror's own config keeps signing and hooks off for the agents' commits in worktrees, even
// though the runner's git calls pass the same values as -c overrides.
func TestMirrorConfigTurnsSigningOff(t *testing.T) {
	r := newGitRig(t)
	for key, want := range map[string]string{"commit.gpgsign": "false", "tag.gpgsign": "false", "core.hooksPath": "/dev/null"} {
		if got := gitT(t, r.mirror, "config", "--local", "--get", key); got != want {
			t.Errorf("mirror %s = %q, want %q", key, got, want)
		}
	}
}

// Claude Code's sandbox creates empty read-only placeholder files to mount over the paths it
// protects, the mirror's config.lock among them, and one left behind blocked every git call on
// the mirror ("could not lock config file config: File exists", 2026-10-16). The runner removes
// that placeholder; git's own lock (created writable) is never touched.
func TestEnsureMirrorClearsASandboxPlaceholderLock(t *testing.T) {
	r := newGitRig(t)
	lock := filepath.Join(r.mirror, "config.lock")
	os.WriteFile(lock, nil, 0o444)
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote + "/", Base: "main"}); err != nil {
		t.Fatalf("refresh with a sandbox placeholder lock: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("placeholder config.lock still there (%v)", err)
	}
	os.WriteFile(lock, []byte("[core]\n"), 0o644) // a git process's own lock, mid-write
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err == nil {
		t.Error("refresh succeeded through another git's live lock")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("git's own lock was removed: %v", err)
	}
}

// Starting an item's branch from origin/<base> must not write tracking config: the mirror's
// config is shared, and agents' sandboxes keep a placeholder config.lock on it while they run.
func TestAddWorktreeWritesNoConfig(t *testing.T) {
	r := newGitRig(t)
	cfg := filepath.Join(r.mirror, "config")
	before, _ := os.ReadFile(cfg)
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, filepath.Join(t.TempDir(), "wt"), "loop/9-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Errorf("AddWorktree changed the mirror's config:\n%s", after)
	}
}
