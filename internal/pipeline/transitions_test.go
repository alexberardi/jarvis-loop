package pipeline

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const sha1 = "0123456789abcdef0123456789abcdef01234567"
const sha2 = "fedcba9876543210fedcba9876543210fedcba98"

func item(stage Stage) Item {
	return Item{ID: 7, Title: "t", Priority: "P3", Stage: stage, Risk: RiskNormal, HeadSHA: sha1, BuilderAgent: "builder", CreatedAt: t0}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want error code %q, got %v", code, err)
	}
	if pe.Code != code {
		t.Fatalf("want error code %q, got %q (%s)", code, pe.Code, pe.Message)
	}
}

// One row per transition in the spec's table, plus the refusals each row implies.
func TestApplyAdvance(t *testing.T) {
	reproduced := map[string]string{"area": "voice", "size": "M", "risk": "normal", "resources": "gpu,voice"}
	prOpened := map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": sha2}
	without := func(m map[string]string, k string) map[string]string {
		out := map[string]string{}
		for kk, v := range m {
			if kk != k {
				out[kk] = v
			}
		}
		return out
	}
	with := func(m map[string]string, k, v string) map[string]string {
		out := without(m, k)
		out[k] = v
		return out
	}

	cases := []struct {
		name     string
		from     Stage
		outcome  Outcome
		fields   map[string]string
		evidence int
		agent    string
		wantTo   Stage
		wantRet  Stage
		wantCode string
	}{
		// triage → build
		{"reproduced", Triage, Reproduced, reproduced, 1, "a", Build, "", ""},
		{"reproduced no resources", Triage, Reproduced, with(reproduced, "resources", ""), 1, "a", Build, "", ""},
		{"reproduced missing area", Triage, Reproduced, without(reproduced, "area"), 1, "a", "", "", CodeMissingField},
		{"reproduced missing size", Triage, Reproduced, without(reproduced, "size"), 1, "a", "", "", CodeMissingField},
		{"reproduced bad size", Triage, Reproduced, with(reproduced, "size", "XL"), 1, "a", "", "", CodeInvalidField},
		{"reproduced bad risk", Triage, Reproduced, with(reproduced, "risk", "maybe"), 1, "a", "", "", CodeInvalidField},
		{"reproduced missing resources", Triage, Reproduced, without(reproduced, "resources"), 1, "a", "", "", CodeMissingField},
		{"reproduced unknown resource", Triage, Reproduced, with(reproduced, "resources", "gpu,tpu"), 1, "a", "", "", CodeInvalidField},
		{"reproduced no evidence", Triage, Reproduced, reproduced, 0, "a", "", "", CodeEvidenceRequired},
		{"reproduced from build", Build, Reproduced, reproduced, 1, "a", "", "", CodeWrongStage},
		// triage → closed
		{"not reproducible", Triage, NotReproducible, map[string]string{"summary": "works on main"}, 1, "a", Closed, "", ""},
		{"not reproducible no evidence", Triage, NotReproducible, map[string]string{"summary": "s"}, 0, "a", "", "", CodeEvidenceRequired},
		{"not reproducible no summary", Triage, NotReproducible, nil, 1, "a", "", "", CodeMissingField},
		{"not reproducible from review", Review, NotReproducible, map[string]string{"summary": "s"}, 1, "a", "", "", CodeWrongStage},
		// question
		{"question from triage", Triage, Question, map[string]string{"question": "which repo?"}, 0, "a", NeedsYou, Triage, ""},
		{"question from build", Build, Question, map[string]string{"question": "which repo?"}, 0, "a", NeedsYou, Build, ""},
		{"question from review", Review, Question, map[string]string{"question": "q"}, 0, "a", "", "", CodeWrongStage},
		{"question from land", Land, Question, map[string]string{"question": "q"}, 0, "a", "", "", CodeWrongStage},
		{"question empty", Triage, Question, map[string]string{"question": "  "}, 0, "a", "", "", CodeMissingField},
		// build → review
		{"pr opened", Build, PROpened, prOpened, 1, "builder2", Review, "", ""},
		{"pr opened bad pr", Build, PROpened, with(prOpened, "pr", "x"), 1, "a", "", "", CodeInvalidField},
		{"pr opened zero pr", Build, PROpened, with(prOpened, "pr", "0"), 1, "a", "", "", CodeInvalidField},
		{"pr opened bad sha", Build, PROpened, with(prOpened, "head_sha", "zzz"), 1, "a", "", "", CodeInvalidField},
		{"pr opened missing branch", Build, PROpened, without(prOpened, "branch"), 1, "a", "", "", CodeMissingField},
		{"pr opened missing repo", Build, PROpened, without(prOpened, "repo"), 1, "a", "", "", CodeMissingField},
		{"pr opened no evidence", Build, PROpened, prOpened, 0, "a", "", "", CodeEvidenceRequired},
		{"pr opened from triage", Triage, PROpened, prOpened, 1, "a", "", "", CodeWrongStage},
		// review → land
		{"approved", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "reviewer", Land, "", ""},
		{"approved wrong sha", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha2}, 0, "reviewer", "", "", CodeSHAMismatch},
		{"approved no summary", Review, Approved, map[string]string{"head_sha": sha1}, 0, "reviewer", "", "", CodeMissingField},
		{"approved by builder", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "builder", "", "", CodeSameAgent},
		{"approved from build", Build, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "reviewer", "", "", CodeWrongStage},
		// review → build (first bounce)
		{"bounce", Review, Bounce, map[string]string{"notes": "no test for the error path"}, 0, "reviewer", Build, "", ""},
		{"bounce no notes", Review, Bounce, nil, 0, "reviewer", "", "", CodeMissingField},
		{"bounce from land", Land, Bounce, map[string]string{"notes": "n"}, 0, "reviewer", "", "", CodeWrongStage},
		// land
		{"merged", Land, Merged, map[string]string{"merge_sha": sha2}, 0, "lander", Done, "", ""},
		{"merged no sha", Land, Merged, nil, 0, "lander", "", "", CodeMissingField},
		{"hold", Land, Hold, map[string]string{"reason": "touches auth"}, 0, "lander", Held, "", ""},
		{"hold no reason", Land, Hold, nil, 0, "lander", "", "", CodeMissingField},
		{"back to build", Land, BackToBuild, map[string]string{"reason": "conflict in internal/"}, 0, "lander", Build, "", ""},
		{"back to build from review", Review, BackToBuild, map[string]string{"reason": "r"}, 0, "lander", "", "", CodeWrongStage},
		// usage_limit, any work stage
		{"usage limit triage", Triage, UsageLimit, map[string]string{"error": "rate limited"}, 0, "a", Triage, "", ""},
		{"usage limit build", Build, UsageLimit, map[string]string{"error": "rate limited", "reset": "at 17:00"}, 0, "a", Build, "", ""},
		{"usage limit review", Review, UsageLimit, map[string]string{"error": "e"}, 0, "a", Review, "", ""},
		{"usage limit land", Land, UsageLimit, map[string]string{"error": "e"}, 0, "a", Land, "", ""},
		{"usage limit no error", Build, UsageLimit, nil, 0, "a", "", "", CodeMissingField},
		// failed, any work stage
		{"failed triage", Triage, Failed, map[string]string{"error": "boom"}, 0, "a", NeedsYou, Triage, ""},
		{"failed land", Land, Failed, map[string]string{"error": "boom"}, 0, "a", NeedsYou, Land, ""},
		{"failed no error", Build, Failed, nil, 0, "a", "", "", CodeMissingField},
		// not work stages
		{"advance needs_you", NeedsYou, Failed, map[string]string{"error": "e"}, 0, "a", "", "", CodeWrongStage},
		{"advance held", Held, Merged, map[string]string{"merge_sha": sha2}, 0, "a", "", "", CodeWrongStage},
		{"advance done", Done, Failed, map[string]string{"error": "e"}, 0, "a", "", "", CodeWrongStage},
		{"unknown outcome", Build, Outcome("shipped"), nil, 0, "a", "", "", CodeUnknownOutcome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := item(tc.from)
			tr, err := ApplyAdvance(it, Advance{Outcome: tc.outcome, Fields: tc.fields, Evidence: tc.evidence, Agent: tc.agent}, t0)
			wantCode(t, err, tc.wantCode)
			if tc.wantCode != "" {
				return
			}
			if tr.From != tc.from || tr.To != tc.wantTo || tr.Item.Stage != tc.wantTo {
				t.Fatalf("from %s to %s (item %s), want %s → %s", tr.From, tr.To, tr.Item.Stage, tc.from, tc.wantTo)
			}
			if tr.Item.ReturnStage != tc.wantRet {
				t.Fatalf("return stage %q, want %q", tr.Item.ReturnStage, tc.wantRet)
			}
			if tr.Outcome != tc.outcome {
				t.Fatalf("outcome %q, want %q", tr.Outcome, tc.outcome)
			}
			if got := tr.ActiveAgentsZero; got != (tc.outcome == UsageLimit) {
				t.Fatalf("ActiveAgentsZero = %v", got)
			}
		})
	}
}

func TestReproducedSetsTriageResult(t *testing.T) {
	tr, err := ApplyAdvance(item(Triage), Advance{Outcome: Reproduced, Evidence: 1, Fields: map[string]string{
		"area": "voice", "size": "L", "risk": "hold", "resources": " voice , gpu,voice",
	}}, t0)
	wantCode(t, err, "")
	it := tr.Item
	if it.Area != "voice" || it.Size != "L" || it.Risk != RiskHold {
		t.Fatalf("triage result not recorded: %+v", it)
	}
	if len(it.Resources) != 2 || it.Resources[0] != "gpu" || it.Resources[1] != "voice" {
		t.Fatalf("resources = %v, want sorted, deduplicated [gpu voice]", it.Resources)
	}
}

func TestPROpenedRecordsBuilderAndVoidsApproval(t *testing.T) {
	it := item(Build)
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, Advance{Outcome: PROpened, Evidence: 1, Agent: "builder-2", Fields: map[string]string{
		"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": sha2,
	}}, t0)
	wantCode(t, err, "")
	if tr.Item.BuilderAgent != "builder-2" || tr.Item.PR != 142 || tr.Item.Branch != "fix/x" || tr.Item.HeadSHA != sha2 || tr.Item.Repo != "jarvis-server" {
		t.Fatalf("pr_opened not recorded: %+v", tr.Item)
	}
	if tr.Item.Approval != nil {
		t.Fatalf("approval for the old head must be void after a new head sha")
	}

	// Same head sha keeps it (nothing the user saw has changed).
	it.HeadSHA = sha2
	it.Approval = &Approval{By: "alex", HeadSHA: sha2, At: t0}
	tr, err = ApplyAdvance(it, Advance{Outcome: PROpened, Evidence: 1, Agent: "b", Fields: map[string]string{
		"repo": "r", "pr": "1", "branch": "b", "head_sha": sha2,
	}}, t0)
	wantCode(t, err, "")
	if tr.Item.Approval == nil {
		t.Fatalf("approval for the same head sha should stand")
	}
}

func TestBounceCounting(t *testing.T) {
	it := item(Review)
	bounce := Advance{Outcome: Bounce, Agent: "reviewer", Fields: map[string]string{"notes": "missing test"}}

	tr, err := ApplyAdvance(it, bounce, t0)
	wantCode(t, err, "")
	if tr.To != Build || tr.Item.BounceCount != 1 {
		t.Fatalf("1st bounce: to %s count %d, want build 1", tr.To, tr.Item.BounceCount)
	}

	it = tr.Item
	it.Stage = Review
	tr, err = ApplyAdvance(it, bounce, t0)
	wantCode(t, err, "")
	if tr.To != NeedsYou || tr.Item.BounceCount != 2 {
		t.Fatalf("2nd bounce: to %s count %d, want needs_you 2", tr.To, tr.Item.BounceCount)
	}
	if tr.Item.Question != "missing test" || tr.Item.ReturnStage != Build {
		t.Fatalf("2nd bounce: question %q return %q", tr.Item.Question, tr.Item.ReturnStage)
	}
}

func TestMergedHoldRiskNeedsApprovalForHead(t *testing.T) {
	merged := Advance{Outcome: Merged, Agent: "l", Fields: map[string]string{"merge_sha": sha2}}
	it := item(Land)
	it.Risk = RiskHold
	_, err := ApplyAdvance(it, merged, t0)
	wantCode(t, err, CodeApprovalRequired)

	it.Approval = &Approval{By: "alex", HeadSHA: sha2, At: t0} // for another commit
	_, err = ApplyAdvance(it, merged, t0)
	wantCode(t, err, CodeApprovalRequired)

	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, merged, t0)
	wantCode(t, err, "")
	if tr.To != Done || tr.Item.MergeSHA != sha2 {
		t.Fatalf("merged: %+v", tr)
	}
}

func TestHumanTransitions(t *testing.T) {
	t.Run("answer returns to the stage that asked", func(t *testing.T) {
		it := item(NeedsYou)
		it.ReturnStage = Build
		it.Question = "which repo?"
		tr, err := ApplyAnswer(it, "jarvis-server")
		wantCode(t, err, "")
		if tr.To != Build || tr.Item.Stage != Build || tr.Item.Question != "" || tr.Item.ReturnStage != "" {
			t.Fatalf("answer: %+v", tr.Item)
		}
	})
	t.Run("answer needs text", func(t *testing.T) {
		it := item(NeedsYou)
		it.ReturnStage = Build
		_, err := ApplyAnswer(it, " ")
		wantCode(t, err, CodeMissingField)
	})
	t.Run("answer outside needs_you", func(t *testing.T) {
		_, err := ApplyAnswer(item(Build), "x")
		wantCode(t, err, CodeWrongStage)
	})
	t.Run("approve with the head sha", func(t *testing.T) {
		tr, err := ApplyApprove(item(Held), "alex", sha1, "yes, merge it", t0)
		wantCode(t, err, "")
		a := tr.Item.Approval
		if tr.To != Land || a == nil || a.By != "alex" || a.HeadSHA != sha1 || a.Words != "yes, merge it" || !a.At.Equal(t0) {
			t.Fatalf("approve: %+v %+v", tr, a)
		}
	})
	t.Run("approve with another sha is refused", func(t *testing.T) {
		_, err := ApplyApprove(item(Held), "alex", sha2, "", t0)
		wantCode(t, err, CodeSHAMismatch)
	})
	t.Run("approve outside held", func(t *testing.T) {
		_, err := ApplyApprove(item(Land), "alex", sha1, "", t0)
		wantCode(t, err, CodeWrongStage)
	})
	t.Run("hold stays held", func(t *testing.T) {
		tr, err := ApplyHold(item(Held), "not yet")
		wantCode(t, err, "")
		if tr.To != Held {
			t.Fatalf("hold: %+v", tr)
		}
	})
	t.Run("hold outside held", func(t *testing.T) {
		_, err := ApplyHold(item(Review), "")
		wantCode(t, err, CodeWrongStage)
	})
}

func TestApplyPatch(t *testing.T) {
	p1, title, bad, empty := "P1", "new title", "P9", " "
	tr, err := ApplyPatch(item(Build), &p1, &title)
	wantCode(t, err, "")
	if tr.Item.Priority != "P1" || tr.Item.Title != "new title" {
		t.Fatalf("patch: %+v", tr.Item)
	}
	_, err = ApplyPatch(item(Build), &bad, nil)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyPatch(item(Build), nil, &empty)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyPatch(item(Build), nil, nil)
	wantCode(t, err, CodeMissingField)
	for _, s := range []Stage{Done, Closed} {
		_, err = ApplyPatch(item(s), &p1, nil)
		wantCode(t, err, CodeWrongStage)
	}
	for _, s := range []Stage{Triage, NeedsYou, Held} {
		_, err = ApplyPatch(item(s), &p1, nil)
		wantCode(t, err, "")
	}
}

func TestOvertime(t *testing.T) {
	tr := Overtime(item(Build), "running scripts/check", 45*time.Minute)
	if tr.To != NeedsYou || tr.Item.ReturnStage != Build {
		t.Fatalf("overtime: %+v", tr)
	}
	want := "run exceeded 45 minutes; last note: running scripts/check"
	if tr.Item.Question != want {
		t.Fatalf("question %q, want %q", tr.Item.Question, want)
	}
	if q := Overtime(item(Build), "", 45*time.Minute).Item.Question; q != "run exceeded 45 minutes" {
		t.Fatalf("question without a note: %q", q)
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		s       Stage
		claimed bool
		want    string
	}{
		{Triage, false, "intake"}, {Triage, true, "triage"},
		{Build, false, "ready"}, {Build, true, "build"},
		{Review, false, "review (queued)"}, {Review, true, "review"},
		{Land, false, "land (queued)"}, {Land, true, "land"},
		{NeedsYou, false, "needs-you"}, {Held, false, "held"},
		{Done, false, "done"}, {Closed, false, "closed"},
	}
	for _, c := range cases {
		if got := Display(c.s, c.claimed); got != c.want {
			t.Errorf("Display(%s, %v) = %q, want %q", c.s, c.claimed, got, c.want)
		}
	}
}

func TestNewItemDefaults(t *testing.T) {
	cases := []struct {
		pri, src         string
		wantPri, wantSrc string
		code             string
	}{
		{"", "", "P3", "user", ""},
		{"", "scout", "P4", "scout", ""},
		{"P1", "scout", "P1", "scout", ""},
		{"P2", "nightly", "P2", "nightly", ""},
		{"", "finding", "P3", "finding", ""},
		{"P0", "", "", "", CodeInvalidField},
		{"", "slack", "", "", CodeInvalidField},
	}
	for _, c := range cases {
		it, err := NewItem("title", "ask", "jarvis-server", c.pri, c.src, t0)
		wantCode(t, err, c.code)
		if c.code != "" {
			continue
		}
		if it.Priority != c.wantPri || it.Source != c.wantSrc || it.Stage != Triage || it.Risk != RiskNormal {
			t.Errorf("NewItem(%q,%q) = %+v", c.pri, c.src, it)
		}
	}
	_, err := NewItem(" ", "", "", "", "", t0)
	wantCode(t, err, CodeMissingField)
}

func TestControlValidate(t *testing.T) {
	ok := DefaultControl()
	if ok.ActiveAgents != 4 || ok.MaxRunMinutes != 45 || ok.Limits["gpu"] != 2 || ok.Limits["review"] != 2 {
		t.Fatalf("defaults: %+v", ok)
	}
	wantCode(t, ok.Validate(), "")
	for _, mut := range []func(*Control){
		func(c *Control) { c.ActiveAgents = 7 },
		func(c *Control) { c.ActiveAgents = -1 },
		func(c *Control) { c.MaxRunMinutes = 0 },
		func(c *Control) { c.Limits = map[string]int{"tpu": 1} },
		func(c *Control) { c.Limits = map[string]int{"gpu": -1} },
		func(c *Control) { c.Limits = map[string]int{"build": 1} },
	} {
		c := DefaultControl()
		mut(&c)
		wantCode(t, c.Validate(), CodeInvalidField)
	}
}
