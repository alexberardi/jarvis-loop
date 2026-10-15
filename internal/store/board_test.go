package store

import (
	"context"
	"testing"
	"time"
)

func TestBoardItems(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	a := add(t, s, "needs an answer", "P1")
	b := add(t, s, "held", "P2")
	clk.Advance(time.Hour)
	if err := s.SetSlackThread(ctx, a.ID, "C123", "1760000000.000100"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE items SET stage='needs_you', question='which repo?', updated_at=? WHERE id=?`, clk.Now().UnixNano(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE items SET stage='held', hold_reason='touches auth', pr=7, repo='o/r' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.BoardItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	ga, gb := got[0], got[1]
	if ga.ID != a.ID || ga.Display != "needs-you" || ga.Question != "which repo?" || ga.SlackChannel != "C123" ||
		ga.SlackTS != "1760000000.000100" || !ga.UpdatedAt.Equal(clk.Now()) || ga.AgeSeconds != 3600 {
		t.Errorf("needs-you row: %+v", ga)
	}
	if gb.ID != b.ID || gb.Display != "held" || gb.HoldReason != "touches auth" || gb.PR != 7 || gb.Repo != "o/r" || gb.SlackChannel != "" {
		t.Errorf("held row: %+v", gb)
	}
}
