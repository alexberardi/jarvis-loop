package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const goldenBrief = "# Pantry list\n\nA list of **what is in the pantry**, for <the kitchen>.\n\n" +
	"## Users\n\n- the cook\n  - and helpers\n- [the spec](https://example.com/spec)\n- [bad](javascript:alert(1))\n\n" +
	"| Need | Why |\n|---|---|\n| list | `shopping` |\n\n```\n<script>alert(1)</script>\n```\n"

// goldenPlan is a plan at split: brief v2 (v1 approved first, then v2), blueprint v1, turns and
// the items it filed.
func goldenPlan() (store.PlanDetail, map[string]pipeline.PlanDoc) {
	at := now.Add(-50 * time.Hour)
	p := pipeline.Plan{ID: 2, Title: "Pantry list", Stage: pipeline.PlanSplit, Priority: "P2", Repos: []string{"alexberardi/jarvis-server"},
		SlackChannel: "C0PLAN", SlackTS: "1760000002.000300", BriefApproved: 2, BlueprintApproved: 1, CreatedAt: at, UpdatedAt: now.Add(-time.Hour)}
	brief := pipeline.PlanDoc{PlanID: 2, Kind: pipeline.DocBrief, Version: 2, Markdown: goldenBrief, CreatedAt: at.Add(2 * time.Hour)}
	blueprint := pipeline.PlanDoc{PlanID: 2, Kind: pipeline.DocBlueprint, Version: 1, Markdown: "# Blueprint\n\n### 1. Pantry table\n- depends on: none\n",
		CreatedAt: at.Add(3 * time.Hour)}
	d := store.PlanDetail{Plan: p,
		Docs: []pipeline.PlanDoc{
			{PlanID: 2, Kind: pipeline.DocBlueprint, Version: 1, CreatedAt: blueprint.CreatedAt},
			{PlanID: 2, Kind: pipeline.DocBrief, Version: 1, CreatedAt: at.Add(time.Hour)},
			{PlanID: 2, Kind: pipeline.DocBrief, Version: 2, CreatedAt: brief.CreatedAt},
		},
		Turns: []pipeline.PlanTurn{
			{ID: 1, PlanID: 2, Persona: pipeline.PersonaProduct, Messages: []string{"a pantry list", "for <the kitchen>"}, Status: pipeline.TurnDone,
				Reply: "Here's a brief.", Ready: true, CreatedAt: at},
			{ID: 2, PlanID: 2, Persona: pipeline.PersonaEngineering, Status: pipeline.TurnFailed, Error: "the blueprint has a cycle", CreatedAt: at.Add(time.Hour)},
			{ID: 3, PlanID: 2, Persona: pipeline.PersonaProduct, BackToBrief: "say who uses it", Messages: []string{}, Status: pipeline.TurnDone,
				Reply: "Updated.", Ready: true, CreatedAt: at.Add(2 * time.Hour)},
		},
		Items: []store.PlanItem{
			{ID: 12, Title: "Pantry table", Stage: pipeline.Build, Piece: 1, DependsOn: []int64{}},
			{ID: 13, Title: "Pantry API", Stage: pipeline.Triage, Piece: 2, DependsOn: []int64{12}},
		},
	}
	return d, map[string]pipeline.PlanDoc{pipeline.DocBrief: brief, pipeline.DocBlueprint: blueprint}
}

func TestPlanGolden(t *testing.T) {
	d, latest := goldenPlan()
	var b bytes.Buffer
	if err := planTmpl.Execute(&b, planView(d, latest, now, "https://jarvis.slack.com")); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "plan.golden.html", b.Bytes())
	out := b.String()
	for _, want := range []string{
		`<h1>Pantry list</h1>`, `<a href="https://example.com/spec" rel="noopener noreferrer">the spec</a>`,
		`<a href="/board/plans/2/brief/1">v1</a>`, `<a href="/board/items/12">#12 Pantry table</a>`,
		`<a href="https://jarvis.slack.com/archives/C0PLAN/p1760000002000300">thread</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script", "javascript:", "<the kitchen>"} {
		if strings.Contains(out, bad) {
			t.Errorf("plan page has %q", bad)
		}
	}
}

func TestPlanDocGolden(t *testing.T) {
	d, _ := goldenPlan()
	v1 := pipeline.PlanDoc{PlanID: 2, Kind: pipeline.DocBrief, Version: 1, Markdown: "# Pantry\n\nFirst go.\n", CreatedAt: now.Add(-49 * time.Hour)}
	var b bytes.Buffer
	if err := docTmpl.Execute(&b, docView(d, v1)); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "plan_brief_v1.golden.html", b.Bytes())
}

// The plan pages through the store: the Planning column, the latest brief rendered, older
// versions linked and served, and 404 for whatever is not there.
func TestBoardPlanPages(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	st.SetRepos([]string{"o/r"})
	p, err := st.CreatePlan(ctx, "user:alex", store.NewPlan{Title: "Pantry list", Priority: "P2", Repos: []string{"o/r"}, Text: "a pantry list"})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(brief string) {
		t.Helper()
		c, err := st.Claim(ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{pipeline.WorkPlan}})
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if _, _, err := st.PlanTurnResult(ctx, "devbox", p.ID, c.Turn.ID, pipeline.TurnResult{Reply: "ok", Brief: brief, Ready: true}); err != nil {
			t.Fatal(err)
		}
	}
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}

	// No brief yet: the page says so.
	if resp, body := get(t, srv.URL+"/board/plans/1", cookie); resp.StatusCode != 200 || !strings.Contains(body, "Not written yet.") {
		t.Fatalf("plan without a brief: %d %s", resp.StatusCode, body)
	}
	turn("# First\n\nv1 text")
	if _, err := st.PlanMessage(ctx, "user:alex", p.ID, "more"); err != nil {
		t.Fatal(err)
	}
	turn("# Second\n\n<b>v2</b> text")

	resp, body := get(t, srv.URL+"/board", cookie)
	if resp.StatusCode != 200 || !strings.Contains(body, `<a class="title" href="/board/plans/1">P1 Pantry list</a>`) ||
		!strings.Contains(body, "your turn") || !strings.Contains(body, "<h2>Planning · 1</h2>") {
		t.Fatalf("board: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, srv.URL+"/board/plans/1", cookie)
	if resp.StatusCode != 200 || !strings.Contains(body, "<h1>Second</h1>") || !strings.Contains(body, "&lt;b&gt;v2&lt;/b&gt;") ||
		!strings.Contains(body, `<a href="/board/plans/1/brief/1">v1</a>`) || strings.Contains(body, "<b>") {
		t.Fatalf("plan: %d %s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'none'; frame-ancestors 'none'" {
		t.Errorf("plan CSP %q", csp)
	}
	resp, body = get(t, srv.URL+"/board/plans/1/brief/1", cookie)
	if resp.StatusCode != 200 || !strings.Contains(body, "<h1>First</h1>") || !strings.Contains(body, `<a href="/board/plans/1/brief/2">v2</a>`) {
		t.Fatalf("brief v1: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/board/plans/9", "/board/plans/x", "/board/plans/0", "/board/plans/1/brief/3", "/board/plans/1/brief/0",
		"/board/plans/1/brief/x", "/board/plans/1/blueprint/1", "/board/plans/1/notes/1", "/board/plans/9/brief/1"} {
		if resp, _ := get(t, srv.URL+path, cookie); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
	// Without the cookie, nothing.
	if resp, _ := get(t, srv.URL+"/board/plans/1", nil); resp.StatusCode != 401 {
		t.Errorf("plan without a token: %d", resp.StatusCode)
	}
}

// Gemini's critique shows after the blueprint, never marked approved (approving files the
// blueprint, the critique is a record).
func TestPlanCritiqueSection(t *testing.T) {
	d, latest := goldenPlan()
	crit := pipeline.PlanDoc{PlanID: 2, Kind: pipeline.DocCritique, Version: 1, Markdown: "# Gemini's critique of the blueprint\n\nGemini raised 1 point: 1 taken, 0 answered.\n",
		CreatedAt: now.Add(-40 * time.Hour)}
	d.Docs = append(d.Docs, pipeline.PlanDoc{PlanID: 2, Kind: pipeline.DocCritique, Version: 1, CreatedAt: crit.CreatedAt})
	latest[pipeline.DocCritique] = crit
	var b bytes.Buffer
	if err := planTmpl.Execute(&b, planView(d, latest, now, "")); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	bp, cr := strings.Index(out, "<h2>Blueprint</h2>"), strings.Index(out, "<h2>Gemini critique</h2>")
	if bp < 0 || cr < bp || !strings.Contains(out[cr:], "Gemini raised 1 point: 1 taken, 0 answered.") {
		t.Fatalf("no critique section after the blueprint:\n%s", out)
	}
	b.Reset()
	if err := docTmpl.Execute(&b, docView(d, crit)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "approved") {
		t.Fatalf("the critique page says approved:\n%s", b.String())
	}
}
