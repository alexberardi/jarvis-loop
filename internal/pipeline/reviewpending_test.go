package pipeline

import (
	"testing"
	"time"
)

func reviewPending(head string) Advance {
	return Advance{Outcome: ReviewPending, Agent: "lander", Fields: map[string]string{
		"review": "REVIEW_REQUIRED", "head_sha": head}}
}

// review_pending keeps the item in land, not a failure: no needs_you, no bounce, claims skip it
// until ReviewRecheckAfter, and only the first one on a head is marked first (it posts the PR
// link once); there is no give-up, the user may take hours.
func TestReviewPendingStaysInLand(t *testing.T) {
	it := item(Land)
	tr, err := ApplyAdvance(it, reviewPending(sha1), t0)
	wantCode(t, err, "")
	if tr.From != Land || tr.To != Land || tr.Item.Stage != Land || tr.Kind != "advanced" {
		t.Fatalf("review_pending: %s → %s (%s), kind %s", tr.From, tr.To, tr.Item.Stage, tr.Kind)
	}
	if tr.Item.BounceCount != 0 || tr.Item.Question != "" || IsRunFailure(tr.Kind, tr.Outcome) {
		t.Fatalf("review_pending counted as a failure: %+v", tr.Item)
	}
	if !tr.Item.NotBefore.Equal(t0.Add(ReviewRecheckAfter)) || tr.Item.ReviewPendingSHA != sha1 || !tr.Item.ReviewPendingSince.Equal(t0) {
		t.Fatalf("not before %v, sha %q, since %v", tr.Item.NotBefore, tr.Item.ReviewPendingSHA, tr.Item.ReviewPendingSince)
	}
	if !tr.Item.Deferred(t0.Add(time.Minute)) {
		t.Fatalf("not deferred")
	}
	if tr.Data["first"] != true || tr.Data["not_before"] != t0.Add(ReviewRecheckAfter).Format(time.RFC3339) || tr.Data["since"] != t0.Format(time.RFC3339) {
		t.Fatalf("event data %+v", tr.Data)
	}

	// The next one on the same head is not the first, and keeps the wait's start, hours later.
	again, err := ApplyAdvance(tr.Item, reviewPending(sha1), t0.Add(5*time.Hour))
	wantCode(t, err, "")
	if again.Data["first"] == true || !again.Item.ReviewPendingSince.Equal(t0) || again.Item.Stage != Land {
		t.Fatalf("same head: first %v, since %v, stage %s", again.Data["first"], again.Item.ReviewPendingSince, again.Item.Stage)
	}
	// A new head is a new wait.
	moved := again.Item
	moved.HeadSHA = sha2
	fresh, err := ApplyAdvance(moved, reviewPending(sha2), t0.Add(6*time.Hour))
	wantCode(t, err, "")
	if fresh.Data["first"] != true || fresh.Item.ReviewPendingSHA != sha2 || !fresh.Item.ReviewPendingSince.Equal(t0.Add(6*time.Hour)) {
		t.Fatalf("new head: first %v, sha %q, since %v", fresh.Data["first"], fresh.Item.ReviewPendingSHA, fresh.Item.ReviewPendingSince)
	}

	// Any other outcome ends the wait.
	failed, err := ApplyAdvance(tr.Item, Advance{Outcome: Failed, Fields: map[string]string{"error": "x"}}, t0.Add(time.Hour))
	wantCode(t, err, "")
	if !failed.Item.NotBefore.IsZero() || failed.Item.ReviewPendingSHA != "" || !failed.Item.ReviewPendingSince.IsZero() {
		t.Fatalf("failed kept the wait: %+v", failed.Item)
	}
}

func TestReviewPendingRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		from Stage
		drop string
		set  [2]string
		code string
	}{
		{"from build", Build, "", [2]string{}, CodeWrongStage},
		{"from review", Review, "", [2]string{}, CodeWrongStage},
		{"no review", Land, "review", [2]string{}, CodeMissingField},
		{"no head", Land, "head_sha", [2]string{}, CodeMissingField},
		{"bad head", Land, "", [2]string{"head_sha", "HEAD"}, CodeInvalidField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := reviewPending(sha1)
			delete(a.Fields, tc.drop)
			if tc.set[0] != "" {
				a.Fields[tc.set[0]] = tc.set[1]
			}
			_, err := ApplyAdvance(item(tc.from), a, t0)
			wantCode(t, err, tc.code)
		})
	}
}
