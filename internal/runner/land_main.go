package runner

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// startMainWatch watches main's CI on the merge commit in the background: the item is done and
// its lease ended at the merge, so the runner's slot is free for other work meanwhile. Run (and
// Wait) wait for it at shutdown.
func (r *Runner) startMainWatch(ctx context.Context, lr *landRun, mergeSHA string) {
	r.watching.Add(1)
	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		defer r.watching.Add(-1)
		r.watchMain(ctx, lr, mergeSHA)
	}()
}

// Watching is how many merges' main CI the runner is watching.
func (r *Runner) Watching() int { return int(r.watching.Load()) }

// Wait waits until no main-CI watch is running (they end with the runner's context).
func (r *Runner) Wait() { r.bg.Wait() }

// watchMain polls the Actions runs on the merge commit until all complete, up to the config's
// land_main_wait, then records main_ci on the item: green, red (and files a P1 item naming the
// failing runs, which the bot posts with an @mention of the approver) or timeout.
func (r *Runner) watchMain(ctx context.Context, lr *landRun, mergeSHA string) {
	wait := r.Cfg.mainWait
	if wait <= 0 {
		wait = 30 * time.Minute
	}
	deadline := r.Clock.NewTimer(wait)
	defer deadline.Stop()
	report := context.WithoutCancel(ctx)
	result, failing := "", []string(nil)
	var runs []WorkflowRun
	for result == "" {
		rs, err := r.Land.CommitRuns(ctx, lr.repo.Slug, mergeSHA)
		if err != nil {
			lr.log.Warn("main CI", "err", err)
		} else {
			runs = rs
			result, failing = MainResult(rs)
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
	if result == "red" {
		title := clip(fmt.Sprintf("main red after #%d: %s", lr.item.PR, strings.Join(failing, ", ")), 200)
		ask := fmt.Sprintf("main's CI failed on `%s`, the squash merge of %s#%d.\n\nCaused by item #%d / merge %s.\n\nRuns:\n- %s\n",
			short7(mergeSHA), lr.repo.Slug, lr.item.PR, lr.item.ID, mergeSHA, strings.Join(urls, "\n- "))
		id, err := r.Client.AddItem(report, NewItem{Title: title, Ask: ask, Repo: lr.repo.Slug, Priority: "P1", Source: "finding"})
		if err != nil {
			lr.log.Error("filing the P1 for a red main", "err", err)
		} else {
			data["p1_item"] = id
		}
	}
	if err := r.Client.Event(report, lr.item.ID, "main_ci", data); err != nil {
		lr.log.Error("main_ci event", "err", err)
		return
	}
	lr.log.Info("main CI", "result", result, "merge", short7(mergeSHA))
}
