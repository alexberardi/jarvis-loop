package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Held watch (docs/decisions/2026-10-16-held-back-to-build.md): a held item is in no work stage,
// so no land run polls its PR. Every land poll the runner asks loopd for the held items of its
// repos and GitHub for each one's PR. A PR that turned CONFLICTING gets the base merged in as land
// would (mergeBase: a clean merge, or conflicts only under docs/), so it is mergeable the moment
// the user approves: the new head goes to review (docs/decisions/2026-10-18-approval-carry.md).
// A conflict that needs a build goes back to build with the conflict as the builder's notes,
// instead of waiting for an approval that could not merge.

// CheckHeld merges the base into the held PRs GitHub calls CONFLICTING, or sends them back to
// build. Items of repos this runner does not have are left to other runners; a failure leaves the
// item for the next pass.
func (r *Runner) CheckHeld(ctx context.Context) error {
	if r.Land == nil {
		return nil
	}
	rows, err := r.Client.Held(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		key, repo, ok := r.Config().Repo(row.Repo)
		if !ok || row.PR <= 0 {
			continue
		}
		log := r.Log.With("item", row.ID, "pr", row.PR, "repo", repo.Slug)
		pr, err := r.Land.LandView(ctx, repo.Slug, row.PR)
		if err != nil {
			log.Warn("held item's PR", "err", err)
			continue
		}
		if pr.State != "OPEN" || pr.Mergeable != "CONFLICTING" {
			continue
		}
		head, how, conflict, err := r.heldMerge(ctx, key, repo, row, pr)
		if err != nil {
			log.Warn("merge the base into the held PR", "err", err)
			continue
		}
		var what string
		if conflict == "" {
			notes := fmt.Sprintf("PR #%d conflicted with %s while it was held; the runner merged %s into the branch (%s). The new head `%s` needs a review before the user's approval.",
				row.PR, repo.Base, repo.Base, how, short7(head))
			err, what = r.Client.HeldMerge(ctx, row.ID, head, notes), "held PR conflicted; merged the base in, back to review"
		} else {
			notes := fmt.Sprintf("PR #%d conflicts with %s while it was held. Merge origin/%s into the branch, resolve the conflicts, and keep the change as it was; the user approves the new head. (%s.)",
				row.PR, repo.Base, repo.Base, conflict)
			err, what = r.Client.Rebuild(ctx, row.ID, notes), "held PR conflicts; back to build"
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == pipeline.CodeWrongStage {
			continue // the user approved or answered first, or another runner got there
		}
		if err != nil {
			log.Warn("report the held PR's conflict", "err", err)
			continue
		}
		log.Info(what)
	}
	return nil
}

// heldMerge merges the base into a held item's PR (mergeBase), in the mirror fetched first.
func (r *Runner) heldMerge(ctx context.Context, key string, repo RepoConfig, row BoardRow, pr LandPR) (head, how, conflict string, err error) {
	if !strings.HasPrefix(pr.HeadRefName, fmt.Sprintf("loop/%d-", row.ID)) {
		return "", "", "", fmt.Errorf("PR #%d's branch %q is not the item's", row.PR, pr.HeadRefName)
	}
	mirror := r.mirrorPath(key)
	unlock := r.lockMirror(mirror)
	err = r.Git.EnsureMirror(ctx, mirror, repo)
	unlock()
	if err != nil {
		return "", "", "", err
	}
	wt := filepath.Join(r.Config().WorkRoot, fmt.Sprintf("%d-held", row.ID))
	return r.mergeBase(ctx, mirror, repo, wt, pr.HeadRefOid, pr.HeadRefName, row.PR)
}

// heldWatchLoop runs CheckHeld every land poll until ctx ends, while land is in the stages (a
// reload may add or remove it).
func (r *Runner) heldWatchLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if _, land := r.Lanes(); land != nil {
			if err := r.CheckHeld(ctx); err != nil {
				r.Log.Warn("check held PRs", "err", err)
			}
		}
		t := r.Clock.NewTimer(LandPollEvery)
		select {
		case <-ctx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
