package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// verifyRepo gives the demo repo verify steps.
func (g *rig) verifyRepo(steps ...VerifyStep) {
	g.t.Helper()
	rc := g.r.Cfg.Repos["demo"]
	rc.Verify = steps
	g.r.Cfg.Repos["demo"] = rc
}

// A passing verify: the runner runs it in the worktree after the agent, with the LOOP_*
// environment; what it writes to LOOP_EVIDENCE_DIR and its log are attached with the runner's
// label; the PR's footer lists it apart from the agent's evidence.
func TestVerifyPasses(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(
		VerifyStep{Name: "unit tests", Run: `echo "tests pass at $LOOP_HEAD"; pwd > "$LOOP_EVIDENCE_DIR/cwd.txt"; ` +
			`env | grep '^LOOP_' | sort > "$LOOP_EVIDENCE_DIR/env.txt"; printf '\x89PNG\r\n' > "$LOOP_EVIDENCE_DIR/home-light.png"; mkdir "$LOOP_EVIDENCE_DIR/shots"; test -f feature.txt`},
		VerifyStep{Name: "admin screenshots", Run: "touch should-not-run", WhenChanged: []string{"web/**"}},
	)
	id := g.toBuild("Add a feature")
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.PR != 7 {
		t.Fatalf("stage %s pr %d (question %q); log:\n%s", d.Item.Stage, d.Item.PR, d.Item.Question, g.logs.String())
	}
	labels := map[string]string{}
	for _, e := range d.Evidence {
		labels[e.Name] = e.Label
	}
	for name, label := range map[string]string{
		"verify-1-unit-tests.log":                 `runner verify "unit tests": log, passed (0s)`,
		"verify-1-unit-tests-home-light.png":      `runner verify "unit tests": verify-1-unit-tests-home-light.png`,
		"verify-1-unit-tests-cwd.txt":             `runner verify "unit tests": verify-1-unit-tests-cwd.txt`,
		"after.txt":                               "tests",
		"verify-2-admin-screenshots.log":          "",
		"verify-2-admin-screenshots-anything.txt": "",
	} {
		if got, ok := labels[name]; label == "" && ok || label != "" && got != label {
			t.Errorf("evidence %s: label %q (present %v), want %q; all: %v", name, got, ok, label, labels)
		}
	}
	if logs := g.logs.String(); !strings.Contains(logs, "verify evidence: skipped directories and dotfiles") || !strings.Contains(logs, "skipped=shots") {
		t.Errorf("a skipped subfolder is not logged:\n%s", logs)
	}
	run := g.lastRun(id, StageBuild)
	ev := filepath.Join(run, "verify", "verify-1-unit-tests", "evidence")
	head := g.remoteHead(ItemBranchName(id, "Add a feature"))
	env := g.read(filepath.Join(ev, "env.txt"))
	for _, want := range []string{"LOOP_ITEM=1\n", "LOOP_HEAD=" + head + "\n", "LOOP_WORKTREE=" + filepath.Join(g.r.Cfg.WorkRoot, "1-build") + "\n",
		"LOOP_EVIDENCE_DIR=" + ev + "\n", "LOOP_SANDBOX=loop-1-verify\n", "LOOP_REPO=alex/demo\n", "LOOP_BASE=main\n",
		"LOOP_MIRROR=" + g.r.mirrorPath("demo") + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("verify env lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "LOOP_TOKEN") {
		t.Error("the verify step got loopd's token")
	}
	if cwd := strings.TrimSpace(g.read(filepath.Join(ev, "cwd.txt"))); cwd != filepath.Join(g.r.Cfg.WorkRoot, "1-build") {
		t.Errorf("cwd %q", cwd)
	}
	if !strings.Contains(g.read(filepath.Join(run, "verify", "verify-1-unit-tests", "verify-1-unit-tests.log")), "tests pass at "+head) {
		t.Error("the log tail lacks the step's output")
	}
	body := g.prs.bodies[7]
	for _, want := range []string{"**Runner verify**", "- unit tests: passed (0s); `verify-1-unit-tests.log`, `verify-1-unit-tests-cwd.txt`",
		"- admin screenshots: skipped (no changed path matches web/**)", "Evidence files (on loop item #1):\n\n- `after.txt`: tests"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, body)
		}
	}
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, "1-build", "should-not-run")); err == nil {
		t.Error("a when_changed step ran without a matching path")
	}
	// The build brief named the steps, the when_changed one with what makes it run (#546); the
	// agent did not run them.
	if brief := g.read(filepath.Join(run, "brief.md")); !strings.Contains(brief, "the runner runs this repo's verify steps itself (outside your sandbox), before it pushes: unit tests; admin screenshots (only when a path you change matches web/**);") {
		t.Errorf("brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}
}

// A failing verify: nothing is pushed and no PR is opened; the item goes back to build counting
// like a bounce, with the log as evidence; the next build gets .loop/review/verify.md, and a
// second failure goes to the user.
func TestVerifyFailsBackToBuild(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "unit tests", Run: `echo "FAIL: TestHome (the screen is blank)"; exit 3`})
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 1 || d.Item.PR != 0 {
		t.Fatalf("stage %s bounces %d pr %d (question %q); log:\n%s", d.Item.Stage, d.Item.BounceCount, d.Item.PR, d.Item.Question, g.logs.String())
	}
	branch := ItemBranchName(id, "Add a feature")
	if g.remoteHead(branch) != "" || g.prs.calls() != 0 {
		t.Fatalf("pushed %q / PR calls %d after a failed verify", g.remoteHead(branch), g.prs.calls())
	}
	ev := g.advanced(id)
	reason, _ := ev.Data["reason"].(string)
	logTail, _ := ev.Data["log_tail"].(string)
	if ev.Outcome != "verify_failed" || reason != `Runner verify "unit tests" failed (exit 3, 0s); nothing was pushed` ||
		!strings.Contains(logTail, "FAIL: TestHome (the screen is blank)") || ev.Data["verify_step"] != "unit tests" {
		t.Fatalf("event %s %+v", ev.Outcome, ev.Data)
	}
	var names []string
	for _, e := range d.Evidence {
		names = append(names, e.Name)
	}
	if !slices.Contains(names, "verify-1-unit-tests.log") || slices.Contains(names, "after.txt") {
		t.Fatalf("evidence %v", names)
	}

	// The next build continues the branch (from the mirror: it was never pushed) and reads why.
	g.runOnce()
	run := g.lastRun(id, StageBuild)
	brief := g.read(filepath.Join(run, "brief.md"))
	if !strings.Contains(brief, "- Sent back 1 time(s) by the runner's verify (nothing was pushed). Fix what `.loop/review/verify.md` shows") ||
		!strings.Contains(brief, "`.loop/review/verify.md` (the runner's verify output, newest last)") ||
		!strings.Contains(brief, "this branch already has 1 commit from a previous run") {
		t.Fatalf("second brief:\n%s", brief[strings.Index(brief, ItemHeading):])
	}
	d = g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.ReturnStage != pipeline.Build || !strings.Contains(d.Item.Question, `Runner verify "unit tests" failed`) ||
		strings.Contains(d.Item.Question, "FAIL: TestHome") {
		t.Fatalf("after a second failure: %s (question %q)", d.Item.Stage, d.Item.Question)
	}
	if g.remoteHead(branch) != "" || g.prs.calls() != 0 {
		t.Fatal("pushed after the second failed verify")
	}
}

// A verify whose FAIL lines come early (go test streams per package: the failing one finishes
// first, later packages' ok lines fill the end): the uploaded log evidence keeps the lines around
// the failure, and the requeue event's log_tail starts at it, so the next build sees what failed
// instead of a tail of ok lines (#531).
func TestVerifyFailKeepsFailWindow(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "race tests", Run: `echo "--- FAIL: TestSIPBind (0.01s)"
echo "    sip_test.go:42: bind 127.0.0.66: operation not permitted"
i=1; while [ $i -le 120 ]; do printf 'ok  \tgithub.com/alexberardi/jarvis-server/internal/modules/longish/package/path/number%03d\t(cached) with some extra -race flags making the line long\n' $i; i=$((i+1)); done
exit 1`})
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 1 {
		t.Fatalf("stage %s bounces %d; log:\n%s", d.Item.Stage, d.Item.BounceCount, g.logs.String())
	}
	n := 0
	for _, e := range d.Evidence {
		if e.Name == "verify-1-race-tests.log" {
			n = e.N
		}
	}
	if n == 0 {
		t.Fatalf("no verify log in the evidence: %+v", d.Evidence)
	}
	_, f, err := g.st.OpenEvidence(g.ctx, id, n)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "--- FAIL: TestSIPBind") || !strings.Contains(log, "sip_test.go:42: bind 127.0.0.66") {
		t.Fatalf("the verify log evidence lost the FAIL lines:\n%s", clip(log, 600))
	}
	if !strings.Contains(log, "number120\t") {
		t.Errorf("the verify log evidence lost the tail:\n…%s", log[max(0, len(log)-300):])
	}
	ev := g.advanced(id)
	logTail, _ := ev.Data["log_tail"].(string)
	if !strings.HasPrefix(logTail, "--- FAIL: TestSIPBind") || !strings.Contains(logTail, "sip_test.go:42: bind 127.0.0.66") {
		t.Fatalf("log_tail does not start at the failure:\n%s", clip(logTail, 600))
	}
}

// What verify.md holds: each failure's step, result, commit and output.
func TestVerifyNotesForTheNextBuild(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "lint", Run: `echo "src/x.ts:3 unused import"; exit 1`})
	id := g.toBuild("Add a feature")
	g.runOnce()
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.prepare(g.ctx, ru); err != nil {
		t.Fatal(err)
	}
	defer g.r.cleanup(g.ctx, ru, g.r.Log)
	v := g.read(filepath.Join(ru.wt, ".loop", "review", "verify.md"))
	for _, want := range []string{"# The runner's verify (failed; nothing was pushed)", "## Failure 1 (", `Runner verify "lint" failed (exit 1, 0s)`,
		"src/x.ts:3 unused import", "`verify-<n>-…` files in `.loop/triage/`"} {
		if !strings.Contains(v, want) {
			t.Errorf("verify.md lacks %q:\n%s", want, v)
		}
	}
}

// A step can run in a fresh checkout of another configured repo at its base (jarvis-server's
// mobile-shot for jarvis-node-mobile); the checkout is removed afterwards.
func TestVerifyInAnotherRepo(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "shots", Repo: "other", Run: `pwd > "$LOOP_EVIDENCE_DIR/cwd.txt"; test -f README.md && test ! -e feature.txt`})
	id := g.toBuild("Add a feature")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	cwd := strings.TrimSpace(g.read(filepath.Join(g.lastRun(id, StageBuild), "verify", "verify-1-shots", "evidence", "cwd.txt")))
	if cwd != filepath.Join(g.r.Cfg.WorkRoot, "1-verify-other") {
		t.Fatalf("cwd %q", cwd)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatal("the other repo's checkout was left behind")
	}
	if out := gitT(t, g.r.mirrorPath("other"), "worktree", "list"); strings.Contains(out, "verify") {
		t.Fatalf("still registered:\n%s", out)
	}
}

// A change to a file the runner trusts (deny_write) is not verified with the agent's version of
// it (the guard refuses the agent's edits there, but a shell command could still change one):
// nothing runs, and the build reports a failure for the user.
func TestVerifyRefusesAChangedTrustedFile(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	marker := filepath.Join(t.TempDir(), "ran")
	g.verifyRepo(VerifyStep{Name: "harness", Run: "touch " + marker})
	rc := g.r.Cfg.Repos["demo"]
	rc.DenyWrite = []string{"scripts"}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Add a feature")
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.r.runVerify(g.ctx, ru, strings.Repeat("a", 40), []string{"M feature.txt", "M scripts/harness.sh"})
	if err == nil || !strings.Contains(err.Error(), "the change touches scripts/harness.sh") {
		t.Fatalf("runVerify: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("verify ran")
	}
	for _, c := range []struct {
		deny  []string
		lines []string
		want  string
	}{
		{[]string{"scripts/sandbox"}, []string{"M scripts/sandbox"}, "scripts/sandbox"},
		{[]string{"scripts/"}, []string{"A scripts/new.sh"}, "scripts/new.sh"},
		{[]string{"scripts/*.sh"}, []string{"R scripts/a.sh tools/a.sh"}, "scripts/a.sh"},
		{[]string{"scripts/sandbox"}, []string{"M scripts/sandbox-helper"}, ""},
		{nil, []string{"M x"}, ""},
	} {
		if got := touchesDenyWrite(c.deny, c.lines); got != c.want {
			t.Errorf("touchesDenyWrite(%v, %v) = %q, want %q", c.deny, c.lines, got, c.want)
		}
	}
}

// The approver's trust OK lets verify run that one commit's change to trusted files, once: another
// commit, an OK not given yet, or one already used is refused as before.
func TestVerifyRunsATrustedFileChangeOnTheOK(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "harness", Run: "touch ran"})
	rc := g.r.Cfg.Repos["demo"]
	rc.DenyWrite = []string{"scripts"}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Change the harness")
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(ru.wt, "ran") // the fence's /tmp is private; the step runs in the worktree
	if err := os.MkdirAll(ru.wt, 0o755); err != nil {
		t.Fatal(err)
	}
	trusted, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	changed := []string{"M feature.txt", "R scripts/old.sh scripts/harness.sh"}
	for name, tr := range map[string]*ItemTrust{
		"no OK":        nil,
		"not given":    {HeadSHA: trusted},
		"used":         {HeadSHA: trusted, By: "user:alex", Used: true},
		"other commit": {HeadSHA: other, By: "user:alex"},
	} {
		ru.item.Trust = tr
		_, err := g.r.runVerify(g.ctx, ru, trusted, changed)
		var refusal *trustRefusal
		if !errors.As(err, &refusal) || refusal.head != trusted || strings.Join(refusal.paths, ",") != "scripts/harness.sh,scripts/old.sh" {
			t.Fatalf("%s: runVerify: %v (%+v)", name, err, refusal)
		}
		if _, err := os.Stat(marker); err == nil || ru.trustUsed != "" {
			t.Fatalf("%s: verify ran", name)
		}
	}
	ru.item.Trust = &ItemTrust{HeadSHA: trusted, By: "user:alex"}
	res, err := g.r.runVerify(g.ctx, ru, trusted, changed)
	if err != nil || len(res) != 1 || !res[0].Passed() {
		t.Fatalf("trusted runVerify: %+v %v", res, err)
	}
	if _, err := os.Stat(marker); err != nil || ru.trustUsed != trusted {
		t.Fatalf("verify did not run the trusted commit (used %q)", ru.trustUsed)
	}
}

// A change to generated deny_write files alone is trusted once their generator, run at the head,
// reproduces them byte for byte: verify then runs without the user's OK. Bytes the generator
// does not reproduce, or a change to the generator too, go to the user as before, and the
// worktree is left as the head has it (#593).
func TestVerifyTrustsRegeneratedFiles(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "harness", Run: "touch ran"})
	rc := g.r.Cfg.Repos["demo"]
	rc.DenyWrite = []string{"gen"}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Add a chat tool")
	cl, err := g.r.Client.Claim(g.ctx, "a", nil, []string{StageBuild})
	if err != nil || cl == nil || cl.Item.ID != id {
		t.Fatalf("claim %+v %v", cl, err)
	}
	ru, err := g.r.newRun(cl, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ru.wt, "gen", "sets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, text := range map[string]string{"gen/sets/desktop.json": "{\"tools\": 2}\n", "gen/main.go": "package main\n"} {
		if err := os.WriteFile(filepath.Join(ru.wt, p), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, ru.wt, "init", "-q")
	gitT(t, ru.wt, "add", ".")
	gitT(t, ru.wt, "commit", "-q", "-m", "head")
	head := gitT(t, ru.wt, "rev-parse", "HEAD")
	marker := filepath.Join(ru.wt, "ran")
	toolset := []string{"M gen/sets/desktop.json"}
	for name, c := range map[string]struct {
		run     string
		changed []string
	}{
		"other bytes":       {`printf '{"tools": 3}\n' > gen/sets/desktop.json`, toolset},
		"a new file":        {`printf '{"tools": 2}\n' > gen/sets/desktop.json; echo x > gen/sets/phone.json`, toolset},
		"generator fails":   {"exit 1", toolset},
		"generator changed": {`printf '{"tools": 2}\n' > gen/sets/desktop.json`, append([]string{"M gen/main.go"}, toolset...)},
	} {
		ru.repo.Generated = &GeneratedFiles{Paths: []string{"gen/sets/*.json"}, Run: c.run}
		_, err := g.r.runVerify(g.ctx, ru, head, c.changed)
		var refusal *trustRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("%s: runVerify: %v", name, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: verify ran", name)
		}
		if st := gitT(t, ru.wt, "status", "--porcelain"); st != "" {
			t.Fatalf("%s: worktree left changed:\n%s", name, st)
		}
	}
	ru.repo.Generated = &GeneratedFiles{Paths: []string{"gen/sets/*.json"}, Run: `printf '{"tools": 2}\n' > gen/sets/desktop.json`}
	res, err := g.r.runVerify(g.ctx, ru, head, append([]string{"M feature.txt"}, toolset...))
	if err != nil || len(res) != 1 || !res[0].Passed() {
		t.Fatalf("regenerated runVerify: %+v %v", res, err)
	}
	if _, err := os.Stat(marker); err != nil || ru.trustUsed != "" {
		t.Fatalf("verify did not run, or used an OK (%q)", ru.trustUsed)
	}
}

// One verify at a time on the machine: a run waits for the lock (heartbeating), then verifies.
func TestVerifyWaitsForTheMachineLock(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.verifyRepo(VerifyStep{Name: "unit tests", Run: "true"})
	held, err := lockFile(g.r.Cfg.VerifyLock)
	if err != nil {
		t.Fatal(err)
	}
	id := g.toBuild("Add a feature")
	wait := g.runAsync()
	for i := 0; i < 600 && !strings.Contains(g.logs.String(), "waiting for the verify lock"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(g.logs.String(), "waiting for the verify lock") {
		t.Fatalf("the run did not wait for the lock; log:\n%s", g.logs.String())
	}
	if d := g.item(id); d.Item.Stage != pipeline.Build || g.prs.calls() != 0 {
		t.Fatalf("went on while the lock was held: %s", d.Item.Stage)
	}
	held.Close()
	wait()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("after the lock was freed: %s (question %q)", d.Item.Stage, d.Item.Question)
	}
}

// Setup commands run in the checkout before the agent, outside its sandbox, logged in the run
// dir; what they install is excluded from git (never committed, never dirty). A failing one
// fails the run with its output.
func TestSetupRunsBeforeTheAgent(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	rc := g.r.Cfg.Repos["demo"]
	rc.Setup = []string{"mkdir -p node_modules/x .venv && echo installed > node_modules/x/index.js && echo setup done"}
	g.r.Cfg.Repos["demo"] = rc
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if log := g.read(filepath.Join(g.lastRun(id, StageBuild), "setup.log")); !strings.Contains(log, "setup done") || !strings.HasPrefix(log, "$ mkdir") {
		t.Fatalf("setup.log:\n%s", log)
	}
	if files := gitT(t, g.remote, "show", "--name-only", "--format=", ItemBranchName(id, "Add a feature")); strings.Contains(files, "node_modules") {
		t.Fatalf("installed files were committed: %s", files)
	}

	g2 := newRig(t, fakeclaude.Good)
	rc = g2.r.Cfg.Repos["demo"]
	rc.Setup = []string{"echo npm ERR! missing lockfile; exit 1"}
	g2.r.Cfg.Repos["demo"] = rc
	id = g2.toBuild("Add a feature")
	g2.runOnce()
	if d := g2.item(id); d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "npm ERR! missing lockfile") {
		t.Fatalf("%s %q", d.Item.Stage, d.Item.Question)
	}
}

// ItemBranchName is the branch the runner uses for an item (as newRun names it).
func ItemBranchName(id int64, title string) string {
	return fmt.Sprintf("loop/%d-%s", id, Slug(title))
}

// A step that fails reading a file the fence masks (verify_mask) is the runner's problem, not the
// change's: the item goes to the user without a bounce, and the message names the path, never its
// contents.
func TestVerifyMaskedFileIsNotTheChangesFault(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	secret := filepath.Join(t.TempDir(), "prod-llm.env")
	if err := os.WriteFile(secret, []byte("PROD_APP_ID=SECRET-ID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.VerifyMask = append(g.r.Cfg.VerifyMask, secret)
	g.verifyRepo(VerifyStep{Name: "pipeline scenarios", Run: `set -eu; . ` + secret + `; echo "app $PROD_APP_ID"`})
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.BounceCount != 0 || d.Item.PR != 0 {
		t.Fatalf("stage %s bounces %d pr %d (question %q); log:\n%s", d.Item.Stage, d.Item.BounceCount, d.Item.PR, d.Item.Question, g.logs.String())
	}
	ev := g.advanced(id)
	msg, _ := ev.Data["error"].(string)
	want := `the runner's verify "pipeline scenarios" could not read ` + secret + `: verify_mask hides it from fenced steps, so this is the runner's config, not the change; nothing was pushed`
	if ev.Outcome != "failed" || !strings.HasPrefix(msg, want) || strings.Contains(msg, "SECRET-ID") {
		t.Fatalf("event %s %+v", ev.Outcome, ev.Data)
	}
	var names []string
	for _, e := range d.Evidence {
		names = append(names, e.Name)
	}
	if !slices.Contains(names, "verify-1-pipeline-scenarios.log") {
		t.Fatalf("evidence %v", names)
	}
	if g.remoteHead(ItemBranchName(id, "Add a feature")) != "" || g.prs.calls() != 0 {
		t.Fatal("pushed after a masked verify")
	}
}

func TestMaskedRead(t *testing.T) {
	mask := []string{"/h/.config/jarvis-harness/prod-llm.env", "/h/.ssh"}
	for _, c := range []struct{ log, want string }{
		{"/h/.config/jarvis-harness/prod-background.env: line 3: /h/.config/jarvis-harness/prod-llm.env: Permission denied\nPROD_APP_ID: unbound variable\n",
			"/h/.config/jarvis-harness/prod-llm.env"},
		{"cat: /h/.ssh/id_ed25519: No such file or directory\n", "/h/.ssh"},
		{"open /h/.ssh/config: permission denied\n", "/h/.ssh"},
		// Not masked: the change's own failure.
		{"/repo/scripts/run: Permission denied\n", ""},
		{"/h/.config/jarvis-harness/prod-llm.env.bak: Permission denied\n", ""},
		{"/h/.sshx/key: Permission denied\n", ""},
		{"FAIL: TestHome\n", ""},
	} {
		if got := maskedRead(c.log, mask); got != c.want {
			t.Errorf("maskedRead(%q) = %q, want %q", c.log, got, c.want)
		}
	}
}

// verifyHarness gives the demo repo the test harness (harnessRepo) with a down that appends
// "down <LOOP_SANDBOX>" to the returned file.
func (g *rig) verifyHarness() (calls string) {
	g.t.Helper()
	g.harnessRepo("")
	dir := g.t.TempDir()
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, dir)
	calls = filepath.Join(dir, "calls")
	rc := g.r.Cfg.Repos["demo"]
	rc.Harness.Down = `echo "down $LOOP_SANDBOX" >> ` + calls
	g.r.Cfg.Repos["demo"] = rc
	return calls
}

// With a harness, verify runs under its own sandbox name (not the build agent's), and before each
// step the runner purges both names with the harness's down and waits for it (#327).
func TestVerifyPurgesTheSandboxesFirst(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	log := g.verifyHarness()
	g.verifyRepo(VerifyStep{Name: "pipeline", Run: `echo "step $LOOP_SANDBOX" >> ` + log})
	id := g.toBuild("Add a feature")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if got, want := g.read(log), "down loop-1\ndown loop-1-verify\nstep loop-1-verify\n"; got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}

// A repo-scoped harness (harness.repo) keeps its scripts in another repo, so the purge has to run
// where they are: a step in that same repo has a checkout of them and purges in it, and a step
// that is not in it has none, so the purge is skipped - and an environment failure is not retried
// with nothing purged - instead of running a down where the scripts cannot be (#546).
func TestVerifyPurgeWithARepoScopedHarness(t *testing.T) {
	// repoScoped points demo's harness at the other repo. The log is absolute and in
	// VerifyWritable, so down can append to it from any checkout; its cwd goes in the line.
	repoScoped := func(g *rig) string {
		log := g.verifyHarness()
		rc := g.r.Cfg.Repos["demo"]
		rc.Harness.Repo = "other"
		rc.Harness.Down = `echo "down $LOOP_SANDBOX in $(basename "$(pwd)")" >> ` + log
		g.r.Cfg.Repos["demo"] = rc
		return log
	}
	t.Run("a step in the harness's repo purges in its own checkout", func(t *testing.T) {
		g := newRig(t, fakeclaude.Good)
		log := repoScoped(g)
		g.verifyRepo(VerifyStep{Name: "flows", Repo: "other", Run: `echo "step $LOOP_SANDBOX" >> ` + log})
		id := g.toBuild("Add a feature")
		g.runOnce()
		if d := g.item(id); d.Item.Stage != pipeline.Review {
			t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
		}
		want := "down loop-1 in 1-verify-other\ndown loop-1-verify in 1-verify-other\nstep loop-1-verify\n"
		if got := g.read(log); got != want {
			t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("a step outside it does not purge and is not retried", func(t *testing.T) {
		g := newRig(t, fakeclaude.Good)
		log := repoScoped(g)
		g.verifyRepo(VerifyStep{Name: "flows", Run: `echo step >> ` + log + `; exit 3`})
		id := g.toBuild("Add a feature")
		g.runOnce()
		if d := g.item(id); d.Item.Stage != pipeline.Build {
			t.Fatalf("stage %s; log:\n%s", d.Item.Stage, g.logs.String())
		}
		// One try and no down: the scripts are in other, which this step has no checkout of.
		if got := g.read(log); got != "step\n" {
			t.Fatalf("calls:\n%s\nwant one try and no down", got)
		}
		if !strings.Contains(g.logs.String(), "the harness down cannot purge here") {
			t.Error("the skipped purge is not logged")
		}
	})
}

// A step that fails on its environment (exit 3: the harness did not come up; or envFailure) is
// purged and run once more; only a second failure sends the build back. A real failure is not
// retried.
func TestVerifyRetriesAnEnvironmentFailureOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		run    string
		stage  pipeline.Stage
		calls  string
		status string
	}{
		"exit 3 then pass": {`n=$(cat "$C" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$C"; echo "try $n" >> "$L"; [ $n -ge 2 ] || exit 3`,
			pipeline.Review, "down loop-1\ndown loop-1-verify\ntry 1\ndown loop-1\ndown loop-1-verify\ntry 2\n", "passed (0s) on the second try (the first failed (exit 3, 0s))"},
		"exit 3 twice": {`echo try >> "$L"; echo "400 node_id already exists"; exit 3`,
			pipeline.Build, "down loop-1\ndown loop-1-verify\ntry\ndown loop-1\ndown loop-1-verify\ntry\n", ""},
		"setup failed twice": {`echo try >> "$L"; echo "FAIL x [setup failed]"; exit 1`,
			pipeline.Build, "down loop-1\ndown loop-1-verify\ntry\ndown loop-1\ndown loop-1-verify\ntry\n", ""},
		"real failure": {`echo try >> "$L"; echo "FAIL: TestHome"; exit 1`,
			pipeline.Build, "down loop-1\ndown loop-1-verify\ntry\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			l := g.verifyHarness()
			c := filepath.Join(filepath.Dir(l), "count")
			g.verifyRepo(VerifyStep{Name: "pipeline", Run: "L=" + l + " C=" + c + "; " + tc.run})
			id := g.toBuild("Add a feature")
			g.runOnce()
			d := g.item(id)
			if d.Item.Stage != tc.stage {
				t.Fatalf("stage %s (question %q); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
			}
			if got := g.read(l); got != tc.calls {
				t.Fatalf("calls:\n%s\nwant:\n%s", got, tc.calls)
			}
			if tc.status != "" {
				if body := g.prs.bodies[7]; !strings.Contains(body, "- pipeline: "+tc.status) {
					t.Errorf("PR body lacks the retry:\n%s", body)
				}
				return
			}
			ev := g.advanced(id)
			if ev.Outcome != "verify_failed" {
				t.Fatalf("event %s %+v", ev.Outcome, ev.Data)
			}
		})
	}
}
