package runner

import (
	"context"
	"errors"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Wait watch (docs/decisions/2026-10-22-waiting-stage.md): a waiting item may wait on a PR that is
// not a loop item (owner/name#444). loopd holds no GitHub login, so every poll the runner asks
// loopd for the waiting items and GitHub for the state of each PR they wait on, with its own gh
// (the PR closer's PRState), and reports a merged or closed one: a merge that was the last thing
// the item waited on sends it back to the stage it left; a close without a merge, to the user.
// Only PRs of repos this runner has are its; the rest are left to the runners that have them.

// CheckWaitPRs reports the waited-on PRs GitHub calls MERGED or CLOSED. A failure leaves the PR
// for the next pass.
func (r *Runner) CheckWaitPRs(ctx context.Context) error {
	if r.Close == nil {
		return nil
	}
	rows, err := r.Client.Waiting(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		for _, ref := range row.WaitPRs {
			slug, n, ok := pipeline.SplitWaitPR(ref)
			if !ok {
				continue
			}
			if _, repo, ok := r.Config().Repo(slug); !ok || repo.Slug != slug {
				continue // another runner's repo
			}
			log := r.Log.With("item", row.ID, "pr", ref)
			st, err := r.Close.PRState(ctx, slug, n)
			if err != nil {
				log.Warn("waited-on PR", "err", err)
				continue
			}
			if st != "MERGED" && st != "CLOSED" {
				continue
			}
			err = r.Client.WaitPR(ctx, row.ID, ref, st)
			var ae *APIError
			if errors.As(err, &ae) && (ae.Code == pipeline.CodeWrongStage || ae.Code == pipeline.CodeInvalidField) {
				break // another runner got there first, or the user moved the item
			}
			if err != nil {
				log.Warn("report the waited-on PR", "err", err)
				continue
			}
			log.Info("waited-on PR", "state", st)
			if st == "CLOSED" {
				break // the item went to the user
			}
		}
	}
	return nil
}
