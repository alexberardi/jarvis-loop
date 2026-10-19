package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeagy"
	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// toEngineering runs product's turn, approves the brief and sets engineering's play.
func (g *rig) toEngineering(p pipeline.Plan, play string) {
	g.t.Helper()
	g.runOnce()
	doc, _ := g.st.GetPlanDoc(g.ctx, p.ID, "brief", 0)
	if _, err := g.st.ApprovePlan(g.ctx, "user:alex", p.ID, "brief", doc.SHA256, "yes"); err != nil {
		g.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.scripts, "P1-engineering.json"), []byte(`{"play":"`+play+`"}`), 0o644)
}

func (g *rig) lastTurn(id int64) pipeline.PlanTurn {
	d := g.plan(id)
	return d.Turns[len(d.Turns)-1]
}

func (g *rig) docs(id int64, kind string) []pipeline.PlanDoc {
	var out []pipeline.PlanDoc
	for _, d := range g.plan(id).Docs {
		if d.Kind == kind {
			doc, _ := g.st.GetPlanDoc(g.ctx, id, kind, d.Version)
			out = append(out, doc)
		}
	}
	return out
}

// A ready blueprint is critiqued by Gemini before the Approve button: the engineering session is
// resumed to answer each point, revises the blueprint for the ones it takes, and the critique
// with the answers is the plan's critique document; the reply ends with the summary.
func TestPlanCritique(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.toEngineering(p, fakeclaude.PlanBlueprint)
	g.gemini(false, fakeagy.OK)
	g.runOnce()
	last := g.lastTurn(p.ID)
	if last.Persona != "engineering" || last.Status != pipeline.TurnDone || !last.Ready {
		t.Fatalf("turn %+v\n%s", last, g.logs.String())
	}
	if last.Reply != "Gemini raised 3 points: 2 taken, 1 answered.\n\nGemini's points: 1: agree; 2: disagree; 3: agree." {
		t.Fatalf("reply %q", last.Reply)
	}
	bps := g.docs(p.ID, pipeline.DocBlueprint)
	if len(bps) != 1 || !strings.Contains(bps[0].Markdown, "Gemini's point 1, taken") || !strings.Contains(bps[0].Markdown, "Gemini's point 3, taken") {
		t.Fatalf("blueprints %+v", bps)
	}
	cs := g.docs(p.ID, pipeline.DocCritique)
	if len(cs) != 1 {
		t.Fatalf("critiques %+v", cs)
	}
	for _, want := range []string{"Gemini raised 3 points: 2 taken, 1 answered.", "## 1. high, missing: piece 1", "**Taken.** Right: point 1",
		"## 2. medium, order: piece 3", "**Answered (disagreed).** The pieces already cover it.", "engineering revised it", "`" + DefaultGeminiModel + "`"} {
		if !strings.Contains(cs[0].Markdown, want) {
			t.Fatalf("the critique has no %q:\n%s", want, cs[0].Markdown)
		}
	}
	// agy ran once, in the plan's directory, with the critique's schema; the answer turn resumed
	// engineering's session, with the points.
	calls := g.agyCalls()
	if len(calls) != 1 || calls[0].Cwd != filepath.Join(g.r.Cfg.WorkRoot, "plan-1") {
		t.Fatalf("agy calls %+v", calls)
	}
	prompt := calls[0].Args[1]
	if !strings.Contains(prompt, "## The product brief (approved)") || !strings.Contains(prompt, "### 2. The API") {
		t.Fatalf("agy's prompt:\n%s", prompt)
	}
	run := g.planRuns(p.ID, "engineering")[0]
	if b, _ := os.ReadFile(filepath.Join(run, "gemini-critique-schema.json")); !strings.Contains(string(b), `"points"`) {
		t.Fatalf("schema %s", b)
	}
	argv := argvOf(t, run)
	b, _ := os.ReadFile(filepath.Join(run, "argv-critique.json"))
	if i := slices.Index(argv, "--resume"); i >= 0 || !strings.Contains(string(b), `"--resume","fake-session-P1-engineering"`) {
		t.Fatalf("argv %q, the answer turn's %s", argv, b)
	}
	if b, _ := os.ReadFile(filepath.Join(run, "brief.md")); !strings.HasPrefix(string(b), "## Gemini's critique") || !strings.Contains(string(b), "3. **low, risk** (Risks)") {
		t.Fatalf("the answer prompt:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(run, "stream-blueprint.jsonl")); err != nil {
		t.Fatalf("the blueprint turn's transcript: %v", err)
	}
}

// A blueprint critiqued already is not critiqued again; Gemini with no points gives a critique
// document and a summary line, and no answer turn.
func TestPlanCritiqueOncePerBlueprint(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.toEngineering(p, fakeclaude.PlanBlueprint)
	g.gemini(false, fakeagy.None)
	g.runOnce()
	if last := g.lastTurn(p.ID); !strings.HasPrefix(last.Reply, "Gemini raised no points on the blueprint.\n\nHere is the blueprint") {
		t.Fatalf("reply %q", last.Reply)
	}
	if b, _ := os.ReadFile(filepath.Join(g.planRuns(p.ID, "engineering")[0], "brief.md")); strings.HasPrefix(string(b), "## Gemini's critique") {
		t.Fatal("an answer turn ran with no points")
	}
	g.st.PlanMessage(g.ctx, "user:alex", p.ID, "looks right")
	g.runOnce()
	last := g.lastTurn(p.ID)
	if last.Status != pipeline.TurnDone || !last.Ready || strings.Contains(last.Reply, "Gemini") || len(g.agyCalls()) != 1 || len(g.docs(p.ID, pipeline.DocCritique)) != 1 {
		t.Fatalf("second turn %+v, %d agy calls\n%s", last, len(g.agyCalls()), g.logs.String())
	}
}

// A blueprint engineering revised after the critique is the critiqued one too: a later ready turn
// that keeps it runs no second agy call and no second answer turn.
func TestPlanCritiqueOnceAfterRevision(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.toEngineering(p, fakeclaude.PlanKeep)
	g.gemini(false, fakeagy.OK)
	g.runOnce()
	bps := g.docs(p.ID, pipeline.DocBlueprint)
	if len(bps) != 1 || !strings.Contains(bps[0].Markdown, "Gemini's point 1, taken") {
		t.Fatalf("blueprints %+v\n%s", bps, g.logs.String())
	}
	if cs := g.docs(p.ID, pipeline.DocCritique); len(cs) != 1 || !strings.Contains(cs[0].Markdown, bps[0].SHA256) {
		t.Fatalf("the critique does not name the revised blueprint %s: %+v", bps[0].SHA256, cs)
	}
	g.st.PlanMessage(g.ctx, "user:alex", p.ID, "looks right")
	g.runOnce()
	last := g.lastTurn(p.ID)
	if last.Status != pipeline.TurnDone || !last.Ready || last.Reply != "The blueprint stands (turn 3)." {
		t.Fatalf("second turn %+v\n%s", last, g.logs.String())
	}
	if n := len(g.agyCalls()); n != 1 || len(g.docs(p.ID, pipeline.DocCritique)) != 1 || len(g.docs(p.ID, pipeline.DocBlueprint)) != 1 {
		t.Fatalf("%d agy calls, %d critiques, %d blueprints", n, len(g.docs(p.ID, pipeline.DocCritique)), len(g.docs(p.ID, pipeline.DocBlueprint)))
	}
	if runs := g.planRuns(p.ID, "engineering"); len(runs) != 2 || fileHas(filepath.Join(runs[1], "argv-critique.json")) {
		t.Fatalf("a second answer turn ran: %v", runs)
	}
}

func fileHas(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Whatever goes wrong with agy, the blueprint is posted as the persona left it, and the reply
// says the second opinion was unavailable.
func TestPlanCritiqueUnavailable(t *testing.T) {
	for play, reason := range map[string]string{fakeagy.Quota: AgyQuota, fakeagy.Garbage: AgyBadOutput, fakeagy.Failed: AgyError} {
		t.Run(play, func(t *testing.T) {
			g, p := planRig(t, fakeclaude.PlanBrief)
			g.toEngineering(p, fakeclaude.PlanBlueprint)
			g.gemini(true, play)
			g.runOnce()
			last := g.lastTurn(p.ID)
			if last.Status != pipeline.TurnDone || !last.Ready || !strings.HasPrefix(last.Reply, "_Gemini second opinion unavailable ("+reason+"): posted without it._\n\nHere is the blueprint") {
				t.Fatalf("turn %+v\n%s", last, g.logs.String())
			}
			if len(g.docs(p.ID, pipeline.DocCritique)) != 0 || strings.Contains(g.docs(p.ID, pipeline.DocBlueprint)[0].Markdown, "Gemini") {
				t.Fatal("a critique or a revision without Gemini")
			}
			if strings.Contains(last.Reply, "RESOURCE_EXHAUSTED") {
				t.Fatal("agy's stderr reached loopd")
			}
		})
	}
}

// When the answer turn gives no answers, the blueprint is posted as it was, and the critique
// lists the points unanswered.
func TestPlanCritiqueUnanswered(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.toEngineering(p, fakeclaude.PlanUnanswered)
	g.gemini(false, fakeagy.OK)
	g.runOnce()
	last := g.lastTurn(p.ID)
	if last.Status != pipeline.TurnDone || !last.Ready || !strings.HasSuffix(last.Reply, "\n\nHere is the blueprint (turn 2): three pieces.") ||
		!strings.HasPrefix(last.Reply, "Gemini raised 3 points; they were not answered (no usable out/result.json, exit 0)") {
		t.Fatalf("turn %+v\n%s", last, g.logs.String())
	}
	cs := g.docs(p.ID, pipeline.DocCritique)
	if len(cs) != 1 || strings.Count(cs[0].Markdown, "**Not answered.**") != 3 || strings.Contains(g.docs(p.ID, pipeline.DocBlueprint)[0].Markdown, "Gemini") {
		t.Fatalf("critique %+v", cs)
	}
}

// Under the fence (the default) agy runs in the plan's directory read-only.
func TestPlanCritiqueFenced(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	if err := g.r.ProbeFence(); err != nil {
		t.Skipf("no fence here: %v", err)
	}
	g.toEngineering(p, fakeclaude.PlanBlueprint)
	g.gemini(false, fakeagy.OK)
	on := true
	g.r.Cfg.Gemini.Fence = &on
	g.r.Cfg.Gemini.Home = t.TempDir()
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, g.scripts)
	g.runOnce()
	calls := g.agyCalls()
	if len(calls) != 1 || calls[0].Writable || calls[0].Cwd != filepath.Join(g.r.Cfg.WorkRoot, "plan-1") {
		t.Fatalf("calls %+v; log:\n%s", calls, g.logs.String())
	}
	if last := g.lastTurn(p.ID); !strings.HasPrefix(last.Reply, "Gemini raised 3 points: 2 taken, 1 answered.") {
		t.Fatalf("reply %q; log:\n%s", last.Reply, g.logs.String())
	}
}

// No gemini block: no agy call, no critique.
func TestPlanCritiqueOff(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.toEngineering(p, fakeclaude.PlanBlueprint)
	g.runOnce()
	if last := g.lastTurn(p.ID); !last.Ready || strings.Contains(last.Reply, "Gemini") || len(g.agyCalls()) != 0 || len(g.docs(p.ID, pipeline.DocCritique)) != 0 {
		t.Fatalf("turn %+v", last)
	}
}

func TestParseCritique(t *testing.T) {
	out := `{"status": "success", "response": "{\"points\": [` +
		`{\"severity\": \"HIGH\", \"section\": \"piece 1\", \"kind\": \"missing\", \"point\": \"a\"},` +
		`{\"severity\": \"urgent\", \"section\": \"x\", \"kind\": \"risk\", \"point\": \"b\"},` +
		`{\"severity\": \"low\", \"section\": \"x\", \"kind\": \"vibes\", \"point\": \"c\"},` +
		`{\"severity\": \"low\", \"section\": \"x\", \"kind\": \"risk\", \"point\": \" \"}]}"}`
	pts, dropped, why := ParseCritique([]byte(out))
	if why != "" || dropped != 2 || len(pts) != 2 || pts[0] != (CritiquePoint{ID: 1, Severity: "high", Section: "piece 1", Kind: "missing", Point: "a"}) || pts[1].ID != 2 || pts[1].Kind != "risk" {
		t.Fatalf("points %+v dropped %d why %q", pts, dropped, why)
	}
	if _, _, why := ParseCritique([]byte(`{"status": "success", "response": {"findings": []}}`)); why != AgyBadOutput {
		t.Fatalf("no points: %q", why)
	}
	if _, _, why := ParseCritique([]byte(`{"status": "error", "error": "429 quota"}`)); why != AgyQuota {
		t.Fatalf("quota: %q", why)
	}
}

func TestValidateCritiqueAnswers(t *testing.T) {
	pts := []CritiquePoint{{ID: 1}, {ID: 2}}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bp.md"), []byte("## Pieces\n### 1. A\n- depends on: 1\n"), 0o644)
	ok := `{"reply": "r", "answers": [{"id": 1, "verdict": "agree", "why": "w"}, {"id": 2, "verdict": "disagree", "why": "w"}]}`
	if reply, a, bp, err := ValidateCritiqueAnswers([]byte(ok), pts, dir, nil); err != nil || reply != "r" || len(a) != 2 || bp != "" {
		t.Fatalf("%q %v %q %v", reply, a, bp, err)
	}
	for raw, want := range map[string]string{
		`{"answers": []}`: "no reply",
		`{"reply": "r", "answers": [{"id": 1, "verdict": "agree", "why": "w"}]}`:                                            "1 answers for 2 points",
		`{"reply": "r", "answers": [{"id": 3, "verdict": "agree", "why": "w"}]}`:                                            "point 3, which does not exist",
		`{"reply": "r", "answers": [{"id": 1, "verdict": "maybe", "why": "w"}, {"id": 2, "verdict": "agree", "why": "w"}]}`: "agree or disagree",
		`{"reply": "r", "answers": [{"id": 1, "verdict": "agree", "why": " "}, {"id": 2, "verdict": "agree", "why": "w"}]}`: "why is required",
		`{"reply": "r", "answers": [{"id": 1, "verdict": "agree", "why": "w"}, {"id": 1, "verdict": "agree", "why": "w"}]}`: "two answers",
		`{"reply": "r", "ready": true, "answers": []}`:                                                                      "unknown field",
		strings.TrimSuffix(ok, "}") + `, "blueprint": "bp.md"}`:                                                             "piece 1 depends on itself",
	} {
		if _, _, _, err := ValidateCritiqueAnswers([]byte(raw), pts, dir, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}

func TestWithSummaryKeepsTheReplyWithinSlack(t *testing.T) {
	got := withSummary("Gemini raised 1 point: 1 taken, 0 answered.", strings.Repeat("é", pipeline.MaxReply))
	if n := len([]rune(got)); n != pipeline.MaxReply || !strings.HasPrefix(got, "Gemini raised 1 point: 1 taken, 0 answered.\n\néé") || !strings.HasSuffix(got, "é…") {
		t.Fatalf("%d runes", n)
	}
}
