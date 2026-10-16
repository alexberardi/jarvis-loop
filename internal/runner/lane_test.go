package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The configured stages split into the agent lane (Slots goroutines) and the land lane (one). An
// empty agent lane is nil, never [], because loopd reads an empty stages list as any stage.
func TestLanes(t *testing.T) {
	for _, c := range []struct {
		stages      []string
		agent, land []string
	}{
		{[]string{StageTriage, StageBuild, StageReview}, []string{StageTriage, StageBuild, StageReview}, nil},
		{[]string{StageBuild, StageLand}, []string{StageBuild}, []string{StageLand}},
		{[]string{StageLand}, nil, []string{StageLand}},
		{[]string{StagePlan, StageLand}, []string{StagePlan}, []string{StageLand}},
		{[]string{StageLand, StageReview}, []string{StageReview}, []string{StageLand}},
	} {
		r := &Runner{Cfg: &Config{Stages: c.stages}}
		agent, land := r.Lanes()
		if !slices.Equal(agent, c.agent) || (agent == nil) != (c.agent == nil) || !slices.Equal(land, c.land) {
			t.Errorf("%v: agent %#v land %#v, want %#v %#v", c.stages, agent, land, c.agent, c.land)
		}
	}
}

// toLand walks an item to land (the runner builds it, review is done by hand), and gives the
// runner a GitHub whose PR for it is green and mergeable.
func (g *rig) toLand(title string) (int64, *fakeLand) {
	g.t.Helper()
	id := g.toReview(title)
	it := g.item(id).Item
	if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "reviewer", Stages: []pipeline.Stage{pipeline.Review}}); err != nil || c == nil || c.Item.ID != id {
		g.t.Fatalf("review claim %v %v", c, err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Approved, map[string]string{"summary": "fine", "head_sha": it.HeadSHA}, nil); err != nil {
		g.t.Fatal(err)
	}
	if st := g.item(id).Item.Stage; st != pipeline.Land {
		g.t.Fatalf("stage %s after approve", st)
	}
	gh := &fakeLand{pr: LandPR{Number: it.PR, State: "OPEN", HeadRefName: it.Branch, HeadRefOid: it.HeadSHA, BaseRefName: "main",
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Body: g.prs.bodies[it.PR]}}
	gh.setChecks(BucketPass)
	g.r.Land = gh
	return id, gh
}

// waitMerged polls (real time) until land asked GitHub for a merge.
// waitMerged waits until GitHub saw the merge and the runner reported it to loopd: the report
// comes after the merge call, so checking loopd right after GitHub races on a slow machine.
func (g *rig) waitMerged(gh *fakeLand, id int64) {
	g.t.Helper()
	for i := 0; i < 600; i++ {
		if _, m := gh.seen(); m > 0 {
			if evs := g.events(id, "advanced"); len(evs) > 0 && evs[len(evs)-1].Outcome == "merged" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("land never merged; log:\n%s", g.logs.String())
}

// runInBackground runs r.Run until the returned func cancels it and waits for it to end.
func (g *rig) runInBackground() func() {
	ctx, cancel := context.WithCancel(g.ctx)
	done := make(chan struct{})
	go func() { defer close(done); g.r.Run(ctx) }()
	return func() {
		g.t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			g.t.Fatalf("Run did not end; log:\n%s", g.logs.String())
		}
	}
}

// Land has its own lane: with the only agent slot held by a build, an approved item still lands
// (docs/decisions/2026-10-16-land-lane.md).
func TestLandRunsWhileAnAgentHoldsTheSlot(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	landing, gh := g.toLand("Approved already")
	building := g.toBuild("Slow build")
	g.script(building, StageBuild, fakeclaude.Hang, "")
	// The build goes first, as on 2026-10-16: it takes the slot before the land item is looked at.
	p1 := "P1"
	if _, err := g.st.PatchItem(g.ctx, "user:alex", building, &p1, nil); err != nil {
		t.Fatal(err)
	}
	g.stages(StageBuild, StageLand)

	stop := g.runInBackground()
	g.clk.BlockUntil(2) // the build's heartbeat and time cap: the agent runs in the only slot
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", building), "wip.txt"))
	g.waitMerged(gh, landing)

	if d := g.item(building); d.Item.Stage != pipeline.Build || d.Lease == nil {
		t.Fatalf("the build should still hold its lease: %s %+v", d.Item.Stage, d.Lease)
	}
	if ev := g.advanced(landing); ev.Outcome != "merged" {
		t.Fatalf("land's last advance %+v; log:\n%s", ev, g.logs.String())
	}
	stop()
}

// A land-only runner runs no agent lane: it never claims the build item next to the land one
// (an empty stages list would have asked loopd for any stage).
func TestLandOnlyRunnerClaimsOnlyLand(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	landing, gh := g.toLand("Approved already")
	building := g.toBuild("Not for this runner")
	g.stages(StageLand)

	stop := g.runInBackground()
	g.waitMerged(gh, landing)
	g.clk.BlockUntil(1) // the land lane's poll timer, and nothing else
	// The one claim is triage's, by hand.
	if len(g.events(building, "claimed")) != 1 {
		t.Fatalf("the land-only runner claimed the build item")
	}
	if ev := g.advanced(landing); ev.Outcome != "merged" {
		t.Fatalf("land's last advance %+v", ev)
	}
	stop()
}

// A paused runner (auto mode unavailable) stops claiming agent work, but land needs no agent and
// goes on.
func TestPausedRunnerStillLands(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	landing, gh := g.toLand("Approved already")
	building := g.toBuild("Waits for auto mode")
	g.stages(StageBuild, StageLand)
	g.r.paused.Store("auto mode unavailable")

	if !g.runOnce() {
		t.Fatal("a paused runner claimed nothing")
	}
	if _, m := gh.seen(); m != 1 || g.advanced(landing).Outcome != "merged" {
		t.Fatalf("not merged; log:\n%s", g.logs.String())
	}
	if len(g.events(building, "claimed")) != 1 { // triage's, by hand
		t.Fatal("a paused runner claimed agent work")
	}
	if g.runOnce() {
		t.Fatal("a paused runner claimed the build")
	}
}
