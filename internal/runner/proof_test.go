package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// toQAWithProof takes a new item through land to qa with a proof of the given checks.
func (g *rig) toQAWithProof(t *testing.T, checks string) int64 {
	t.Helper()
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
	if _, err := g.st.OrchProof(g.ctx, "orchestrator:loop", id, json.RawMessage(checks), "test"); err != nil {
		t.Fatal(err)
	}
	return id
}

// The repo's Prepare and Setup commands run in the proof's checkout before its command, as they
// do before a build or a regression run (#282: jarvis-server's go:embed of the fetched libs).
func TestProofRunsTheRepoSetup(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toQAWithProof(t, `[{"kind":"command","run":"test -f libs/ok"}]`)
	rc := g.r.Cfg.Repos["demo"]
	rc.Prepare = []string{"mkdir -p libs"}
	rc.Setup = []string{"touch libs/ok && echo fetched"}
	g.r.Cfg.Repos["demo"] = rc
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if d := g.item(id); d.Item.Stage != pipeline.Done || len(g.events(id, "verified")) != 1 {
		t.Fatalf("stage %s, proof %+v; log:\n%s", d.Item.Stage, d.Item.Proof, g.logs.String())
	}
	if log := g.read(filepath.Join(g.r.Cfg.WorkRoot, "proofs", fmt.Sprintf("%d-0", id), "setup.log")); !strings.Contains(log, "fetched") {
		t.Fatalf("setup.log:\n%s", log)
	}
}

// A check that could not run for its environment's sake (the setup failed; the command's output
// shows go's [setup failed] or a missing tool) is reported as an error: recorded on the item, warned
// in the runner's log, and never counted toward the reopen, however often it happens.
func TestProofEnvironmentErrorDoesNotReopen(t *testing.T) {
	for _, c := range []struct {
		name, setup, run, evidence string
	}{
		{name: "setup fails", setup: "echo no network; exit 1", run: "true", evidence: "could not run: setup \"echo no network; exit 1\""},
		{name: "go setup failed", run: `printf 'FAIL\tgithub.com/x/cc [setup failed]\n'; exit 1`, evidence: "could not run: exit 1 after 0s: FAIL\tgithub.com/x/cc [setup failed]"},
		{name: "missing tool", run: "no-such-tool-282 --version", evidence: "could not run: exit 127 after 0s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			checks, _ := json.Marshal([]map[string]string{{"kind": "command", "run": c.run}})
			id := g.toQAWithProof(t, string(checks))
			if c.setup != "" {
				rc := g.r.Cfg.Repos["demo"]
				rc.Setup = []string{c.setup}
				g.r.Cfg.Repos["demo"] = rc
			}
			for i := 0; i < pipeline.ProofFailsToReopen+1; i++ {
				if err := g.r.CheckProofs(g.ctx); err != nil {
					t.Fatal(err)
				}
				g.loopdClk.Advance(pipeline.ProofRecheck)
			}
			d := g.item(id)
			if d.Item.Stage != pipeline.QA || len(g.events(id, "proof_checked")) != pipeline.ProofFailsToReopen+1 {
				t.Fatalf("stage %s; log:\n%s", d.Item.Stage, g.logs.String())
			}
			ck := d.Item.Proof.Checks[0]
			if ck.State != pipeline.CheckPending || ck.Fails != 0 || !strings.HasPrefix(ck.Evidence, c.evidence) {
				t.Fatalf("check %+v, want evidence %q", ck, c.evidence)
			}
			if !strings.Contains(g.logs.String(), `level=WARN msg="proof: command check could not run"`) {
				t.Fatalf("no warning; log:\n%s", g.logs.String())
			}
		})
	}
}

// The repo's setup venv is the proof command's python, and $TMPDIR is writable (#461): a check
// running pytest found the system python, not the .venv the setup built.
func TestProofCommandUsesTheSetupVenv(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toQAWithProof(t, `[{"kind":"command","run":"test \"$(python3 -c 'import sys; print(sys.prefix)')\" = \"$PWD/.venv\" && test \"$VIRTUAL_ENV\" = \"$PWD/.venv\""},`+
		`{"kind":"command","run":"echo hi > \"$TMPDIR/x\" && test -s \"$TMPDIR/x\""}]`)
	rc := g.r.Cfg.Repos["demo"]
	rc.Setup = []string{"python3 -m venv --without-pip .venv"}
	g.r.Cfg.Repos["demo"] = rc
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if d := g.item(id); d.Item.Stage != pipeline.Done {
		t.Fatalf("stage %s, proof %+v; log:\n%s", d.Item.Stage, d.Item.Proof, g.logs.String())
	}
}

func TestVenvEnv(t *testing.T) {
	dir := t.TempDir()
	env := []string{"HOME=/h", "PATH=/a:/b", "PATH=/usr/bin"}
	if got := venvEnv(env, dir); len(got) != len(env) {
		t.Fatalf("no .venv: got %v", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := venvEnv(env, dir)
	want := append(env, "VIRTUAL_ENV="+filepath.Join(dir, ".venv"), "PATH="+filepath.Join(dir, ".venv", "bin")+":/usr/bin")
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
