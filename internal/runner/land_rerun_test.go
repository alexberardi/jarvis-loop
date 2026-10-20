package runner

import (
	"slices"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #341: an item that had a review bounce reaches needs_you on its first red CI in land. The
// orchestrator's first red_ci answer returns it to land, which re-runs the failed jobs once
// (ci_rerun) and waits for them: green merges; red again goes back to needs_you, and the second
// red_ci answer goes to build.
func TestLandRerunsFailedJobsOnceAfterRedCIAnswer(t *testing.T) {
	for _, green := range []bool{true, false} {
		name := map[bool]string{true: "rerun passes", false: "red again"}[green]
		t.Run(name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			id := g.toReview("Add a feature")
			if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "reviewer", Stages: []pipeline.Stage{pipeline.Review}}); err != nil || c == nil {
				t.Fatalf("review claim %v %v", c, err)
			}
			if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Bounce, map[string]string{"notes": "fix the test"}, nil); err != nil {
				t.Fatal(err)
			}
			g.stages(StageBuild)
			if !g.runOnce() || g.item(id).Item.Stage != pipeline.Review {
				t.Fatalf("rebuild did not reach review; log:\n%s", g.logs.String())
			}
			it := g.item(id).Item
			if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "reviewer", Stages: []pipeline.Stage{pipeline.Review}}); err != nil || c == nil {
				t.Fatalf("review claim %v %v", c, err)
			}
			if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Approved, map[string]string{"summary": "fine", "head_sha": it.HeadSHA}, nil); err != nil {
				t.Fatal(err)
			}
			gh := &fakeLand{pr: LandPR{Number: it.PR, State: "OPEN", HeadRefName: it.Branch, HeadRefOid: it.HeadSHA, BaseRefName: "main",
				Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Body: g.prs.bodies[it.PR]}}
			red := []Check{{Name: "check", Bucket: BucketFail, URL: "https://github.com/alex/demo/actions/runs/78"}}
			gh.pr.Checks = red
			g.r.Land = gh
			g.stages(StageLand)

			// Without a red_ci answer, land does not re-run anything: the bounce limit asks.
			g.runOnce()
			if d := g.item(id).Item; d.Stage != pipeline.NeedsYou || d.ReturnStage != pipeline.Build || len(gh.reruns) != 0 {
				t.Fatalf("stage %s return %s, reruns %v; log:\n%s", d.Stage, d.ReturnStage, gh.reruns, g.logs.String())
			}
			if got, err := g.st.OrchAnswer(g.ctx, "orchestrator:loop", id, pipeline.AnswerRedCI, "Land reads CI again: re-run it once.", "maybe flaky"); err != nil || got.Stage != pipeline.Land {
				t.Fatalf("first red_ci answer: %s %v", got.Stage, err)
			}

			wait := g.runAsync()
			g.clk.BlockUntil(2) // the heartbeat and the poll after the re-run
			if !slices.Equal(gh.reruns, []int64{78}) {
				t.Fatalf("reruns %v; log:\n%s", gh.reruns, g.logs.String())
			}
			if e := g.lastEvent(id, "ci_rerun"); e == nil || e.Data["head_sha"] != it.HeadSHA || e.Data["jobs"] != "check" {
				t.Fatalf("ci_rerun event %+v", e)
			}
			if green {
				gh.setChecks(BucketPass)
			}
			g.clk.Advance(LandPollEvery)
			wait()
			if green {
				if d := g.item(id).Item; d.Stage != pipeline.Done || len(gh.merges) != 1 {
					t.Fatalf("stage %s, merges %v; log:\n%s", d.Stage, gh.merges, g.logs.String())
				}
				return
			}
			if d := g.item(id).Item; d.Stage != pipeline.NeedsYou || len(gh.reruns) != 1 {
				t.Fatalf("red again: stage %s, reruns %v; log:\n%s", d.Stage, gh.reruns, g.logs.String())
			}
			if got, err := g.st.OrchAnswer(g.ctx, "orchestrator:loop", id, pipeline.AnswerRedCI, "Red again: fix the code.", "not flaky"); err != nil || got.Stage != pipeline.Build {
				t.Fatalf("second red_ci answer: %s %v", got.Stage, err)
			}
		})
	}
}
