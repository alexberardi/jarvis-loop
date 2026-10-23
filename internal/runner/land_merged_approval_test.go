package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #535: a held PR gets a new head (gh pr update-branch) and is merged by hand on GitHub. loopd
// refuses the merged advance (409 approval_required: the approval is for the old head), and the
// runner parks the item in held at the merged head — not needs_you, where loop approve is refused
// wrong_stage and every answer replays the same 409. The Approve button then binds to the merged
// head, and the next land run records the merge.
func TestLandParksAMergedHeadNeedingApproval(t *testing.T) {
	g := newRig(t, "")
	id, gh := g.toHeld("held")
	it := g.item(id).Item
	if _, err := g.st.Approve(g.ctx, "alex", id, it.HeadSHA, "ship it"); err != nil {
		t.Fatal(err)
	}
	// Someone updates the branch (a non-merge commit: the approval cannot carry) and merges the
	// PR by hand on GitHub.
	c := filepath.Join(t.TempDir(), "pr")
	gitT(t, filepath.Dir(c), "clone", "-q", "-b", it.Branch, g.remote, c)
	os.WriteFile(filepath.Join(c, "extra.txt"), []byte("updated\n"), 0o644)
	gitT(t, c, "add", "extra.txt")
	gitT(t, c, "commit", "-q", "-m", "update branch")
	gitT(t, c, "push", "-q", "origin", it.Branch)
	head := gitT(t, c, "rev-parse", "HEAD")
	gh.mu.Lock()
	gh.pr.State, gh.pr.HeadRefOid, gh.pr.MergeCommit = "MERGED", head, mergeM
	gh.mu.Unlock()
	g.stages(StageLand)
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Held || d.Item.HeadSHA != head || d.Item.Approval != nil {
		t.Fatalf("stage %s head %s approval %+v; log:\n%s", d.Item.Stage, short7(d.Item.HeadSHA), d.Item.Approval, g.logs.String())
	}
	ev := g.advanced(id)
	reason, _ := ev.Data["reason"].(string)
	if ev.Outcome != string(pipeline.Hold) || !strings.Contains(reason, "already merged on GitHub") {
		t.Fatalf("advanced %s reason %q; log:\n%s", ev.Outcome, reason, g.logs.String())
	}

	// The user approves the merged head: the next land run records the merge. Land itself never
	// called merge (the one merge on the PR is the hand merge).
	if _, err := g.st.Approve(g.ctx, "alex", id, head, "the merge is real"); err != nil {
		t.Fatal(err)
	}
	if !g.runOnce() {
		t.Fatal("nothing claimed after the approval")
	}
	d = g.item(id)
	if d.Item.Stage != pipeline.Done || d.Item.MergeSHA != mergeM || len(gh.merges) != 0 {
		t.Fatalf("stage %s merge %s, land's merges %v; log:\n%s", d.Item.Stage, short7(d.Item.MergeSHA), gh.merges, g.logs.String())
	}
	if ev := g.advanced(id); ev.Outcome != "merged" || ev.Data["merged_outside"] != "true" {
		t.Fatalf("advanced %s %+v", ev.Outcome, ev.Data)
	}
}
