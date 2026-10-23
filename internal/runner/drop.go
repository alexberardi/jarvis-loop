package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// DropComment is what the runner says on a dropped item's PR when it closes it.
const DropComment = "Dropped by the owner"

// PRCloser closes a dropped item's PR with the runner's own login (gh), never the agent's
// (docs/decisions/2026-10-15-drop.md).
type PRCloser interface {
	// PRState is the PR's state: OPEN, CLOSED or MERGED.
	PRState(ctx context.Context, slug string, pr int) (string, error)
	// ClosePR closes the PR with a comment and keeps its branch (never --delete-branch).
	ClosePR(ctx context.Context, slug string, pr int, comment string) error
}

// PRState reads the PR's state (gh pr view --json state).
func (g GH) PRState(ctx context.Context, slug string, pr int) (string, error) {
	out, err := g.run(ctx, "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", "state")
	if err != nil {
		return "", err
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return v.State, nil
}

// ClosePR closes the PR with a comment (gh pr close). The branch stays: no --delete-branch.
func (g GH) ClosePR(ctx context.Context, slug string, pr int, comment string) error {
	if pr <= 0 {
		return fmt.Errorf("no PR to close")
	}
	_, err := g.run(ctx, "pr", "close", strconv.Itoa(pr), "--repo", slug, "--comment", comment)
	return err
}

// CloseDropped closes the PRs of the items the user dropped, once each: it asks loopd for the
// dropped items whose PR no runner has closed yet, closes each one still open on GitHub, and
// records pr_closed. A PR already closed or merged (by hand) is only recorded. A failure leaves
// the item for the next pass.
func (r *Runner) CloseDropped(ctx context.Context) error {
	if r.Close == nil {
		return nil
	}
	rows, err := r.Client.PRsToClose(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		_, repo, ok := r.Config().Repo(row.Repo)
		if !ok || row.PR <= 0 {
			continue // another runner's repo
		}
		log := r.Log.With("item", row.ID, "pr", row.PR, "repo", repo.Slug)
		st, err := r.Close.PRState(ctx, repo.Slug, row.PR)
		if err != nil {
			log.Warn("dropped item's PR", "err", err)
			continue
		}
		recorded := "already_closed"
		switch st {
		case "OPEN":
			if err := r.Close.ClosePR(ctx, repo.Slug, row.PR, DropComment); err != nil {
				log.Warn("close dropped item's PR", "err", err)
				continue
			}
			recorded = "closed"
		case "MERGED":
			recorded = "already_merged"
		}
		err = r.Client.Event(ctx, row.ID, "pr_closed", map[string]any{"pr": row.PR, "state": recorded})
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "already_recorded" {
			err = nil // another runner got there first
		}
		if err != nil {
			log.Warn("pr_closed event", "err", err)
			continue
		}
		log.Info("closed dropped item's PR", "state", recorded)
	}
	return nil
}

// closeDroppedLoop runs CloseDropped and CheckWaitPRs (with the same gh) every poll until ctx ends.
func (r *Runner) closeDroppedLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.CloseDropped(ctx); err != nil {
			r.Log.Warn("close dropped PRs", "err", err)
		}
		if err := r.CheckWaitPRs(ctx); err != nil {
			r.Log.Warn("check waited-on PRs", "err", err)
		}
		t := r.Clock.NewTimer(time.Duration(r.Config().PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
