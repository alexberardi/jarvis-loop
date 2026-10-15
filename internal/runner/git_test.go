package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main"); err != nil {
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
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main"); err != nil || n != 0 {
		t.Fatalf("fresh: %d %v", n, err)
	}
	// An earlier run committed but never pushed (a question): the mirror's branch is continued.
	first := r.commitIn(wt, "b.txt")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main"); err != nil || n != 1 || gitT(t, wt, "rev-parse", "HEAD") != first {
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
	if n, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main"); err != nil || n != 2 || gitT(t, wt, "rev-parse", "HEAD") != ahead {
		t.Fatalf("remote resume: %d %v", n, err)
	}
	// Diverged: refused, so neither side's commits are lost.
	r.commitIn(wt, "local.txt")
	r.commitIn(other, "remote.txt")
	gitT(t, other, "push", "-q", "origin", "loop/1-x")
	r.g.RemoveWorktree(r.ctx, r.mirror, wt)
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main"); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged: %v", err)
	}
}

func TestFactsSeeUncommittedWork(t *testing.T) {
	r := newGitRig(t)
	wt := filepath.Join(t.TempDir(), "wt")
	r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main")
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
