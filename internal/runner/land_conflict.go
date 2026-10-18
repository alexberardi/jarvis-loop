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
// scratch worktree of the runner's mirror. A merge git does cleanly, or one whose conflicts are
// all under docs/ (resolved by git's union driver, both sides kept; no -X strategy option), is
// pushed to the item's branch (only loop/<item>-…, never forced) and goes back to review as a new
// head. A conflict anywhere else goes back to build.
func (r *Runner) landConflict(ctx context.Context, lr *landRun, f LandFacts) (Decision, []string) {
	base := "refs/remotes/origin/" + lr.repo.Base
	wt := filepath.Join(r.Config().WorkRoot, fmt.Sprintf("%d-land", lr.item.ID))
	unlock := r.lockMirror(lr.mirror)
	defer unlock()
	defer r.Git.RemoveWorktree(context.WithoutCancel(ctx), lr.mirror, wt)
	if err := r.Git.AddDetached(ctx, lr.mirror, wt, f.PR.HeadRefOid); err != nil {
		return failed("land could not check out PR #%d to merge %s in: %s", f.PR.Number, lr.repo.Base, clip(err.Error(), 600)), nil
	}
	msg := fmt.Sprintf("Merge %s into %s (loop land)", lr.repo.Base, f.Branch)
	ident := []string{"-c", "user.name=jarvis-loop", "-c", "user.email=jarvis-loop@localhost"}
	merge := func(extra ...string) error {
		args := append(append(append([]string{}, ident...), extra...), "merge", "--no-ff", "--no-edit", "-m", msg, base)
		_, err := r.Git.run(ctx, wt, args...)
		return err
	}
	how := "git merged it cleanly"
	if err := merge(); err != nil {
		out, _ := r.Git.run(ctx, wt, "diff", "--name-only", "--diff-filter=U")
		conflicted := strings.Fields(out)
		_, _ = r.Git.run(ctx, wt, "merge", "--abort")
		if len(conflicted) == 0 {
			return advance("back_to_build", map[string]string{"reason": fmt.Sprintf("PR #%d does not merge with %s: %s", f.PR.Number, lr.repo.Base, clip(err.Error(), 400))}), f.Paths
		}
		if outside := outsideDocs(conflicted); len(outside) > 0 {
			return advance("back_to_build", map[string]string{"reason": fmt.Sprintf("Merge conflict with %s outside docs/: %s", lr.repo.Base, strings.Join(outside, ", "))}), f.Paths
		}
		attrs := filepath.Join(r.Config().WorkRoot, "land-union.attributes")
		if err := os.WriteFile(attrs, []byte(unionDocs), 0o600); err != nil {
			return failed("land: %v", err), nil
		}
		if err := merge("-c", "core.attributesFile="+attrs); err != nil {
			_, _ = r.Git.run(ctx, wt, "merge", "--abort")
			return advance("back_to_build", map[string]string{"reason": fmt.Sprintf("Merge conflict with %s in docs/ that git could not resolve: %s", lr.repo.Base, strings.Join(conflicted, ", "))}), f.Paths
		}
		how = "conflicts only in docs/ (" + strings.Join(conflicted, ", ") + "), both sides kept by git's union merge"
	}
	head, err := r.Git.run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return failed("land: %v", err), nil
	}
	// The item's branch in the mirror moves to the merge only forward (it may be absent, or behind
	// the PR's head when the build ran elsewhere).
	ref := "refs/heads/" + f.Branch
	if r.Git.hasRef(ctx, lr.mirror, ref) && !r.Git.isAncestor(ctx, lr.mirror, ref, head) {
		return failed("land: the mirror's %s is not behind the merge; reconcile it by hand", f.Branch), nil
	}
	if _, err := r.Git.run(ctx, lr.mirror, "update-ref", ref, head); err != nil {
		return failed("land: %v", err), nil
	}
	push := r.Push
	if push == nil {
		push = r.Git.PushBranch
	}
	if err := push(ctx, lr.mirror, f.Branch); err != nil {
		return failed("land could not push the merge of %s into %s: %s", lr.repo.Base, f.Branch, clip(err.Error(), 600)), nil
	}
	if remote, err := r.Git.RemoteHead(ctx, lr.mirror, f.Branch); err != nil || remote != head {
		return failed("after the push, %s on the remote is %q, not the merge %s", f.Branch, remote, head), nil
	}
	paths, err := r.Git.DiffPaths(ctx, lr.mirror, lr.repo.Base, head, 0)
	if err != nil {
		return failed("land pushed the merge `%s` but could not diff it: %v", short7(head), err), nil
	}
	return advance("back_to_review", map[string]string{"head_sha": head,
		"notes": fmt.Sprintf("Land merged %s into the branch (%s); the new head `%s` needs a review.", lr.repo.Base, how, short7(head))}), paths
}
