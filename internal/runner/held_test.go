package runner

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// toHeld takes an item to land and has land hold it, as the land script does for a PR on the hold
// list.
func (g *rig) toHeld(title string) (int64, *fakeLand) {
	g.t.Helper()
	id, gh := g.toLand(title)
	it := g.item(id).Item
	if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}); err != nil || c == nil || c.Item.ID != id {
		g.t.Fatalf("land claim %v %v", c, err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Hold, map[string]string{"reason": "on the hold list", "head_sha": it.HeadSHA},
		[]string{"A feature.txt"}); err != nil {
		g.t.Fatal(err)
	}
	if st := g.item(id).Item.Stage; st != pipeline.Held {
		g.t.Fatalf("stage %s after hold", st)
	}
	return id, gh
}

// A held PR that turns conflicting goes back to build on its own, with the conflict as the
// builder's notes; a clean one stays held.
func TestCheckHeldSendsAConflictBackToBuild(t *testing.T) {
	g := newRig(t, "")
	id, gh := g.toHeld("held")
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	if st := g.item(id).Item.Stage; st != pipeline.Held || gh.views != 1 {
		t.Fatalf("a clean held PR: stage %s, %d views", st, gh.views)
	}

	// main changes the PR's file: a conflict outside docs/ needs a build.
	g.pushMain("feature.txt", "main's own feature\n")
	gh.mu.Lock()
	gh.pr.Mergeable, gh.pr.MergeStateStatus = "CONFLICTING", "DIRTY"
	gh.mu.Unlock()
	branch := g.item(id).Item.Branch
	before := g.remoteHead(branch)
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.remoteHead(branch) != before {
		t.Fatal("pushed a merge with a code conflict")
	}
	d := g.item(id)
	ev := g.lastEvent(id, "rebuild")
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 0 || ev == nil || ev.Actor != "runner:devbox" {
		t.Fatalf("stage %s bounces %d, rebuild %+v", d.Item.Stage, d.Item.BounceCount, ev)
	}
	if notes, _ := ev.Data["notes"].(string); !strings.Contains(notes, "conflicts with main") || !strings.Contains(notes, "outside docs/: feature.txt") {
		t.Fatalf("notes %q", notes)
	}
	// Nothing is held any more: GitHub is not asked again.
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	if gh.views != 2 {
		t.Fatalf("%d views, want 2", gh.views)
	}
}

// A held PR that GitHub calls conflicting but git merges (here cleanly; docs/ conflicts go the same
// way, as land's do) gets the base merged in and pushed while it waits: the new head goes to
// review, so it is mergeable once the user approves.
func TestCheckHeldMergesTheBaseIn(t *testing.T) {
	g := newRig(t, "")
	id, gh := g.toHeld("held")
	it := g.item(id).Item
	g.pushMain("other.txt", "an unrelated change on main\n")
	gh.mu.Lock()
	gh.pr.Mergeable, gh.pr.MergeStateStatus = "CONFLICTING", "DIRTY"
	gh.mu.Unlock()
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	d := g.item(id)
	head := g.remoteHead(it.Branch)
	if d.Item.Stage != pipeline.Review || head == it.HeadSHA || d.Item.HeadSHA != head {
		t.Fatalf("stage %s, head %s (remote %s, was %s); log:\n%s", d.Item.Stage, d.Item.HeadSHA, head, it.HeadSHA, g.logs.String())
	}
	if parents := strings.Fields(gitT(t, g.remote, "rev-list", "--parents", "-n", "1", head)); len(parents) != 3 || parents[1] != it.HeadSHA {
		t.Fatalf("the new head is not a merge of the held head: %v", parents)
	}
	ev := g.lastEvent(id, pipeline.KindHeldMerge)
	if ev == nil || ev.Actor != "runner:devbox" || ev.Data["from_sha"] != it.HeadSHA {
		t.Fatalf("event %+v", ev)
	}
	if notes, _ := ev.Data["notes"].(string); !strings.Contains(notes, "merged main into the branch (git merged it cleanly)") {
		t.Fatalf("notes %q", notes)
	}
}

// A runner without GitHub for land does not watch held PRs.
func TestCheckHeldWithoutLand(t *testing.T) {
	g := newRig(t, "")
	id, _ := g.toHeld("held")
	g.r.Land = nil
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	if st := g.item(id).Item.Stage; st != pipeline.Held {
		t.Fatalf("stage %s", st)
	}
}

// A build after a held item came back finds the notes (the user's, or the conflict) in
// .loop/review/held.md, and the brief points at it.
func TestHeldNotesForTheBuilder(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, _ := g.toHeld("held")
	if _, err := g.st.Rebuild(g.ctx, "slack:U0C9AFPAMD4", id, "rebase and fix the typo in the heading"); err != nil {
		t.Fatal(err)
	}
	cl, err := g.r.Client.Claim(g.ctx, "b", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, _ := g.r.newRun(cl, "b")
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	notes := g.read(filepath.Join(ru.wt, ".loop", "review", "held.md"))
	if !strings.Contains(notes, "## Sent back by the user (2026-10-15 12:00 UTC)\n\nrebase and fix the typo in the heading\n") {
		t.Fatalf("notes:\n%s", notes)
	}
	if !strings.Contains(brief, "Sent back from held; PR #7 is open on this branch. Do what `.loop/review/held.md` asks") {
		t.Fatalf("brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}
	if _, err := os.Stat(filepath.Join(ru.wt, ".loop", "review", "notes.md")); err == nil {
		t.Fatal("review notes written without a bounce")
	}
}

// A needs_you item the user sent back to build (a rebuild reply or the Rebuild button) gets the
// note in .loop/review/held.md too, with or without a PR yet, and the brief says it came from a
// question.
func TestNeedsYouRebuildNotesForTheBuilder(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("asks")
	if _, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "builder", Stages: []pipeline.Stage{pipeline.Build}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Question, map[string]string{"question": "which plug?"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.Rebuild(g.ctx, "slack:U0C9AFPAMD4", id, "rebuild with the kasa plug"); err != nil {
		t.Fatal(err)
	}
	cl, err := g.r.Client.Claim(g.ctx, "b", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, _ := g.r.newRun(cl, "b")
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	notes := g.read(filepath.Join(ru.wt, ".loop", "review", "held.md"))
	if !strings.Contains(notes, "## Sent back by the user, instead of an answer to: which plug? (2026-10-15 12:00 UTC)\n\nrebuild with the kasa plug\n") {
		t.Fatalf("notes:\n%s", notes)
	}
	if !strings.Contains(brief, "- Sent back to build by the user instead of an answer to its question. Do what `.loop/review/held.md` asks.") {
		t.Fatalf("brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}
}

// pushMain commits file on the remote's main, from a clone of its own (not the user's).
func (g *rig) pushMain(file, text string) {
	g.t.Helper()
	c := filepath.Join(g.t.TempDir(), "main")
	gitT(g.t, filepath.Dir(c), "clone", "-q", g.remote, c)
	os.WriteFile(filepath.Join(c, file), []byte(text), 0o644)
	gitT(g.t, c, "add", file)
	gitT(g.t, c, "commit", "-q", "-m", "main: "+file)
	gitT(g.t, c, "push", "-q", "origin", "main")
}

// The user approved a held PR; land merged main in to resolve a conflict and sent the new head
// to review. The review run computes the delta from the approved commit, gives it to the reviewer
// and attaches it, and loopd carries the approval to the new head (only merges of main).
func TestReviewCarriesTheApproval(t *testing.T) {
	g := newRig(t, "")
	id, _ := g.toHeld("held")
	it := g.item(id).Item
	if _, err := g.st.Approve(g.ctx, "alex", id, it.HeadSHA, "ship it"); err != nil {
		t.Fatal(err)
	}
	g.pushMain("other.txt", "main moved on\n")
	c := filepath.Join(t.TempDir(), "pr")
	gitT(t, filepath.Dir(c), "clone", "-q", "-b", it.Branch, g.remote, c)
	gitT(t, c, "merge", "-q", "--no-ff", "--no-edit", "origin/main")
	gitT(t, c, "push", "-q", "origin", it.Branch)
	head := gitT(t, c, "rev-parse", "HEAD")
	if cl, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}); err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("land claim %v %v", cl, err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.BackToReview, map[string]string{"notes": "land merged main in", "head_sha": head},
		[]string{"A feature.txt"}); err != nil {
		t.Fatal(err)
	}
	g.stages(StageReview)
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if a := d.Item.Approval; d.Item.Stage != pipeline.Land || a == nil || a.HeadSHA != head || a.CarriedFrom != it.HeadSHA || a.By != "alex" {
		t.Fatalf("stage %s approval %+v; log:\n%s", d.Item.Stage, d.Item.Approval, g.logs.String())
	}
	ev := g.lastEvent(id, pipeline.KindApprovalCarried)
	if ev == nil || ev.Data["from_sha"] != it.HeadSHA || ev.Data["to_sha"] != head {
		t.Fatalf("event %+v", ev)
	}
	var delta *store.Evidence
	for i := range d.Evidence {
		if d.Evidence[i].Name == ApprovalDeltaFile {
			delta = &d.Evidence[i]
		}
	}
	if delta == nil {
		t.Fatalf("no %s in the evidence: %+v", ApprovalDeltaFile, d.Evidence)
	}
	_, f, err := g.st.OpenEvidence(g.ctx, id, delta.N)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	f.Close()
	for _, want := range []string{"# Approval delta: `" + short7(it.HeadSHA) + "` → `" + short7(head) + "`", "- Merges of origin/main: `" + short7(head) + "`", "**carryable**"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the delta lacks %q:\n%s", want, body)
		}
	}
}
