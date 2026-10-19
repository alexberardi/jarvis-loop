package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// unionDocs is the attributes file land's merge uses when the only conflicts are under docs/:
// git's union merge driver keeps both sides' lines. It is passed with -c core.attributesFile, so
// nothing is written into the mirror or the branch.
const unionDocs = "docs/** merge=union\n"

// landConflict handles a PR GitHub calls CONFLICTING: it merges the base into the branch in a
// scratch worktree of the runner's mirror (mergeBase). A merge git does cleanly, or one whose
// conflicts are all under docs/, is pushed to the item's branch and goes back to review as a new
// head. A conflict anywhere else goes back to build.
func (r *Runner) landConflict(ctx context.Context, lr *landRun, f LandFacts) (Decision, []string) {
	wt := filepath.Join(r.Config().WorkRoot, fmt.Sprintf("%d-land", lr.item.ID))
	head, how, conflict, err := r.mergeBase(ctx, lr.mirror, lr.repo, wt, f.PR.HeadRefOid, f.Branch, f.PR.Number)
	if err != nil {
		return failed("land: %s", err.Error()), nil
	}
	if conflict != "" {
		return advance("back_to_build", map[string]string{"reason": conflict}), f.Paths
	}
	paths, err := r.Git.DiffPaths(ctx, lr.mirror, lr.repo.Base, head, 0)
	if err != nil {
		return failed("land pushed the merge `%s` but could not diff it: %v", short7(head), err), nil
	}
	return advance("back_to_review", map[string]string{"head_sha": head,
		"notes": fmt.Sprintf("Land merged %s into the branch (%s); the new head `%s` needs a review.", lr.repo.Base, how, short7(head))}), paths
}

// mergeBase merges origin/<base> into prHead in the scratch worktree wt of mirror, under the
// mirror's lock. A merge git does cleanly, or one whose conflicts are all under docs/ (resolved by
// git's union driver, both sides kept; no -X strategy option), is pushed to branch (only
// loop/<item>-…, never forced): it returns the new head and how it merged. A conflict anywhere
// else returns conflict, the reason the merge needs a build, and pushes nothing.
func (r *Runner) mergeBase(ctx context.Context, mirror string, rc RepoConfig, wt, prHead, branch string, pr int) (head, how, conflict string, err error) {
	base := "refs/remotes/origin/" + rc.Base
	unlock := r.lockMirror(mirror)
	defer unlock()
	defer r.Git.RemoveWorktree(context.WithoutCancel(ctx), mirror, wt)
	if err := r.Git.AddDetached(ctx, mirror, wt, prHead); err != nil {
		return "", "", "", fmt.Errorf("could not check out PR #%d to merge %s in: %s", pr, rc.Base, clip(err.Error(), 600))
	}
	msg := fmt.Sprintf("Merge %s into %s (loop land)", rc.Base, branch)
	ident := []string{"-c", "user.name=jarvis-loop", "-c", "user.email=jarvis-loop@localhost"}
	merge := func(extra ...string) error {
		args := append(append(append([]string{}, ident...), extra...), "merge", "--no-ff", "--no-edit", "-m", msg, base)
		_, err := r.Git.run(ctx, wt, args...)
		return err
	}
	how = "git merged it cleanly"
	if err := merge(); err != nil {
		out, _ := r.Git.run(ctx, wt, "diff", "--name-only", "--diff-filter=U")
		conflicted := strings.Fields(out)
		_, _ = r.Git.run(ctx, wt, "merge", "--abort")
		if len(conflicted) == 0 {
			return "", "", fmt.Sprintf("PR #%d does not merge with %s: %s", pr, rc.Base, clip(err.Error(), 400)), nil
		}
		if outside := outsideDocs(conflicted); len(outside) > 0 {
			return "", "", fmt.Sprintf("Merge conflict with %s outside docs/: %s", rc.Base, strings.Join(outside, ", ")), nil
		}
		attrs := filepath.Join(r.Config().WorkRoot, "land-union.attributes")
		if err := os.WriteFile(attrs, []byte(unionDocs), 0o600); err != nil {
			return "", "", "", err
		}
		if err := merge("-c", "core.attributesFile="+attrs); err != nil {
			_, _ = r.Git.run(ctx, wt, "merge", "--abort")
			return "", "", fmt.Sprintf("Merge conflict with %s in docs/ that git could not resolve: %s", rc.Base, strings.Join(conflicted, ", ")), nil
		}
		how = "conflicts only in docs/ (" + strings.Join(conflicted, ", ") + "), both sides kept by git's union merge"
	}
	head, err = r.Git.run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return "", "", "", err
	}
	// The item's branch in the mirror moves to the merge only forward (it may be absent, or behind
	// the PR's head when the build ran elsewhere).
	ref := "refs/heads/" + branch
	if r.Git.hasRef(ctx, mirror, ref) && !r.Git.isAncestor(ctx, mirror, ref, head) {
		return "", "", "", fmt.Errorf("the mirror's %s is not behind the merge; reconcile it by hand", branch)
	}
	if _, err := r.Git.run(ctx, mirror, "update-ref", ref, head); err != nil {
		return "", "", "", err
	}
	push := r.Push
	if push == nil {
		push = r.Git.PushBranch
	}
	if err := push(ctx, mirror, branch); err != nil {
		return "", "", "", fmt.Errorf("could not push the merge of %s into %s: %s", rc.Base, branch, clip(err.Error(), 600))
	}
	if remote, err := r.Git.RemoteHead(ctx, mirror, branch); err != nil || remote != head {
		return "", "", "", fmt.Errorf("after the push, %s on the remote is %q, not the merge %s", branch, remote, head)
	}
	return head, how, "", nil
}
