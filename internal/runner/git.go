package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git for the runner. Every call turns hooks and signing off: the worktree is the
// agent's, so a hook it planted must never run with the runner's (unsandboxed) rights.
type Git struct {
	Env []string
}

func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, g.Env...), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, clip(strings.TrimSpace(errb.String()), 500))
	}
	return strings.TrimSpace(out.String()), nil
}

// EnsureMirror creates or refreshes the runner's own bare mirror of a repo: objects seeded from
// the user's clone when given (read only), then fetched from the remote. The user's clone is
// never written.
func (g Git) EnsureMirror(ctx context.Context, mirror string, r RepoConfig) error {
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(mirror, 0o700); err != nil {
			return err
		}
		if _, err := g.run(ctx, mirror, "init", "--bare", "-q", "."); err != nil {
			return err
		}
		for _, kv := range [][2]string{
			{"remote.origin.url", r.Remote},
			{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
			{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"},
			{"core.hooksPath", "/dev/null"},
		} {
			if _, err := g.run(ctx, mirror, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(mirror, "info"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mirror, "info", "exclude"), []byte("# the runner's files, never committed\n.loop/\n"), 0o644); err != nil {
			return err
		}
	} else if _, err := g.run(ctx, mirror, "config", "remote.origin.url", r.Remote); err != nil {
		return err
	}
	if r.Local != "" {
		if _, err := os.Stat(r.Local); err == nil {
			// Only objects travel; a failure here just means a slower fetch below.
			_, _ = g.run(ctx, mirror, "fetch", "--no-tags", "-q", r.Local, "+refs/heads/"+r.Base+":refs/loop/seed")
		}
	}
	_, err := g.run(ctx, mirror, "fetch", "--no-tags", "--prune", "-q", "origin")
	return err
}

func (g Git) hasRef(ctx context.Context, dir, ref string) bool {
	_, err := g.run(ctx, dir, "rev-parse", "--verify", "-q", ref)
	return err == nil
}

// AddWorktree makes a fresh worktree on branch: from the remote branch when an earlier run pushed
// one, else from the base.
func (g Git) AddWorktree(ctx context.Context, mirror, path, branch, base string) error {
	g.RemoveWorktree(ctx, mirror, path)
	start := "refs/remotes/origin/" + base
	if g.hasRef(ctx, mirror, "refs/remotes/origin/"+branch) {
		start = "refs/remotes/origin/" + branch
	}
	if !g.hasRef(ctx, mirror, start) {
		return fmt.Errorf("the remote has no branch %s", base)
	}
	_, err := g.run(ctx, mirror, "worktree", "add", "-q", "-B", branch, path, start)
	return err
}

// RemoveWorktree removes a worktree the runner made (and nothing else).
func (g Git) RemoveWorktree(ctx context.Context, mirror, path string) {
	_, _ = g.run(ctx, mirror, "worktree", "remove", "--force", path)
	_ = os.RemoveAll(path)
	_, _ = g.run(ctx, mirror, "worktree", "prune")
}

// Facts computes HEAD, the pushed head of the branch and the changed paths, from git.
func (g Git) Facts(ctx context.Context, mirror, wt, base, branch string) (GitFacts, error) {
	var f GitFacts
	if _, err := g.run(ctx, mirror, "fetch", "--no-tags", "--prune", "-q", "origin"); err != nil {
		return f, err
	}
	head, err := g.run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return f, err
	}
	f.HeadSHA = head
	if rh, err := g.run(ctx, mirror, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch); err == nil {
		f.RemoteHead = rh
	}
	diff, err := g.run(ctx, wt, "diff", "--name-status", "-M", "refs/remotes/origin/"+base+"...HEAD")
	if err != nil {
		return f, err
	}
	for _, l := range strings.Split(diff, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			f.ChangedPaths = append(f.ChangedPaths, l)
		}
	}
	return f, nil
}

// SaveWIP commits whatever is in the worktree as WIP and pushes it to the item's branch only
// (never forced). It reports whether something was pushed.
func (g Git) SaveWIP(ctx context.Context, wt, branch, base, msg string) (bool, error) {
	if _, err := g.run(ctx, wt, "add", "-A"); err != nil { // .loop/ is excluded in the mirror
		return false, err
	}
	if _, err := g.run(ctx, wt, "diff", "--cached", "--quiet"); err != nil {
		if _, err := g.run(ctx, wt, "-c", "user.name=jarvis-loop", "-c", "user.email=jarvis-loop@localhost",
			"commit", "-q", "--no-verify", "-m", msg); err != nil {
			return false, err
		}
	}
	n, err := g.run(ctx, wt, "rev-list", "--count", "refs/remotes/origin/"+base+"..HEAD")
	if err != nil || n == "0" {
		return false, err
	}
	if _, err := g.run(ctx, wt, "push", "-q", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return false, err
	}
	return true, nil
}
