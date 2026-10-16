package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const planRepo = "alexberardi/jarvis-loop"

const blueprint3 = `# Blueprint

## Pieces

### 1. Tables
- repo: alexberardi/jarvis-loop
- depends on: none

The migration.

### 2. Store
- depends on: 1

The store.

### 3. API
- depends on: 1

The routes.
`

const blueprintCycle = "## Pieces\n### 1. A\n- depends on: 2\n### 2. B\n- depends on: 1\n"

func openPlans(t *testing.T) (*Store, func(time.Duration)) {
	t.Helper()
	s, clk := open(t)
	s.SetRepos([]string{planRepo, "alexberardi/jarvis-server"})
	return s, func(d time.Duration) { clk.Advance(d) }
}

var planKinds = []string{pipeline.WorkItem, pipeline.WorkPlan}

func claimTurn(t *testing.T, s *Store, agent string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: agent, Kinds: planKinds})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newPlan(t *testing.T, s *Store, title, pri string) pipeline.Plan {
	t.Helper()
	p, err := s.CreatePlan(context.Background(), user, NewPlan{Title: title, Priority: pri, Text: title + ", please", Repos: []string{planRepo}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func result(t *testing.T, s *Store, c *Claim, r pipeline.TurnResult) pipeline.PlanTurn {
	t.Helper()
	_, turn, err := s.PlanTurnResult(context.Background(), runner, c.Plan.ID, c.Turn.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func latestSHA(t *testing.T, s *Store, id int64, kind string, v int) string {
	t.Helper()
	d, err := s.GetPlanDoc(context.Background(), id, kind, v)
	if err != nil {
		t.Fatal(err)
	}
	return d.SHA256
}

func TestPlanConversationAndHandoff(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Pantry list", "P2")
	if p.Stage != pipeline.PlanBrief || p.Priority != "P2" || !slices.Equal(p.Repos, []string{planRepo}) {
		t.Fatalf("plan %+v", p)
	}
	// A message before the first turn is claimed joins it.
	if _, err := s.PlanMessage(ctx, user, p.ID, "for the kitchen"); err != nil {
		t.Fatal(err)
	}
	// A runner that does not take plans never gets a turn.
	if c := claim(t, s, "old-runner"); c != nil {
		t.Fatalf("an item-only runner got %+v", c)
	}
	c := claimTurn(t, s, "agent-1")
	if c == nil || c.Kind != pipeline.WorkPlan || c.Turn.Persona != pipeline.PersonaProduct || c.Turn.Status != pipeline.TurnRunning ||
		!slices.Equal(c.Turn.Messages, []string{"Pantry list, please", "for the kitchen"}) || c.Lease.ID == "" {
		t.Fatalf("claim %+v %+v", c, c.Turn)
	}
	// While it runs, a message queues the next turn, which waits for this one.
	if _, err := s.PlanMessage(ctx, user, p.ID, "and the fridge"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanMessage(ctx, user, p.ID, "and the freezer"); err != nil {
		t.Fatal(err)
	}
	if c2 := claimTurn(t, s, "agent-2"); c2 != nil {
		t.Fatalf("a second turn of the same plan ran: %+v", c2.Turn)
	}
	turn := result(t, s, c, pipeline.TurnResult{Reply: "Here's a brief.", Brief: "brief v1", Ready: true, SessionID: "sess-p"})
	if turn.Status != pipeline.TurnDone || !turn.Ready || turn.Reply != "Here's a brief." {
		t.Fatalf("turn %+v", turn)
	}
	c = claimTurn(t, s, "agent-1")
	if c == nil || !slices.Equal(c.Turn.Messages, []string{"and the fridge", "and the freezer"}) || c.Plan.ProductSession != "sess-p" {
		t.Fatalf("second turn %+v", c)
	}
	result(t, s, c, pipeline.TurnResult{Reply: "Updated.", Brief: "brief v2", Ready: true})

	// The button for v1 is stale: refused, naming v2, and recorded.
	v1 := latestSHA(t, s, p.ID, pipeline.DocBrief, 1)
	_, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBrief, v1, "ok")
	if code(err) != pipeline.CodeSHAMismatch || !strings.Contains(err.Error(), "the brief changed; approve v2 instead") {
		t.Fatalf("stale approve: %v", err)
	}
	d, err := s.GetPlan(ctx, p.ID)
	if err != nil || d.Plan.Stage != pipeline.PlanBrief || !hasEvent(d.Events, "refused") {
		t.Fatalf("after the stale approve: %+v %v", d.Plan, err)
	}
	a, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBrief, latestSHA(t, s, p.ID, pipeline.DocBrief, 0), "ok")
	if err != nil || a.Plan.Stage != pipeline.PlanBlueprint || a.Plan.BriefApproved != 2 || len(a.Items) != 0 {
		t.Fatalf("approve: %+v %v", a, err)
	}

	// Approving the brief queues engineering's first turn.
	c = claimTurn(t, s, "agent-1")
	if c == nil || c.Turn.Persona != pipeline.PersonaEngineering || len(c.Turn.Messages) != 0 {
		t.Fatalf("engineering turn %+v", c)
	}
	result(t, s, c, pipeline.TurnResult{Reply: "Blueprint ready.", Blueprint: blueprint3, Ready: true, SessionID: "sess-e"})

	add(t, s, "unrelated", "P3")
	a, err = s.ApprovePlan(ctx, user, p.ID, pipeline.DocBlueprint, latestSHA(t, s, p.ID, pipeline.DocBlueprint, 0), "go")
	if err != nil || a.Plan.Stage != pipeline.PlanSplit || a.Plan.BlueprintApproved != 1 || len(a.Items) != 3 {
		t.Fatalf("approve blueprint: %+v %v", a, err)
	}
	ids := []int64{a.Items[0].ID, a.Items[1].ID, a.Items[2].ID}
	for i, it := range a.Items {
		d, err := s.GetItem(ctx, it.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := d.Item
		if got.Source != pipeline.SourcePlan || got.PlanID != p.ID || got.PlanPiece != i+1 || got.Priority != "P2" || got.Repo != planRepo ||
			!strings.HasSuffix(got.Ask, "The "+[]string{"migration.", "store.", "routes."}[i]) {
			t.Fatalf("item %d: %+v", i+1, got)
		}
		want := []int64{}
		if i > 0 {
			want = []int64{ids[0]}
		}
		if !slices.Equal(got.DependsOn, want) {
			t.Fatalf("piece %d depends on %v, want %v", i+1, got.DependsOn, want)
		}
	}
	d, _ = s.GetPlan(ctx, p.ID)
	if len(d.Items) != 3 || !hasEvent(d.Events, "split") {
		t.Fatalf("plan detail %+v", d)
	}
	// Only piece 1 (and the unrelated item) is claimable; a split plan has no turns to run.
	if _, err := s.SetControl(ctx, user, ControlPatch{Limits: map[string]int{"triage": 4}}); err != nil {
		t.Fatal(err)
	}
	var got []int64
	for {
		c := claimTurn(t, s, "agent-x")
		if c == nil {
			break
		}
		if c.Item.ID == 0 {
			t.Fatalf("claimed a turn after the split: %+v", c.Turn)
		}
		got = append(got, c.Item.ID)
	}
	if !slices.Equal(got, []int64{ids[0], 1}) {
		t.Fatalf("claimed %v, want piece 1 (#%d, P2) then the unrelated #1", got, ids[0])
	}
	if _, err := s.PlanMessage(ctx, user, p.ID, "one more thing"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("message to a split plan: %v", err)
	}
}

func hasEvent(evs []Event, kind string) bool {
	return slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == kind })
}

func toBlueprint(t *testing.T, s *Store, p pipeline.Plan) {
	t.Helper()
	ctx := context.Background()
	c := claimTurn(t, s, "agent-1")
	result(t, s, c, pipeline.TurnResult{Reply: "brief", Brief: "the brief", Ready: true})
	if _, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBrief, latestSHA(t, s, p.ID, pipeline.DocBrief, 0), ""); err != nil {
		t.Fatal(err)
	}
}

func TestPlanBlueprintCycleFilesNothing(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Loops", "")
	toBlueprint(t, s, p)
	c := claimTurn(t, s, "agent-1")
	turn := result(t, s, c, pipeline.TurnResult{Reply: "Here it is.", Blueprint: blueprintCycle, Ready: true, SessionID: "sess-e"})
	if turn.Status != pipeline.TurnFailed || !strings.Contains(turn.Error, "cycle: 1 → 2 → 1") {
		t.Fatalf("turn %+v", turn)
	}
	d, err := s.GetPlan(ctx, p.ID)
	if err != nil || d.Plan.Stage != pipeline.PlanBlueprint || d.Plan.EngineeringSession != "sess-e" || len(d.Items) != 0 ||
		slices.ContainsFunc(d.Docs, func(x pipeline.PlanDoc) bool { return x.Kind == pipeline.DocBlueprint }) {
		t.Fatalf("after the failed turn: %+v docs %+v items %v", d.Plan, d.Docs, d.Items)
	}
	if _, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBlueprint, pipeline.DocSHA256(blueprintCycle), ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("approve with no blueprint: %v", err)
	}
	if rows, _ := s.ListItems(ctx, "", ""); len(rows) != 0 {
		t.Fatalf("items filed: %+v", rows)
	}
}

func TestPlanHandoffRefusalLeavesThePlan(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Two repos", "")
	toBlueprint(t, s, p)
	c := claimTurn(t, s, "agent-1")
	bp := strings.Replace(blueprint3, "### 2. Store\n", "### 2. Store\n- repo: alexberardi/jarvis-server\n", 1)
	result(t, s, c, pipeline.TurnResult{Reply: "ok", Blueprint: bp, Ready: true})
	// The configured repos change before the approval: the hand-off is refused as a whole.
	s.SetRepos([]string{planRepo})
	_, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBlueprint, pipeline.DocSHA256(bp), "")
	if code(err) != pipeline.CodeInvalidField || !strings.Contains(err.Error(), "piece 2") {
		t.Fatalf("approve: %v", err)
	}
	d, _ := s.GetPlan(ctx, p.ID)
	if d.Plan.Stage != pipeline.PlanBlueprint || len(d.Items) != 0 {
		t.Fatalf("plan %+v items %v", d.Plan, d.Items)
	}
	if rows, _ := s.ListItems(ctx, "", ""); len(rows) != 0 {
		t.Fatalf("items filed: %+v", rows)
	}
}

func TestPlanBackToBrief(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Big one", "")
	toBlueprint(t, s, p)
	c := claimTurn(t, s, "agent-1")
	result(t, s, c, pipeline.TurnResult{Reply: "This is far bigger.", BackToBrief: "needs a new service"})
	d, _ := s.GetPlan(ctx, p.ID)
	if d.Plan.Stage != pipeline.PlanBrief {
		t.Fatalf("stage %s", d.Plan.Stage)
	}
	c = claimTurn(t, s, "agent-1")
	if c == nil || c.Turn.Persona != pipeline.PersonaProduct || c.Turn.BackToBrief != "needs a new service" {
		t.Fatalf("product turn %+v", c)
	}
}

func TestPlanTurnLeases(t *testing.T) {
	s, advance := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Leases", "")
	c := claimTurn(t, s, "agent-1")
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, "thinking"); err != nil || stop {
		t.Fatalf("heartbeat: %v %v", stop, err)
	}
	if _, err := s.Heartbeat(ctx, "other", c.Lease.ID, ""); code(err) != CodeNotYourLease {
		t.Fatalf("other runner's heartbeat: %v", err)
	}
	// No heartbeat: the turn goes back to the queue, and the late result is refused.
	advance(LeaseTTL + time.Second)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetPlan(ctx, p.ID)
	if d.Turns[0].Status != pipeline.TurnQueued || !hasEvent(d.Events, "plan_turn_expired") {
		t.Fatalf("after expiry: %+v", d.Turns)
	}
	if _, _, err := s.PlanTurnResult(ctx, runner, p.ID, c.Turn.ID, pipeline.TurnResult{Reply: "late"}); code(err) != CodeNoLease {
		t.Fatalf("late result: %v", err)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat on an ended turn lease: %v %v", stop, err)
	}
	// A new message while it waits joins the requeued turn.
	if _, err := s.PlanMessage(ctx, user, p.ID, "still there?"); err != nil {
		t.Fatal(err)
	}
	c = claimTurn(t, s, "agent-2")
	if c == nil || c.Turn.ID != d.Turns[0].ID || len(c.Turn.Messages) != 2 {
		t.Fatalf("requeued turn %+v", c)
	}
	if _, _, err := s.PlanTurnResult(ctx, "other", p.ID, c.Turn.ID, pipeline.TurnResult{Reply: "x"}); code(err) != CodeNotYourLease {
		t.Fatalf("other runner's result: %v", err)
	}
	// Over the max run time: the turn fails (the user replies to try again).
	ctl, _ := s.Control(ctx)
	for i := 0; i < ctl.MaxRunMinutes/4+1; i++ {
		advance(4 * time.Minute)
		if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	d, _ = s.GetPlan(ctx, p.ID)
	if d.Turns[0].Status != pipeline.TurnFailed || !strings.Contains(d.Turns[0].Error, "max run time") {
		t.Fatalf("after overtime: %+v", d.Turns[0])
	}
	// Stop: the heartbeat stops the run and the turn waits again.
	if _, err := s.PlanMessage(ctx, user, p.ID, "again"); err != nil {
		t.Fatal(err)
	}
	c = claimTurn(t, s, "agent-1")
	stopped := true
	if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &stopped}); err != nil {
		t.Fatal(err)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat while stopped: %v %v", stop, err)
	}
	d, _ = s.GetPlan(ctx, p.ID)
	if last := d.Turns[len(d.Turns)-1]; last.Status != pipeline.TurnQueued {
		t.Fatalf("after stop: %+v", last)
	}
}

func TestPlanTurnsShareTheLimit(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	one := 1
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &one}); err != nil {
		t.Fatal(err)
	}
	add(t, s, "an item", "P2")
	newPlan(t, s, "a plan", "P2")
	c := claimTurn(t, s, "agent-1")
	if c == nil || c.Turn == nil {
		t.Fatalf("the waiting user should win at the same priority: %+v", c)
	}
	if c2 := claimTurn(t, s, "agent-2"); c2 != nil {
		t.Fatalf("over the limit: %+v", c2)
	}
}

func TestDropPlan(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Never mind", "")
	c := claimTurn(t, s, "agent-1")
	if _, err := s.PlanMessage(ctx, user, p.ID, "queued too"); err != nil {
		t.Fatal(err)
	}
	got, err := s.DropPlan(ctx, user, p.ID, "not now")
	if err != nil || got.Stage != pipeline.PlanDropped {
		t.Fatalf("drop: %+v %v", got, err)
	}
	d, _ := s.GetPlan(ctx, p.ID)
	for _, tn := range d.Turns {
		if tn.Status != pipeline.TurnFailed {
			t.Fatalf("turn %+v", tn)
		}
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat after the drop: %v %v", stop, err)
	}
	if _, err := s.PlanMessage(ctx, user, p.ID, "x"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("message: %v", err)
	}
	if _, err := s.DropPlan(ctx, user, p.ID, ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("second drop: %v", err)
	}
	if c := claimTurn(t, s, "agent-1"); c != nil {
		t.Fatalf("claimed %+v", c)
	}
	rows, _ := s.ListPlans(ctx, true)
	if len(rows) != 0 {
		t.Fatalf("open plans %+v", rows)
	}
}

func TestPlanDoneWhenItsItemsAre(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Finish", "")
	toBlueprint(t, s, p)
	c := claimTurn(t, s, "agent-1")
	result(t, s, c, pipeline.TurnResult{Reply: "ok", Blueprint: blueprint3, Ready: true})
	a, err := s.ApprovePlan(ctx, user, p.ID, pipeline.DocBlueprint, pipeline.DocSHA256(blueprint3), "")
	if err != nil {
		t.Fatal(err)
	}
	// Two pieces done (by hand here), the third dropped: the plan is done.
	if _, err := s.w.Exec(`UPDATE items SET stage = 'done' WHERE id IN (?, ?)`, a.Items[0].ID, a.Items[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Drop(ctx, user, a.Items[2].ID, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetPlan(ctx, p.ID)
	if d.Plan.Stage != pipeline.PlanDone || !hasEvent(d.Events, "plan_done") {
		t.Fatalf("plan %+v", d.Plan)
	}
	rows, _ := s.ListPlans(ctx, false)
	if len(rows) != 1 || rows[0].Stage != pipeline.PlanDone || rows[0].Turn != "" {
		t.Fatalf("plans %+v", rows)
	}
}
