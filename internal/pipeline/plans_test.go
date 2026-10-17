package pipeline

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var testRepos = []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}

func doc(kind string, v int, md string) PlanDoc {
	return PlanDoc{PlanID: 7, Kind: kind, Version: v, Markdown: md, SHA256: DocSHA256(md)}
}

func code(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

const blueprint3 = `# Blueprint

Approach: two tables and a route.

## Pieces

### 1. Tables
- repo: alexberardi/jarvis-loop
- size: S
- builder: qwen
- depends on: none

The migration.

### 2. Store
- **Repo:** alexberardi/jarvis-loop
- **Builder:** Claude
- **Depends on:** 1

The store methods.

### 3. API
- depends on: piece 1

The routes.

## Risks

None worth naming.
`

func TestNewPlan(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	p, err := NewPlan("  Pantry list  ", "", []string{"alexberardi/jarvis-loop"}, testRepos, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Pantry list" || p.Priority != "P3" || p.Stage != PlanBrief || !slices.Equal(p.Repos, []string{"alexberardi/jarvis-loop"}) {
		t.Fatalf("plan %+v", p)
	}
	for _, tc := range []struct {
		name, title, pri string
		repos            []string
		code             string
	}{
		{"no title", " ", "", nil, CodeMissingField},
		{"bad priority", "x", "P9", nil, CodeInvalidField},
		{"unknown repo", "x", "", []string{"someone/else"}, CodeInvalidField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPlan(tc.title, tc.pri, tc.repos, testRepos, now); code(err) != tc.code {
				t.Fatalf("err %v, want %s", err, tc.code)
			}
		})
	}
}

func TestApplyPlanApprove(t *testing.T) {
	briefs := []PlanDoc{doc(DocBrief, 1, "v1"), doc(DocBrief, 2, "v2"), doc(DocBrief, 3, "v3")}
	atBrief := Plan{ID: 7, Stage: PlanBrief, Priority: "P2"}
	atBlueprint := Plan{ID: 7, Stage: PlanBlueprint, Priority: "P2"}
	cases := []struct {
		name    string
		plan    Plan
		kind    string
		sha     string
		docs    []PlanDoc
		code    string
		message string
		to      PlanStage
	}{
		{"latest brief", atBrief, DocBrief, briefs[2].SHA256, briefs, "", "", PlanBlueprint},
		{"stale brief names the newer version", atBrief, DocBrief, briefs[0].SHA256, briefs, CodeSHAMismatch, "the brief changed; approve v3 instead", ""},
		{"unknown sha", atBrief, DocBrief, strings.Repeat("a", 64), briefs, CodeSHAMismatch, "approve v3 instead", ""},
		{"no sha", atBrief, DocBrief, "", briefs, CodeMissingField, "sha256", ""},
		{"no brief yet", atBrief, DocBrief, "abc", nil, CodeWrongStage, "no brief yet", ""},
		{"blueprint at brief", atBrief, DocBlueprint, "abc", nil, CodeWrongStage, "plan P7 is at brief, not blueprint", ""},
		{"brief at blueprint", atBlueprint, DocBrief, briefs[2].SHA256, briefs, CodeWrongStage, "plan P7 is at blueprint", ""},
		{"unknown kind", atBrief, "memo", "abc", nil, CodeInvalidField, "kind", ""},
		{"blueprint", atBlueprint, DocBlueprint, DocSHA256(blueprint3), []PlanDoc{doc(DocBlueprint, 1, blueprint3)}, "", "", PlanSplit},
		{"blueprint with a cycle", atBlueprint, DocBlueprint, DocSHA256("## Pieces\n### 1. A\n- depends on: 2\n### 2. B\n- depends on: 1\n"),
			[]PlanDoc{doc(DocBlueprint, 1, "## Pieces\n### 1. A\n- depends on: 2\n### 2. B\n- depends on: 1\n")}, CodeInvalidField, "cycle: 1 → 2 → 1", ""},
		{"dropped", Plan{ID: 7, Stage: PlanDropped}, DocBrief, briefs[2].SHA256, briefs, CodeWrongStage, "dropped", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ApplyPlanApprove(tc.plan, tc.kind, tc.sha, tc.docs, testRepos)
			if code(err) != tc.code {
				t.Fatalf("err %v, want code %q", err, tc.code)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("err %q, want it to say %q", err, tc.message)
				}
				return
			}
			if a.Plan.Stage != tc.to {
				t.Fatalf("stage %s, want %s", a.Plan.Stage, tc.to)
			}
			last := tc.docs[len(tc.docs)-1]
			if a.Doc.Version != last.Version {
				t.Fatalf("approved v%d, want v%d", a.Doc.Version, last.Version)
			}
			switch tc.kind {
			case DocBrief:
				if a.Plan.BriefApproved != last.Version {
					t.Fatalf("brief_approved %d", a.Plan.BriefApproved)
				}
			case DocBlueprint:
				if a.Plan.BlueprintApproved != last.Version || len(a.Pieces) != 3 {
					t.Fatalf("blueprint_approved %d, %d pieces", a.Plan.BlueprintApproved, len(a.Pieces))
				}
			}
		})
	}
}

func TestApplyPlanDrop(t *testing.T) {
	for _, st := range []PlanStage{PlanBrief, PlanBlueprint, PlanSplit} {
		p, err := ApplyPlanDrop(Plan{Stage: st})
		if err != nil || p.Stage != PlanDropped {
			t.Fatalf("%s: %v %s", st, err, p.Stage)
		}
	}
	for _, st := range []PlanStage{PlanDone, PlanDropped} {
		if _, err := ApplyPlanDrop(Plan{Stage: st}); code(err) != CodeWrongStage {
			t.Fatalf("%s: %v", st, err)
		}
	}
}

func TestParseBlueprint(t *testing.T) {
	ps, err := ParseBlueprint(blueprint3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("%d pieces", len(ps))
	}
	want := []struct {
		title, repo, size, builder string
		deps                       []int
		section                    string
	}{
		{"Tables", "alexberardi/jarvis-loop", "S", "qwen", nil, "The migration."},
		{"Store", "alexberardi/jarvis-loop", "", "claude", []int{1}, "The store methods."},
		{"API", "", "", "", []int{1}, "The routes."},
	}
	for i, w := range want {
		p := ps[i]
		if p.N != i+1 || p.Title != w.title || p.Repo != w.repo || p.Size != w.size || p.Builder != w.builder || !slices.Equal(p.DependsOn, w.deps) {
			t.Errorf("piece %d: %+v", i+1, p)
		}
		if !strings.HasPrefix(p.Section, "### ") || !strings.HasSuffix(p.Section, w.section) {
			t.Errorf("piece %d section %q", i+1, p.Section)
		}
		if strings.Contains(p.Section, "Risks") {
			t.Errorf("piece %d section runs past the pieces: %q", i+1, p.Section)
		}
	}

	if ps, err := ParseBlueprint("## Pieces\n### 1. A\n- builder: `QWEN`\n"); err != nil || ps[0].Builder != "qwen" {
		t.Errorf("backticked builder: %+v %v", ps, err)
	}

	// "#N" is an existing loop item the piece waits for; a bare N is a piece.
	after := []struct {
		dep    string
		pieces []int
		items  []int64
	}{
		{"1, #30", []int{1}, []int64{30}},
		{"#30", nil, []int64{30}},
		{"#30, #30, 1, #7", []int{1}, []int64{30, 7}},
		{"item #30", nil, []int64{30}},
		{"none", nil, nil},
	}
	for _, tc := range after {
		ps, err := ParseBlueprint("## Pieces\n### 1. A\n### 2. B\n- depends on: " + tc.dep + "\n")
		if err != nil || !slices.Equal(ps[1].DependsOn, tc.pieces) || !slices.Equal(ps[1].After, tc.items) {
			t.Errorf("depends on: %s -> %+v %v", tc.dep, ps, err)
		}
	}

	bad := []struct{ name, md, msg string }{
		{"no pieces section", "# Blueprint\n\nJust prose.\n", "no Pieces section"},
		{"empty pieces", "## Pieces\n\nNone yet.\n", "no pieces"},
		{"numbering skips", "## Pieces\n### 1. A\n### 3. C\n", "piece 3 comes after piece 1"},
		{"unknown dependency", "## Pieces\n### 1. A\n### 2. B\n- depends on: 4\n", "piece 2 depends on piece 4, which does not exist"},
		{"self dependency", "## Pieces\n### 1. A\n- depends on: 1\n", "piece 1 depends on itself"},
		{"cycle", "## Pieces\n### 1. A\n- depends on: 3\n### 2. B\n- depends on: 1\n### 3. C\n- depends on: 2\n", "cycle: 1 → 3 → 2 → 1"},
		{"bad size", "## Pieces\n### 1. A\n- size: XL\n", "piece 1: size must be S, M or L"},
		{"no title", "## Pieces\n### 1.\n", "piece 1 has no title"},
		{"bad builder", "## Pieces\n### 1. A\n- builder: gpt\n", `piece 1: builder must be qwen or claude, not "gpt"`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBlueprint(tc.md)
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err %v, want %q", err, tc.msg)
			}
		})
	}
}

func TestPlanItems(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	ps, err := ParseBlueprint(blueprint3)
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{ID: 7, Title: "Pantry", Priority: "P2", Stage: PlanSplit, Repos: []string{"alexberardi/jarvis-server"}}
	items, err := PlanItems(p, ps, testRepos, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
	for i, it := range items {
		if it.Source != SourcePlan || it.Priority != "P2" || it.PlanID != 7 || it.PlanPiece != i+1 || it.Stage != Triage {
			t.Errorf("item %d: %+v", i, it)
		}
		if !strings.HasPrefix(it.Ask, "Piece "+string(rune('1'+i))+" of plan P7 (Pantry).") || !strings.HasSuffix(it.Ask, ps[i].Section) {
			t.Errorf("item %d ask %q", i, it.Ask)
		}
	}
	// The builder goes onto the item; a piece without one gets claude, the safe builder.
	if b := []string{items[0].Builder, items[1].Builder, items[2].Builder}; !slices.Equal(b, []string{"qwen", "claude", "claude"}) {
		t.Errorf("builders %q", b)
	}
	// A piece without a repo takes the plan's only repo.
	if items[0].Repo != "alexberardi/jarvis-loop" || items[2].Repo != "alexberardi/jarvis-server" {
		t.Errorf("repos %q %q", items[0].Repo, items[2].Repo)
	}
	ps[0].Repo = "someone/else"
	if _, err := PlanItems(p, ps, testRepos, now); code(err) != CodeInvalidField {
		t.Fatalf("unknown repo: %v", err)
	}
}

func TestPlanSettled(t *testing.T) {
	cases := []struct {
		stages []Stage
		want   bool
	}{
		{nil, false},
		{[]Stage{Done, Done}, true},
		{[]Stage{Done, Closed}, true},
		{[]Stage{Closed, Closed}, false},
		{[]Stage{Done, QA}, false},
		{[]Stage{Done, Build}, false},
	}
	for _, tc := range cases {
		if got := PlanSettled(tc.stages); got != tc.want {
			t.Errorf("%v: %v", tc.stages, got)
		}
	}
}

func TestApplyTurnResult(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	brief := Plan{ID: 7, Stage: PlanBrief}
	blue := Plan{ID: 7, Stage: PlanBlueprint, BriefApproved: 1}
	product := PlanTurn{ID: 1, PlanID: 7, Persona: PersonaProduct, Status: TurnRunning}
	eng := PlanTurn{ID: 2, PlanID: 7, Persona: PersonaEngineering, Status: TurnRunning}
	b1 := doc(DocBrief, 1, "brief one")
	cases := []struct {
		name   string
		plan   Plan
		turn   PlanTurn
		latest map[string]PlanDoc
		res    TurnResult
		failed string
		docs   []int // versions added
		stage  PlanStage
		ready  bool
	}{
		{"first brief", brief, product, nil, TurnResult{Reply: "here", Brief: "brief one", Ready: true, SessionID: "s1"}, "", []int{1}, PlanBrief, true},
		{"new brief version", brief, product, map[string]PlanDoc{DocBrief: b1}, TurnResult{Reply: "x", Brief: "brief two"}, "", []int{2}, PlanBrief, false},
		{"same brief adds nothing", brief, product, map[string]PlanDoc{DocBrief: b1}, TurnResult{Reply: "x", Brief: "brief one", Ready: true}, "", nil, PlanBrief, true},
		{"ready without a brief", brief, product, nil, TurnResult{Reply: "x", Ready: true}, "ready, but there is no brief", nil, PlanBrief, false},
		{"no reply", brief, product, nil, TurnResult{}, "no reply", nil, PlanBrief, false},
		{"reply too long", brief, product, nil, TurnResult{Reply: strings.Repeat("x", 3001)}, "reply has 3001 characters", nil, PlanBrief, false},
		{"runner reports failure", brief, product, nil, TurnResult{Error: "claude exited 1", SessionID: "s1"}, "claude exited 1", nil, PlanBrief, false},
		{"product writes a blueprint", brief, product, nil, TurnResult{Reply: "x", Blueprint: blueprint3}, "only engineering writes the blueprint", nil, PlanBrief, false},
		{"product sends back", brief, product, nil, TurnResult{Reply: "x", BackToBrief: "why"}, "only engineering", nil, PlanBrief, false},
		{"unknown repo", brief, product, nil, TurnResult{Reply: "x", Repos: []string{"a/b"}}, "repo", nil, PlanBrief, false},
		{"blueprint", blue, eng, nil, TurnResult{Reply: "x", Blueprint: blueprint3, Ready: true}, "", []int{1}, PlanBlueprint, true},
		{"blueprint cycle", blue, eng, nil, TurnResult{Reply: "x", Blueprint: "## Pieces\n### 1. A\n- depends on: 1\n"}, "blueprint: piece 1 depends on itself", nil, PlanBlueprint, false},
		{"engineering writes a brief", blue, eng, nil, TurnResult{Reply: "x", Brief: "b"}, "only product writes the brief", nil, PlanBlueprint, false},
		{"back to brief", blue, eng, nil, TurnResult{Reply: "x", BackToBrief: "far bigger than it looks"}, "", nil, PlanBrief, false},
		{"back to brief and ready", blue, eng, nil, TurnResult{Reply: "x", BackToBrief: "why", Ready: true}, "both ready and back_to_brief", nil, PlanBlueprint, false},
		{"stage moved on: reply only", blue, product, nil, TurnResult{Reply: "x", Brief: "late", Ready: true}, "", nil, PlanBlueprint, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := ApplyTurnResult(tc.plan, tc.turn, tc.res, tc.latest, testRepos, now)
			if tc.failed == "" && o.Failed != "" || tc.failed != "" && !strings.Contains(o.Failed, tc.failed) {
				t.Fatalf("failed %q, want %q", o.Failed, tc.failed)
			}
			var vs []int
			for _, d := range o.Docs {
				vs = append(vs, d.Version)
				if d.SHA256 != DocSHA256(d.Markdown) || d.PlanID != 7 {
					t.Fatalf("doc %+v", d)
				}
			}
			if !slices.Equal(vs, tc.docs) {
				t.Fatalf("docs %v, want %v", vs, tc.docs)
			}
			if o.Plan.Stage != tc.stage || o.Ready != tc.ready {
				t.Fatalf("stage %s ready %v", o.Plan.Stage, o.Ready)
			}
			if tc.res.SessionID != "" && tc.turn.Persona == PersonaProduct && o.Plan.ProductSession != tc.res.SessionID {
				t.Fatalf("session not kept: %+v", o.Plan)
			}
		})
	}
}

func TestChooseWork(t *testing.T) {
	ctl := DefaultControl()
	both := ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{WorkItem, WorkPlan}}
	turn := func(id int64, pri string, age time.Duration) TurnCandidate {
		return TurnCandidate{TurnID: id, PlanID: id, Priority: pri, CreatedAt: t0.Add(-age)}
	}
	full := DefaultControl()
	full.ActiveAgents = 1
	cases := []struct {
		name  string
		ctl   Control
		items []Item
		turns []TurnCandidate
		live  []LiveLease
		req   ClaimRequest
		item  int64
		turn  int64
	}{
		{"items only by default", ctl, []Item{waiting(1, Triage, "P3", 0)}, []TurnCandidate{turn(5, "P1", 0)}, nil,
			ClaimRequest{Agent: "a"}, 1, 0},
		{"a waiting user beats an item of the same priority", ctl, []Item{waiting(1, Build, "P2", time.Hour)}, []TurnCandidate{turn(5, "P2", 0)}, nil, both, 0, 5},
		{"a higher-priority item wins", ctl, []Item{waiting(1, Build, "P1", 0)}, []TurnCandidate{turn(5, "P2", time.Hour)}, nil, both, 1, 0},
		{"a higher-priority turn wins", ctl, []Item{waiting(1, Build, "P3", 0)}, []TurnCandidate{turn(5, "P2", 0)}, nil, both, 0, 5},
		{"oldest turn first", ctl, nil, []TurnCandidate{turn(5, "P3", 0), turn(6, "P3", time.Hour)}, nil, both, 0, 6},
		{"plan only", ctl, []Item{waiting(1, Build, "P1", 0)}, []TurnCandidate{turn(5, "P4", 0)}, nil,
			ClaimRequest{Agent: "a", Kinds: []string{WorkPlan}}, 0, 5},
		{"shared limit", full, nil, []TurnCandidate{turn(5, "P1", 0)}, []LiveLease{{ItemID: 1, Stage: Build}}, both, 0, 0},
		{"a running turn counts", full, []Item{waiting(1, Build, "P1", 0)}, nil, []LiveLease{{Stage: PlanTurnStage}}, both, 0, 0},
		{"stopped", Control{Stopped: true, ActiveAgents: 4, MaxRunMinutes: 45}, nil, []TurnCandidate{turn(5, "P1", 0)}, nil, both, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWork(tc.ctl, tc.items, nil, tc.live, tc.turns, nil, tc.req)
			var gotItem, gotTurn int64
			if ok && w.Item != nil {
				gotItem = w.Item.ID
			}
			if ok && w.Turn != nil {
				gotTurn = w.Turn.TurnID
			}
			if ok != (tc.item != 0 || tc.turn != 0) || gotItem != tc.item || gotTurn != tc.turn {
				t.Fatalf("got item %d turn %d (ok %v), want item %d turn %d", gotItem, gotTurn, ok, tc.item, tc.turn)
			}
		})
	}
}
