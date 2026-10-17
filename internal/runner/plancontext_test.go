package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// exec runs a statement on loopd's database (what the API cannot do: a later draft after the
// approval, an item's plan link).
func (g *rig) exec(q string, args ...any) {
	g.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(filepath.Dir(g.remote), "..", "loopd", "loop.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		g.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		g.t.Fatal(err)
	}
}

// approvedPlan plays a plan to its hand-off (brief and blueprint v1, both approved), then stores
// a later blueprint draft, v2, that nobody approved. It returns the plan's approved blueprint.
func approvedPlan(g *rig, p pipeline.Plan) pipeline.PlanDoc {
	g.t.Helper()
	g.runOnce()
	brief, _ := g.st.GetPlanDoc(g.ctx, p.ID, "brief", 0)
	if _, err := g.st.ApprovePlan(g.ctx, "user:alex", p.ID, "brief", brief.SHA256, "yes"); err != nil {
		g.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.scripts, "P1-engineering.json"), []byte(`{"play":"blueprint"}`), 0o644)
	g.runOnce()
	bp, _ := g.st.GetPlanDoc(g.ctx, p.ID, "blueprint", 0)
	if _, err := g.st.ApprovePlan(g.ctx, "user:alex", p.ID, "blueprint", bp.SHA256, "yes"); err != nil {
		g.t.Fatal(err)
	}
	draft := "# A later draft nobody approved\n"
	g.exec(`INSERT INTO plan_docs (plan_id, kind, version, markdown, sha256, created_at) VALUES (?, 'blueprint', 2, ?, ?, 0)`,
		p.ID, draft, pipeline.DocSHA256(draft))
	return bp
}

func (g *rig) prepareClaimed(stage string, id int64) (*run, string) {
	g.t.Helper()
	cl, err := g.r.Client.Claim(g.ctx, "b", nil, []string{stage})
	if err != nil || cl == nil || cl.Item.ID != id {
		g.t.Fatalf("%s claim %+v %v", stage, cl, err)
	}
	ru, _ := g.r.newRun(cl, "b")
	brief, err := g.r.prepare(g.ctx, ru)
	if err != nil {
		g.t.Fatal(err)
	}
	return ru, brief
}

// An item filed from a plan gets the plan's approved blueprint and brief (the versions the user
// approved, not a later draft) in .loop/context/ for triage, build and review, and the brief says
// to read them there rather than ask (item #78 asked the user to paste blueprint section 3).
func TestPlanItemsGetTheApprovedBlueprint(t *testing.T) {
	g, p := planRig(t, fakeclaude.PlanBrief)
	bp := approvedPlan(g, p)
	brief, _ := g.st.GetPlanDoc(g.ctx, p.ID, "brief", 1)
	pieces := g.plan(p.ID).Items
	if len(pieces) != 3 {
		t.Fatalf("hand-off filed %d items", len(pieces))
	}
	check := func(stage string, ru *run, b string, piece int) {
		t.Helper()
		if got := g.read(filepath.Join(ru.wt, ".loop", "context", "blueprint.md")); got != bp.Markdown {
			t.Fatalf("%s: blueprint.md is not the approved v1:\n%s", stage, got)
		}
		if got := g.read(filepath.Join(ru.wt, ".loop", "context", "brief.md")); got != brief.Markdown {
			t.Fatalf("%s: brief.md is not the approved brief:\n%s", stage, got)
		}
		item := b[strings.Index(b, ItemHeading):]
		for _, want := range []string{
			"**This item is piece " + itoa(int64(piece)) + " of plan P1.** Your piece is in the ask; the plan's whole approved documents are in `.loop/context/blueprint.md` and `.loop/context/brief.md`, for the sections your piece points at",
			"do not ask the user for what they contain.",
			"`.loop/context/blueprint.md` (plan P1's approved blueprint, v1, sha256 " + bp.SHA256[:7] + ")",
		} {
			if !strings.Contains(item, want) {
				t.Fatalf("%s: the brief lacks %q:\n%s", stage, want, item)
			}
		}
		// Triage works in a plain directory, not a checkout (git status there would read whatever
		// repo the test's temp dir happens to sit in).
		if stage == StageTriage {
			return
		}
		if s := gitT(t, ru.wt, "status", "--porcelain"); s != "" {
			t.Fatalf("%s: the context shows in git status:\n%s", stage, s)
		}
	}

	// Triage of piece 1, as the hand-off filed it.
	ru, b := g.prepareClaimed(StageTriage, pieces[0].ID)
	check(StageTriage, ru, b, 1)
	g.r.cleanup(g.ctx, ru, g.r.Log)
	g.exec(`UPDATE leases SET ended_at = 1 WHERE ended_at IS NULL`)
	g.exec(`UPDATE items SET stage = 'closed' WHERE plan_id = ?`, p.ID)

	// Build and review of an item linked to piece 2.
	id := g.toBuild("Piece two")
	g.exec(`UPDATE items SET plan_id = ?, plan_piece = 2 WHERE id = ?`, p.ID, id)
	ru, b = g.prepareClaimed(StageBuild, id)
	check(StageBuild, ru, b, 2)
	g.r.cleanup(g.ctx, ru, g.r.Log)

	rid := g.toReview("Piece two, built")
	g.exec(`UPDATE items SET plan_id = ?, plan_piece = 2 WHERE id = ?`, p.ID, rid)
	ru, b = g.prepareClaimed(StageReview, rid)
	check(StageReview, ru, b, 2)
	g.r.cleanup(g.ctx, ru, g.r.Log)
}

// An item not from a plan gets no plan documents and no plan line.
func TestNoPlanNoPlanContext(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Plain")
	ru, b := g.prepareClaimed(StageBuild, id)
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	for _, f := range []string{"blueprint.md", "brief.md"} {
		if _, err := os.Stat(filepath.Join(ru.wt, ".loop", "context", f)); err == nil {
			t.Fatalf("%s without a plan", f)
		}
	}
	if strings.Contains(b, "of plan P") {
		t.Fatal("the brief names a plan")
	}
}

// planContext asks loopd for the approved versions by number, checks them against their sha256,
// and says what it could not fetch; the run goes on.
func TestPlanContextFetchesTheApprovedVersions(t *testing.T) {
	good := "# Blueprint v1\n"
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch r.URL.RequestURI() {
		case "/plans/5":
			json.NewEncoder(w).Encode(map[string]any{"plan": pipeline.Plan{ID: 5, BriefApproved: 3, BlueprintApproved: 1}})
		case "/plans/5/docs/blueprint?v=1":
			json.NewEncoder(w).Encode(pipeline.PlanDoc{PlanID: 5, Kind: "blueprint", Version: 1, Markdown: good, SHA256: pipeline.DocSHA256(good)})
		case "/plans/5/docs/brief?v=3":
			json.NewEncoder(w).Encode(pipeline.PlanDoc{PlanID: 5, Kind: "brief", Version: 3, Markdown: "tampered", SHA256: pipeline.DocSHA256("other")})
		default:
			http.Error(w, `{"error":{"code":"internal","message":"boom"}}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	r := &Runner{Client: &Client{Base: srv.URL, Token: "t"}}
	ru := &run{wt: t.TempDir(), item: Item{ID: 9, PlanID: 5, PlanPiece: 2}}
	info := &RunInfo{}
	r.planContext(context.Background(), ru, info)
	if strings.Join(asked, " ") != "/plans/5 /plans/5/docs/blueprint?v=1 /plans/5/docs/brief?v=3" {
		t.Fatalf("asked %q", asked)
	}
	if b, _ := os.ReadFile(filepath.Join(ru.wt, ".loop", "context", "blueprint.md")); string(b) != good {
		t.Fatalf("blueprint.md %q", b)
	}
	if _, err := os.Stat(filepath.Join(ru.wt, ".loop", "context", "brief.md")); err == nil {
		t.Fatal("wrote a brief whose sha256 does not match")
	}
	if len(info.PlanDocs) != 1 || len(info.ContextErrs) != 1 || !strings.Contains(info.ContextErrs[0], "approved product brief (v3): its text does not match") {
		t.Fatalf("docs %q errs %q", info.PlanDocs, info.ContextErrs)
	}

	ru, info = &run{wt: t.TempDir(), item: Item{ID: 9, PlanID: 6}}, &RunInfo{}
	r.planContext(context.Background(), ru, info)
	if len(info.PlanDocs) != 0 || len(info.ContextErrs) != 1 || !strings.HasPrefix(info.ContextErrs[0], "plan P6 from loopd:") {
		t.Fatalf("docs %q errs %q", info.PlanDocs, info.ContextErrs)
	}
	if b := Brief(StageBuild, ru.item, *info); strings.Contains(b, "of plan P6") || !strings.Contains(b, "Not available this run: plan P6 from loopd") {
		t.Fatalf("brief without the plan's documents:\n%s", b[strings.Index(b, ItemHeading):])
	}
}
