package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #plan in Slack (docs/phases/07-planning.md, "Slack"): a plan found by its thread, and each turn
// knows the Slack messages it carries, so the bot can react to them.
func TestPlanSlackThreadAndMessages(t *testing.T) {
	s, advance := openPlans(t)
	ctx := context.Background()
	p, err := s.CreatePlan(ctx, user, NewPlan{Title: "Pantry", Text: "a pantry list", SlackChannel: "C1", SlackTS: "100.1"})
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.PlanByThread(ctx, "C1", "100.1"); err != nil || id != p.ID {
		t.Fatalf("PlanByThread = %d, %v", id, err)
	}
	for _, q := range [][2]string{{"C1", "999.9"}, {"C2", "100.1"}, {"C1", ""}} {
		if id, err := s.PlanByThread(ctx, q[0], q[1]); err != nil || id != 0 {
			t.Fatalf("PlanByThread%v = %d, %v", q, id, err)
		}
	}
	// The first turn carries the top-level message; replies join the queued turn with their ts.
	if _, err := s.PlanSlackMessage(ctx, user, p.ID, "for the kitchen", "100.2"); err != nil {
		t.Fatal(err)
	}
	c := claimTurn(t, s, "agent-1")
	if !slices.Equal(c.Turn.SlackTS, []string{"100.1", "100.2"}) {
		t.Fatalf("first turn's messages %v", c.Turn.SlackTS)
	}
	// One sent while it runs goes to the next turn; a message from the API has no ts.
	if _, err := s.PlanSlackMessage(ctx, user, p.ID, "and the fridge", "100.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanMessage(ctx, user, p.ID, "from the CLI"); err != nil {
		t.Fatal(err)
	}
	// The running turn's lease expires: its messages join the queued turn, ts and all.
	advance(LeaseTTL + time.Second)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	c = claimTurn(t, s, "agent-2")
	if !slices.Equal(c.Turn.SlackTS, []string{"100.1", "100.2", "100.3"}) || len(c.Turn.Messages) != 4 {
		t.Fatalf("merged turn %v %v", c.Turn.SlackTS, c.Turn.Messages)
	}
	d, err := s.GetPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Turns[0].SlackTS; !slices.Equal(got, []string{"100.1", "100.2"}) {
		t.Fatalf("the failed turn keeps its own messages: %v", got)
	}

	// The bot's header message in the thread, and a thread for a plan started without one.
	if err := s.SetPlanSlack(ctx, p.ID, "C1", "100.1", "100.5"); err != nil {
		t.Fatal(err)
	}
	q := newPlan(t, s, "From the CLI", "")
	if err := s.SetPlanSlack(ctx, q.ID, "C1", "200.1", "200.1"); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		id            int64
		ts, header    string
		threadFindsIt string
	}{{p.ID, "100.1", "100.5", "100.1"}, {q.ID, "200.1", "200.1", "200.1"}} {
		d, _ := s.GetPlan(ctx, x.id)
		if d.Plan.SlackChannel != "C1" || d.Plan.SlackTS != x.ts || d.Plan.SlackHeader != x.header {
			t.Fatalf("plan P%d slack %q %q %q", x.id, d.Plan.SlackChannel, d.Plan.SlackTS, d.Plan.SlackHeader)
		}
		if id, _ := s.PlanByThread(ctx, "C1", x.threadFindsIt); id != x.id {
			t.Fatalf("PlanByThread(%s) = %d, want %d", x.threadFindsIt, id, x.id)
		}
	}
	if err := s.SetPlanSlack(ctx, 99, "C1", "1", "1"); code(err) != CodeNotFound {
		t.Fatalf("unknown plan: %v", err)
	}
	// Setting Slack state writes no event and leaves the plan's stage alone.
	d2, _ := s.GetPlan(ctx, q.ID)
	if d2.Plan.Stage != pipeline.PlanBrief || len(d2.Events) != 2 {
		t.Fatalf("after SetPlanSlack: %s, %d events", d2.Plan.Stage, len(d2.Events))
	}
}
