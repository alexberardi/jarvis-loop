package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{name: "go toolchain", run: `printf 'go: no such tool "vet"\n'; exit 2`, evidence: "could not run: exit 2 after 0s"},
		{name: "pytest collection", run: "echo 'ImportError while importing test module tests/test_x.py'; echo '8 errors in 0.05s'; exit 2",
			evidence: "could not run: exit 2 after 0s"},
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

// A proof command runs only once the fetched main has the item's merge commit (#532: a stale view
// of main right after a merge ran the command against pre-merge code and reopened #504, #515).
// Until then the check is not run and not reported: it stays pending and the next pass retries.
func TestProofWaitsForTheMergeSHA(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	id, _ := g.toLand("Proved change")
	it := g.item(id).Item
	if c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}); err != nil || c == nil {
		t.Fatalf("land claim %v %v", c, err)
	}
	// The merge commit exists only in the user's clone: "GitHub" (the bare remote) has not shown
	// it on main yet.
	if err := os.WriteFile(filepath.Join(g.local, "merged.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, g.local, "add", "merged.txt")
	gitT(t, g.local, "commit", "-q", "-m", "the merge")
	merge := gitT(t, g.local, "rev-parse", "HEAD")
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Merged, map[string]string{"merge_sha": merge, "head_sha": it.HeadSHA, "needs_qa": "true"},
		[]string{"A merged.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.OrchProof(g.ctx, "orchestrator:loop", id, json.RawMessage(`[{"kind":"command","run":"test -f merged.txt"}]`), "test"); err != nil {
		t.Fatal(err)
	}
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	d := g.item(id)
	if ck := d.Item.Proof.Checks[0]; d.Item.Stage != pipeline.QA || ck.State != pipeline.CheckPending || ck.Fails != 0 || ck.Evidence != "" {
		t.Fatalf("before main has the merge: stage %s, check %+v; log:\n%s", d.Item.Stage, ck, g.logs.String())
	}
	if !strings.Contains(g.logs.String(), "does not have the merge commit") {
		t.Fatalf("no waiting log; log:\n%s", g.logs.String())
	}
	// Main catches up; the next pass runs the command in a checkout that has the change.
	gitT(t, g.local, "push", "-q", g.remote, "main")
	g.loopdClk.Advance(pipeline.ProofRecheck)
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if d := g.item(id); d.Item.Stage != pipeline.Done || len(g.events(id, "verified")) != 1 {
		t.Fatalf("after main has the merge: stage %s, proof %+v; log:\n%s", d.Item.Stage, d.Item.Proof, g.logs.String())
	}
}

// A python proof command gets a venv even when the repo's setup made none (#527/#532): the runner
// builds .venv in the checkout, installs the repo's requirements, and the command runs inside it.
func TestProofBuildsAVenvForAPythonCommand(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toQAWithProof(t, `[{"kind":"command","run":"test \"$(python3 -c 'import sys; print(sys.prefix)')\" = \"$PWD/.venv\" && test \"$VIRTUAL_ENV\" = \"$PWD/.venv\""}]`)
	// A python repo whose setup makes no venv: requirements.txt on main, no setup commands.
	if err := os.WriteFile(filepath.Join(g.local, "requirements.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, g.local, "add", "requirements.txt")
	gitT(t, g.local, "commit", "-q", "-m", "a python package")
	gitT(t, g.local, "push", "-q", g.remote, "main")
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if d := g.item(id); d.Item.Stage != pipeline.Done {
		t.Fatalf("stage %s, proof %+v; log:\n%s", d.Item.Stage, d.Item.Proof, g.logs.String())
	}
	if log := g.read(filepath.Join(g.r.Cfg.WorkRoot, "proofs", fmt.Sprintf("%d-0", id), "setup.log")); !strings.Contains(log, "python3 -m venv .venv") ||
		!strings.Contains(log, "pip install -q -r requirements.txt") {
		t.Fatalf("setup.log:\n%s", log)
	}
}

// What never got to test the change (#282, #532): a missing tool, go's [setup failed], a broken
// toolchain, pytest import or collection errors, or an errors-only pytest summary in under a
// second. A [build failed], a plain failure or a slow errors-only run still strikes: the merged
// change may have caused those (docs/decisions/2026-10-19-proof-setup.md).
func TestEnvFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		out  string
		ran  time.Duration
		want bool
	}{
		{"missing tool", 127, "sh: no-such-tool: not found", 0, true},
		{"go setup failed", 1, "FAIL\tgithub.com/x/cc [setup failed]", 100 * time.Millisecond, true},
		{"go toolchain", 2, `go: no such tool "vet"`, 100 * time.Millisecond, true},
		{"no GOROOT", 2, "go: cannot find GOROOT directory: /nope/libexec", 0, true},
		{"mise toolchain", 1, "mise ERROR failed to install core:go@1.27", 400 * time.Millisecond, true},
		{"pytest import error", 1, "ImportError while importing test module '/w/tests/test_x.py'\n8 errors in 0.05s", 50 * time.Millisecond, true},
		{"pytest collection interrupt", 2, "!!! Interrupted: 3 errors during collection !!!", 300 * time.Millisecond, true},
		{"pytest one collection error", 2, "!!! Interrupted: 1 error during collection !!!", 300 * time.Millisecond, true},
		{"pytest errors-only summary, fast", 1, "========== 8 errors in 0.05s ==========", 50 * time.Millisecond, true},
		{"pytest errors-only summary, slow", 1, "========== 3 errors in 12.41s ==========", 12 * time.Second, false},
		{"pytest real failures with errors", 1, "===== 2 failed, 1 error in 0.30s =====", 300 * time.Millisecond, false},
		{"go build failed", 1, "FAIL\tgithub.com/x/y [build failed]", 200 * time.Millisecond, false},
		{"plain fast failure", 1, "no nope in README.md", 0, false},
		{"real test failure", 1, "--- FAIL: TestX\nFAIL", 90 * time.Second, false},
		{"passed", 0, "ok  \tgithub.com/x/y\t3.1s", 3 * time.Second, false},
	} {
		if got := envFailure(c.code, c.out, c.ran); got != c.want {
			t.Errorf("%s: envFailure(%d, %q, %s) = %v, want %v", c.name, c.code, c.out, c.ran, got, c.want)
		}
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
