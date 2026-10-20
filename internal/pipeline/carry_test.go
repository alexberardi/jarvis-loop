package pipeline

import (
	"strings"
	"testing"
)

// A held item the user approved at sha1 whose PR then took a merge of main (land's or a
// build's): review approved the new head sha2 with the runner's delta.
func carryItem() Item {
	it := item(Review)
	it.Repo, it.Risk, it.HoldReason, it.HeadSHA = "jarvis-server", RiskHold, "land: on the hold list", sha2
	it.Approval = &Approval{By: "alex", Words: "ship it", HeadSHA: sha1, At: t0}
	return it
}

func carryReview(extra map[string]string) Advance {
	f := map[string]string{"summary": "ok", "head_sha": sha2, "carry_from": sha1, "carry_merges": shaM}
	for k, v := range extra {
		f[k] = v
	}
	return Advance{Outcome: Approved, Agent: "reviewer", Fields: f, HoldRules: DefaultHoldRules()}
}

func noteOf(tr Transition, kind string) *Note {
	for i := range tr.Also {
		if tr.Also[i].Kind == kind {
			return &tr.Also[i]
		}
	}
	return nil
}

func TestApprovalCarried(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"clean merge":                     nil,
		"conflict outside the hold rules": {"carry_conflicts": "docs/x.md\ninternal/modules/chat/x.go"},
	} {
		t.Run(name, func(t *testing.T) {
			tr, err := ApplyAdvance(carryItem(), carryReview(extra), t0)
			wantCode(t, err, "")
			a := tr.Item.Approval
			if tr.To != Land || a == nil || a.HeadSHA != sha2 || a.CarriedFrom != sha1 || a.By != "alex" || a.Words != "ship it" {
				t.Fatalf("not carried: %+v %+v", tr.Item, a)
			}
			n := noteOf(tr, KindApprovalCarried)
			if n == nil || n.Data["from_sha"] != sha1 || n.Data["to_sha"] != sha2 || n.Data["by"] != "alex" {
				t.Fatalf("note %+v", tr.Also)
			}
			// The merge gate takes the carried approval for the new head, and only for it.
			if _, err := ApplyAdvance(tr.Item, merged(sha2, []string{"M internal/modules/chat/x.go"}), t0); err != nil {
				t.Fatalf("merge of the new head refused: %v", err)
			}
			_, err = ApplyAdvance(tr.Item, merged(sha1, []string{"M internal/modules/chat/x.go"}), t0)
			wantCode(t, err, CodeApprovalRequired)
		})
	}
}

func TestApprovalNotCarried(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra map[string]string
		why   string
	}{
		{"own lines changed", map[string]string{"carry_changed": "internal/modules/chat/x.go"}, "own lines changed"},
		{"hold-path conflict", map[string]string{"carry_conflicts": "internal/modules/auth/token.go"}, "auth: M internal/modules/auth/token.go"},
		{"not only merges", map[string]string{"carry_problem": "commits on the branch since the approval: abc1234"}, "abc1234"},
		{"needs review", map[string]string{"needs_review": "true"}, "needs review"},
		{"no delta", map[string]string{"carry_from": ""}, "no delta"},
		{"delta of another commit", map[string]string{"carry_from": shaM}, "not the approved"},
		{"no merges", map[string]string{"carry_merges": ""}, "no merge"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, err := ApplyAdvance(carryItem(), carryReview(c.extra), t0)
			wantCode(t, err, "")
			if tr.To != Land || tr.Item.Approval != nil {
				t.Fatalf("carried: %+v", tr.Item.Approval)
			}
			n := noteOf(tr, KindApprovalNotCarried)
			if n == nil || n.Data["from_sha"] != sha1 || n.Data["to_sha"] != sha2 || !strings.Contains(n.Data["reason"].(string), c.why) {
				t.Fatalf("note %+v", tr.Also)
			}
			_, err = ApplyAdvance(tr.Item, merged(sha2, []string{"M internal/modules/chat/x.go"}), t0)
			wantCode(t, err, CodeApprovalRequired)
		})
	}
	t.Run("no approval to carry", func(t *testing.T) {
		it := carryItem()
		it.Approval = nil
		tr, err := ApplyAdvance(it, carryReview(nil), t0)
		wantCode(t, err, "")
		if tr.Item.Approval != nil || len(tr.Also) != 0 {
			t.Fatalf("%+v %+v", tr.Item.Approval, tr.Also)
		}
	})
}

// A new head keeps the old approval as the one review may carry; it does not let that head merge.
func TestNewHeadKeepsApprovalToCarry(t *testing.T) {
	it := item(Land)
	it.Risk = RiskHold
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, Advance{Outcome: BackToReview, Fields: map[string]string{"notes": "merged main", "head_sha": sha2},
		ChangedPaths: clean, HoldRules: DefaultHoldRules()}, t0)
	wantCode(t, err, "")
	if a := tr.Item.Approval; a == nil || a.HeadSHA != sha1 || tr.Data["approval_voided"] != true {
		t.Fatalf("approval %+v data %+v", a, tr.Data)
	}
	t.Run("the runner's conflict rebuild keeps it", func(t *testing.T) {
		it := item(Held)
		it.Approval = &Approval{By: "alex", HeadSHA: shaM}
		tr, err := ApplyRebuild(it, "PR #12 conflicts with main", true)
		wantCode(t, err, "")
		if tr.Item.Approval == nil || tr.Data["approval_voided"] != nil {
			t.Fatalf("%+v %+v", tr.Item.Approval, tr.Data)
		}
	})
	t.Run("the user's rebuild drops it", func(t *testing.T) {
		it := item(Held)
		it.Approval = &Approval{By: "alex", HeadSHA: shaM}
		tr, err := ApplyRebuild(it, "change the wording", false)
		wantCode(t, err, "")
		if tr.Item.Approval != nil || tr.Data["approval_voided"] != true {
			t.Fatalf("%+v %+v", tr.Item.Approval, tr.Data)
		}
	})
	t.Run("the user's hold drops it", func(t *testing.T) {
		it := item(Held)
		it.Approval = &Approval{By: "alex", HeadSHA: shaM}
		tr, err := ApplyHold(it, "not yet")
		wantCode(t, err, "")
		if tr.Item.Approval != nil {
			t.Fatalf("%+v", tr.Item.Approval)
		}
	})
}

// The runner's merge of main into a held PR: held → review with the new head.
func TestHeldMerge(t *testing.T) {
	it := item(Held)
	it.PR, it.Risk = 12, RiskHold
	tr, err := ApplyHeldMerge(it, sha2, "merged main in")
	wantCode(t, err, "")
	if tr.From != Held || tr.To != Review || tr.Kind != KindHeldMerge || tr.Item.HeadSHA != sha2 || tr.Data["from_sha"] != sha1 || tr.Data["head_sha"] != sha2 {
		t.Fatalf("%+v", tr)
	}
	_, err = ApplyHeldMerge(item(Review), sha2, "n")
	wantCode(t, err, CodeWrongStage)
	_, err = ApplyHeldMerge(it, sha1, "n")
	wantCode(t, err, CodeSHAMismatch)
	_, err = ApplyHeldMerge(it, "nope", "n")
	wantCode(t, err, CodeInvalidField)
}
