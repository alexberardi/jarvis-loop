package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// resumedForResult checks that a stage's last run resumed the first attempt's session with the
// note alone, and kept the first attempt's files (#208).
func (g *rig) resumedForResult(id int64, stage string) {
	g.t.Helper()
	run := g.lastRun(id, stage)
	if argv := argvOf(g.t, run); slices.Index(argv, "--resume") < 0 || argv[slices.Index(argv, "--resume")+1] != "fake-session-background" {
		g.t.Fatalf("the second run should resume the first's session: %q", argv)
	}
	if b := g.read(filepath.Join(run, "brief.md")); !strings.HasPrefix(b, NoResultHeading) || strings.Contains(b, ItemHeading) {
		g.t.Fatalf("the resumed run's prompt is the note alone:\n%s", b)
	}
	if b := g.read(filepath.Join(run, "brief-1.md")); !strings.Contains(b, ItemHeading) {
		g.t.Fatalf("brief-1.md should be the first attempt's whole brief:\n%s", b)
	}
}

// A build that ends its turn while a command runs in the background, with no result: the runner
// resumes the same session once, and the resumed run's result is pushed and opened as a PR.
func TestBuildWithoutResultResumesTheSession(t *testing.T) {
	g := newRig(t, fakeclaude.Background)
	id := g.toBuild("Waits on the race suite")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || g.starts(id) != 2 || g.prs.calls() != 1 {
		t.Fatalf("stage %s %q after %d runs; log:\n%s", d.Item.Stage, d.Item.Question, g.starts(id), g.logs.String())
	}
	if ev := g.advanced(id); ev.Data["result_resumed"] != "true" || ev.Data["first_session_id"] != "fake-session-background" {
		t.Fatalf("advance %+v", ev.Data)
	}
	g.resumedForResult(id, StageBuild)
}

func TestReviewWithoutResultResumesTheSession(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.stages(StageReview)
	g.script(id, StageReview, fakeclaude.Background, "")
	before := g.starts(id)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Land || g.starts(id)-before != 2 {
		t.Fatalf("stage %s %q after %d runs; log:\n%s", d.Item.Stage, d.Item.Question, g.starts(id)-before, g.logs.String())
	}
	if ev := g.advanced(id); ev.Outcome != "approved" || ev.Data["result_resumed"] != "true" {
		t.Fatalf("advance %s %+v", ev.Outcome, ev.Data)
	}
	g.resumedForResult(id, StageReview)
}

func TestTriageWithoutResultResumesTheSession(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.stages(StageTriage)
	it, err := g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: "Waits", Ask: "Add a feature file.", Repo: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	g.script(it.ID, StageTriage, fakeclaude.Background, "")
	g.runOnce()
	d := g.item(it.ID)
	if d.Item.Stage != pipeline.Build || g.starts(it.ID) != 2 {
		t.Fatalf("stage %s %q after %d runs; log:\n%s", d.Item.Stage, d.Item.Question, g.starts(it.ID), g.logs.String())
	}
	g.resumedForResult(it.ID, StageTriage)
}
