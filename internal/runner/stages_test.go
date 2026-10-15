package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// script sets how the fake claude plays an item's runs of a stage.
func (g *rig) script(id int64, stage, play, repo string) {
	g.t.Helper()
	b, _ := json.Marshal(map[string]string{"play": play, "repo": repo})
	if err := os.WriteFile(filepath.Join(g.scripts, fmt.Sprintf("%d-%s.json", id, stage)), b, 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) stages(st ...string) { g.r.Cfg.Stages = st }

// lastRun is the run dir of the item's latest run of a stage.
func (g *rig) lastRun(id int64, stage string) string {
	g.t.Helper()
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-%s-*", id, stage)))
	if len(runs) == 0 {
		g.t.Fatalf("no %s run of #%d", stage, id)
	}
	slices.Sort(runs)
	return runs[len(runs)-1]
}

func (g *rig) read(p string) string {
	g.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		g.t.Fatal(err)
	}
	return string(b)
}

// toReview builds an item with the runner (build only), so it waits in review with PR 7.
func (g *rig) toReview(title string) int64 {
	g.t.Helper()
	id := g.toBuild(title)
	g.stages(StageBuild)
	if !g.runOnce() || g.item(id).Item.Stage != pipeline.Review {
		g.t.Fatalf("build did not reach review; log:\n%s", g.logs.String())
	}
	return id
}

// An item from Slack has no repo: triage sees every configured repo checked out, the context
// files, and picks one; loopd records it and the item moves to build.
func TestTriagePicksTheRepo(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.stages(StageTriage)
	g.prs.merged = []MergedPR{{Number: 3, Title: "Add the digest", MergedAt: "2026-10-14T10:00:00Z", Files: []string{"internal/slack/digest.go"}}}
	it, err := g.st.AddItem(g.ctx, "slack:U1", store.NewItem{Title: "Show the repo", Ask: "show the item's repo in /loop show"})
	if err != nil {
		t.Fatal(err)
	}
	g.script(it.ID, StageTriage, fakeclaude.Reproduce, "other")
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(it.ID)
	if d.Item.Stage != pipeline.Build || d.Item.Repo != "alex/other" || d.Item.Area != "sandbox" || d.Item.Size != "S" {
		t.Fatalf("item %+v (question %q); log:\n%s", d.Item, d.Item.Question, g.logs.String())
	}
	evs := g.events(it.ID, "advanced")
	ev := evs[len(evs)-1]
	if ev.Outcome != "reproduced" || ev.Data["repo"] != "alex/other" || ev.Data["runner_agent"] == nil {
		t.Fatalf("advance %+v", ev.Data)
	}
	var names, labels []string
	for _, e := range d.Evidence {
		names, labels = append(names, e.Name), append(labels, e.Label)
	}
	if !slices.Equal(names, []string{"triage-notes.md", "repro.txt"}) || labels[0] != "triage notes" {
		t.Fatalf("evidence %v %v", names, labels)
	}
	run := g.lastRun(it.ID, StageTriage)
	brief := g.read(filepath.Join(run, "brief.md"))
	for _, want := range []string{"# Triage stage", "- Stage: triage", "Repository: none yet", "`repos/demo/` (`alex/demo`, `origin/main`)",
		"`repos/other/` (`alex/other`, `origin/main`)", "`.loop/context/repos.md` (the configured repos)", "`.loop/context/open-items.md`",
		"`.loop/context/merged-prs-demo.md`", "`.loop/context/merged-prs-other.md`", "> show the item's repo in /loop show"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief[strings.Index(brief, ItemHeading):])
		}
	}
	var gc guard.Config
	json.Unmarshal([]byte(g.read(filepath.Join(run, "guard.json"))), &gc)
	if !gc.ReadOnly || !slices.Contains(gc.DenyWrite, "repos/demo/.git") {
		t.Fatalf("guard config %+v", gc)
	}
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-triage", it.ID))); !os.IsNotExist(err) {
		t.Fatal("the triage dir was left behind")
	}
	for _, m := range []string{"demo", "other"} {
		if out := gitT(t, g.r.mirrorPath(m), "worktree", "list"); strings.Contains(out, "triage") {
			t.Fatalf("a triage checkout is still registered in %s:\n%s", m, out)
		}
	}
	g.userUntouched()
}

// What triage is given, before the agent runs.
func TestTriageContextFiles(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.prs.merged = []MergedPR{{Number: 3, Title: "Add | the digest", MergedAt: "2026-10-14T10:00:00Z", Files: []string{"internal/slack/digest.go"}}}
	built := g.toReview("Fix the digest wording")
	g.stages(StageTriage)
	it, _ := g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: "Fix the digest wording", Repo: "demo"})
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageTriage})
	if err != nil || cl == nil || cl.Item.ID != it.ID {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	open := g.read(filepath.Join(ru.wt, ".loop", "context", "open-items.md"))
	if !strings.Contains(open, fmt.Sprintf("| #%d | Fix the digest wording | review (queued) | alex/demo | #7 |", built)) ||
		!strings.Contains(open, fmt.Sprintf("| #%d | Fix the digest wording (this item) | triage | demo |", it.ID)) {
		t.Fatalf("open items:\n%s", open)
	}
	merged := g.read(filepath.Join(ru.wt, ".loop", "context", "merged-prs-demo.md"))
	if !strings.Contains(merged, "- #3 Add | the digest (merged 2026-10-14T10:00:00Z): internal/slack/digest.go") {
		t.Fatalf("merged:\n%s", merged)
	}
	repos := g.read(filepath.Join(ru.wt, ".loop", "context", "repos.md"))
	if !strings.Contains(repos, "| demo | alex/demo | main | the demo repo |") || !strings.Contains(repos, "| other | alex/other |") {
		t.Fatalf("repos:\n%s", repos)
	}
	// The item has a repo: only it is checked out, and the brief says to leave repo out.
	if len(ru.checkouts) != 1 || ru.checkouts[0].Key != "demo" || !strings.Contains(brief, "leave `repo` out") ||
		strings.Contains(brief, "merged-prs-other") {
		t.Fatalf("checkouts %+v\n%s", ru.checkouts, brief[strings.Index(brief, ItemHeading):])
	}
	if _, err := os.Stat(filepath.Join(ru.wt, "repos", "demo", "README.md")); err != nil {
		t.Fatal("the repo is not checked out under repos/demo")
	}
}

func TestTriageRefusals(t *testing.T) {
	cases := []struct {
		play, repo, want string
	}{
		{fakeclaude.TriageBadRepo, "", "not a configured repo"},
		{fakeclaude.Reproduce, "", "must name one of the configured repos"},
	}
	for _, c := range cases {
		t.Run(c.play, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			g.stages(StageTriage)
			it, _ := g.st.AddItem(g.ctx, "slack:U1", store.NewItem{Title: "No repo"})
			g.script(it.ID, StageTriage, c.play, c.repo)
			g.runOnce()
			d := g.item(it.ID)
			if d.Item.Stage != pipeline.NeedsYou || d.Item.Repo != "" || !strings.Contains(d.Item.Question, c.want) {
				t.Fatalf("%s %q %q", d.Item.Stage, d.Item.Repo, d.Item.Question)
			}
		})
	}
}

// A second item with the same ask as one in flight is closed as a duplicate, naming both.
func TestTriageDuplicate(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	first := g.toReview("Fix the digest wording")
	g.stages(StageTriage)
	it, _ := g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: "Fix the digest wording", Repo: "demo"})
	g.script(it.ID, StageTriage, fakeclaude.Duplicate, "")
	g.runOnce()
	d := g.item(it.ID)
	if d.Item.Stage != pipeline.Closed || !strings.HasPrefix(d.Item.Summary, fmt.Sprintf("duplicate of #%d / PR #7", first)) {
		t.Fatalf("%s %q; log:\n%s", d.Item.Stage, d.Item.Summary, g.logs.String())
	}
}

// Review checks out the PR head, posts its review as a PR comment (never an approval) and
// reports approved for that head.
func TestReviewApproves(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	head := g.item(id).Item.HeadSHA
	g.stages(StageReview)
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Land {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	ev := g.advanced(id)
	if ev.Outcome != "approved" || ev.Data["head_sha"] != head || ev.Data["pr_comment"] != "posted" ||
		!strings.Contains(fmt.Sprint(ev.Data["summary"]), "feature.txt") {
		t.Fatalf("advance %+v", ev.Data)
	}
	if ev.Data["runner_agent"] == d.Item.BuilderAgent {
		t.Fatal("the builder reviewed its own work")
	}
	cs := g.prs.comments[7]
	if len(cs) != 1 {
		t.Fatalf("comments %v", cs)
	}
	for _, want := range []string{"## Loop review: approved", "**Approved.** The tests cover `feature.txt`", "Reviewed `" + head + "`",
		"not a GitHub approval", "- `review-check.txt`: review check", "Run: "} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, cs[0])
		}
	}
	run := g.lastRun(id, StageReview)
	brief := g.read(filepath.Join(run, "brief.md"))
	for _, want := range []string{"# Review stage", "- PR: #7, branch `loop/", "head `" + head + "`", "`.loop/context/pr.md`",
		"`.loop/context/diff.patch` (git diff origin/main..." + head[:7] + ")", "`.loop/context/evidence/1-repro.txt` (repro)",
		"`.loop/context/evidence/2-after.txt` (tests)"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief[strings.Index(brief, ItemHeading):])
		}
	}
	if !strings.Contains(g.read(filepath.Join(run, "argv.json")), "demo-review.json") {
		t.Fatal("review runs have their own settings file")
	}
	if g.remoteHead(fmt.Sprintf("loop/%d-add-a-feature", id)) != head {
		t.Fatal("the branch moved during review")
	}
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-review", id))); !os.IsNotExist(err) {
		t.Fatal("the review worktree was left behind")
	}
	g.userUntouched()
}

// A review agent that tries to commit is refused by the guard; the review still completes.
func TestReviewRefusesCommit(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	head := g.item(id).Item.HeadSHA
	g.stages(StageReview)
	g.script(id, StageReview, fakeclaude.ReviewCommit, "")
	g.runOnce()
	refs := g.events(id, "guard_refused")
	if len(refs) != 1 || refs[0].Data["rule"] != guard.RuleGitCommit {
		t.Fatalf("refusals %+v; log:\n%s", refs, g.logs.String())
	}
	if d := g.item(id); d.Item.Stage != pipeline.Land || g.remoteHead(d.Item.Branch) != head {
		t.Fatalf("stage %s", d.Item.Stage)
	}
}

// A bounce posts the review, records the notes and sends the item back to build; the build run
// gets the notes and the PR's comments in .loop/review/ and updates the same PR; a second review
// approves.
func TestBounceThenRebuildThenApprove(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.stages(StageReview)
	g.script(id, StageReview, fakeclaude.Bounce, "")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 1 {
		t.Fatalf("stage %s bounces %d; log:\n%s", d.Item.Stage, d.Item.BounceCount, g.logs.String())
	}
	if ev := g.advanced(id); ev.Outcome != "bounce" || ev.Data["notes"] != "`feature.txt:1`: add a test for the empty case." {
		t.Fatalf("bounce %+v", ev.Data)
	}
	if cs := g.prs.comments[7]; len(cs) != 1 || !strings.Contains(cs[0], "## Loop review: changes requested") ||
		!strings.Contains(cs[0], "### What to change\n\n`feature.txt:1`: add a test") {
		t.Fatalf("comments %q", cs)
	}

	g.stages(StageBuild)
	g.runOnce()
	d = g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.PR != 7 || len(g.prs.created) != 1 || !slices.Equal(g.prs.edited, []int{7}) {
		t.Fatalf("stage %s pr %d created %v edited %v; log:\n%s", d.Item.Stage, d.Item.PR, g.prs.created, g.prs.edited, g.logs.String())
	}
	if !strings.Contains(g.prs.bodies[7], "Addressed the review (.loop/review/notes.md, .loop/review/pr-comments.md).") {
		t.Fatalf("body %s", g.prs.bodies[7])
	}
	brief := g.read(filepath.Join(g.lastRun(id, StageBuild), "brief.md"))
	if !strings.Contains(brief, "Review bounced it 1 time(s); PR #7 is open on this branch") || !strings.Contains(brief, "`.loop/review/notes.md`") {
		t.Fatalf("brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}

	g.stages(StageReview)
	g.script(id, StageReview, fakeclaude.Approve, "")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Land {
		t.Fatalf("stage %s %q", d.Item.Stage, d.Item.Question)
	}
	if run := g.read(filepath.Join(g.lastRun(id, StageReview), "brief.md")); !strings.Contains(run, "`.loop/context/previous-reviews.md`") {
		t.Fatal("the second review is not shown the first one's notes")
	}
}

// What a build run after a bounce finds in .loop/review/.
func TestBounceContextFiles(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.stages(StageReview)
	g.script(id, StageReview, fakeclaude.Bounce, "")
	g.runOnce()
	cl, err := g.r.Client.Claim(g.ctx, "b", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, _ := g.r.newRun(cl, "b")
	if _, err := g.r.prepare(g.ctx, ru); err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	notes := g.read(filepath.Join(ru.wt, ".loop", "review", "notes.md"))
	if !strings.Contains(notes, "## Bounce 1 (2026-10-15 12:00 UTC)\n\n`feature.txt:1`: add a test for the empty case.") {
		t.Fatalf("notes:\n%s", notes)
	}
	cs := g.read(filepath.Join(ru.wt, ".loop", "review", "pr-comments.md"))
	if !strings.Contains(cs, "## Loop review: changes requested") || !strings.Contains(cs, "on `feature.txt:1`\n\nName the test after the case.") {
		t.Fatalf("comments:\n%s", cs)
	}
	if s := gitT(t, ru.wt, "status", "--porcelain"); s != "" {
		t.Fatalf("the runner's files show in git status:\n%s", s)
	}
}

// The second bounce sends the item to the user with the notes.
func TestSecondBounceNeedsYou(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.script(id, StageReview, fakeclaude.Bounce, "")
	for i := 0; i < 2; i++ {
		g.stages(StageReview)
		g.runOnce()
		if i == 0 {
			g.stages(StageBuild)
			g.runOnce()
		}
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.BounceCount != 2 || !strings.Contains(d.Item.Question, "add a test for the empty case") {
		t.Fatalf("%s %d %q; log:\n%s", d.Item.Stage, d.Item.BounceCount, d.Item.Question, g.logs.String())
	}
	if len(g.prs.comments[7]) != 2 {
		t.Fatalf("comments %d", len(g.prs.comments[7]))
	}
}

// A PR that moved since loopd recorded its head is not reviewed as if it had not.
func TestReviewOfAMovedPRFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.stages(StageReview)
	g.r.VerifyPR = func(context.Context, string, int) (PRInfo, error) {
		return PRInfo{HeadRefName: "x", HeadRefOid: otherSHA, State: "OPEN"}, nil
	}
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "not posted") || len(g.prs.comments[7]) != 0 {
		t.Fatalf("%s %q %v", d.Item.Stage, d.Item.Question, g.prs.comments)
	}
}

// Every stage's prompt starts with the same bytes for every item (the static brief, a golden file
// per stage), and its runs share the argv and settings bytes: one cacheable prefix per stage.
func TestStagePrefixesAreStable(t *testing.T) {
	for _, stage := range AgentStages {
		got := StaticBrief(stage)
		path := filepath.Join("testdata", "brief_"+stage+".golden.md")
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err, "(run go test ./internal/runner -run Prefixes -update)")
		}
		if got != string(want) {
			t.Errorf("%s differs from the %s brief's static part; review and rerun with -update", path, stage)
		}
		if strings.Contains(got, ItemHeading) {
			t.Errorf("the %s skill contains the item heading", stage)
		}
	}

	g := newRig(t, fakeclaude.Good)
	type prep struct{ brief, settings, argv []byte }
	preps := map[string][]prep{}
	var ids []int64
	for _, title := range []string{"First item", "Second, quite different item"} {
		ids = append(ids, g.toReview(title))
	}
	for _, title := range []string{"Third item", "Fourth, with another ask"} {
		g.st.AddItem(g.ctx, "user:alex", store.NewItem{Title: title, Ask: "ask " + title, Repo: "demo"})
	}
	for _, stage := range []string{StageTriage, StageReview} {
		for i := 0; i < 2; i++ {
			cl, err := g.r.Client.Claim(g.ctx, fmt.Sprintf("p-%s-%d", stage, i), nil, []string{stage})
			if err != nil || cl == nil {
				t.Fatalf("claim %s: %+v %v", stage, cl, err)
			}
			ru, _ := g.r.newRun(cl, "p")
			brief, err := g.r.prepare(g.ctx, ru)
			if err != nil {
				t.Fatal(err)
			}
			cut := strings.Index(brief, ItemHeading)
			settings, _ := os.ReadFile(ru.settings)
			argv, _ := os.ReadFile(filepath.Join(ru.dir, "argv.json"))
			preps[stage] = append(preps[stage], prep{[]byte(brief[:cut]), settings, argv})
			g.r.cleanup(g.ctx, ru, g.r.Log)
			// End the lease (stage limits: one triage at a time).
			if err := g.r.Client.Advance(g.ctx, cl.Item.ID, "failed", map[string]string{"error": "prefix test"}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	for stage, ps := range preps {
		a, b := ps[0], ps[1]
		if !bytes.Equal(a.brief, b.brief) || string(a.brief) != StaticBrief(stage) {
			t.Errorf("%s: the briefs' static part differs between items", stage)
		}
		if len(a.settings) == 0 || !bytes.Equal(a.settings, b.settings) || !bytes.Equal(a.argv, b.argv) {
			t.Errorf("%s: settings or argv differ between items", stage)
		}
	}
	// Triage and review never commit: their settings deny it (a build's do not).
	for _, stage := range []string{StageTriage, StageReview} {
		if !bytes.Contains(preps[stage][0].settings, []byte(`"Bash(git commit *)"`)) {
			t.Errorf("%s settings lack the commit deny rules", stage)
		}
	}
}
