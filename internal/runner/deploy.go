package runner

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): an item of a repo with
// qa_after_deploy (the loop itself) waits in qa until a loop build that contains its merge commit
// is running. Every poll the runner asks loopd for the qa items still waiting, and reports
// deployed for those whose merge the running loop contains.
//
// The running loop is two builds, not one: loopd is deployed on its own since it moved to fly.io
// (docs/decisions/2026-10-23-loopd-on-fly.md), while the runner is deployed on the dev box. So an
// item is deployed only once BOTH contain its merge: the runner asks loopd for its revision
// (GET /version) and checks both against the repo's mirror. The revision it reports is loopd's,
// which is the build the user is asked to check.

// ReportDeployed reports deployed for each qa item awaiting a deploy whose merge commit both the
// running loopd and this runner's own build contain. Items of repos without qa_after_deploy here,
// or of repos this runner does not have, are left to other runners; a failure leaves the item for
// the next pass.
func (r *Runner) ReportDeployed(ctx context.Context) error {
	rows, err := r.Client.AwaitingDeploy(ctx)
	if err != nil {
		return err
	}
	// The repos whose awaiting items this runner can judge; loopd is asked once per pass, and
	// only when there is something to judge.
	var mine []BoardRow
	repos := map[string]RepoConfig{}
	keys := map[string]string{}
	for _, row := range rows {
		key, repo, ok := r.Config().Repo(row.Repo)
		if !ok || !repo.QAAfterDeploy {
			continue
		}
		mine = append(mine, row)
		repos[row.Repo], keys[row.Repo] = repo, key
	}
	if len(mine) == 0 {
		return nil
	}
	// Either build unknown: report nothing, said once rather than every poll, as the runner's own
	// missing stamp always has. The user falls back to /loop verified.
	if r.Revision == "" {
		if !r.noOwnRev.Swap(true) {
			r.Log.Warn("this runner's build revision is unknown: no deployed reports for qa_after_deploy repos (use /loop verified)")
		}
		return nil
	}
	r.noOwnRev.Store(false)
	loopdRev, err := r.loopdRevision(ctx)
	if err != nil || loopdRev == "" {
		return err
	}
	for _, row := range mine {
		repo, key := repos[row.Repo], keys[row.Repo]
		log := r.Log.With("item", row.ID, "repo", repo.Slug, "revision", short7(loopdRev), "runner_revision", short7(r.Revision))
		d, err := r.Client.Detail(ctx, row.ID)
		if err != nil {
			log.Warn("deployed: read item", "err", err)
			continue
		}
		in, err := r.buildContains(ctx, key, repo, d.Item.MergeSHA, loopdRev, r.Revision)
		if err != nil {
			log.Warn("deployed: ancestry check", "merge_sha", short7(d.Item.MergeSHA), "err", err)
			continue
		}
		if !in {
			continue // a later build will
		}
		err = r.Client.Event(ctx, row.ID, "deployed", map[string]any{"revision": loopdRev})
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

// loopdRevision is the commit the running loopd was built from (GET /version), or "" when it has
// none, said once rather than every poll: until loopd is redeployed there is nothing to do.
func (r *Runner) loopdRevision(ctx context.Context) (string, error) {
	b, err := r.Client.Version(ctx)
	if err != nil {
		return "", err
	}
	if b.Revision == "" {
		if !r.noLoopdRev.Swap(true) {
			r.Log.Warn("loopd's build revision is unknown: no deployed reports for qa_after_deploy repos (use /loop verified)", "why", b.Why)
		}
		return "", nil
	}
	r.noLoopdRev.Store(false)
	return b.Revision, nil
}

// hasQAAfterDeployRepo says whether any repo this runner has needs deployed reports; without one,
// Run starts no deployed-report loop.
func (r *Runner) hasQAAfterDeployRepo() bool {
	for _, repo := range r.Config().Repos {
		if repo.QAAfterDeploy {
			return true
		}
	}
	return false
}

// buildContains says whether the merge commit is an ancestor of (or is) every one of the running
// loop's builds, in the repo's mirror. The mirror is fetched only when it lacks one of the
// commits; a revision the remote does not have either (a local build) is an error.
func (r *Runner) buildContains(ctx context.Context, key string, repo RepoConfig, merge string, builds ...string) (bool, error) {
	if merge == "" {
		return false, fmt.Errorf("the item has no merge commit")
	}
	mirror := r.mirrorPath(key)
	unlock := r.lockMirror(mirror)
	defer unlock()
	has := func(sha string) bool { return r.Git.hasRef(ctx, mirror, sha+"^{commit}") }
	want := append([]string{merge}, builds...)
	fetch := false
	for _, sha := range want {
		if !has(sha) {
			fetch = true
		}
	}
	if fetch {
		if err := r.Git.EnsureMirror(ctx, mirror, repo); err != nil {
			return false, err
		}
		for _, sha := range want {
			if !has(sha) {
				return false, fmt.Errorf("commit %s is not on %s", short7(sha), repo.Remote)
			}
		}
	}
	for _, sha := range builds {
		if !r.Git.isAncestor(ctx, mirror, merge, sha) {
			return false, nil
		}
	}
	return true, nil
}

// reportDeployedLoop runs ReportDeployed every poll until ctx ends.
func (r *Runner) reportDeployedLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.ReportDeployed(ctx); err != nil {
			r.Log.Warn("report deployed", "err", err)
		}
		t := r.Clock.NewTimer(time.Duration(r.Config().PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
