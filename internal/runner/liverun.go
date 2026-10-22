package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// One live run per item (#453): a run whose lease ended without it knowing (it expired while the
// run was being set up) went on, and its cleanup removed the worktree under the item's next run.
// prepareAlive (#454) keeps the lease alive while the run prepares; on top of it the runner holds
// each item's worktree for one run at a time, and a run's cleanup removes a worktree only while it
// is still the run's own (docs/decisions/2026-10-22-one-live-run-per-item.md).

// liveRun is an item's agent run on this runner; ended closes once its cleanup is done.
type liveRun struct {
	lease string
	ended chan struct{}
}

// leaseFile names the run that owns a worktree (under .loop/, which git ignores).
const leaseFile = "lease"

// holdItem makes ru the item's one run on this runner and returns the func that lets it go (after
// the run's cleanup). While an earlier run of the item is still ending here, it waits for that
// run's cleanup, keeping its own lease alive, up to the run's time cap. A nil func means do not
// run: loopd ended this lease while it waited (err nil), or the wait timed out (err says so).
func (r *Runner) holdItem(ctx context.Context, ru *run, log *slog.Logger) (func(), error) {
	for {
		r.liveMu.Lock()
		if r.live == nil {
			r.live = map[int64]*liveRun{}
		}
		prev := r.live[ru.item.ID]
		if prev == nil {
			mine := &liveRun{lease: ru.lease, ended: make(chan struct{})}
			r.live[ru.item.ID] = mine
			r.liveMu.Unlock()
			return func() {
				r.liveMu.Lock()
				if r.live[ru.item.ID] == mine {
					delete(r.live, ru.item.ID)
				}
				r.liveMu.Unlock()
				close(mine.ended)
			}, nil
		}
		r.liveMu.Unlock()
		log.Warn("an earlier run of the item is still ending on this runner; waiting for its cleanup", "lease", prev.lease)
		limit := r.maxRun(ctx, ru)
		switch r.keepAliveIn(ctx, ru, "setup", "waiting for the item's earlier run to end", "waiting for the earlier run", prev.ended, limit) {
		case waitDone:
		case waitTimeout:
			return nil, fmt.Errorf("an earlier run of the item on runner %s did not end within %d minutes", ru.cfg.Name, int(limit.Minutes()))
		default:
			return nil, nil
		}
	}
}

// claimWorktree marks the worktree as this run's.
func claimWorktree(ru *run) error {
	return os.WriteFile(filepath.Join(ru.wt, ".loop", leaseFile), []byte(ru.lease+"\n"), 0o644)
}

// worktreeOwner is the lease that last claimed the worktree ("" for none: not made yet, or by a
// runner from before the marker).
func worktreeOwner(wt string) string {
	b, err := os.ReadFile(filepath.Join(wt, ".loop", leaseFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// takenOver says why the run's worktree is no longer its own to remove (nil when it is): a later
// run of the item claimed it, or holds the item on this runner.
func (r *Runner) takenOver(ru *run) error {
	if o := worktreeOwner(ru.wt); o != "" && o != ru.lease {
		return fmt.Errorf("the worktree is lease %s's", o)
	}
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if l := r.live[ru.item.ID]; l != nil && l.lease != ru.lease {
		return errors.New("lease " + l.lease + " holds the item")
	}
	return nil
}
