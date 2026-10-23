package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #521: GitHub answers mergeStateStatus BEHIND when the base requires up-to-date heads; merging
// as-is is refused ("the head branch is not up to date with the base branch"). Land merges the
// base into the branch (as landConflict and the held watch do), carries the review approval over
// the clean merge, re-runs the gates on the new head and merges — no failed, no back_to_review.
func TestLandUpdatesBehindBranchAndMerges(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, gh := g.toLand("Behind main")
	it := g.item(id).Item
	oldHead := it.HeadSHA
	g.pushMain("other.txt", "someone else's change\n")
	// GitHub: the branch's live head; BEHIND until the update brings main's tip into it.
	gh.view = func(pr *LandPR) {
		if pr.State != "OPEN" {
			return
		}
		pr.HeadRefOid = g.remoteHead(it.Branch)
		pr.MergeStateStatus = "BEHIND"
		if pr.HeadRefOid != oldHead {
			pr.MergeStateStatus = "CLEAN"
		}
	}
	g.stages(StageLand)
	if !g.runOnce() {
		t.Fatalf("no land claim; log:\n%s", g.logs.String())
	}
	newHead := g.remoteHead(it.Branch)
	if newHead == oldHead {
		t.Fatalf("the branch was not updated; log:\n%s", g.logs.String())
	}
	if d := g.item(id).Item; d.Stage != pipeline.Done || d.HeadSHA != newHead {
		t.Fatalf("stage %s at %s, want done at the merge %s; log:\n%s", d.Stage, short7(d.HeadSHA), short7(newHead), g.logs.String())
	}
	gh.mu.Lock()
	merges := append([]string(nil), gh.merges...)
	gh.mu.Unlock()
	if len(merges) != 1 || merges[0] != newHead {
		t.Fatalf("merges %v, want only the updated head %s", merges, short7(newHead))
	}
	if e := g.lastEvent(id, "land_update"); e == nil || e.Data["old_head"] != oldHead || e.Data["new_head"] != newHead {
		t.Fatalf("land_update event %+v", e)
	}
	if e := g.advanced(id); e.Outcome != "merged" || e.Data["review_carried_from"] != oldHead || e.Data["head_sha"] != newHead {
		t.Fatalf("merged event %+v, want review_carried_from %s", e.Data, short7(oldHead))
	}
}

// A BEHIND branch whose update conflicts outside docs/ goes back to build with the conflict (the
// real conflict escalates); nothing merges and nothing is pushed.
func TestLandUpdateConflictGoesBackToBuild(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, gh := g.toLand("Behind and conflicting")
	it := g.item(id).Item
	oldHead := it.HeadSHA
	g.pushMain("feature.txt", "main's own feature\n") // the file the build wrote
	gh.pr.MergeStateStatus = "BEHIND"
	g.stages(StageLand)
	if !g.runOnce() {
		t.Fatalf("no land claim; log:\n%s", g.logs.String())
	}
	if d := g.item(id).Item; d.Stage != pipeline.Build {
		t.Fatalf("stage %s, want build; log:\n%s", d.Stage, g.logs.String())
	}
	if e := g.advanced(id); e.Outcome != "back_to_build" || !strings.Contains(fmt.Sprint(e.Data["reason"]), "feature.txt") {
		t.Fatalf("advanced %+v", e)
	}
	gh.mu.Lock()
	merges := len(gh.merges)
	gh.mu.Unlock()
	if merges != 0 || g.remoteHead(it.Branch) != oldHead {
		t.Fatalf("merges %d, branch head %s, want nothing merged or pushed", merges, short7(g.remoteHead(it.Branch)))
	}
}

// A base that keeps moving is bounded: after MaxLandUpdates updates in one run the item fails to
// the user instead of chasing it forever.
func TestLandUpdateCap(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, gh := g.toLand("Chasing main")
	it := g.item(id).Item
	g.pushMain("other.txt", "someone else's change\n")
	gh.view = func(pr *LandPR) {
		if pr.State == "OPEN" {
			pr.HeadRefOid = g.remoteHead(it.Branch)
			pr.MergeStateStatus = "BEHIND" // GitHub never catches up
		}
	}
	g.stages(StageLand)
	if !g.runOnce() {
		t.Fatalf("no land claim; log:\n%s", g.logs.String())
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "still behind main") {
		t.Fatalf("stage %s question %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if n := len(g.events(id, "land_update")); n != MaxLandUpdates {
		t.Fatalf("%d land_update events, want %d", n, MaxLandUpdates)
	}
	gh.mu.Lock()
	merges := len(gh.merges)
	gh.mu.Unlock()
	if merges != 0 {
		t.Fatalf("%d merges, want none", merges)
	}
}
