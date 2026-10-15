package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Git runs git for the runner. Every call turns hooks and signing off: the worktree is the
// agent's, so a hook it planted must never run with the runner's (unsandboxed) rights.
type Git struct {
	// Env is git's environment; nil means the runner's own (the user's git credentials, which the
	// runner's push needs), minus loopd's and Slack's secrets.
	Env []string
}

func (g Git) env() []string {
	src := g.Env
	if src == nil {
		src = scrubEnv(os.Environ())
	}
	return append(append([]string{}, src...), "GIT_TERMINAL_PROMPT=0")
}

func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = g.env()
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

// AddWorktree makes a fresh worktree on branch and says how many commits the branch already has
// over the base. It continues the item's branch when an earlier run left one: in the mirror (the
// agent's commits, pushed or not) or on the remote (a WIP push, or a bounced PR), whichever
// contains the other. If they diverged, it refuses: one of them would be lost.
func (g Git) AddWorktree(ctx context.Context, mirror, path, branch, base string) (int, error) {
	g.RemoveWorktree(ctx, mirror, path)
	baseRef := "refs/remotes/origin/" + base
	if !g.hasRef(ctx, mirror, baseRef) {
		return 0, fmt.Errorf("the remote has no branch %s", base)
	}
	local, remote := "refs/heads/"+branch, "refs/remotes/origin/"+branch
	hasLocal, hasRemote := g.hasRef(ctx, mirror, local), g.hasRef(ctx, mirror, remote)
	start := baseRef
	switch {
	case hasLocal && hasRemote:
		switch {
		case g.isAncestor(ctx, mirror, remote, local):
			start = local
		case g.isAncestor(ctx, mirror, local, remote):
			start = remote
		default:
			return 0, fmt.Errorf("%s in the runner's mirror and on the remote have diverged; reconcile them by hand", branch)
		}
	case hasLocal:
		start = local
	case hasRemote:
		start = remote
	}
	ahead := 0
	if start != baseRef {
		n, err := g.run(ctx, mirror, "rev-list", "--count", baseRef+".."+start)
		if err != nil {
			return 0, err
		}
		ahead, _ = strconv.Atoi(n)
		if ahead == 0 {
			start = baseRef // nothing of its own: start fresh from the base
		}
	}
	if start == local {
		_, err := g.run(ctx, mirror, "worktree", "add", "-q", path, branch)
		return ahead, err
	}
	_, err := g.run(ctx, mirror, "worktree", "add", "-q", "-B", branch, path, start)
	return ahead, err
}

// AddDetached makes a fresh worktree at rev, detached: triage's checkouts and review's PR head.
// rev must already be in the mirror.
func (g Git) AddDetached(ctx context.Context, mirror, path, rev string) error {
	g.RemoveWorktree(ctx, mirror, path)
	if !g.hasRef(ctx, mirror, rev+"^{commit}") {
		return fmt.Errorf("the mirror has no commit %s", rev)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := g.run(ctx, mirror, "worktree", "add", "-q", "--detach", path, rev)
	return err
}

func (g Git) isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := g.run(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
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
	st, err := g.run(ctx, wt, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return f, err
	}
	for _, l := range strings.Split(st, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			f.Dirty = append(f.Dirty, l)
		}
	}
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

// PushBranch pushes the item's branch from the runner's mirror to its remote, with the runner's
// (the user's) credentials: only refs/heads/loop/..., to the same name, never forced. A push the
// remote refuses as not a fast-forward fails; nothing is overwritten.
func (g Git) PushBranch(ctx context.Context, mirror, branch string) error {
	if !strings.HasPrefix(branch, "loop/") || strings.ContainsAny(branch, ":+ \t\n*?[\\^~") || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("the runner pushes only an item's loop/ branch, not %q", branch)
	}
	ref := "refs/heads/" + branch
	if _, err := g.run(ctx, mirror, "check-ref-format", ref); err != nil {
		return fmt.Errorf("%q is not a branch name: %w", branch, err)
	}
	if !g.hasRef(ctx, mirror, ref) {
		return fmt.Errorf("the mirror has no branch %s", branch)
	}
	_, err := g.run(ctx, mirror, "push", "-q", "--no-verify", "origin", ref+":"+ref)
	return err
}

// RemoteHead fetches and returns the remote's head of branch ("" when it has none).
func (g Git) RemoteHead(ctx context.Context, mirror, branch string) (string, error) {
	if _, err := g.run(ctx, mirror, "fetch", "--no-tags", "-q", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		if !g.hasRef(ctx, mirror, "refs/remotes/origin/"+branch) {
			return "", nil
		}
		return "", err
	}
	return g.run(ctx, mirror, "rev-parse", "refs/remotes/origin/"+branch)
}

// DiffPaths are the files head changes against base (`git diff --name-status -M
// origin/<base>...<head>`), from the runner's mirror. head must be in the mirror; when it is not
// (a branch GitHub deleted after a merge), it is fetched from the PR's ref first.
func (g Git) DiffPaths(ctx context.Context, mirror, base, head string, pr int) ([]string, error) {
	if !g.hasRef(ctx, mirror, head+"^{commit}") && pr > 0 {
		_, _ = g.run(ctx, mirror, "fetch", "--no-tags", "-q", "origin", fmt.Sprintf("+refs/pull/%d/head:refs/loop/pr/%d", pr, pr))
	}
	if !g.hasRef(ctx, mirror, head+"^{commit}") {
		return nil, fmt.Errorf("the mirror has no commit %s", head)
	}
	out, err := g.run(ctx, mirror, "diff", "--name-status", "-M", "refs/remotes/origin/"+base+"..."+head)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}
