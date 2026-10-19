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

// staleMirrorRig: the mirror pushed loop/1-x (old), then "runner B" rebased it onto a newer main
// from its own clone and force-pushed it (recorded, the head loopd has), so this mirror's branch is
// stale (#283).
func staleMirrorRig(t *testing.T) (r gitRig, wt, old, recorded string) {
	r = newGitRig(t)
	wt = filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	old = r.commitIn(wt, "b.txt")
	if err := r.g.PushBranch(r.ctx, r.mirror, "loop/1-x"); err != nil {
		t.Fatal(err)
	}
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	r.commitIn(r.seed, "main2.txt")
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, "", "clone", "-q", r.remote, other)
	gitT(t, other, "checkout", "-q", "loop/1-x")
	gitT(t, other, "-c", "user.name=b", "-c", "user.email=b@x", "rebase", "-q", "origin/main")
	recorded = gitT(t, other, "rev-parse", "HEAD")
	gitT(t, other, "push", "-q", "-f", "origin", "loop/1-x")
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err != nil {
		t.Fatal(err)
	}
	return r, wt, old, recorded
}

func TestAddWorktreeResetsAStaleMirrorToTheRecordedHead(t *testing.T) {
	r, wt, old, recorded := staleMirrorRig(t)
	// The remote is not the head loopd recorded (someone else pushed), or nothing is recorded:
	// refused as before, nothing reset.
	for name, rec := range map[string]Recorded{
		"nothing recorded":       {},
		"the mirror's is the PR": {Prev: old},
		"another head":           {Prev: strings.Repeat("a", 40)},
	} {
		if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", rec); err == nil || !strings.Contains(err.Error(), "diverged") {
			t.Fatalf("%s: %v", name, err)
		}
		if h := gitT(t, r.mirror, "rev-parse", "refs/heads/loop/1-x"); h != old {
			t.Fatalf("%s: the mirror's branch moved to %s", name, h)
		}
	}
	// The remote is the recorded head: it is the truth. The mirror's branch is kept under
	// refs/backup/ and reset to the remote's.
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{Prev: recorded}); err != nil || n != 1 {
		t.Fatalf("stale mirror: %d %v", n, err)
	}
	if h := gitT(t, wt, "rev-parse", "HEAD"); h != recorded {
		t.Fatalf("HEAD %s, want the remote's %s", h, recorded)
	}
	if h := gitT(t, r.mirror, "rev-parse", "refs/heads/loop/1-x"); h != recorded {
		t.Fatalf("the mirror's branch is %s, want %s", h, recorded)
	}
	backup := BackupRef("loop/1-x", old)
	if backup != "refs/backup/1-"+old {
		t.Fatalf("backup ref %q", backup)
	}
	if h := gitT(t, r.mirror, "rev-parse", backup); h != old {
		t.Fatalf("backup %s, want %s", h, old)
	}
	if r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", recorded) {
		t.Fatal("continuing the remote's head is not a rewrite")
	}
	// The next refresh keeps the backup.
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	if err := r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if h := gitT(t, r.mirror, "rev-parse", backup); h != old {
		t.Fatalf("backup after a refresh %s, want %s", h, old)
	}
}

func TestAddWorktreeContinuesAnUnpushedEquivalentRebase(t *testing.T) {
	// This runner rebased the PR's head onto a newer main and its push failed (#241, #258): the
	// mirror's commits are the remote's patches on a newer base, and the remote is the recorded
	// head. The run continues the rebase; publish pushes it with the lease.
	r, wt, prev, rebased := rebaseRig(t)
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{Prev: prev}); err != nil || n != 1 ||
		gitT(t, wt, "rev-parse", "HEAD") != rebased {
		t.Fatalf("equivalent rebase: %d %v", n, err)
	}
	if !r.g.Rewrites(r.ctx, r.mirror, "loop/1-x", prev) {
		t.Fatal("the rebase over the PR's head is not a rewrite")
	}
	if gitT(t, r.mirror, "for-each-ref", "refs/backup/") != "" {
		t.Fatal("a backup for a branch that was not reset")
	}
}

func TestAddWorktreeResetsADifferentLocalRewrite(t *testing.T) {
	// The mirror's rewrite of the PR's head has other patches (here, a commit more): not the
	// recorded head's, so the remote wins and the mirror's is kept as a backup.
	r, wt, prev, rebased := rebaseRig(t)
	gitT(t, r.mirror, "worktree", "add", "-q", wt, "loop/1-x")
	extra := r.commitIn(wt, "c.txt")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{Prev: prev}); err != nil || n != 1 ||
		gitT(t, wt, "rev-parse", "HEAD") != prev {
		t.Fatalf("different rewrite: %d %v", n, err)
	}
	if h := gitT(t, r.mirror, "rev-parse", BackupRef("loop/1-x", extra)); h != extra {
		t.Fatalf("backup %s, want %s (over %s)", h, extra, rebased)
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

// plantReadOnly puts a Go module cache (read-only dirs and files, as `go` leaves it) under the
// worktree's scratch, as an agent running Go with HOME there did on the dev box (#276).
func plantReadOnly(t *testing.T, wt string) {
	t.Helper()
	ro := filepath.Join(wt, ".loop", "scratch", "home", "go", "pkg", "mod", "github.com", "coder", "websocket@v1.8.15")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o444)
	os.Chmod(ro, 0o555)
	os.Chmod(filepath.Dir(ro), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Dir(ro), 0o755); os.Chmod(ro, 0o755) })
}

// A leftover worktree holding read-only dirs must not stop the next AddWorktree (#276).
func TestAddWorktreeOverReadOnlyLeftover(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "217-build")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	plantReadOnly(t, wt)
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatalf("second AddWorktree: %v", err)
	}
}

// The same for a leftover that git no longer knows as a worktree (pruned, only the dir is left).
func TestAddDetachedOverReadOnlyLeftover(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "217-review")
	plantReadOnly(t, wt)
	if err := r.g.AddDetached(r.ctx, r.mirror, wt, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("AddDetached: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveWorktreeLeavesNothing(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	plantReadOnly(t, wt)
	if err := r.g.RemoveWorktree(r.ctx, r.mirror, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
}

// removeTree makes only directories inside path writable: a symlink in the tree is not followed.
func TestRemoveTreeDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644)
	os.Chmod(outside, 0o555)
	t.Cleanup(func() { os.Chmod(outside, 0o755) })
	tree := filepath.Join(root, "tree")
	os.MkdirAll(tree, 0o755)
	os.Symlink(outside, filepath.Join(tree, "link"))
	if err := removeTree(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree still there: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("outside changed: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal(err)
	}
}
