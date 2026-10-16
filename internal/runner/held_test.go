package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
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

	gh.mu.Lock()
	gh.pr.Mergeable, gh.pr.MergeStateStatus = "CONFLICTING", "DIRTY"
	gh.mu.Unlock()
	if err := g.r.CheckHeld(g.ctx); err != nil {
		t.Fatal(err)
	}
	d := g.item(id)
	ev := g.lastEvent(id, "rebuild")
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 0 || ev == nil || ev.Actor != "runner:devbox" {
		t.Fatalf("stage %s bounces %d, rebuild %+v", d.Item.Stage, d.Item.BounceCount, ev)
	}
	if notes, _ := ev.Data["notes"].(string); !strings.Contains(notes, "conflicts with main") {
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
