package pipeline

import (
	"strings"
	"testing"
	"time"
)

// Done proofs (docs/decisions/2026-10-18-done-proofs.md): a done item's verification status moves
// with its proof; its stage never does.

func doneItem(t *testing.T, raw string) Item {
	t.Helper()
	it := Item{ID: 9, Stage: Done, MergeSHA: strings.Repeat("b", 40), Verify: DoneUnproven}
	if raw != "" {
		p, err := ParseProof(raw, ProofBySweep)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := ApplySetProof(it, p)
		if err != nil {
			t.Fatal(err)
		}
		it = tr.Item
	}
	return it
}

func TestApplySetProofOnDone(t *testing.T) {
	for _, c := range []struct{ raw, verify string }{
		{`[{"kind":"command","run":"go test ./x"}]`, DoneProving},
		{`[{"kind":"usage","event":"claimed"},{"kind":"manual","look":"x"}]`, DoneProving},
		{`[{"kind":"pi","run":"uptime"}]`, DoneWaiting},
		{`[{"kind":"e2e","tests":["TestA"]},{"kind":"manual","look":"x"}]`, DoneWaiting},
		{`[{"kind":"manual","look":"the board on a phone"}]`, DoneNeedsYou},
	} {
		it := doneItem(t, c.raw)
		if it.Stage != Done || it.Verify != c.verify {
			t.Errorf("%s: stage %s verify %s, want done %s", c.raw, it.Stage, it.Verify, c.verify)
		}
	}
	p, _ := ParseProof(`[{"kind":"manual","look":"x"}]`, ProofBySweep)
	for name, it := range map[string]Item{
		"already verified": {Stage: Done, Verify: DoneVerified},
		"has a proof":      doneItem(t, `[{"kind":"manual","look":"x"}]`),
		"not done":         {Stage: Build},
	} {
		if _, err := ApplySetProof(it, p); err == nil {
			t.Errorf("%s: a proof was set", name)
		}
	}
}

func TestDoneProofDue(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	it := doneItem(t, `[{"kind":"command","run":"true"}]`)
	if !ProofDue(it, now) {
		t.Fatal("a proving done item is due")
	}
	for _, v := range []string{DoneUnproven, DoneVerified, DoneFailed, DoneNeedsYou, DoneWaiting, ""} {
		x := it
		x.Verify = v
		if ProofDue(x, now) {
			t.Errorf("verify %q: not due", v)
		}
	}
}

func TestApplyProofCheckedOnDone(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	raw := `[{"kind":"usage","event":"claimed"},{"kind":"command","run":"scripts/check"}]`
	for _, c := range []struct {
		name    string
		raw     string
		fails   int
		results []CheckResult
		usage   map[int]UsageSeen
		kind    string
		verify  string
		changed bool
	}{
		{"all pass: verified, stays done", raw, 0, []CheckResult{{Check: 1, State: CheckPassed}}, map[int]UsageSeen{0: {Count: 3, Last: 9}},
			KindDoneVerified, DoneVerified, true},
		{"usage not seen: still proving", raw, 0, []CheckResult{{Check: 1, State: CheckPassed}}, map[int]UsageSeen{0: {}},
			KindDoneProofChecked, DoneProving, false},
		{"one failure: still proving", raw, 0, []CheckResult{{Check: 1, State: CheckFailed, Evidence: "exit 1"}}, nil,
			KindDoneProofChecked, DoneProving, false},
		{"two failures: failed, stays done", raw, 1, []CheckResult{{Check: 1, State: CheckFailed, Evidence: "exit 1: FAIL"}}, nil,
			KindDoneFailed, DoneFailed, true},
		{"automatic passed, manual left: needs you", `[{"kind":"command","run":"true"},{"kind":"manual","look":"x"}]`, 0,
			[]CheckResult{{Check: 0, State: CheckPassed}}, nil, KindDoneProofChecked, DoneNeedsYou, true},
		{"pi left: waiting", `[{"kind":"command","run":"true"},{"kind":"pi","run":"uptime"}]`, 0,
			[]CheckResult{{Check: 0, State: CheckPassed}}, nil, KindDoneProofChecked, DoneWaiting, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := doneItem(t, c.raw)
			for i := range it.Proof.Checks {
				if it.Proof.Checks[i].Kind == ProofCommand && c.fails > 0 {
					it.Proof.Checks[i].Fails, it.Proof.Checks[i].State = c.fails, CheckFailed
				}
			}
			tr, err := ApplyProofChecked(it, c.results, c.usage, now)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Kind != c.kind || tr.From != Done || tr.To != Done || tr.Item.Stage != Done {
				t.Fatalf("kind %s %s → %s (item %s), want %s, done → done", tr.Kind, tr.From, tr.To, tr.Item.Stage, c.kind)
			}
			if tr.Item.Verify != c.verify || tr.Data["verify"] != c.verify {
				t.Fatalf("verify %s (data %v), want %s", tr.Item.Verify, tr.Data["verify"], c.verify)
			}
			if c.kind == KindDoneProofChecked && tr.Data["changed"] != c.changed {
				t.Fatalf("changed %v, want %v", tr.Data["changed"], c.changed)
			}
			if c.kind == KindDoneFailed && !strings.HasPrefix(tr.Data["words"].(string), "Proof of the merged change failed 2 times in a row: command: `scripts/check`: exit 1: FAIL") {
				t.Fatalf("words %q", tr.Data["words"])
			}
			if tr.Item.VerifyReason == "" {
				t.Fatal("a reason is recorded")
			}
		})
	}
}

func TestDoneVerifyAtTheTransitions(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	qa := proofItem(t, `[{"kind":"command","run":"true"}]`)
	tr, err := ApplyProofChecked(qa, []CheckResult{{Check: 0, State: CheckPassed}}, nil, now)
	if err != nil || tr.Kind != "verified" || tr.Item.Verify != DoneVerified {
		t.Fatalf("qa proof verified: kind %s verify %s err %v", tr.Kind, tr.Item.Verify, err)
	}
	tr, err = ApplyVerified(Item{Stage: QA}, "")
	if err != nil || tr.Item.Verify != DoneVerified {
		t.Fatalf("user verified: %s %v", tr.Item.Verify, err)
	}
	tr, err = ApplyReopen(Item{Stage: QA}, "broken")
	if err != nil || tr.Item.Verify != DoneFailed {
		t.Fatalf("reopened: %s %v", tr.Item.Verify, err)
	}
	// The user's check of a done item the sweep could not prove: verified, still done.
	tr, err = ApplyVerified(doneItem(t, `[{"kind":"manual","look":"x"}]`), "looks right")
	if err != nil || tr.Kind != "verified" || tr.From != Done || tr.To != Done || tr.Item.Verify != DoneVerified {
		t.Fatalf("done verified by the user: %+v %v", tr, err)
	}
	if _, err := ApplyVerified(Item{Stage: Done, Verify: DoneVerified}, ""); err == nil {
		t.Fatal("a verified done item is not verified twice")
	}
	tr, err = ApplyVerifiedByCI(Item{Stage: Done, Verify: DoneUnproven})
	if err != nil || tr.Kind != KindDoneVerified || tr.Item.Verify != DoneVerified || tr.Item.VerifyReason != VerifiedByCI {
		t.Fatalf("by CI: %+v %v", tr, err)
	}
	if _, err := ApplyVerifiedByCI(Item{Stage: Done, Verify: DoneFailed}); err == nil {
		t.Fatal("only an unproven item is verified by CI")
	}
}

func TestMergedStraightToDone(t *testing.T) {
	for _, c := range []struct {
		proof  string
		verify string
	}{
		{"", DoneUnproven},
		{`[{"kind":"command","run":"go test ./x"}]`, DoneProving},
	} {
		it := item(Land)
		if c.proof != "" {
			it.Proof, _ = ParseProof(c.proof, ProofByBuild)
		}
		tr, err := ApplyAdvance(it, merged(it.HeadSHA, clean), t0)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Item.Stage != Done || tr.Item.Verify != c.verify {
			t.Errorf("proof %q: stage %s verify %s, want done %s", c.proof, tr.Item.Stage, tr.Item.Verify, c.verify)
		}
	}
}

func TestMainRedItem(t *testing.T) {
	for _, c := range []struct {
		title, source string
		want          bool
	}{
		{"main red in jarvis-server: ci", "finding", true},
		{"main red after #82: ci", "finding", true},
		{"main red after #82: ci", "user", false},
		{"fix main red handling", "finding", false},
	} {
		if got := MainRedItem(Item{Title: c.title, Source: c.source}); got != c.want {
			t.Errorf("%q/%s: %v, want %v", c.title, c.source, got, c.want)
		}
	}
}

func TestApplyReopenNotReproducible(t *testing.T) {
	failed := doneItem(t, `[{"kind":"command","run":"go test ./x"}]`)
	failed.Verify, failed.VerifyReason = DoneFailed, "Proof failed"
	tr, err := ApplyReopenNotReproducible(failed, 12, "  works here\nlong story ")
	if err != nil {
		t.Fatal(err)
	}
	if it := tr.Item; it.Stage != Done || it.Verify != DoneUnproven || it.Proof != nil ||
		it.VerifyReason != "reopen #12 closed not reproducible: works here" || tr.Kind != KindDoneReproof || tr.From != Done || tr.To != Done {
		t.Fatalf("%+v", tr)
	}
	for name, it := range map[string]Item{
		"unproven": doneItem(t, ""),
		"not done": {ID: 9, Stage: QA, Verify: DoneFailed},
	} {
		if _, err := ApplyReopenNotReproducible(it, 12, "works"); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	if _, err := ApplyReopenNotReproducible(failed, 12, "Duplicate of #3"); err == nil {
		t.Error("duplicate: applied")
	}
}
