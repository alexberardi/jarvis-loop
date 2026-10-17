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
