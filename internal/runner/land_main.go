package runner

import (
	"context"
	"fmt"
	"time"
)

// startMainWatch watches main's CI on the merge commit in the background: the item is done and
// its lease ended at the merge, so the runner's slot is free for other work meanwhile. Run (and
// Wait) wait for it at shutdown. prev is main's head the merge was checked against ("" when not
// known).
func (r *Runner) startMainWatch(ctx context.Context, lr *landRun, mergeSHA, prev string) {
	r.watching.Add(1)
	r.bg.Add(1)
	done := r.waits.add(fmt.Sprintf("main CI of #%d", lr.item.ID), r.Clock.Now())
	go func() {
		defer r.bg.Done()
		defer done()
		defer r.watching.Add(-1)
		r.watchMain(ctx, lr, mergeSHA, prev)
	}()
}

// Watching is how many merges' main CI the runner is watching.
func (r *Runner) Watching() int { return int(r.watching.Load()) }

// Wait waits until no main-CI watch is running (they end with the runner's context).
func (r *Runner) Wait() { r.bg.Wait() }

// watchMain polls the Actions runs on the merge commit until all complete, up to the config's
// land_main_wait, then records main_ci on the item: green, red, superseded (a run was cancelled,
// none failed) or timeout. A red result first gets one re-run of the failed jobs, to rule out a
// flake: green then is green (rerun names the jobs), red again is red, and loopd then freezes the
// repo and files (or reuses) its main-red P1 from the failing jobs, the runs, the merge and the
// last green commit (prev, when its CI was green).
func (r *Runner) watchMain(ctx context.Context, lr *landRun, mergeSHA, prev string) {
	wait := r.Config().mainWait
	if wait <= 0 {
		wait = 30 * time.Minute
	}
	deadline := r.Clock.NewTimer(wait)
	defer func() { deadline.Stop() }()
	report := context.WithoutCancel(ctx)
	result, failing := "", []string(nil)
	var runs []WorkflowRun
	var rerun []string
	for result == "" {
		rs, err := r.Land.CommitRuns(ctx, lr.repo.Slug, mergeSHA)
		if err != nil {
			lr.log.Warn("main CI", "err", err)
		} else {
			runs = rs
			result, failing = MainResult(rs)
			if result == "red" && rerun == nil {
				if rerun = r.rerunFailed(ctx, lr, rs); len(rerun) > 0 {
					result, failing = "", nil
					deadline.Stop()
					deadline = r.Clock.NewTimer(wait)
				}
			}
			if result != "" {
				break
			}
		}
		poll := r.Clock.NewTimer(LandPollEvery)
		select {
		case <-ctx.Done():
			poll.Stop()
			lr.log.Warn("main CI watch ended by shutdown; no result recorded", "merge", mergeSHA)
			return
		case <-deadline.C():
			result = "timeout"
		case <-poll.C():
		}
		poll.Stop()
	}
	urls := []string{}
	for _, run := range runs {
		if run.URL != "" && len(urls) < 20 {
			urls = append(urls, run.URL)
		}
	}
	data := map[string]any{"result": result, "urls": urls, "merge_sha": mergeSHA}
	if len(failing) > 0 {
		data["failing"] = failing
	}
	if len(rerun) > 0 {
		data["rerun"] = rerun
	}
	if result == "red" && prev != "" && prev != mergeSHA {
		if rs, err := r.Land.CommitRuns(ctx, lr.repo.Slug, prev); err == nil {
			if res, _ := MainResult(rs); res == "green" {
				data["last_green"] = prev
			}
		}
	}
	if err := r.Client.Event(report, lr.item.ID, "main_ci", data); err != nil {
		lr.log.Error("main_ci event", "err", err)
		return
	}
	lr.log.Info("main CI", "result", result, "merge", short7(mergeSHA), "rerun", rerun)
}

// rerunFailed re-runs the failed jobs of main's failed runs, and names the jobs it re-ran (none
// when GitHub refused every re-run: the red stands).
func (r *Runner) rerunFailed(ctx context.Context, lr *landRun, runs []WorkflowRun) []string {
	var names []string
	for _, run := range runs {
		switch run.Conclusion {
		case "success", "neutral", "skipped", "cancelled":
			continue
		}
		if err := r.Land.RerunFailed(ctx, lr.repo.Slug, run.ID); err != nil {
			lr.log.Warn("re-running main's failed jobs", "run", run.ID, "err", err)
			continue
		}
		name := run.Name
		if name == "" {
			name = run.Workflow
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		lr.log.Info("main red: re-running the failed jobs once", "jobs", names)
	}
	return names
}
