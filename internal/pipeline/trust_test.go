package pipeline

import (
	"strings"
	"testing"
)

// refused is an item verify refused for changing trusted files at sha1.
func refused(t *testing.T) Item {
	t.Helper()
	tr, err := ApplyAdvance(item(Build), Advance{Outcome: Failed, Fields: map[string]string{
		"error": "the change touches tools/pipeline", FieldTrustSHA: sha1,
		FieldTrustPaths: "tools/pipeline, scripts/sandbox", FieldTrustDiffstat: " tools/pipeline | 2 +-"}}, t0)
	wantCode(t, err, "")
	if tr.To != NeedsYou || tr.Item.Trust == nil || tr.Item.Trust.HeadSHA != sha1 || tr.Item.Trust.By != "" {
		t.Fatalf("refusal: to %s, trust %+v", tr.To, tr.Item.Trust)
	}
	if got := strings.Join(tr.Item.Trust.Paths, ","); got != "tools/pipeline,scripts/sandbox" || tr.Item.Trust.Diffstat != "tools/pipeline | 2 +-" {
		t.Fatalf("trust paths %q, diffstat %q", got, tr.Item.Trust.Diffstat)
	}
	if tr.Data["trust_needed"] != true {
		t.Fatalf("data %v", tr.Data)
	}
	return tr.Item
}

func TestTrustRefusedThenOKForTheSHA(t *testing.T) {
	it := refused(t)
	if TrustAllows(it.Trust, sha1) {
		t.Fatal("a refusal alone allows nothing")
	}
	tr, err := ApplyTrust(it, "user:alex", sha1, t0)
	wantCode(t, err, "")
	if tr.To != Build || tr.Kind != "trusted" || tr.Item.Question != "" || tr.Item.ReturnStage != "" {
		t.Fatalf("trust: %+v", tr)
	}
	if tr.Item.Trust.By != "user:alex" || !tr.Item.Trust.At.Equal(t0) || !TrustAllows(tr.Item.Trust, sha1) {
		t.Fatalf("trust %+v", tr.Item.Trust)
	}
	if TrustAllows(tr.Item.Trust, sha2) {
		t.Fatal("the OK is for one commit only")
	}
	if tr.Item.Risk != RiskHold || !strings.Contains(tr.Item.HoldReason, TrustReason) {
		t.Fatalf("land must hold it: risk %s, reason %q", tr.Item.Risk, tr.Item.HoldReason)
	}
}

func TestTrustRefusals(t *testing.T) {
	it := refused(t)
	_, err := ApplyTrust(it, "alex", sha2, t0)
	wantCode(t, err, CodeSHAMismatch)
	_, err = ApplyTrust(it, "alex", " ", t0)
	wantCode(t, err, CodeMissingField)

	b := it
	b.Stage = Build
	_, err = ApplyTrust(b, "alex", sha1, t0)
	wantCode(t, err, CodeWrongStage)

	// A needs-you that is not verify's refusal has nothing to trust.
	plain := item(NeedsYou)
	_, err = ApplyTrust(plain, "alex", sha1, t0)
	wantCode(t, err, CodeWrongStage)
}

func TestTrustUsedOnce(t *testing.T) {
	tr, err := ApplyTrust(refused(t), "alex", sha1, t0)
	wantCode(t, err, "")
	it := tr.Item

	pr := map[string]string{"repo": "alexberardi/jarvis-server", "pr": "9", "branch": "b", "head_sha": sha1, FieldTrustUsed: sha1}
	a := Advance{Outcome: PROpened, Fields: pr, Evidence: 1, ChangedPaths: []string{"M tools/pipeline"}}
	tr, err = ApplyAdvance(it, a, t0)
	wantCode(t, err, "")
	if !tr.Item.Trust.Used || TrustAllows(tr.Item.Trust, sha1) {
		t.Fatalf("the OK is used up by the verify: %+v", tr.Item.Trust)
	}
	if tr.Item.Risk != RiskHold {
		t.Fatal("the trusted PR stays held")
	}

	// A second report under the same OK is refused.
	again := tr.Item
	again.Stage = Build
	_, err = ApplyAdvance(again, a, t0)
	wantCode(t, err, CodeInvalidField)

	// The OK is for the head verify ran; the PR must be that head.
	b := map[string]string{}
	for k, v := range pr {
		b[k] = v
	}
	b["head_sha"] = sha2
	_, err = ApplyAdvance(it, Advance{Outcome: PROpened, Fields: b, Evidence: 1, ChangedPaths: []string{"M x"}}, t0)
	wantCode(t, err, CodeSHAMismatch)

	// verify_failed under the OK uses it up too.
	tr, err = ApplyAdvance(it, Advance{Outcome: VerifyFailed, Evidence: 1,
		Fields: map[string]string{"reason": "red", FieldTrustUsed: sha1}}, t0)
	wantCode(t, err, "")
	if !tr.Item.Trust.Used {
		t.Fatal("verify_failed used the OK")
	}
}

func TestTrustNewCommitRefusedAgain(t *testing.T) {
	tr, err := ApplyTrust(refused(t), "alex", sha1, t0)
	wantCode(t, err, "")
	// The rebuild made a new head: the runner refuses it and records the new commit, unapproved.
	tr, err = ApplyAdvance(tr.Item, Advance{Outcome: Failed, Fields: map[string]string{
		"error": "touches tools/pipeline", FieldTrustSHA: sha2, FieldTrustPaths: "tools/pipeline"}}, t0)
	wantCode(t, err, "")
	if tr.Item.Trust.HeadSHA != sha2 || tr.Item.Trust.By != "" || TrustAllows(tr.Item.Trust, sha2) {
		t.Fatalf("a new head needs a new OK: %+v", tr.Item.Trust)
	}
	_, err = ApplyTrust(tr.Item, "alex", sha1, t0)
	wantCode(t, err, CodeSHAMismatch)
}

func TestTrustFieldsChecked(t *testing.T) {
	// trust_used without an OK.
	_, err := ApplyAdvance(item(Build), Advance{Outcome: VerifyFailed, Evidence: 1,
		Fields: map[string]string{"reason": "red", FieldTrustUsed: sha1}}, t0)
	wantCode(t, err, CodeInvalidField)
	// A refusal with no paths, or on another outcome or stage.
	_, err = ApplyAdvance(item(Build), Advance{Outcome: Failed, Fields: map[string]string{"error": "x", FieldTrustSHA: sha1}}, t0)
	wantCode(t, err, CodeMissingField)
	_, err = ApplyAdvance(item(Build), Advance{Outcome: Question, Fields: map[string]string{"question": "x", FieldTrustSHA: sha1, FieldTrustPaths: "p"}}, t0)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyAdvance(item(Review), Advance{Outcome: Failed, Fields: map[string]string{"error": "x", FieldTrustSHA: sha1, FieldTrustPaths: "p"}}, t0)
	wantCode(t, err, CodeInvalidField)
}

// A run that ends without a verdict on the code (a runner fault, a question, the usage limit)
// leaves the OK valid for its commit: the next verify of the same head runs without a new OK.
func TestTrustKeptWithoutAVerdict(t *testing.T) {
	tr, err := ApplyTrust(refused(t), "alex", sha1, t0)
	wantCode(t, err, "")
	it := tr.Item
	for _, a := range []Advance{
		{Outcome: Failed, Fields: map[string]string{"error": "verify_mask: no such file", FieldTrustUsed: sha1}},
		{Outcome: Question, Fields: map[string]string{"question": "resubmit?", FieldTrustUsed: sha1}},
		{Outcome: UsageLimit, Fields: map[string]string{"error": "usage limit", FieldTrustUsed: sha1}},
	} {
		tr, err := ApplyAdvance(it, a, t0)
		wantCode(t, err, "")
		if tr.Item.Trust == nil || tr.Item.Trust.Used || !TrustAllows(tr.Item.Trust, sha1) {
			t.Fatalf("%s used up the OK: %+v", a.Outcome, tr.Item.Trust)
		}
		if TrustAllows(tr.Item.Trust, sha2) {
			t.Fatalf("%s: the OK is still for %s only", a.Outcome, sha1)
		}
		// trust_used is still checked: a commit the approver did not OK is refused.
		bad := Advance{Outcome: a.Outcome, Fields: map[string]string{}}
		for k, v := range a.Fields {
			bad.Fields[k] = v
		}
		bad.Fields[FieldTrustUsed] = sha2
		_, err = ApplyAdvance(it, bad, t0)
		wantCode(t, err, CodeInvalidField)
	}
}

// digest is the sha256 of the trusted files' diff verify refused at sha1 (refusedWithDigest).
var digest = strings.Repeat("ab", 32)

// refusedWithDigest is an item verify refused at sha1 and the approver OK'd, whose refusal
// recorded the trusted files' diff digest.
func refusedWithDigest(t *testing.T) Item {
	t.Helper()
	tr, err := ApplyAdvance(item(Build), Advance{Outcome: Failed, Fields: map[string]string{
		"error": "the change touches tools/pipeline", FieldTrustSHA: sha1, FieldTrustPaths: "tools/pipeline",
		FieldTrustDiffSHA: digest}}, t0)
	wantCode(t, err, "")
	if tr.Item.Trust.DiffSHA256 != digest {
		t.Fatalf("the refusal records the diff digest: %+v", tr.Item.Trust)
	}
	tr, err = ApplyTrust(tr.Item, "user:alex", sha1, t0)
	wantCode(t, err, "")
	return tr.Item
}

// A new head whose trusted files' diff is byte-identical to the one the approver OK'd carries the
// OK (#277): loopd checks the digest, rebinds the OK to the new head and records trust_carried.
func TestTrustCarriedToAHeadWithTheSameTrustedDiff(t *testing.T) {
	it := refusedWithDigest(t)
	// The PR opened at sha1 under the OK, which used it up.
	pr := map[string]string{"repo": "alexberardi/jarvis-server", "pr": "9", "branch": "b", "head_sha": sha1, FieldTrustUsed: sha1}
	tr, err := ApplyAdvance(it, Advance{Outcome: PROpened, Fields: pr, Evidence: 1, ChangedPaths: []string{"M tools/pipeline"}}, t0)
	wantCode(t, err, "")
	used := tr.Item
	used.Stage = Build

	// A review fix at sha2 changed only other files; the runner carried the OK.
	carry := map[string]string{"repo": "alexberardi/jarvis-server", "pr": "9", "branch": "b", "head_sha": sha2,
		FieldTrustUsed: sha2, FieldTrustCarried: sha2, FieldTrustDiffSHA: digest}
	tr, err = ApplyAdvance(used, Advance{Outcome: PROpened, Fields: carry, Evidence: 1, ChangedPaths: []string{"M tools/pipeline", "M x.go"}}, t0)
	wantCode(t, err, "")
	got := tr.Item.Trust
	if got.HeadSHA != sha2 || got.CarriedFrom != sha1 || got.By != "user:alex" || !got.Used || got.DiffSHA256 != digest {
		t.Fatalf("carried trust %+v", got)
	}
	if len(tr.Also) != 1 || tr.Also[0].Kind != KindTrustCarried || tr.Also[0].Data["from"] != sha1 ||
		tr.Also[0].Data["to"] != sha2 || tr.Also[0].Data["diff_sha256"] != digest {
		t.Fatalf("trust_carried note %+v", tr.Also)
	}
	if tr.Item.Risk != RiskHold {
		t.Fatal("the carried OK keeps the PR held")
	}

	// A run without a verdict rebinds the OK but leaves it unused, for the new head only.
	tr, err = ApplyAdvance(used, Advance{Outcome: Failed, Fields: map[string]string{"error": "fault",
		FieldTrustUsed: sha2, FieldTrustCarried: sha2, FieldTrustDiffSHA: digest}}, t0)
	wantCode(t, err, "")
	if !TrustAllows(tr.Item.Trust, sha2) || TrustAllows(tr.Item.Trust, sha1) {
		t.Fatalf("unused carried trust %+v", tr.Item.Trust)
	}
}

func TestTrustCarryChecked(t *testing.T) {
	it := refusedWithDigest(t)
	ok := func() map[string]string {
		return map[string]string{"error": "fault", FieldTrustUsed: sha2, FieldTrustCarried: sha2, FieldTrustDiffSHA: digest}
	}
	adv := func(it Item, from Stage, f map[string]string) error {
		it.Stage = from
		_, err := ApplyAdvance(it, Advance{Outcome: Failed, Fields: f}, t0)
		return err
	}
	wantCode(t, adv(it, Build, ok()), "")

	// Another diff: one byte differs, so the digest does.
	f := ok()
	f[FieldTrustDiffSHA] = strings.Repeat("ab", 31) + "ac"
	wantCode(t, adv(it, Build, f), CodeInvalidField)
	// No digest sent.
	f = ok()
	delete(f, FieldTrustDiffSHA)
	wantCode(t, adv(it, Build, f), CodeInvalidField)
	// The carry must name the head verify ran.
	f = ok()
	f[FieldTrustUsed] = sha1
	wantCode(t, adv(it, Build, f), CodeInvalidField)
	// Not from build.
	wantCode(t, adv(it, Review, ok()), CodeInvalidField)
	// No OK given yet: nothing to carry.
	notGiven := it
	notGiven.Trust = &Trust{HeadSHA: sha1, Paths: []string{"tools/pipeline"}, DiffSHA256: digest}
	wantCode(t, adv(notGiven, Build, ok()), CodeInvalidField)
	// An OK recorded before digests (none to compare with).
	old := it
	o := *it.Trust
	o.DiffSHA256 = ""
	old.Trust = &o
	wantCode(t, adv(old, Build, ok()), CodeInvalidField)
	// No trust at all.
	none := it
	none.Trust = nil
	wantCode(t, adv(none, Build, ok()), CodeInvalidField)
	// A refusal's digest must be a sha256.
	_, err := ApplyAdvance(item(Build), Advance{Outcome: Failed, Fields: map[string]string{
		"error": "x", FieldTrustSHA: sha1, FieldTrustPaths: "p", FieldTrustDiffSHA: "nope"}}, t0)
	wantCode(t, err, CodeInvalidField)
}
