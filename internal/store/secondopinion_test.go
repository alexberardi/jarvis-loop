package store

import (
	"context"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The board's numbers add up the runner's second_opinion events, per item and in total.
func TestSecondOpinions(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "P2")
	b, err := s.AddItem(ctx, user, NewItem{Title: "b", Repo: "jarvis-node-mobile", Priority: "P2"})
	if err != nil {
		t.Fatal(err)
	}
	two := 2
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &two, Limits: map[string]int{"triage": 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "x"}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "y"}); err != nil || c == nil {
		t.Fatal(c, err)
	}
	for _, e := range []struct {
		id   int64
		data map[string]any
	}{
		{a.ID, map[string]any{"status": "ok", "findings": 3, "judged": 3, "agreed": 2, "high": 1, "medium": 1, "low": 1, "agreed_high": 1, "agreed_low": 1, "bounced": true}},
		{a.ID, map[string]any{"status": "unavailable", "reason": "quota", "findings": 0, "judged": 0, "agreed": 0, "bounced": false}},
		{b.ID, map[string]any{"status": "unjudged", "findings": 2, "judged": 0, "agreed": 0, "medium": 2, "bounced": false}},
	} {
		if _, err := s.RunnerEvent(ctx, runner, e.id, KindSecondOpinion, e.data); err != nil {
			t.Fatal(err)
		}
	}
	per, total, err := s.SecondOpinions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := SecondOpinionStats{Runs: 2, Unavailable: 1, Findings: 3, Judged: 3, Agreed: 2, High: 1, Medium: 1, Low: 1, AgreedHigh: 1, AgreedLow: 1, Bounced: 1}
	if per[a.ID] != want || per[a.ID].AgreedPct() != 67 {
		t.Fatalf("a: %+v", per[a.ID])
	}
	if b := per[b.ID]; b.Findings != 2 || b.Medium != 2 || b.Judged != 0 || b.AgreedPct() != 0 {
		t.Fatalf("b: %+v", b)
	}
	if total.Runs != 3 || total.Findings != 5 || total.Judged != 3 || total.Agreed != 2 || total.Medium != 3 || total.Unavailable != 1 {
		t.Fatalf("total: %+v", total)
	}
}
