package store

import (
	"context"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A land run that reports ci_pending gives back its lease: the item stays in land, is not claimed
// before its not-before time, then is, with the first wait's start on the claim's item.
func TestCIPendingRequeuesLand(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	a := add(t, s, "slow ci", "P2")
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
	clk.Advance(30 * time.Minute)
	step("lander", pipeline.CIPending, map[string]string{"wait": "CI (check)", "head_sha": shaA, "since": since.Format(time.RFC3339)}, nil, false)

	d, err := s.GetItem(ctx, a.ID)
	if err != nil || d.Item.Stage != pipeline.Land || d.Item.CIPendingSHA != shaA || !d.Item.CIPendingSince.Equal(since) ||
		!d.Item.NotBefore.Equal(clk.Now().Add(pipeline.LandRequeueAfter)) {
		t.Fatalf("after ci_pending: %+v %v", d.Item, err)
	}
	if c := claim(t, s, "lander"); c != nil {
		t.Fatalf("claimed before its not-before time: %+v", c.Item)
	}
	clk.Advance(pipeline.LandRequeueAfter)
	c := claim(t, s, "lander")
	if c == nil || c.Item.ID != a.ID || c.Item.Stage != pipeline.Land || !c.Item.CIPendingSince.Equal(since) {
		t.Fatalf("claim once due: %+v", c)
	}
	d, err = s.GetItem(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := d.Events[len(d.Events)-1]
	if last.Kind != "claimed" || last.Data["ci_pending"] != true {
		t.Fatalf("claimed event: %+v", last)
	}
}
