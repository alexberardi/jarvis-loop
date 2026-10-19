package pipeline

import (
	"testing"
	"time"
)

func ciPending(head string, since time.Time) Advance {
	return Advance{Outcome: CIPending, Agent: "lander", Fields: map[string]string{
		"wait": "CI (check)", "head_sha": head, "since": since.UTC().Format(time.RFC3339)}}
}

// ci_pending keeps the item in land, not a failure: no needs_you, no bounce, and claims skip it
// until LandRequeueAfter has passed.
func TestCIPendingStaysInLand(t *testing.T) {
	it := item(Land)
	since := t0.Add(-30 * time.Minute)
	tr, err := ApplyAdvance(it, ciPending(sha1, since), t0)
	wantCode(t, err, "")
	if tr.From != Land || tr.To != Land || tr.Item.Stage != Land || tr.Kind != "advanced" {
		t.Fatalf("ci_pending: %s → %s (%s), kind %s", tr.From, tr.To, tr.Item.Stage, tr.Kind)
	}
	if tr.Item.BounceCount != 0 || tr.Item.Question != "" || IsRunFailure(tr.Kind, tr.Outcome) {
		t.Fatalf("ci_pending counted as a failure: %+v", tr.Item)
	}
	if !tr.Item.NotBefore.Equal(t0.Add(LandRequeueAfter)) || !tr.Item.CIPendingSince.Equal(since) || tr.Item.CIPendingSHA != sha1 {
		t.Fatalf("not before %v, since %v, sha %q", tr.Item.NotBefore, tr.Item.CIPendingSince, tr.Item.CIPendingSHA)
	}
	if tr.Data["not_before"] != t0.Add(LandRequeueAfter).Format(time.RFC3339) {
		t.Fatalf("event data %+v", tr.Data)
	}

	// The next ci_pending on the same head keeps the first wait's start.
	again, err := ApplyAdvance(tr.Item, ciPending(sha1, t0.Add(10*time.Minute)), t0.Add(40*time.Minute))
	wantCode(t, err, "")
	if !again.Item.CIPendingSince.Equal(since) {
		t.Fatalf("same head: since %v, want %v", again.Item.CIPendingSince, since)
	}
	// A new head starts the clock again.
	moved := again.Item
	moved.HeadSHA = sha2
	fresh, err := ApplyAdvance(moved, ciPending(sha2, t0.Add(50*time.Minute)), t0.Add(80*time.Minute))
	wantCode(t, err, "")
	if !fresh.Item.CIPendingSince.Equal(t0.Add(50*time.Minute)) || fresh.Item.CIPendingSHA != sha2 {
		t.Fatalf("new head: since %v sha %q", fresh.Item.CIPendingSince, fresh.Item.CIPendingSHA)
	}

	// Any other outcome ends the wait: a merge, or land giving up.
	failed, err := ApplyAdvance(tr.Item, Advance{Outcome: Failed, Fields: map[string]string{"error": "CI (check) still pending after 3h"}}, t0.Add(3*time.Hour))
	wantCode(t, err, "")
	if !failed.Item.NotBefore.IsZero() || !failed.Item.CIPendingSince.IsZero() || failed.Item.CIPendingSHA != "" {
		t.Fatalf("failed kept the wait: %+v", failed.Item)
	}
}

func TestCIPendingRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		from Stage
		drop string
		set  [2]string
		code string
	}{
		{"from build", Build, "", [2]string{}, CodeWrongStage},
		{"from review", Review, "", [2]string{}, CodeWrongStage},
		{"no wait", Land, "wait", [2]string{}, CodeMissingField},
		{"no head", Land, "head_sha", [2]string{}, CodeMissingField},
		{"bad head", Land, "", [2]string{"head_sha", "HEAD"}, CodeInvalidField},
		{"no since", Land, "since", [2]string{}, CodeMissingField},
		{"bad since", Land, "", [2]string{"since", "an hour ago"}, CodeInvalidField},
		{"since in the future", Land, "", [2]string{"since", t0.Add(time.Hour).Format(time.RFC3339)}, CodeInvalidField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := ciPending(sha1, t0.Add(-time.Hour))
			delete(a.Fields, tc.drop)
			if tc.set[0] != "" {
				a.Fields[tc.set[0]] = tc.set[1]
			}
			_, err := ApplyAdvance(item(tc.from), a, t0)
			wantCode(t, err, tc.code)
		})
	}
}

// A land item waiting out its ci_pending is not claimed until its not-before time; then it is.
func TestClaimSkipsDeferredLand(t *testing.T) {
	c := DefaultControl()
	it := item(Land)
	it.NotBefore = t0.Add(LandRequeueAfter)
	req := ClaimRequest{Agent: "lander", Capabilities: nil}
	if got, ok := Choose(c, Ready([]Item{it}, t0.Add(time.Minute)), nil, nil, req, nil); ok {
		t.Fatalf("claimed a deferred item: #%d", got.ID)
	}
	if !it.Deferred(t0.Add(time.Minute)) || it.Deferred(t0.Add(LandRequeueAfter)) {
		t.Fatalf("Deferred wrong around %v", it.NotBefore)
	}
	got, ok := Choose(c, Ready([]Item{it}, t0.Add(LandRequeueAfter)), nil, nil, req, nil)
	if !ok || got.ID != it.ID {
		t.Fatalf("not claimed once due: %v %v", got.ID, ok)
	}
	// Only land defers: a stale not-before on an item that went on to build is ignored.
	b := item(Build)
	b.NotBefore = t0.Add(time.Hour)
	if b.Deferred(t0) {
		t.Fatalf("a build item deferred")
	}
}
