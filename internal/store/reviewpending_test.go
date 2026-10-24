package store

import (
	"context"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A land run that reports review_pending gives back its lease: the item stays in land with the
// waiting head, is not claimed before its not-before time, then is, and that claim is marked (Slack
// posts nothing for it); the flow panel names the wait.
func TestReviewPendingRequeuesLand(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	a := add(t, s, "needs a github review", "P2")
	step := func(agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) {
		t.Helper()
		c := claim(t, s, agent)
		if c == nil || c.Item.ID != a.ID {
			t.Fatalf("claim for %s: %+v", out, c)
		}
		if ev {
			evidence(t, s, a.ID)
		}
		if _, err := s.Advance(ctx, runner, a.ID, out, f, paths); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	step("triager", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil, true)
	step("builder", pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, []string{"M x.go"}, true)
	step("reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)
	since := clk.Now()
	step("lander", pipeline.ReviewPending, map[string]string{"review": "REVIEW_REQUIRED", "head_sha": shaA}, nil, false)

	d, err := s.GetItem(ctx, a.ID)
	if err != nil || d.Item.Stage != pipeline.Land || d.Item.ReviewPendingSHA != shaA || !d.Item.ReviewPendingSince.Equal(since) ||
		!d.Item.NotBefore.Equal(clk.Now().Add(pipeline.ReviewRecheckAfter)) {
		t.Fatalf("after review_pending: %+v %v", d.Item, err)
	}
	fl, err := s.Flow(ctx, "24h")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range fl.Waiting {
		found = found || w.Reason == pipeline.WaitReviewPending
	}
	if !found {
		t.Fatalf("flow waits %+v, want %q", fl.Waiting, pipeline.WaitReviewPending)
	}
	if c := claim(t, s, "lander"); c != nil {
		t.Fatalf("claimed before its not-before time: %+v", c.Item)
	}
	clk.Advance(pipeline.ReviewRecheckAfter)
	c := claim(t, s, "lander")
	if c == nil || c.Item.ID != a.ID || c.Item.ReviewPendingSHA != shaA {
		t.Fatalf("claim once due: %+v", c)
	}
	d, err = s.GetItem(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := d.Events[len(d.Events)-1]
	if last.Kind != "claimed" || last.Data["review_pending"] != true {
		t.Fatalf("claimed event: %+v", last)
	}
}
