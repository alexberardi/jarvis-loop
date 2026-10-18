package store

import (
	"context"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A runner says it is draining (loop-runner drain); its next claim, after the restart, says it
// works again. The orchestrator's snapshot shows the state too.
func TestRunnerDrainingUntilItClaims(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.SetRunnerState(ctx, "devbox", "asleep"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("an unknown state: %v", err)
	}
	if err := s.SetRunnerState(ctx, "devbox", RunnerDraining); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunnerStates(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "devbox" || got[0].State != RunnerDraining || !got[0].Since.Equal(t0) {
		t.Fatalf("states %+v %v", got, err)
	}
	snap, err := s.OrchSnapshot(ctx)
	if err != nil || len(snap.Runners) != 1 || snap.Runners[0].State != RunnerDraining {
		t.Fatalf("snapshot runners %+v %v", snap.Runners, err)
	}
	if _, err := s.Claim(ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "devbox-1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RunnerStates(ctx); err != nil || len(got) != 0 {
		t.Fatalf("still %+v after a claim (%v)", got, err)
	}
}
