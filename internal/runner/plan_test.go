package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func TestPlanPrompt(t *testing.T) {
	p := pipeline.Plan{ID: 3, Title: "Pantry list", Priority: "P2", Repos: []string{"alex/demo"}}
	first := PlanPrompt(PlanTurnInfo{Plan: p, Turn: pipeline.PlanTurn{ID: 5, Persona: pipeline.PersonaProduct, Messages: []string{"a pantry list", "for the kitchen\nonly"}},
		Dir: "/w/plan-3", Checkouts: []Checkout{{Key: "demo", Slug: "alex/demo", Base: "main", Dir: "repos/demo"}},
		Context: []TriageFile{{Name: ".loop/context/repos.md", Label: "the configured repos"}}})
	// A persona's first prompt opens with its static brief (the cacheable prefix), then the turn.
	static := StaticBrief(pipeline.PersonaProduct)
	if !strings.HasPrefix(first, static+TurnHeading) || !strings.HasPrefix(first, "# Product (planning)") {
		t.Fatalf("the first prompt must start with the product brief:\n%s", first[:200])
	}
	for _, want := range []string{"- Plan: P3: Pantry list\n", "- Persona: product\n", "- Turn: 5, your first", "`repos/demo/` (`alex/demo`, `origin/main`)",
		"`.loop/context/repos.md` (the configured repos)", "`/w/plan-3`", "> a pantry list\n", "> for the kitchen\n> only\n", `"brief": "brief.md"`} {
		if !strings.Contains(first, want) {
			t.Errorf("the first prompt lacks %q", want)
		}
	}
	// A resumed session gets only the turn: its messages, verbatim; the reason of a hand-back first.
	later := PlanPrompt(PlanTurnInfo{Plan: p, Resume: "s-1", Dir: "/w/plan-3",
		Turn: pipeline.PlanTurn{ID: 9, Persona: pipeline.PersonaProduct, Messages: []string{"ok"}, BackToBrief: "offline sync is too big"}})
	if !strings.HasPrefix(later, "## This turn\n") || strings.Contains(later, "# Product (planning)") || strings.Contains(later, "result contract") {
		t.Fatalf("a resumed prompt is the turn alone:\n%s", later)
	}
	if i, j := strings.Index(later, "offline sync is too big"), strings.Index(later, "> ok"); i < 0 || j < i {
		t.Fatalf("the hand-back's reason comes before the messages:\n%s", later)
	}
	if !strings.Contains(later, "- Turn: 9 (your session continues") {
		t.Fatalf("resumed turn line:\n%s", later)
	}
	// Engineering's first turn has no message: it starts from the approved brief.
	eng := PlanPrompt(PlanTurnInfo{Plan: p, Turn: pipeline.PlanTurn{ID: 6, Persona: pipeline.PersonaEngineering}})
	if !strings.HasPrefix(eng, StaticBrief(pipeline.PersonaEngineering)+TurnHeading) || !strings.Contains(eng, "The brief was approved: start the blueprint.") {
		t.Fatalf("engineering's first prompt:\n%s", eng[len(eng)-400:])
	}
	// The static briefs name their result fields and the blueprint format ParseBlueprint reads.
	if e := StaticBrief(pipeline.PersonaEngineering); !strings.Contains(e, `"back_to_brief"`) || !strings.Contains(e, "### 1. Pantry table\n- repo: owner/name\n- size: S\n- depends on: none") {
		t.Fatal("the engineering brief lacks its contract or the pieces format")
	}
}

// The blueprint example in the engineering skill parses as loopd's hand-off reads it.
func TestEngineeringSkillExampleParses(t *testing.T) {
	e := StaticBrief(pipeline.PersonaEngineering)
	i := strings.Index(e, "```markdown\n")
	j := strings.Index(e[i+12:], "```")
	example := strings.ReplaceAll(e[i+12:i+12+j], "\n...\n", "\n")
	pieces, err := pipeline.ParseBlueprint(example)
	if err != nil || len(pieces) != 2 || pieces[1].Size != "M" || !slices.Equal(pieces[1].DependsOn, []int{1}) || pieces[0].Repo != "owner/name" {
		t.Fatalf("%+v %v", pieces, err)
	}
}

func TestValidatePlanResult(t *testing.T) {
	out := t.TempDir()
	os.WriteFile(filepath.Join(out, "brief.md"), []byte("# Brief\n\nA list.\n"), 0o644)
	os.WriteFile(filepath.Join(out, "empty.md"), []byte("  \n"), 0o644)
	os.WriteFile(filepath.Join(out, "bp.md"), []byte("# B\n\n## Pieces\n\n### 1. One\n- repo: alex/demo\n\n### 2. Two\n- depends on: 1\n"), 0o644)
	os.WriteFile(filepath.Join(out, "cycle.md"), []byte("## Pieces\n\n### 1. One\n- depends on: 2\n\n### 2. Two\n- depends on: 1\n"), 0o644)
	os.WriteFile(filepath.Join(out, "badrepo.md"), []byte("## Pieces\n\n### 1. One\n- repo: nobody/nothing\n"), 0o644)
	os.Symlink("/etc/hostname", filepath.Join(out, "link.md"))
	slugs := []string{"alex/demo", "alex/other"}
	long := strings.Repeat("é", pipeline.MaxReply+1)
	for _, c := range []struct {
		name, persona, raw, err string
	}{
		{"brief", "product", `{"reply":"Here it is.","brief":"brief.md","ready":true,"repos":["alex/demo"]}`, ""},
		{"brief under out/", "product", `{"reply":"ok","brief":"out/brief.md"}`, ""},
		{"blueprint", "engineering", `{"reply":"ok","blueprint":"bp.md","ready":true}`, ""},
		{"back", "engineering", `{"reply":"too big","back_to_brief":"offline sync"}`, ""},
		{"no reply", "product", `{"reply":"  ","brief":"brief.md"}`, "no reply"},
		{"long reply", "product", `{"reply":"` + long + `"}`, "at most 3000"},
		{"unknown field", "product", `{"reply":"ok","outcome":"done"}`, `unknown field "outcome"`},
		{"not json", "product", `{"reply":`, "out/result.json"},
		{"product blueprint", "product", `{"reply":"ok","blueprint":"bp.md"}`, "only engineering writes the blueprint"},
		{"product back", "product", `{"reply":"ok","back_to_brief":"x"}`, "only engineering sends"},
		{"engineering brief", "engineering", `{"reply":"ok","brief":"brief.md"}`, "only product writes the brief"},
		{"engineering repos", "engineering", `{"reply":"ok","repos":["alex/demo"]}`, "only product sets"},
		{"back and ready", "engineering", `{"reply":"ok","back_to_brief":"x","ready":true}`, "back_to_brief goes alone"},
		{"unknown repo", "product", `{"reply":"ok","repos":["nobody/nothing"]}`, `"nobody/nothing" is not a configured repo`},
		{"missing doc", "product", `{"reply":"ok","brief":"nope.md"}`, "brief: \"nope.md\""},
		{"escaping doc", "product", `{"reply":"ok","brief":"../brief.md"}`, "not a file in out/"},
		{"absolute doc", "product", `{"reply":"ok","brief":"/etc/hostname"}`, "not a file in out/"},
		{"symlink doc", "product", `{"reply":"ok","brief":"link.md"}`, "not a plain file"},
		{"empty doc", "product", `{"reply":"ok","brief":"empty.md"}`, "is empty"},
		{"cycle", "engineering", `{"reply":"ok","blueprint":"cycle.md"}`, "blueprint: the pieces' dependencies make a cycle: "},
		{"piece repo", "engineering", `{"reply":"ok","blueprint":"badrepo.md"}`, `piece 1's repo "nobody/nothing" is not a configured repo`},
	} {
		res, err := ValidatePlanResult([]byte(c.raw), c.persona, out, slugs)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.err)
		}
		if c.name == "brief" && (res.Brief != "# Brief\n\nA list.\n" || !res.Ready || !slices.Equal(res.Repos, []string{"alex/demo"}) || res.Reply != "Here it is.") {
			t.Errorf("brief result %+v", res)
		}
		if c.name == "blueprint" && !strings.Contains(res.Blueprint, "### 2. Two") {
			t.Errorf("blueprint result %+v", res)
		}
	}
}

// planRig is a runner that claims plan turns only, with a plan at brief.
func planRig(t *testing.T, scenario string) (*rig, pipeline.Plan) {
	g := newRig(t, scenario)
	g.r.Cfg.Stages = []string{StagePlan}
	g.r.Cfg.PlanTurnMinutes = 15
	p, err := g.st.CreatePlan(g.ctx, "user:alex", store.NewPlan{Title: "Pantry list", Priority: "P2", Repos: []string{"alex/demo"}, Text: "a pantry list"})
	if err != nil {
		t.Fatal(err)
	}
	return g, p
}

func (g *rig) plan(id int64) store.PlanDetail {
	g.t.Helper()
	d, err := g.st.GetPlan(g.ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return d
}

func (g *rig) planRuns(id int64, persona string) []string {
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", "P"+itoa(id)+"-"+persona+"-*"))
	slices.Sort(runs)
	return runs
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func argvOf(t *testing.T, run string) []string {
	t.Helper()
	var argv []string
	b, err := os.ReadFile(filepath.Join(run, "argv.json"))
	if err != nil || json.Unmarshal(b, &argv) != nil {
		t.Fatalf("argv.json: %v", err)
	}
	return argv
}

// A plan's turns: the first of a persona starts a session in the plan's own directory, loopd
// stores its id, the next resumes it; each turn sees its messages and an empty out/; the
// checkouts are read-only (the guard refuses a write outside out/).
func TestPlanTurnsResumeTheSession(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanEscape)
	if !g.runOnce() {
		t.Fatal("no turn claimed")
	}
	d := g.plan(p.ID)
	if len(d.Turns) != 1 || d.Turns[0].Status != pipeline.TurnDone || d.Plan.ProductSession != "fake-session-P1-product" {
		t.Fatalf("turn %+v session %q\n%s", d.Turns, d.Plan.ProductSession, g.logs.String())
	}
	brief, err := g.st.GetPlanDoc(g.ctx, p.ID, "brief", 0)
	if err != nil || !strings.Contains(brief.Markdown, "Turn 1 heard: a pantry list") {
		t.Fatalf("brief %+v %v", brief, err)
	}
	dir := filepath.Join(g.r.Cfg.WorkRoot, "plan-1")
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err == nil {
		t.Fatal("the agent wrote outside out/")
	}
	if b, _ := os.ReadFile(filepath.Join(g.planRuns(p.ID, "product")[0], "refusals.jsonl")); !strings.Contains(string(b), "notes.md") {
		t.Fatalf("the guard did not refuse the write outside out/: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "repos", "demo", "README.md")); err != nil {
		t.Fatalf("the plan's repo is not checked out: %v", err)
	}
	if argv := argvOf(t, g.planRuns(p.ID, "product")[0]); slices.Contains(argv, "--resume") {
		t.Fatalf("the first turn resumed something: %q", argv)
	}
	os.WriteFile(filepath.Join(dir, "out", "stale.md"), []byte("old"), 0o644)

	if _, err := g.st.PlanMessage(g.ctx, "user:alex", p.ID, "only the kitchen"); err != nil {
		t.Fatal(err)
	}
	if !g.runOnce() {
		t.Fatal("no second turn")
	}
	d = g.plan(p.ID)
	if len(d.Turns) != 2 || d.Turns[1].Status != pipeline.TurnDone || d.Plan.ProductSession != "fake-session-P1-product" {
		t.Fatalf("second turn %+v\n%s", d.Turns, g.logs.String())
	}
	runs := g.planRuns(p.ID, "product")
	a, b := argvOf(t, runs[0]), argvOf(t, runs[1])
	i := slices.Index(b, "--resume")
	if i < 0 || b[i+1] != "fake-session-P1-product" || !slices.Equal(a, slices.Delete(slices.Clone(b), i, i+2)) {
		t.Fatalf("the second turn's argv %q, the first's %q", b, a)
	}
	prompt, _ := os.ReadFile(filepath.Join(runs[1], "brief.md"))
	if !strings.HasPrefix(string(prompt), "## This turn\n") || !strings.Contains(string(prompt), "> only the kitchen") || strings.Contains(string(prompt), "a pantry list") {
		t.Fatalf("the second prompt:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "stale.md")); err == nil {
		t.Fatal("out/ was not emptied for the turn")
	}
	if ctx, _ := os.ReadFile(filepath.Join(dir, ".loop", "context", "brief.md")); !strings.Contains(string(ctx), "Turn 1 heard") {
		t.Fatalf("the latest brief is not in the context: %q", ctx)
	}
	// claude lost the session: the next turn starts a new one, with the whole product brief.
	os.RemoveAll(filepath.Join(g.scripts, "sessions"))
	g.st.PlanMessage(g.ctx, "user:alex", p.ID, "and the fridge")
	g.runOnce()
	d = g.plan(p.ID)
	if last := d.Turns[2]; last.Status != pipeline.TurnDone || !strings.Contains(g.logs.String(), "the session to resume is gone") {
		t.Fatalf("third turn %+v\n%s", last, g.logs.String())
	}
	runs = g.planRuns(p.ID, "product")
	if len(runs) != 4 || slices.Contains(argvOf(t, runs[3]), "--resume") {
		t.Fatalf("runs %v", runs)
	}
	if prompt, _ := os.ReadFile(filepath.Join(runs[3], "brief.md")); !strings.HasPrefix(string(prompt), StaticBrief("product")) {
		t.Fatal("a new session's prompt starts with the persona's brief")
	}
	// Both turns ran with the same settings file, shared by every plan.
	if s := argvOf(t, runs[0]); s[slices.Index(s, "--settings")+1] != filepath.Join(g.r.Cfg.WorkRoot, "settings", "plan-product.json") {
		t.Fatalf("settings %q", s)
	}
}

// Engineering starts its own session after the brief's approval, and a blueprint that loops
// fails the turn in the runner, keeping the session.
func TestPlanTurnFailsOnABadBlueprint(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	g.runOnce()
	doc, _ := g.st.GetPlanDoc(g.ctx, p.ID, "brief", 0)
	if _, err := g.st.ApprovePlan(g.ctx, "user:alex", p.ID, "brief", doc.SHA256, "yes"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.scripts, "P1-engineering.json"), []byte(`{"play":"cycle"}`), 0o644)
	g.runOnce()
	d := g.plan(p.ID)
	last := d.Turns[len(d.Turns)-1]
	if last.Persona != "engineering" || last.Status != pipeline.TurnFailed || !strings.Contains(last.Error, "blueprint: the pieces' dependencies make a cycle") {
		t.Fatalf("turn %+v", last)
	}
	if d.Plan.EngineeringSession != "fake-session-P1-engineering" || d.Plan.ProductSession != "fake-session-P1-product" {
		t.Fatalf("sessions %q %q", d.Plan.ProductSession, d.Plan.EngineeringSession)
	}
	for _, doc := range d.Docs {
		if doc.Kind == "blueprint" {
			t.Fatal("a failed turn stored a blueprint")
		}
	}
	// Both personas run read-only (the settings deny commits; the read-only golden's input), with
	// the same settings bytes; nothing in them is per plan.
	sp, _ := os.ReadFile(filepath.Join(g.r.Cfg.WorkRoot, "settings", "plan-product.json"))
	se, _ := os.ReadFile(filepath.Join(g.r.Cfg.WorkRoot, "settings", "plan-engineering.json"))
	if len(sp) == 0 || string(sp) != string(se) || !strings.Contains(string(sp), `"Bash(git commit *)"`) || strings.Contains(string(sp), "plan-1") {
		t.Fatalf("plan settings:\n%s", sp)
	}
	if in := g.r.planSettingsInput("/h"); !in.ReadOnly || len(in.ExcludedCommands) > 0 {
		t.Fatalf("plan settings input %+v", in)
	}
}

// A turn whose agent fails (no result) or runs over plan_turn_minutes fails with why.
func TestPlanTurnFailures(t *testing.T) {
	g, p := planRig(t, fakeclaude.Exit3)
	g.runOnce()
	last := g.plan(p.ID).Turns[0]
	if last.Status != pipeline.TurnFailed || !strings.Contains(last.Error, "without a usable out/result.json") {
		t.Fatalf("turn %+v", last)
	}

	g, p = planRig(t, fakeclaude.Hang)
	g.r.Cfg.PlanTurnMinutes = 1 // under loopd's max_run_minutes: the turn's own cap applies
	wait := g.runAsync()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.scripts, "P1.state"))
	g.clk.Advance(30 * time.Second) // 1 minute minus the runner's 30 s margin
	wait()
	last = g.plan(p.ID).Turns[0]
	if last.Status != pipeline.TurnFailed || !strings.Contains(last.Error, "time cap (1 minutes)") {
		t.Fatalf("turn %+v\n%s", last, g.logs.String())
	}
}
