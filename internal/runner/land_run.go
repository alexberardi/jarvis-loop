package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// LandPollEvery is how often a land run looks at GitHub again while it waits.
const LandPollEvery = 30 * time.Second

// LandWaitingAfter is how long a wait lasts before the item's Slack parent says so.
const LandWaitingAfter = 10 * time.Minute

// errNotLanding: the item left land while the run waited (the user dropped it); the run stops
// without merging or reporting.
var errNotLanding = errors.New("the item is no longer in land")

// errFrozen: main is red in the item's repo and the item is not its fix: the run stops before it
// can merge, and loopd ends its lease (the item waits in land until the freeze lifts).
var errFrozen = errors.New("main is red in the item's repo: frozen")

// landRun is one land claim.
type landRun struct {
	item    Item
	lease   string
	agent   string
	repoKey string
	repo    RepoConfig
	mirror  string
	log     *slog.Logger
	// mainTip is main's head at the last pass of the gates: the merge's base, which watchMain
	// names as the last green commit when main goes red and it was green.
	mainTip string
}

// ciGiveUp is land_ci_give_up: how long CI may stay pending on one head over all land runs.
func (r *Runner) ciGiveUp() time.Duration {
	if d := r.Config().ciGiveUp; d > 0 {
		return d
	}
	return 3 * time.Hour
}

// land works a land claim in-process: no agent, no worktree for one, no guard. It re-runs the
// gates from the top after every wait and after a refused merge, and reports one outcome.
func (r *Runner) land(ctx context.Context, cl *Claim, agent string) {
	lr := &landRun{item: cl.Item, lease: cl.Lease.ID, agent: agent, log: r.Log.With("item", cl.Item.ID, "stage", StageLand)}
	// Reports must reach loopd even when the runner is shutting down mid-report.
	report := context.WithoutCancel(ctx)
	key, repo, ok := r.Config().Repo(cl.Item.Repo)
	switch {
	case !ok:
		r.landAdvance(report, lr, failed("runner %s has no repo %q configured", r.Config().Name, cl.Item.Repo), nil)
		return
	case r.Land == nil:
		r.landAdvance(report, lr, failed("runner %s has no GitHub client for land (a --dry-run runner cannot land)", r.Config().Name), nil)
		return
	case cl.Item.PR <= 0:
		r.landAdvance(report, lr, failed("the item has no PR to land"), nil)
		return
	}
	lr.repoKey, lr.repo, lr.mirror = key, repo, r.mirrorPath(key)
	lr.log.Info("land starting", "pr", cl.Item.PR, "repo", repo.Slug)

	ciWait := r.Config().ciWait
	if ciWait <= 0 {
		ciWait = 30 * time.Minute
	}
	// Stay inside loopd's per-run cap, so the wait ends as land's own failure, not an overtime.
	if ctl, err := r.Client.Control(ctx); err == nil && ctl.MaxRunMinutes > 0 {
		if capAt := time.Duration(ctl.MaxRunMinutes)*time.Minute - 2*time.Minute; capAt > 0 && capAt < ciWait {
			ciWait = capAt
		}
	}
	started := r.Clock.Now()
	hb := r.Clock.NewTimer(r.heartbeatEvery())
	defer func() { hb.Stop() }()
	// sleep waits one poll, keeping the lease alive; false means stop (shutdown, or loopd ended
	// the lease: the item stays in land for the next claim).
	sleep := func(note string) bool {
		poll := r.Clock.NewTimer(LandPollEvery)
		defer poll.Stop()
		waiting := r.Clock.Now()
		for {
			select {
			case <-ctx.Done():
				return false
			case <-poll.C():
				return true
			case <-hb.C():
				stop, err := r.Client.Heartbeat(ctx, lr.lease, note, r.stepActivity(StageLand, note, started, waiting))
				var ae *APIError
				if err == nil && stop || errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
					lr.log.Info("land stopped by loopd", "err", err)
					return false
				}
				if err != nil {
					lr.log.Warn("heartbeat", "err", err)
				}
				hb = r.Clock.NewTimer(r.heartbeatEvery())
			}
		}
	}

	var waitSince time.Time
	noted, reran, refusals := false, false, 0
	for {
		f, err := r.landFacts(ctx, lr)
		if ctx.Err() != nil {
			return // shutdown: the lease expires and the item waits in land
		}
		if errors.Is(err, errNotLanding) {
			lr.log.Info("land stopped: the item is " + lr.item.Stage)
			return
		}
		if errors.Is(err, errFrozen) {
			// The heartbeat is how loopd ends the lease of a land run in a frozen repo.
			if _, err := r.Client.Heartbeat(report, lr.lease, "land: main is red in "+lr.repo.Slug+"; not merging", nil); err != nil {
				lr.log.Warn("heartbeat", "err", err)
			}
			lr.log.Info("land stopped: main is red in " + lr.repo.Slug)
			return
		}
		if err != nil {
			r.landAdvance(report, lr, failed("land could not read the PR's state: %s", clip(err.Error(), 800)), nil)
			return
		}
		d := Decide(f)
		lr.log.Info("land gates", "decision", d.Action.String(), "outcome", d.Outcome, "wait", d.Wait, "head", short7(f.PR.HeadRefOid))
		switch d.Action {
		case ActAdvance:
			if d.Outcome == "merged" { // merged outside the loop: still watch main's CI
				d.Fields["main_ci"] = "watching"
				if r.landAdvance(report, lr, d, f.Paths) {
					r.startMainWatch(ctx, lr, f.PR.MergeCommit, "")
				}
				return
			}
			if d.Outcome == "back_to_build" && d.Fields["ci_head_sha"] != "" {
				if f.RerunCI && !reran {
					// Once per run, whatever loopd took: a re-run that GitHub refused leaves the red standing.
					reran = true
					if r.rerunPRFailed(ctx, lr, f.PR) {
						if !sleep("land: re-running the failed CI jobs once") {
							return
						}
						continue
					}
				}
				r.landCIFailure(ctx, lr, f.PR)
				if sha := r.landMainFixed(ctx, lr, f.PR.HeadRefOid); sha != "" {
					d.Fields["main_fixed_sha"] = sha
					d.Fields["reason"] += fmt.Sprintf("; main was red at this PR's base and is green now at `%s`: rebase on main", short7(sha))
				}
			}
			r.landAdvance(report, lr, d, f.Paths)
			return
		case ActWait:
			now := r.Clock.Now()
			if waitSince.IsZero() {
				waitSince = now
			}
			waited := now.Sub(waitSince)
			if waited >= ciWait || len(f.PR.Checks) > 0 && now.Sub(pendingSince(f.PR, lr.item, waitSince)) >= r.ciGiveUp() {
				d := ciWaitEnded(d, f.PR, lr.item, waitSince, now, r.ciGiveUp())
				if d.Outcome == "ci_pending" {
					lr.log.Info("land: CI still pending; back to the land queue", "since", d.Fields["since"])
				}
				r.landAdvance(report, lr, d, f.Paths)
				return
			}
			// Once per head: a run after ci_pending on the same head has already said so.
			if waited >= LandWaitingAfter && !noted && lr.item.CIPendingSHA != f.PR.HeadRefOid {
				noted = true
				if err := r.Client.Event(ctx, lr.item.ID, "land_waiting", map[string]any{"waiting_for": d.Wait,
					"since": waitSince.UTC().Format(time.RFC3339), "head_sha": f.PR.HeadRefOid}); err != nil {
					lr.log.Warn("land_waiting event", "err", err)
				}
			}
			if !sleep(fmt.Sprintf("land: waiting for %s (%s)", d.Wait, roundDur(waited))) {
				return
			}
		case ActConflict:
			d, paths := r.landConflict(ctx, lr, f)
			r.landAdvance(report, lr, d, paths)
			return
		case ActMerge:
			head := f.PR.HeadRefOid
			if err := r.Land.Merge(ctx, lr.repo.Slug, f.PR.Number, head); err != nil {
				refusals++
				lr.log.Warn("merge refused", "n", refusals, "err", err)
				if refusals >= 2 {
					r.landAdvance(report, lr, failed("GitHub refused the merge of PR #%d twice; last: %s", f.PR.Number, clip(err.Error(), 800)), f.Paths)
					return
				}
				continue // the gates again, from the top
			}
			after, err := r.Land.LandView(report, lr.repo.Slug, f.PR.Number)
			if err != nil || after.State != "MERGED" || after.MergeCommit == "" {
				// gh returned, yet the PR is not merged: a merge queue or auto-merge took it. Nothing
				// the gates checked binds what merges later, so say so loudly.
				msg := fmt.Sprintf("gh pr merge returned for PR #%d at `%s`, but the PR is %q with no merge commit", f.PR.Number, short7(head), after.State)
				if err != nil {
					msg = fmt.Sprintf("PR #%d: gh pr merge returned, but reading it back failed: %s", f.PR.Number, clip(err.Error(), 400))
				}
				r.landAdvance(report, lr, failed("%s; check it on GitHub (auto-merge or a merge queue would merge it unchecked)", msg), f.Paths)
				return
			}
			d := advance("merged", map[string]string{"merge_sha": after.MergeCommit, "head_sha": head, "main_ci": "watching"})
			if r.landAdvance(report, lr, d, f.Paths) {
				r.startMainWatch(ctx, lr, after.MergeCommit, lr.mainTip)
			}
			return
		}
	}
}

// landFacts re-derives every gate input: the item and its history from loopd, the PR from
// GitHub, the changed paths from git, and the hold rules from loopd's control.
func (r *Runner) landFacts(ctx context.Context, lr *landRun) (LandFacts, error) {
	var f LandFacts
	d, err := r.Client.Detail(ctx, lr.item.ID)
	if err != nil {
		return f, fmt.Errorf("loopd: %w", err)
	}
	it := d.Item
	lr.item = it
	// A merge cannot be undone: the stage is read again before every pass of the gates, so a
	// drop stops the run here, not at its next heartbeat.
	if it.Stage != StageLand {
		return f, errNotLanding
	}
	if d.Frozen != nil && d.Frozen.ItemID != it.ID {
		return f, errFrozen
	}
	pr, err := r.Land.LandView(ctx, lr.repo.Slug, it.PR)
	if err != nil {
		return f, err
	}
	f.PR, f.Branch, f.Base = pr, it.Branch, lr.repo.Base
	if !strings.HasPrefix(f.Branch, "loop/") {
		return f, fmt.Errorf("the item's branch %q is not a loop/ branch", f.Branch)
	}
	if !shaRe.MatchString(pr.HeadRefOid) {
		return f, fmt.Errorf("GitHub gave PR #%d no head commit", pr.Number)
	}
	unlock := r.lockMirror(lr.mirror)
	err = r.Git.EnsureMirror(ctx, lr.mirror, lr.repo)
	if err == nil {
		f.Paths, err = r.Git.DiffPaths(ctx, lr.mirror, lr.repo.Base, pr.HeadRefOid, pr.Number)
	}
	if err == nil {
		if _, tip, terr := r.Git.MergeBase(ctx, lr.mirror, lr.repo.Base, pr.HeadRefOid); terr == nil {
			lr.mainTip = tip
		}
	}
	unlock()
	if err != nil {
		return f, err
	}
	// The last review approval, and the build run whose evidence counts.
	buildLease := ""
	for _, e := range d.Events {
		if e.Kind != "advanced" {
			continue
		}
		switch e.Outcome {
		case string(pipeline.Approved):
			f.ReviewedHead, _ = e.Data["head_sha"].(string)
		case string(pipeline.PROpened):
			buildLease, _ = e.Data["lease"].(string)
		}
	}
	for _, ev := range d.Evidence {
		if buildLease != "" && ev.LeaseID == buildLease {
			f.BuildEvidence++
		}
	}
	if it.Approval != nil {
		f.ApprovedHead = it.Approval.HeadSHA
	}
	f.RerunCI = rerunOwed(d.Events, pr.HeadRefOid)
	f.FollowedBy = d.FollowedBy
	// Hold: loopd's risk, or loopd's rules on the paths computed here. loopd checks again when
	// the merge is reported, but by then it has happened; this check is the one that stops it.
	f.Hold = it.Risk == pipeline.RiskHold
	if !f.Hold {
		ctl, err := r.Client.Control(ctx)
		if err != nil {
			return f, fmt.Errorf("loopd control: %w", err)
		}
		paths, err := pipeline.ParseChangedPaths(f.Paths)
		if err != nil && len(f.Paths) > 0 {
			return f, err
		}
		if m := pipeline.MatchHoldRules(ctl.HoldRules, it.Repo, paths); len(m) > 0 {
			f.Hold = true
			lr.log.Info("land: hold rules match the computed paths", "matches", fmt.Sprint(m))
		}
	}
	return f, nil
}

// landAdvance reports a land outcome with the changed paths; it says whether loopd took it. A
// merge loopd refuses (it should not: land checked the same rules first) goes to the user.
func (r *Runner) landAdvance(ctx context.Context, lr *landRun, d Decision, paths []string) bool {
	fields := map[string]string{}
	for k, v := range d.Fields {
		fields[k] = v
	}
	fields["runner_agent"] = lr.agent
	if d.Outcome == "merged" && lr.repo.QA {
		fields["needs_qa"] = "true" // the repo's default: loopd puts the item in qa
	}
	if d.Outcome == "merged" && lr.repo.QAAfterDeploy {
		fields["qa_after_deploy"] = "true" // and asks for the check once a build with it runs
	}
	if d.Outcome == "failed" || d.Outcome == "ci_pending" {
		paths = nil
	}
	err := r.Client.Advance(ctx, lr.item.ID, d.Outcome, fields, paths)
	if err == nil {
		lr.log.Info("land reported", "outcome", d.Outcome)
		return true
	}
	lr.log.Error("land advance", "outcome", d.Outcome, "err", err)
	if d.Outcome == "merged" {
		msg := fmt.Sprintf("PR #%d is merged on GitHub as `%s`, but loopd refused to record it: %s", lr.item.PR, short7(fields["merge_sha"]), clip(err.Error(), 600))
		if err2 := r.Client.Advance(ctx, lr.item.ID, "failed", map[string]string{"error": msg, "runner_agent": lr.agent}, nil); err2 != nil {
			lr.log.Error("land advance failed", "err", err2)
		}
	}
	return false
}

// landCIFailure attaches the failing CI log of the PR's head to the item (ci-failure.log), while
// land still holds its lease, before it sends the item back to build. A failure is logged: the
// item goes back either way.
func (r *Runner) landCIFailure(ctx context.Context, lr *landRun, pr LandPR) {
	runs := ciRunsFromChecks(pr.Checks)
	if len(runs) == 0 {
		lr.log.Info("no CI log to attach: no failed check names an Actions run")
		return
	}
	label, err := r.attachCIFailure(ctx, lr.item.ID, lr.repo.Slug, pr.HeadRefOid, runs, "")
	if err != nil {
		lr.log.Warn("attaching the CI failure log", "err", err)
		return
	}
	lr.log.Info("attached the CI failure log", "label", label)
}

// landMainFixed is main's head when main was red at the PR's merge base and is green there now
// (MainFixed), else "". Anything it cannot read counts as no.
func (r *Runner) landMainFixed(ctx context.Context, lr *landRun, head string) string {
	unlock := r.lockMirror(lr.mirror)
	mb, tip, err := r.Git.MergeBase(ctx, lr.mirror, lr.repo.Base, head)
	unlock()
	if err != nil || mb == tip {
		return ""
	}
	atBase, err := r.Land.CommitRuns(ctx, lr.repo.Slug, mb)
	if err != nil {
		lr.log.Warn("main's CI at the PR's base", "err", err)
		return ""
	}
	atTip, err := r.Land.CommitRuns(ctx, lr.repo.Slug, tip)
	if err != nil {
		lr.log.Warn("main's CI at its head", "err", err)
		return ""
	}
	if !MainFixed(mb, tip, atBase, atTip) {
		return ""
	}
	lr.log.Info("red CI on a PR based on a red main that is green now", "base", short7(mb), "main", short7(tip))
	return tip
}

func roundDur(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m") // "3h", not "3h0m"
	}
	return s
}

// rerunOwed says whether the orchestrator's red_ci answer on head (rerun_ci, ApplyOrchAnswer)
// still owes a re-run of the PR's failed jobs: no ci_rerun event on the head since it.
func rerunOwed(evs []EventInfo, head string) bool {
	owed := false
	for _, e := range evs {
		sha, _ := e.Data["head_sha"].(string)
		switch {
		case sha != head:
		case e.Kind == "answered" && e.Data["rerun_ci"] == true:
			owed = true
		case e.Kind == "ci_rerun":
			owed = false
		}
	}
	return owed
}

// rerunPRFailed re-runs the failed jobs behind the PR's failed checks once, and records it
// (ci_rerun) so that no later land run re-runs them again. It says whether GitHub took any re-run:
// when it took none, the red stands and the item goes back to build.
func (r *Runner) rerunPRFailed(ctx context.Context, lr *landRun, pr LandPR) bool {
	var names []string
	var ids []int64
	for _, run := range ciRunsFromChecks(pr.Checks) {
		if err := r.Land.RerunFailed(ctx, lr.repo.Slug, run.ID); err != nil {
			lr.log.Warn("re-running the PR's failed jobs", "run", run.ID, "err", err)
			continue
		}
		names, ids = append(names, run.Name), append(ids, run.ID)
	}
	data := map[string]any{"head_sha": pr.HeadRefOid, "jobs": strings.Join(names, ", "), "runs": ids}
	if err := r.Client.Event(ctx, lr.item.ID, "ci_rerun", data); err != nil {
		lr.log.Warn("ci_rerun event", "err", err)
	}
	if len(names) == 0 {
		lr.log.Info("red CI: no failed job could be re-run", "head", short7(pr.HeadRefOid))
		return false
	}
	lr.log.Info("red CI: re-running the failed jobs once", "jobs", names, "head", short7(pr.HeadRefOid))
	return true
}
