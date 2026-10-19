package pipeline

import (
	"strings"
	"testing"
	"time"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md).

func TestParseProof(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		states    []string
		err       string
	}{
		{"one of each kind", `[{"kind":"usage","event":"claimed","actor":["runner:mbp","runner:mini"]},
			{"kind":"command","run":"scripts/check"},{"kind":"pi","run":"systemctl is-active jarvis"},
			{"kind":"e2e","tests":["TestLogin"]},{"kind":"manual","look":"the board's QA column on a phone"}]`,
			[]string{CheckPending, CheckPending, CheckWaiting, CheckWaiting, CheckManual}, ""},
		{"usage with match", `[{"kind":"usage","event":"rebuild","match":{"source":["needs_you"]}}]`, []string{CheckPending}, ""},
		{"not a list", `{"kind":"manual","look":"x"}`, nil, "JSON list"},
		{"empty", `[]`, nil, "1 to 10"},
		{"too many", "[" + strings.Repeat(`{"kind":"manual","look":"x"},`, 10) + `{"kind":"manual","look":"x"}]`, nil, "1 to 10"},
		{"unknown kind", `[{"kind":"vibes"}]`, nil, "kind must be one of"},
		{"another kind's field", `[{"kind":"command","run":"true","look":"x"}]`, nil, `field "look"`},
		{"unknown field", `[{"kind":"command","run":"true","sql":"select 1"}]`, nil, "unknown field"},
		{"a state is not the writer's", `[{"kind":"command","run":"true","state":"passed"}]`, nil, "unknown field"},
		{"command without run", `[{"kind":"command"}]`, nil, "run is required"},
		{"two-line run", `[{"kind":"command","run":"true\nfalse"}]`, nil, "one line"},
		{"usage without event", `[{"kind":"usage","actor":["runner:mbp"]}]`, nil, "event is required"},
		{"usage event not a kind", `[{"kind":"usage","event":"DROP TABLE"}]`, nil, "event must be"},
		{"usage match key", `[{"kind":"usage","event":"claimed","match":{"Runner Name":["mbp"]}}]`, nil, "match key"},
		{"usage match no values", `[{"kind":"usage","event":"claimed","match":{"runner":[]}}]`, nil, "at least one value"},
		{"e2e without tests", `[{"kind":"e2e","tests":[]}]`, nil, "test names"},
		{"manual without look", `[{"kind":"manual","look":"  "}]`, nil, "look must be"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseProof(c.raw, ProofByBuild)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.By != ProofByBuild || len(p.Checks) != len(c.states) {
				t.Fatalf("proof = %+v", p)
			}
			for i, s := range c.states {
				if p.Checks[i].State != s {
					t.Errorf("check %d (%s) is %s, want %s", i, p.Checks[i].Kind, p.Checks[i].State, s)
				}
			}
		})
	}
}

func TestProofStatus(t *testing.T) {
	mk := func(states ...string) *Proof {
		p := &Proof{}
		for _, s := range states {
			p.Checks = append(p.Checks, ProofCheck{Kind: ProofCommand, State: s})
		}
		return p
	}
	for _, c := range []struct {
		p    *Proof
		want string
	}{
		{nil, ProofNone},
		{mk(CheckPassed, CheckPassed), CheckPassed},
		{mk(CheckPassed, CheckManual), CheckManual},
		{mk(CheckManual, CheckWaiting), CheckWaiting},
		{mk(CheckWaiting, CheckPending, CheckManual), CheckPending},
		{mk(CheckPending, CheckFailed), CheckFailed},
	} {
		if got := c.p.Status(); got != c.want {
			t.Errorf("%+v: status %s, want %s", c.p, got, c.want)
		}
	}
}

func proofItem(t *testing.T, raw string) Item {
	t.Helper()
	p, err := ParseProof(raw, ProofByBuild)
	if err != nil {
		t.Fatal(err)
	}
	return Item{ID: 7, Stage: QA, NeedsQA: true, MergeSHA: strings.Repeat("a", 40), Proof: p}
}

func TestProofDue(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	it := proofItem(t, `[{"kind":"command","run":"true"}]`)
	if !ProofDue(it, now) {
		t.Fatal("a never-checked proof is due")
	}
	at := now.Add(-ProofRecheck + time.Second)
	it.Proof.CheckedAt = &at
	if ProofDue(it, now) {
		t.Fatal("checked under 30 min ago: not due")
	}
	if !ProofDue(it, now.Add(time.Second)) {
		t.Fatal("30 min later it is due again")
	}
	deploy := it
	deploy.AwaitingDeploy = true
	if ProofDue(deploy, now.Add(time.Hour)) {
		t.Fatal("an item waiting for its deploy is not checked yet")
	}
	for _, raw := range []string{`[{"kind":"manual","look":"x"}]`, `[{"kind":"pi","run":"x"}]`, `[{"kind":"e2e","tests":["T"]},{"kind":"manual","look":"x"}]`} {
		if ProofDue(proofItem(t, raw), now) {
			t.Fatalf("%s: nothing to run, never due", raw)
		}
	}
	done := proofItem(t, `[{"kind":"command","run":"true"}]`)
	done.Stage = Done
	if ProofDue(done, now) {
		t.Fatal("only qa items are checked")
	}
}

func TestApplyProofChecked(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	usageCmd := `[{"kind":"usage","event":"claimed","actor":["runner:mbp"]},{"kind":"command","run":"scripts/check"}]`
	for _, c := range []struct {
		name    string
		raw     string
		fails   int // the command check's fails before this check
		results []CheckResult
		usage   map[int]UsageSeen
		kind    string
		to      Stage
		status  string
		words   string
		err     string
	}{
		{name: "all automatic checks pass: verified", raw: usageCmd,
			results: []CheckResult{{Check: 1, State: CheckPassed, Evidence: "exit 0"}}, usage: map[int]UsageSeen{0: {Count: 2, Last: 40}},
			kind: "verified", to: Done, status: CheckPassed, words: "Proof passed: usage: `claimed` by runner:mbp (passed); command: `scripts/check` (passed)"},
		{name: "usage not seen yet: stays", raw: usageCmd,
			results: []CheckResult{{Check: 1, State: CheckPassed}}, usage: map[int]UsageSeen{0: {}},
			kind: "proof_checked", to: QA, status: CheckPending},
		{name: "a first failure is checked again", raw: usageCmd,
			results: []CheckResult{{Check: 1, State: CheckFailed, Evidence: "exit 1: FAIL TestX"}}, usage: map[int]UsageSeen{0: {Count: 1}},
			kind: "proof_checked", to: QA, status: CheckFailed},
		{name: "a second failure in a row reopens", raw: usageCmd, fails: 1,
			results: []CheckResult{{Check: 1, State: CheckFailed, Evidence: "exit 1: FAIL TestX\nmore"}}, usage: map[int]UsageSeen{0: {Count: 1}},
			kind: "reopened", to: Done, status: CheckFailed, words: "Proof failed 2 times in a row: command: `scripts/check`: exit 1: FAIL TestX"},
		{name: "automatic passed, manual left: stays for the user", raw: `[{"kind":"command","run":"true"},{"kind":"manual","look":"the colours"}]`,
			results: []CheckResult{{Check: 0, State: CheckPassed}},
			kind:    "proof_checked", to: QA, status: CheckManual},
		{name: "pi waits: never verified", raw: `[{"kind":"command","run":"true"},{"kind":"pi","run":"uptime"}]`,
			results: []CheckResult{{Check: 0, State: CheckPassed}},
			kind:    "proof_checked", to: QA, status: CheckWaiting},
		{name: "e2e waits: never verified", raw: `[{"kind":"usage","event":"rebuild"},{"kind":"e2e","tests":["TestRebuild"]}]`,
			usage: map[int]UsageSeen{0: {Count: 1}},
			kind:  "proof_checked", to: QA, status: CheckWaiting},
		{name: "a result for a usage check is refused", raw: usageCmd,
			results: []CheckResult{{Check: 0, State: CheckPassed}}, err: "not a command check"},
		{name: "an unknown state is refused", raw: usageCmd,
			results: []CheckResult{{Check: 1, State: "flaky"}}, err: "passed or failed"},
		{name: "out of range", raw: usageCmd,
			results: []CheckResult{{Check: 5, State: CheckPassed}}, err: "not a command check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := proofItem(t, c.raw)
			for i := range it.Proof.Checks {
				if it.Proof.Checks[i].Kind == ProofCommand && c.fails > 0 {
					it.Proof.Checks[i].Fails, it.Proof.Checks[i].State = c.fails, CheckFailed
				}
			}
			tr, err := ApplyProofChecked(it, c.results, c.usage, now)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tr.Kind != c.kind || tr.To != c.to || tr.Item.Stage != c.to {
				t.Fatalf("kind %s → %s (item %s), want %s → %s", tr.Kind, tr.To, tr.Item.Stage, c.kind, c.to)
			}
			if got := tr.Item.Proof.Status(); got != c.status {
				t.Fatalf("status %s, want %s", got, c.status)
			}
			if tr.Item.Proof.CheckedAt == nil || !tr.Item.Proof.CheckedAt.Equal(now) {
				t.Fatal("the check time is recorded")
			}
			if c.words != "" && tr.Data["words"] != c.words {
				t.Fatalf("words %q, want %q", tr.Data["words"], c.words)
			}
			if it.Proof.CheckedAt != nil {
				t.Fatal("the input item's proof must not change")
			}
		})
	}
}

func TestApplyProofCheckedRefusals(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	it := proofItem(t, `[{"kind":"command","run":"true"}]`)
	at := now.Add(-time.Minute)
	recent := it
	recent.Proof = &Proof{Checks: it.Proof.Checks, CheckedAt: &at}
	deploy := it
	deploy.AwaitingDeploy = true
	none := it
	none.Proof = nil
	done := it
	done.Stage = Done
	for name, x := range map[string]Item{"not due": recent, "awaiting deploy": deploy, "no proof": none, "not in qa": done} {
		if _, err := ApplyProofChecked(x, nil, nil, now); err == nil || !strings.Contains(err.Error(), CodeWrongStage) {
			t.Errorf("%s: err = %v, want wrong_stage", name, err)
		}
	}
}

func TestApplySetProof(t *testing.T) {
	p, _ := ParseProof(`[{"kind":"manual","look":"x"}]`, ProofBySweep)
	it := Item{Stage: QA}
	tr, err := ApplySetProof(it, p)
	if err != nil || tr.Item.Proof != p || tr.Kind != "proof_set" || tr.To != QA {
		t.Fatalf("tr %+v err %v", tr, err)
	}
	if _, err := ApplySetProof(tr.Item, p); err == nil {
		t.Fatal("a proof is not replaced")
	}
	if _, err := ApplySetProof(Item{Stage: Land}, p); err == nil {
		t.Fatal("only a qa item gets the sweep's proof")
	}
}

func TestPROpenedTakesTheProof(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	it := Item{ID: 3, Stage: Build, Risk: RiskNormal}
	adv := func(proof string) Advance {
		f := map[string]string{"repo": "o/r", "pr": "9", "branch": "loop/3-x", "head_sha": strings.Repeat("b", 40)}
		if proof != "" {
			f["proof"] = proof
		}
		return Advance{Outcome: PROpened, Fields: f, Evidence: 1, ChangedPaths: []string{"M a.go"}}
	}
	tr, err := ApplyAdvance(it, adv(`[{"kind":"command","run":"go test ./..."}]`), now)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Item.Proof == nil || tr.Item.Proof.By != ProofByBuild || tr.Item.Proof.Checks[0].Run != "go test ./..." {
		t.Fatalf("proof = %+v", tr.Item.Proof)
	}
	kept := tr.Item
	kept.Stage = Build
	tr2, err := ApplyAdvance(kept, adv(""), now)
	if err != nil || tr2.Item.Proof == nil {
		t.Fatalf("a rebuild without a proof keeps the one it had: %v %+v", err, tr2.Item.Proof)
	}
	if _, err := ApplyAdvance(it, adv(`[{"kind":"nope"}]`), now); err == nil || !strings.Contains(err.Error(), "proof") {
		t.Fatalf("a bad proof refuses pr_opened: %v", err)
	}
}

func TestQAGroup(t *testing.T) {
	mk := func(raw string) *Proof {
		p, err := ParseProof(raw, ProofByBuild)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	failed := mk(`[{"kind":"command","run":"scripts/check"},{"kind":"manual","look":"x"}]`)
	failed.Checks[0].State, failed.Checks[0].Evidence = CheckFailed, "exit 1 after 2s: FAIL\nmore"
	passedManual := mk(`[{"kind":"command","run":"true"},{"kind":"manual","look":"the QA column on a phone"}]`)
	passedManual.Checks[0].State = CheckPassed
	for _, c := range []struct {
		name          string
		p             *Proof
		deploy        bool
		group, detail string
	}{
		{"no proof", nil, false, QANeedsYou, "no proof: your check"},
		{"no proof, before the deploy", nil, true, QAWaiting, "waits for the deploy, then your check (no proof)"},
		{"usage and e2e", mk(`[{"kind":"usage","event":"claimed","actor":["runner:mbp","runner:mini"]},{"kind":"e2e","tests":["TestA"]}]`), false,
			QAWaiting, "waits for: usage: `claimed` by runner:mbp or runner:mini; e2e: TestA (waits for the E2E suite)"},
		{"before the deploy", mk(`[{"kind":"usage","event":"rebuild","match":{"source":["needs_you"]}}]`), true,
			QAWaiting, "waits for the deploy, then: usage: `rebuild` source=needs_you"},
		{"manual left", passedManual, false, QANeedsYou, "look at the QA column on a phone"},
		{"failed once", failed, false, QAFailed, "failed once, checked again before it reopens: command: `scripts/check`: exit 1 after 2s: FAIL"},
	} {
		g, d := QAGroup(c.p, c.deploy)
		if g != c.group || d != c.detail {
			t.Errorf("%s: %q, %q; want %q, %q", c.name, g, d, c.group, c.detail)
		}
	}
}
