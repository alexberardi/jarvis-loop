package runner

import (
	"context"
	"fmt"
	"strconv"
)

// LandView reads the PR for the gates (gh pr view): state, head, base, mergeability, body and the
// status rollup of its head commit, in one consistent answer.
func (g GH) LandView(ctx context.Context, slug string, pr int) (LandPR, error) {
	out, err := g.run(ctx, "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", landViewFields)
	if err != nil {
		return LandPR{}, err
	}
	return ParseLandView([]byte(out))
}

// Merge squash-merges the PR, only at head (--match-head-commit: GitHub refuses if the head moved
// since the gates read it), and deletes the branch. It never passes --auto (which would merge
// later, unchecked) or --admin (which would skip the base branch's rules). With --repo, gh deletes
// only the remote branch, never a local one.
func (g GH) Merge(ctx context.Context, slug string, pr int, head string) error {
	if pr <= 0 {
		return fmt.Errorf("no PR to merge")
	}
	if !shaRe.MatchString(head) || len(head) != 40 {
		return fmt.Errorf("merge needs the full head commit the gates checked, not %q", head)
	}
	_, err := g.run(ctx, "pr", "merge", strconv.Itoa(pr), "--repo", slug, "--squash", "--delete-branch", "--match-head-commit", head)
	return err
}

// CommitRuns lists the Actions runs on a commit (gh run list --commit).
func (g GH) CommitRuns(ctx context.Context, slug, sha string) ([]WorkflowRun, error) {
	if !shaRe.MatchString(sha) {
		return nil, fmt.Errorf("not a commit sha: %q", sha)
	}
	out, err := g.run(ctx, "run", "list", "--repo", slug, "--commit", sha, "--limit", "50", "--json", "name,workflowName,status,conclusion,url")
	if err != nil {
		return nil, err
	}
	return ParseRuns([]byte(out))
}
