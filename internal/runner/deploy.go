package runner

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): an item of a repo with
// qa_after_deploy (the loop itself) waits in qa until a loop build that contains its merge commit
// is running. The runner knows the commit it was built from; every poll it asks loopd for the qa
// items still waiting, checks in the repo's mirror whether its own build contains each merge, and
// reports deployed for those. loopd and the runner are deployed together, so the runner's build
// stands for the loop's.

// BuildRevision is the commit this binary was built from (Go's vcs.revision stamp), or "" and why
// not: no stamp (go run, go install pkg@version, a build outside a checkout) or a build with
// uncommitted changes, which contains no commit for certain.
func BuildRevision() (rev, why string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "no build info"
	}
	modified := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	switch {
	case rev == "":
		return "", "the binary has no vcs.revision stamp"
	case modified:
		return "", "the binary was built from a checkout with uncommitted changes (vcs.modified)"
	}
	return rev, ""
}

// ReportDeployed reports deployed for each qa item awaiting a deploy whose merge commit this
// runner's build (Revision) contains. Items of repos without qa_after_deploy here, or of repos
// this runner does not have, are left to other runners; a failure leaves the item for the next
// pass.
func (r *Runner) ReportDeployed(ctx context.Context) error {
	if r.Revision == "" {
		return nil
	}
	rows, err := r.Client.AwaitingDeploy(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		key, repo, ok := r.Cfg.Repo(row.Repo)
		if !ok || !repo.QAAfterDeploy {
			continue
		}
		log := r.Log.With("item", row.ID, "repo", repo.Slug, "revision", short7(r.Revision))
		d, err := r.Client.Detail(ctx, row.ID)
		if err != nil {
			log.Warn("deployed: read item", "err", err)
			continue
		}
		in, err := r.buildContains(ctx, key, repo, d.Item.MergeSHA)
		if err != nil {
			log.Warn("deployed: ancestry check", "merge_sha", short7(d.Item.MergeSHA), "err", err)
			continue
		}
		if !in {
			continue // a later build will
		}
		err = r.Client.Event(ctx, row.ID, "deployed", map[string]any{"revision": r.Revision})
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "wrong_stage" {
			err = nil // another runner got there first, or the user already checked it
		}
		if err != nil {
			log.Warn("deployed event", "err", err)
			continue
		}
		log.Info("reported deployed", "merge_sha", short7(d.Item.MergeSHA))
	}
	return nil
}

// buildContains says whether the merge commit is an ancestor of (or is) this runner's build, in
// the repo's mirror. The mirror is fetched only when it lacks one of the two commits; a revision
// the remote does not have either (a local build) is an error.
func (r *Runner) buildContains(ctx context.Context, key string, repo RepoConfig, merge string) (bool, error) {
	if merge == "" {
		return false, fmt.Errorf("the item has no merge commit")
	}
	mirror := r.mirrorPath(key)
	unlock := r.lockMirror(mirror)
	defer unlock()
	has := func(sha string) bool { return r.Git.hasRef(ctx, mirror, sha+"^{commit}") }
	if !has(merge) || !has(r.Revision) {
		if err := r.Git.EnsureMirror(ctx, mirror, repo); err != nil {
			return false, err
		}
		for _, sha := range []string{merge, r.Revision} {
			if !has(sha) {
				return false, fmt.Errorf("commit %s is not on %s", short7(sha), repo.Remote)
			}
		}
	}
	return r.Git.isAncestor(ctx, mirror, merge, r.Revision), nil
}

// reportDeployedLoop runs ReportDeployed every poll until ctx ends.
func (r *Runner) reportDeployedLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.ReportDeployed(ctx); err != nil {
			r.Log.Warn("report deployed", "err", err)
		}
		t := r.Clock.NewTimer(time.Duration(r.Cfg.PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
