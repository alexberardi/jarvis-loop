package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeagy"
	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// gemini turns on the second opinion with the fake agy (the test binary through a link named
// agy; unfenced: TestSecondOpinionFenced tests the fence) playing play.
func (g *rig) gemini(blocking bool, play string) {
	g.t.Helper()
	self, _ := os.Executable()
	agy := filepath.Join(filepath.Dir(g.scripts), "bin", "agy")
	os.MkdirAll(filepath.Dir(agy), 0o755)
	if err := os.Symlink(self, agy); err != nil && !os.IsExist(err) {
		g.t.Fatal(err)
	}
	off := false
	g.r.Cfg.Gemini = &GeminiConfig{Agy: agy, Blocking: blocking, Fence: &off}
	if err := g.r.Cfg.Gemini.normalize(); err != nil {
		g.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"play": play})
	if err := os.WriteFile(filepath.Join(g.scripts, "agy.json"), b, 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// agyCalls are the fake agy's calls: their arguments and working directories.
type agyCall struct {
	Args     []string `json:"args"`
	Cwd      string   `json:"cwd"`
	Writable bool     `json:"cwd_writable"`
}

func (g *rig) agyCalls() []agyCall {
	var out []agyCall
	b, _ := os.ReadFile(filepath.Join(g.scripts, "agy.calls"))
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var c agyCall
		json.Unmarshal([]byte(l), &c)
		out = append(out, c)
	}
	return out
}

func (g *rig) secondOpinions(id int64) []store.Event { return g.events(id, KindSecondOpinion) }

// Advisory (the default): Gemini's findings are judged, posted as one PR comment and kept as
// evidence, the counts are recorded, and the approval stands even with an agreed high finding.
func TestSecondOpinionAdvisory(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	head := g.item(id).Item.HeadSHA
	g.gemini(false, fakeagy.OK)
	g.stages(StageReview)
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Land {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if ev := g.advanced(id); ev.Outcome != "approved" || ev.Data["second_opinion"] != SOAnswered {
		t.Fatalf("advance %s %+v", ev.Outcome, ev.Data)
	}
	// agy ran once, read-only, in the review's worktree, with the model and the schema.
	calls := g.agyCalls()
	if len(calls) != 1 {
		t.Fatalf("agy calls %+v", calls)
	}
	args := strings.Join(calls[0].Args, " ")
	for _, want := range []string{"--model gemini-3.8-flash-high", "--mode plan", "--sandbox", "--output-format json", "--json-schema " + filepath.Join(g.lastRun(id, StageReview), "gemini-schema.json")} {
		if !strings.Contains(args, want) {
			t.Errorf("agy args lack %q: %s", want, args)
		}
	}
	prompt := calls[0].Args[1]
	for _, want := range []string{"adversarial second reviewer", "Add a feature file.", "**Approved.** The tests cover `feature.txt`", "+++ b/feature.txt"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if calls[0].Cwd != filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-review", id)) || !calls[0].Writable {
		t.Errorf("agy ran in %s", calls[0].Cwd)
	}
	cs := g.prs.comments[7]
	if len(cs) != 2 || !strings.HasPrefix(cs[0], "## Loop review: approved") {
		t.Fatalf("comments %v", cs)
	}
	for _, want := range []string{"## Second opinion (Gemini)", "Gemini (`gemini-3.8-flash-high`)", "**2 findings** (1 high, 0 medium, 1 low); Claude agreed with 2 (1 high).",
		"### 1. high: `feature.txt:1`", "The empty input is not handled", "Evidence: Line 1 indexes", "**Claude agrees:** Checked the line",
		"### 2. low: `feature.txt:2`", "Advisory: recorded only"} {
		if !strings.Contains(cs[1], want) {
			t.Errorf("comment lacks %q:\n%s", want, cs[1])
		}
	}
	so := g.secondOpinions(id)
	if len(so) != 1 {
		t.Fatalf("second_opinion events %+v", so)
	}
	data := so[0].Data
	for k, want := range map[string]any{"status": "ok", "head_sha": head, "findings": 2.0, "high": 1.0, "low": 1.0, "agreed": 2.0,
		"agreed_high": 1.0, "judged": 2.0, "blocking": false, "bounced": false, "model": "gemini-3.8-flash-high"} {
		if data[k] != want {
			t.Errorf("event %s = %v, want %v", k, data[k], want)
		}
	}
	if !strings.Contains(fmt.Sprint(data["gemini_usage"]), "input_tokens") {
		t.Errorf("usage not recorded: %v", data)
	}
	var found bool
	for _, e := range d.Evidence {
		found = found || e.Name == "gemini-findings.json"
	}
	if !found {
		t.Fatalf("no gemini-findings.json evidence: %+v", d.Evidence)
	}
	// The adjudication turn ran in the review's worktree with its own brief; the review's files
	// are kept beside it.
	run := g.lastRun(id, StageReview)
	if b := g.read(filepath.Join(run, "brief.md")); !strings.Contains(b, "- Stage: adjudicate") || !strings.Contains(b, "head `"+head+"`") {
		t.Fatalf("adjudication brief:\n%s", b)
	}
	for _, f := range []string{"brief-review.md", "stream-review.jsonl", "gemini-stdout.json"} {
		if _, err := os.Stat(filepath.Join(run, f)); err != nil {
			t.Errorf("run dir lacks %s", f)
		}
	}
}

// Under the fence (the default) agy runs in the review's worktree read-only, its home writable.
func TestSecondOpinionFenced(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Skipf("no fence here: %v", err)
	}
	id := g.toReview("Add a feature")
	g.gemini(false, fakeagy.OK)
	on := true
	g.r.Cfg.Gemini.Fence = &on
	g.r.Cfg.Gemini.Home = t.TempDir()
	// The fake records its calls in the scripts dir, which may sit under the fence's private /tmp.
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, g.scripts)
	g.stages(StageReview)
	g.runOnce()
	calls := g.agyCalls()
	if len(calls) != 1 || calls[0].Writable {
		t.Fatalf("calls %+v; log:\n%s", calls, g.logs.String())
	}
	if so := g.secondOpinions(id); len(so) != 1 || so[0].Data["status"] != SOAnswered || so[0].Data["findings"] != 2.0 {
		t.Fatalf("events %+v; log:\n%s", so, g.logs.String())
	}
}

// Claude disagrees with every finding: they are recorded, with nothing agreed.
func TestSecondOpinionDisagreed(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.gemini(true, fakeagy.OK)
	g.script(id, "adjudicate", fakeclaude.Disagree, "")
	g.stages(StageReview)
	g.runOnce()
	if st := g.item(id).Item.Stage; st != pipeline.Land {
		t.Fatalf("stage %s; log:\n%s", st, g.logs.String())
	}
	if data := g.secondOpinions(id)[0].Data; data["agreed"] != 0.0 || data["judged"] != 2.0 || data["bounced"] != false {
		t.Fatalf("event %+v", data)
	}
	if c := g.prs.comments[7][1]; !strings.Contains(c, "**Claude disagrees:**") || !strings.Contains(c, "no agreed high finding") {
		t.Fatalf("comment:\n%s", c)
	}
}

// Whatever goes wrong with agy, the second opinion is unavailable: no comment, the approval
// stands, and the event says why.
func TestSecondOpinionUnavailable(t *testing.T) {
	for play, reason := range map[string]string{fakeagy.Quota: AgyQuota, fakeagy.Garbage: AgyBadOutput, fakeagy.Failed: AgyError} {
		t.Run(play, func(t *testing.T) {
			g := newRig(t, fakeclaude.Good)
			id := g.toReview("Add a feature")
			g.gemini(true, play)
			g.stages(StageReview)
			g.runOnce()
			if st := g.item(id).Item.Stage; st != pipeline.Land {
				t.Fatalf("stage %s; log:\n%s", st, g.logs.String())
			}
			if ev := g.advanced(id); ev.Outcome != "approved" || ev.Data["second_opinion"] != SOUnavailable {
				t.Fatalf("advance %s %+v", ev.Outcome, ev.Data)
			}
			if cs := g.prs.comments[7]; len(cs) != 1 {
				t.Fatalf("comments %v", cs)
			}
			so := g.secondOpinions(id)
			if len(so) != 1 || so[0].Data["status"] != SOUnavailable || so[0].Data["reason"] != reason {
				t.Fatalf("events %+v", so)
			}
			// agy's own output stays in the run dir.
			if strings.Contains(fmt.Sprint(so[0].Data), "RESOURCE_EXHAUSTED") {
				t.Fatal("agy's stderr reached loopd")
			}
		})
	}
}

// A call stuck behind a rate limit ends at gemini.timeout, heartbeating the lease meanwhile; the
// approval stands.
func TestSecondOpinionTimeout(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.gemini(false, fakeagy.Hang)
	g.r.Cfg.Gemini.Timeout = "2m"
	g.r.Cfg.Gemini.normalize()
	g.stages(StageReview)
	wait := g.runAsync()
	waitFile(t, filepath.Join(g.scripts, "agy.hanging"))
	g.clk.BlockUntil(2) // heartbeat and the timeout armed
	g.loopdClk.Advance(time.Minute)
	g.clk.Advance(time.Minute)
	g.clk.BlockUntil(2) // re-armed after the heartbeat
	if a := g.item(id).Lease; a == nil || !a.ExpiresAt.Equal(g.loopdClk.Now().Add(store.LeaseTTL)) {
		t.Fatalf("the heartbeat did not extend the lease: %+v", a)
	}
	g.clk.Advance(time.Minute)
	wait()
	if st := g.item(id).Item.Stage; st != pipeline.Land {
		t.Fatalf("stage %s; log:\n%s", st, g.logs.String())
	}
	if so := g.secondOpinions(id); len(so) != 1 || so[0].Data["reason"] != AgyTimeout {
		t.Fatalf("events %+v", so)
	}
}

// With no time left in the run, agy is not called at all.
func TestSecondOpinionNoTime(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.gemini(false, fakeagy.OK)
	five := 5 // less than the adjudication's reserve and a minute for agy
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{MaxRunMinutes: &five})
	g.stages(StageReview)
	g.runOnce()
	if so := g.secondOpinions(id); len(so) != 1 || so[0].Data["reason"] != AgyNoTime || len(g.agyCalls()) != 0 {
		t.Fatalf("events %+v, calls %v", so, g.agyCalls())
	}
}

// No gemini block: no agy call, no event, one comment.
func TestSecondOpinionOff(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.gemini(true, fakeagy.OK)
	g.r.Cfg.Gemini = nil
	g.stages(StageReview)
	g.runOnce()
	if g.item(id).Item.Stage != pipeline.Land || len(g.agyCalls()) != 0 || len(g.secondOpinions(id)) != 0 || len(g.prs.comments[7]) != 1 {
		t.Fatalf("stage %s, calls %v, comments %d", g.item(id).Item.Stage, g.agyCalls(), len(g.prs.comments[7]))
	}
	if _, ok := g.advanced(id).Data["second_opinion"]; ok {
		t.Fatal("second_opinion field without a gemini block")
	}
}

// A bounce from the review agent itself gets no second opinion.
func TestSecondOpinionOnlyOnApproval(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	g.gemini(true, fakeagy.OK)
	g.script(id, StageReview, fakeclaude.Bounce, "")
	g.stages(StageReview)
	g.runOnce()
	if g.item(id).Item.Stage != pipeline.Build || len(g.agyCalls()) != 0 {
		t.Fatalf("stage %s, calls %v", g.item(id).Item.Stage, g.agyCalls())
	}
}

// Blocking: an agreed high finding sends the item back to build with it as the note (a review
// bounce); the same head reviewed again is not sent back a second time.
func TestSecondOpinionBlocking(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toReview("Add a feature")
	head := g.item(id).Item.HeadSHA
	g.gemini(true, fakeagy.OK)
	g.stages(StageReview)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 1 {
		t.Fatalf("stage %s, bounces %d; log:\n%s", d.Item.Stage, d.Item.BounceCount, g.logs.String())
	}
	ev := g.advanced(id)
	notes := fmt.Sprint(ev.Data["notes"])
	if ev.Outcome != "bounce" || ev.Data["second_opinion"] != "bounce" || !strings.Contains(notes, "Second opinion (Gemini), agreed by Claude") ||
		!strings.Contains(notes, "`feature.txt:1`: The empty input is not handled") || strings.Contains(notes, "The name is unclear") {
		t.Fatalf("advance %s %+v", ev.Outcome, ev.Data)
	}
	if c := g.prs.comments[7][1]; !strings.Contains(c, "**Sent back to build:** Claude agreed with 1 high finding(s)") {
		t.Fatalf("comment:\n%s", c)
	}
	if data := g.secondOpinions(id)[0].Data; data["bounced"] != true || data["blocking"] != true {
		t.Fatalf("event %+v", data)
	}
	// The build pushes nothing new: the same head is back in review.
	c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "builder", Stages: []pipeline.Stage{pipeline.Build}})
	if err != nil || c == nil || c.Item.ID != id {
		t.Fatalf("build claim %v %v", c, err)
	}
	if _, err := g.st.AddEvidence(g.ctx, "devbox", id, "tests", "after.txt", strings.NewReader("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.PROpened, map[string]string{"repo": "alex/demo", "pr": "7",
		"branch": d.Item.Branch, "head_sha": head}, []string{"A feature.txt"}); err != nil {
		t.Fatal(err)
	}
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Land || d.Item.BounceCount != 1 {
		t.Fatalf("second review of the same head: stage %s, bounces %d; log:\n%s", d.Item.Stage, d.Item.BounceCount, g.logs.String())
	}
	so := g.secondOpinions(id)
	if len(so) != 2 || so[1].Data["bounced"] != false || so[1].Data["repeat"] != true {
		t.Fatalf("events %+v", so)
	}
	cs := g.prs.comments[7]
	if !strings.Contains(cs[len(cs)-1], "already sent back once") {
		t.Fatalf("comment:\n%s", cs[len(cs)-1])
	}
}

func TestValidateAdjudication(t *testing.T) {
	fs := []Finding{{ID: 1, Severity: "high"}, {ID: 2, Severity: "low"}}
	v, err := ValidateAdjudication([]byte(`{"verdicts":[{"id":1,"verdict":"agree","reason":"yes"},{"id":2,"verdict":"disagree","reason":"no"}]}`), fs)
	if err != nil || v[1].Verdict != "agree" || v[2].Verdict != "disagree" {
		t.Fatalf("%+v %v", v, err)
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"not JSON":    {`nope`, "not JSON"},
		"missing one": {`{"verdicts":[{"id":1,"verdict":"agree","reason":"yes"}]}`, "1 verdicts for 2 findings"},
		"unknown id":  {`{"verdicts":[{"id":3,"verdict":"agree","reason":"yes"}]}`, "does not exist"},
		"twice":       {`{"verdicts":[{"id":1,"verdict":"agree","reason":"yes"},{"id":1,"verdict":"agree","reason":"yes"}]}`, "two verdicts"},
		"bad verdict": {`{"verdicts":[{"id":1,"verdict":"maybe","reason":"yes"}]}`, "agree or disagree"},
		"no reason":   {`{"verdicts":[{"id":1,"verdict":"agree","reason":" "}]}`, "reason is required"},
	} {
		if _, err := ValidateAdjudication([]byte(tc.raw), fs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// The prompt keeps within one argv string: a huge diff is cut.
func TestGeminiPromptCutsTheDiff(t *testing.T) {
	p := GeminiPrompt(Item{ID: 3, Title: "T", Ask: "ask", HeadSHA: "abc"}, Result{Review: "ok"}, strings.Repeat("+x\n", maxPromptDiff))
	if len(p) > 100<<10 || !strings.Contains(p, "[diff cut here") {
		t.Fatalf("prompt is %d bytes", len(p))
	}
}
