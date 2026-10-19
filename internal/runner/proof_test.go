package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md): the runner runs a due proof's command
// checks fenced in a checkout of main and reports them; a check that passed is not run again, and
// one failing twice in a row reopens the item.
func TestCheckProofs(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	id, _ := g.toLand("Proved change")
	it := g.item(id).Item
	if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}); err != nil || c == nil {
		t.Fatalf("land claim %v %v", c, err)
	}
	main := gitT(t, g.local, "rev-parse", "HEAD")
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Merged, map[string]string{"merge_sha": main, "head_sha": it.HeadSHA, "needs_qa": "true"},
		[]string{"A x.txt"}); err != nil {
		t.Fatal(err)
	}
	checks := fmt.Sprintf(`[{"kind":"command","run":"grep -q demo README.md && test \"$LOOP_ITEM\" = %d"},
		{"kind":"command","run":"echo looking; grep -q nope README.md || { echo 'no nope in README.md'; exit 1; }"},
		{"kind":"usage","event":"rebuild"}]`, id)
	if _, err := g.st.OrchProof(g.ctx, "orchestrator:loop", id, json.RawMessage(checks), "test"); err != nil {
		t.Fatal(err)
	}
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	evs := g.events(id, "proof_checked")
	if len(evs) != 1 || evs[0].Actor != "runner:devbox" || evs[0].Data["status"] != pipeline.CheckFailed {
		t.Fatalf("proof_checked: %+v; log:\n%s", evs, g.logs.String())
	}
	p := g.item(id).Item.Proof
	if p.Checks[0].State != pipeline.CheckPassed || !strings.HasPrefix(p.Checks[0].Evidence, "exit 0 after") {
		t.Fatalf("check 1: %+v; log:\n%s", p.Checks[0], g.logs.String())
	}
	if c := p.Checks[1]; c.State != pipeline.CheckFailed || c.Fails != 1 || !strings.HasPrefix(c.Evidence, "exit 1 after 0s: no nope in README.md\nlooking") {
		t.Fatalf("check 2: %+v", c)
	}
	if p.Checks[2].State != pipeline.CheckPending {
		t.Fatalf("usage: %+v", p.Checks[2])
	}
	// Not due again yet: nothing is run or reported.
	if err := g.r.CheckProofs(g.ctx); err != nil || len(g.events(id, "proof_checked")) != 1 {
		t.Fatalf("checked again before it was due: %v", err)
	}
	g.loopdClk.Advance(pipeline.ProofRecheck)
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Done || len(g.events(id, "reopened")) != 1 {
		t.Fatalf("after the second failure: %s; log:\n%s", d.Item.Stage, g.logs.String())
	}
	re := g.events(id, "reopened")[0]
	if re.Actor != pipeline.ActorProof || !strings.Contains(re.Data["words"].(string), "no nope in README.md") {
		t.Fatalf("reopened: %+v", re)
	}
	if strings.Count(g.logs.String(), "proof command ended") != 3 {
		t.Fatalf("a passed check runs once, the failing one twice; log:\n%s", g.logs.String())
	}
}
