package runner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// fakeCloser is GitHub for the drop sweep: PR states, and the closes the runner asked for.
type fakeCloser struct {
	mu     sync.Mutex
	state  map[int]string
	closed []string // "slug#n: comment"
}

func (f *fakeCloser) PRState(_ context.Context, slug string, pr int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[pr]
	if !ok {
		return "", fmt.Errorf("no PR #%d on %s", pr, slug)
	}
	return s, nil
}

func (f *fakeCloser) ClosePR(_ context.Context, slug string, pr int, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, fmt.Sprintf("%s#%d: %s", slug, pr, comment))
	f.state[pr] = "CLOSED"
	return nil
}

// withPR takes an item through triage and build to review, with PR pr on alex/demo.
func (g *rig) withPR(title string, pr int) int64 {
	g.t.Helper()
	id := g.toBuild(title)
	c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "builder", Stages: []pipeline.Stage{pipeline.Build}})
	if err != nil || c == nil || c.Item.ID != id {
		g.t.Fatalf("build claim %v %v", c, err)
	}
	if _, err := g.st.AddEvidence(g.ctx, "devbox", id, "log", "build.log", strings.NewReader("ok")); err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.PROpened, map[string]string{"repo": "alex/demo", "pr": fmt.Sprint(pr),
		"branch": fmt.Sprintf("loop/%d-x", id), "head_sha": strings.Repeat("a", 40)}, []string{"A feature.txt"}); err != nil {
		g.t.Fatal(err)
	}
	return id
}

func TestCloseDroppedPRs(t *testing.T) {
	g := newRig(t, "")
	gh := &fakeCloser{state: map[int]string{7: "OPEN", 8: "CLOSED", 9: "MERGED"}}
	g.r.Close = gh
	open, byHand, merged, kept := g.withPR("kasa", 7), g.withPR("lifx", 8), g.withPR("merged", 9), g.withPR("kept", 10)
	noPR := g.toBuild("no pr")
	for _, id := range []int64{open, byHand, merged, noPR} {
		if _, err := g.st.Drop(g.ctx, "user:alex", id, "no hardware"); err != nil {
			t.Fatal(err)
		}
	}

	if err := g.r.CloseDropped(g.ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alex/demo#7: " + DropComment}; strings.Join(gh.closed, "|") != strings.Join(want, "|") {
		t.Fatalf("closed %q, want %q", gh.closed, want)
	}
	for id, want := range map[int64]string{open: "closed", byHand: "already_closed", merged: "already_merged"} {
		ev := g.lastEvent(id, "pr_closed")
		if ev == nil || ev.Data["state"] != want || ev.Actor != "runner:devbox" {
			t.Fatalf("item %d pr_closed %+v, want %s", id, ev, want)
		}
	}
	if ev := g.lastEvent(kept, "pr_closed"); ev != nil || gh.state[10] != "" {
		t.Fatalf("an item that was not dropped: %+v", ev)
	}
	// The next pass finds nothing to do: no second close, no second event.
	if err := g.r.CloseDropped(g.ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.closed) != 1 || len(g.events(open, "pr_closed")) != 1 {
		t.Fatalf("second pass: closed %q, events %d", gh.closed, len(g.events(open, "pr_closed")))
	}
}

// A PR GitHub cannot tell about stays on the list for the next pass; the others go on.
func TestCloseDroppedPRsKeepsGoing(t *testing.T) {
	g := newRig(t, "")
	gh := &fakeCloser{state: map[int]string{8: "OPEN"}}
	g.r.Close = gh
	unknown, open := g.withPR("unknown", 7), g.withPR("open", 8)
	for _, id := range []int64{unknown, open} {
		if _, err := g.st.Drop(g.ctx, "user:alex", id, ""); err != nil {
			t.Fatal(err)
		}
	}
	g.r.CloseDropped(g.ctx)
	if g.lastEvent(unknown, "pr_closed") != nil || g.lastEvent(open, "pr_closed") == nil {
		t.Fatalf("events: unknown %v, open %v", g.lastEvent(unknown, "pr_closed"), g.lastEvent(open, "pr_closed"))
	}
	if !strings.Contains(g.logs.String(), "no PR #7") {
		t.Fatalf("the failure is not logged: %s", g.logs.String())
	}
}

// The close's gh argv: a comment, never --delete-branch.
func TestGHClosePR(t *testing.T) {
	ctx := context.Background()
	gh, log := fakeGH(t, `case "$1 $2" in
"pr view") echo '{"state":"OPEN"}';;
"pr close") echo '✓ Closed pull request o/r#7';;
esac
`)
	if s, err := gh.PRState(ctx, "o/r", 7); err != nil || s != "OPEN" {
		t.Fatalf("state %q %v", s, err)
	}
	if err := gh.ClosePR(ctx, "o/r", 7, DropComment); err != nil {
		t.Fatal(err)
	}
	if err := gh.ClosePR(ctx, "o/r", 0, DropComment); err == nil {
		t.Fatal("closed PR 0")
	}
	b, _ := os.ReadFile(log)
	want := "pr view 7 --repo o/r --json state\npr close 7 --repo o/r --comment Dropped by the owner"
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("gh argv:\n%s\nwant:\n%s", got, want)
	}
}

// fakeLand is GitHub for a land run: one PR whose checks the test sets, and the merges asked for.
type fakeLand struct {
	mu     sync.Mutex
	pr     LandPR
	views  int
	merges []string
	// runs are the Actions runs on any commit; logs the failed logs by run id, and logCalls the
	// run ids asked for.
	runs     []WorkflowRun
	logs     map[int64]string
	logCalls []int64
	reruns   []int64
	// commits, when set, are the runs by commit instead (none for a commit not in it); rerunErr
	// refuses every re-run. A run with a pending check is refused too, as GitHub does.
	commits  map[string][]WorkflowRun
	rerunErr error
}

func (f *fakeLand) RerunFailed(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rerunErr != nil {
		return f.rerunErr
	}
	for _, c := range f.pr.Checks {
		if c.Bucket == BucketPending && RunIDFromURL(c.URL) == id {
			return fmt.Errorf("run %d cannot be rerun; its workflow run is still in progress", id)
		}
	}
	f.reruns = append(f.reruns, id)
	return nil
}

func (f *fakeLand) LandView(_ context.Context, slug string, pr int) (LandPR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views++
	return f.pr, nil
}

func (f *fakeLand) Merge(_ context.Context, slug string, pr int, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, head)
	f.pr.State, f.pr.MergeCommit = "MERGED", "cccccccccccccccccccccccccccccccccccccccc"
	return nil
}

func (f *fakeLand) CommitRuns(_ context.Context, _ string, sha string) ([]WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commits != nil {
		return f.commits[sha], nil
	}
	return f.runs, nil
}

func (f *fakeLand) FailedLog(_ context.Context, _ string, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls = append(f.logCalls, id)
	l, ok := f.logs[id]
	if !ok {
		return "", fmt.Errorf("run %d not found", id)
	}
	return l, nil
}

func (f *fakeLand) setChecks(bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr.Checks = []Check{{Name: "check", Bucket: bucket, URL: "https://ci/1"}}
}

func (f *fakeLand) seen() (views, merges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.views, len(f.merges)
}

// A drop while land waits for CI stops the run at its next look, before any merge: CI passing
// after the drop, and before land's heartbeat, must not merge the PR.
func TestDropDuringLandDoesNotMerge(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.stages(StageReview)
	if !g.runOnce() || g.item(id).Item.Stage != pipeline.Land {
		t.Fatalf("review did not reach land; log:\n%s", g.logs.String())
	}
	it := g.item(id).Item
	gh := &fakeLand{pr: LandPR{Number: it.PR, State: "OPEN", HeadRefName: it.Branch, HeadRefOid: it.HeadSHA, BaseRefName: "main",
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Body: g.prs.bodies[it.PR]}}
	gh.setChecks(BucketPending)
	g.r.Land = gh
	g.stages(StageLand)

	wait := g.runAsync()
	g.clk.BlockUntil(2) // the heartbeat and the first poll: land is waiting for CI
	if v, m := gh.seen(); v != 1 || m != 0 {
		t.Fatalf("before the drop: %d views, %d merges; log:\n%s", v, m, g.logs.String())
	}
	if _, err := g.st.Drop(g.ctx, "user:alex", id, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	gh.setChecks(BucketPass)
	g.clk.Advance(LandPollEvery) // the next poll comes before the heartbeat
	wait()

	if _, m := gh.seen(); m != 0 {
		t.Fatalf("the dropped item's PR was merged; log:\n%s", g.logs.String())
	}
	if !strings.Contains(g.logs.String(), "land stopped: the item is closed") {
		t.Fatalf("no stop logged:\n%s", g.logs.String())
	}
	if d := g.item(id); d.Item.Stage != pipeline.Closed || g.lastEvent(id, "advanced").Outcome == "merged" {
		t.Fatalf("stage %s, last advance %+v", d.Item.Stage, g.lastEvent(id, "advanced"))
	}
}

// A repo with qa: true: land sends needs_qa with merged, and loopd puts the item in qa
// (docs/decisions/2026-10-15-qa.md). The review agent's needs_qa marks the item the same way.
func TestLandQARepoAndReviewNeedsQA(t *testing.T) {
	for _, c := range []struct {
		name        string
		repoQA      bool
		afterDeploy bool
		play        string
		want        pipeline.Stage
	}{
		{"plain", false, false, fakeclaude.Approve, pipeline.Done},
		{"repo qa", true, false, fakeclaude.Approve, pipeline.QA},
		{"repo qa after deploy", true, true, fakeclaude.Approve, pipeline.QA},
		{"review needs qa", false, false, fakeclaude.ApproveQA, pipeline.QA},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			rc := g.r.Cfg.Repos["demo"]
			rc.QA, rc.QAAfterDeploy = c.repoQA, c.afterDeploy
			g.r.Cfg.Repos["demo"] = rc
			id := g.toReview("Add a feature")
			g.stages(StageReview)
			g.script(id, StageReview, c.play, "")
			if !g.runOnce() || g.item(id).Item.Stage != pipeline.Land {
				t.Fatalf("review did not reach land; log:\n%s", g.logs.String())
			}
			it := g.item(id).Item
			if it.NeedsQA != (c.play == fakeclaude.ApproveQA) {
				t.Fatalf("needs_qa after review: %v", it.NeedsQA)
			}
			gh := &fakeLand{pr: LandPR{Number: it.PR, State: "OPEN", HeadRefName: it.Branch, HeadRefOid: it.HeadSHA, BaseRefName: "main",
				Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Body: g.prs.bodies[it.PR]}}
			gh.setChecks(BucketPass)
			g.r.Land = gh
			g.stages(StageLand)
			g.runOnce()
			d := g.item(id)
			evs := g.events(id, "advanced")
			ev := evs[len(evs)-1]
			if d.Item.Stage != c.want || ev.Outcome != "merged" {
				t.Fatalf("stage %s, last advance %+v; log:\n%s", d.Item.Stage, ev, g.logs.String())
			}
			if got := ev.Data["needs_qa"] == "true"; got != c.repoQA {
				t.Fatalf("merged needs_qa field %v, want %v", ev.Data["needs_qa"], c.repoQA)
			}
			if got := ev.Data["qa_after_deploy"] == "true"; got != c.afterDeploy || d.Item.AwaitingDeploy != c.afterDeploy {
				t.Fatalf("merged qa_after_deploy field %v, awaiting %v, want %v", ev.Data["qa_after_deploy"], d.Item.AwaitingDeploy, c.afterDeploy)
			}
		})
	}
}
