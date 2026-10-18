package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Held watch (docs/decisions/2026-10-16-held-back-to-build.md): a held item is in no work stage,
// so no land run polls its PR. Every land poll the runner asks loopd for the held items of its
// repos and GitHub for each one's PR; a PR that turned CONFLICTING goes back to build with the
// conflict as the builder's notes, instead of waiting for an approval that could not merge.

// CheckHeld sends the held items whose PR GitHub calls CONFLICTING back to build. Items of repos
// this runner does not have are left to other runners; a failure leaves the item for the next
// pass.
func (r *Runner) CheckHeld(ctx context.Context) error {
	if r.Land == nil {
		return nil
	}
	rows, err := r.Client.Held(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		_, repo, ok := r.Config().Repo(row.Repo)
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
		notes := fmt.Sprintf("PR #%d conflicts with %s while it was held. Merge origin/%s into the branch, resolve the conflicts, and keep the change as it was; the user approves the new head.",
			row.PR, repo.Base, repo.Base)
		err = r.Client.Rebuild(ctx, row.ID, notes)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == pipeline.CodeWrongStage {
			continue // the user approved or answered first, or another runner got there
		}
		if err != nil {
			log.Warn("send the held item back to build", "err", err)
			continue
		}
		log.Info("held PR conflicts; back to build")
	}
	return nil
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
