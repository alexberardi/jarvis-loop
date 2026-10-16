package slack

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const planBlueprint = "# Blueprint\n\n## Pieces\n\n### 1. Table\n- depends on: none\n\nThe table.\n\n### 2. API\n- depends on: 1\n\nThe routes.\n"

// planMsg is a message in #plan from user: top-level when threadTS is empty. It returns its ts.
func (e *botEnv) planMsg(user, threadTS, text string) string {
	eventSeq++
	ts := fmt.Sprintf("1800000000.%06d", eventSeq)
	ev := map[string]any{"type": "message", "channel": ChannelPlan, "user": user, "text": text, "ts": ts}
	if threadTS != "" {
		ev["thread_ts"] = threadTS
	}
	e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": fmt.Sprintf("Ev%d", eventSeq), "event": ev}))
	return ts
}

func (e *botEnv) plan(id int64) store.PlanDetail {
	e.t.Helper()
	d, err := e.st.GetPlan(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// turn has a runner take the plan's next turn and report r.
func (e *botEnv) turn(planID int64, r pipeline.TurnResult) pipeline.PlanTurn {
	e.t.Helper()
	cl, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "planner", Kinds: []string{pipeline.WorkPlan}})
	if err != nil || cl == nil || cl.Turn == nil || cl.Turn.PlanID != planID {
		e.t.Fatalf("claim a turn: %+v %v", cl, err)
	}
	if r.SessionID == "" {
		r.SessionID = "s-" + cl.Turn.Persona
	}
	_, t, err := e.st.PlanTurnResult(e.ctx, "devbox", planID, cl.Turn.ID, r)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

func reactions(cs []call, method string) []string {
	var out []string
	for _, c := range cs {
		if c.Method == method {
			out = append(out, fmt.Sprintf("%v %v", c.Body["timestamp"], c.Body["name"]))
		}
	}
	return out
}

func newPlanEnv(t *testing.T) *botEnv {
	e := newBotEnv(t)
	repos := []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}
	e.st.SetRepos(repos)
	e.b.c.Repos = repos
	return e
}

func TestPlanStartsFromTheApproversMessage(t *testing.T) {
	e := newPlanEnv(t)
	top := e.planMsg(Approver, "", "<@UBOT> A pantry list in jarvis-server, high priority\nSo I know what to buy.")
	rows, _ := e.st.ListPlans(e.ctx, true)
	if len(rows) != 1 {
		t.Fatalf("plans %+v", rows)
	}
	d := e.plan(rows[0].ID)
	p := d.Plan
	if p.Title != "A pantry list" || p.Priority != "P2" || !slices.Equal(p.Repos, []string{"alexberardi/jarvis-server"}) ||
		p.SlackChannel != ChannelPlan || p.SlackTS != top {
		t.Fatalf("plan %+v", p)
	}
	if d.Turns[0].Messages[0] != "A pantry list in jarvis-server, high priority\nSo I know what to buy." || !slices.Equal(d.Turns[0].SlackTS, []string{top}) {
		t.Fatalf("first turn %+v", d.Turns[0])
	}
	// The bot answers in the thread with the plan's number and a Drop button, and keeps that
	// message as the plan's header.
	cs := e.calls()
	posts := only(t, cs, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != top || !strings.Contains(allText(posts[0]), "P1 A pantry list") ||
		!slices.Contains(actionIDs(posts[0]), "plan_drop") || elementValue(posts[0], "plan_drop") != "P1" {
		t.Fatalf("header %+v", posts)
	}
	if d := e.plan(1); d.Plan.SlackHeader == "" {
		t.Fatal("no header recorded")
	}
	if strings.Contains(allText(posts[0]), "<@") {
		t.Fatalf("header mentions someone: %s", allText(posts[0]))
	}
}

func TestPlanIgnoresEveryoneElse(t *testing.T) {
	e := newPlanEnv(t)
	e.planMsg(other, "", "a plan of my own")
	// A mention in #plan is the message's, not an add.
	e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event": map[string]any{
		"type": "app_mention", "channel": ChannelPlan, "user": Approver, "text": "<@UBOT> hi", "ts": "1800000001.000001"}}))
	if rows, _ := e.st.ListPlans(e.ctx, false); len(rows) != 0 {
		t.Fatalf("plans %+v", rows)
	}
	if items, _ := e.st.ListItems(e.ctx, "", ""); len(items) != 0 {
		t.Fatalf("items %+v", items)
	}
	if cs := e.calls(); len(cs) != 0 {
		t.Fatalf("Slack calls %+v", cs)
	}
	if !strings.Contains(e.logs.String(), "ignored a #plan message") {
		t.Fatalf("not logged: %s", e.logs.String())
	}
	// Nor do their replies in a plan's thread reach the agent.
	top := e.planMsg(Approver, "", "a pantry list")
	e.calls()
	e.planMsg(other, top, "and mine too")
	if d := e.plan(1); len(d.Turns) != 1 || len(d.Turns[0].Messages) != 1 {
		t.Fatalf("turns %+v", d.Turns)
	}
	if cs := e.calls(); len(cs) != 0 {
		t.Fatalf("Slack calls %+v", cs)
	}
}

func TestPlanConversationInTheThread(t *testing.T) {
	e := newPlanEnv(t)
	top := e.planMsg(Approver, "", "a pantry list")
	r1 := e.planMsg(Approver, top, "for the kitchen")
	e.calls()
	// The runner claims the turn: ⏳ on its messages.
	cl, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "planner", Kinds: []string{pipeline.WorkPlan}})
	if err != nil || cl == nil || cl.Turn == nil {
		t.Fatalf("claim %+v %v", cl, err)
	}
	cs := e.calls()
	if got := reactions(cs, "reactions.add"); !slices.Equal(got, []string{top + " " + ReactionRunning, r1 + " " + ReactionRunning}) {
		t.Fatalf("reactions on claim %v", got)
	}
	// A reply while it runs waits for the next turn.
	r2 := e.planMsg(Approver, top, "and the fridge")
	if _, _, err := e.st.PlanTurnResult(e.ctx, "devbox", 1, cl.Turn.ID, pipeline.TurnResult{Reply: "Who is it for? <!channel>", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	cs = e.calls()
	if got := reactions(cs, "reactions.remove"); !slices.Equal(got, []string{top + " " + ReactionRunning, r1 + " " + ReactionRunning}) {
		t.Fatalf("removed %v", got)
	}
	if got := reactions(cs, "reactions.add"); !slices.Equal(got, []string{top + " " + ReactionDone, r1 + " " + ReactionDone}) {
		t.Fatalf("added %v", got)
	}
	posts := only(t, cs, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != top || !strings.Contains(allText(posts[0]), "Who is it for? &lt;!channel&gt;") ||
		len(actionIDs(posts[0])) != 0 {
		t.Fatalf("reply %+v", posts)
	}
	// The header still says it is the agent's turn (the reply sent meanwhile waits): not edited.
	if ups := only(t, cs, "chat.update"); len(ups) != 0 {
		t.Fatalf("header edited for nothing: %+v", ups)
	}
	// The next turn carries the reply sent meanwhile; when it fails the thread says so.
	cl, _ = e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "planner", Kinds: []string{pipeline.WorkPlan}})
	if !slices.Equal(cl.Turn.SlackTS, []string{r2}) {
		t.Fatalf("second turn %v", cl.Turn.SlackTS)
	}
	e.st.PlanTurnResult(e.ctx, "devbox", 1, cl.Turn.ID, pipeline.TurnResult{SessionID: "s1"})
	cs = e.calls()
	posts = only(t, cs, "chat.postMessage")
	if len(posts) != 1 || !strings.Contains(allText(posts[0]), "That turn failed: ") || !strings.Contains(allText(posts[0]), "Reply to try again.") {
		t.Fatalf("failed turn %+v", posts)
	}
	if got := reactions(cs, "reactions.remove"); !slices.Equal(got, []string{r2 + " " + ReactionRunning}) {
		t.Fatalf("removed %v", got)
	}
	if ups := only(t, cs, "chat.update"); len(ups) == 0 || !strings.Contains(allText(ups[len(ups)-1]), "your turn") {
		t.Fatalf("header update %+v", ups)
	}
}

func TestPlanReactionsAreANicety(t *testing.T) {
	e := newPlanEnv(t)
	top := e.planMsg(Approver, "", "a pantry list")
	e.calls()
	e.f.failNext("reactions.add", "missing_scope")
	e.f.failNext("reactions.remove", "missing_scope")
	e.turn(1, pipeline.TurnResult{Reply: "Who is it for?"})
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != top || !strings.Contains(allText(posts[0]), "Who is it for?") {
		t.Fatalf("reply %+v", posts)
	}
	if !strings.Contains(e.logs.String(), "could not add a reaction") {
		t.Fatalf("not logged: %s", e.logs.String())
	}
}

// pressPlan presses a button on the newest message carrying it.
func (e *botEnv) pressPlan(user string, cs []call, actionID string) {
	e.t.Helper()
	for i := len(cs) - 1; i >= 0; i-- {
		if slices.Contains(actionIDs(cs[i]), actionID) {
			e.press(user, ChannelPlan, "1700000000.999999", actionID, "", elementValue(cs[i], actionID))
			return
		}
	}
	e.t.Fatalf("no %s button", actionID)
}

func TestPlanApprovalsWaitAndBindToTheVersion(t *testing.T) {
	e := newPlanEnv(t)
	top := e.planMsg(Approver, "", "a pantry list in jarvis-loop")
	e.calls()
	e.turn(1, pipeline.TurnResult{Reply: "Brief ready.", Brief: "# Pantry\n\nv1", Ready: true})
	cs := e.calls()
	posts := only(t, cs, "chat.postMessage")
	if len(posts) != 1 || !slices.Contains(actionIDs(posts[0]), "plan_approve") || !strings.Contains(allText(posts[0]), "Approve brief v1") ||
		!strings.Contains(allText(posts[0]), "GET /plans/P1/docs/brief?v=1") {
		t.Fatalf("ready reply %+v", posts)
	}
	v1 := cs

	// Someone else's press is refused.
	e.pressPlan(other, v1, "plan_approve")
	cs = e.calls()
	if eps := only(t, cs, "chat.postEphemeral"); len(eps) != 1 || eps[0].Body["user"] != other {
		t.Fatalf("refusal %+v", cs)
	}
	if d := e.plan(1); d.Events[len(d.Events)-1].Kind != "refused" {
		t.Fatalf("no refusal event: %+v", d.Events[len(d.Events)-1])
	}

	// The approver presses: an echo with Cancel, applied after the window.
	e.pressPlan(Approver, v1, "plan_approve")
	cs = e.calls()
	posts = only(t, cs, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != top || !slices.Contains(actionIDs(posts[0]), "cancel_approval") ||
		!strings.Contains(allText(posts[0]), "Applying in 2 minutes") {
		t.Fatalf("echo %+v", posts)
	}
	e.b.Tick(e.ctx)
	if d := e.plan(1); d.Plan.Stage != pipeline.PlanBrief {
		t.Fatalf("applied early: %s", d.Plan.Stage)
	}
	// Cancel stops it.
	e.press(Approver, ChannelPlan, "1700000000.999998", "cancel_approval", "pending", elementValue(posts[0], "cancel_approval"))
	e.clk.Advance(3 * 60e9)
	e.b.Tick(e.ctx)
	if d := e.plan(1); d.Plan.Stage != pipeline.PlanBrief || d.Events[len(d.Events)-1].Kind != "approval_cancelled" {
		t.Fatalf("after cancel: %s %+v", d.Plan.Stage, d.Events[len(d.Events)-1])
	}
	e.calls()

	// A newer brief makes the v1 button stale: refused when it applies, with the version to approve.
	e.planMsg(Approver, top, "add the freezer")
	e.turn(1, pipeline.TurnResult{Reply: "Brief v2.", Brief: "# Pantry\n\nv2", Ready: true})
	v2 := e.calls()
	e.pressPlan(Approver, v1, "plan_approve")
	cs = e.calls()
	echo := only(t, cs, "chat.postMessage")[0]
	e.clk.Advance(2 * 60e9)
	e.b.Tick(e.ctx)
	cs = e.calls()
	if d := e.plan(1); d.Plan.Stage != pipeline.PlanBrief {
		t.Fatalf("stale approval applied: %s", d.Plan.Stage)
	}
	ups := only(t, cs, "chat.update")
	if len(ups) == 0 || !strings.Contains(allText(ups[0]), "Not applied: the brief changed; approve v2 instead") {
		t.Fatalf("stale echo %+v (echo %v)", ups, echo.Body["text"])
	}
	if posts := only(t, cs, "chat.postMessage"); len(posts) != 1 || !strings.Contains(allText(posts[0]), "Refused approve brief from you") {
		t.Fatalf("refusal line %+v", posts)
	}

	// v2's button goes through: the engineering agent starts; its blueprint's approval files items.
	e.pressPlan(Approver, v2, "plan_approve")
	e.clk.Advance(2 * 60e9)
	e.b.Tick(e.ctx)
	cs = e.calls()
	if d := e.plan(1); d.Plan.Stage != pipeline.PlanBlueprint || d.Plan.BriefApproved != 2 {
		t.Fatalf("after approving v2: %+v", d.Plan)
	}
	if posts := only(t, cs, "chat.postMessage"); len(posts) != 2 || !strings.Contains(allText(posts[1]), "Brief v2 approved by you → blueprint.") {
		t.Fatalf("approved line %+v", posts)
	}
	e.turn(1, pipeline.TurnResult{Reply: "Blueprint ready.", Blueprint: planBlueprint, Ready: true})
	bp := e.calls()
	if posts := only(t, bp, "chat.postMessage"); len(posts) != 1 || !strings.Contains(allText(posts[0]), "Approve blueprint v1") {
		t.Fatalf("blueprint ready %+v", posts)
	}
	e.pressPlan(Approver, bp, "plan_approve")
	e.clk.Advance(2 * 60e9)
	e.b.Tick(e.ctx)
	cs = e.calls()
	var split string
	for _, c := range only(t, cs, "chat.postMessage") {
		if c.Body["channel"] == ChannelPlan && strings.Contains(allText(c), "Filed 2 items") {
			split = allText(c)
		}
	}
	if !strings.Contains(split, "#1 Table") || !strings.Contains(split, "#2 API (after #1)") {
		t.Fatalf("split message %q", split)
	}
	if d := e.plan(1); d.Plan.Stage != pipeline.PlanSplit || len(d.Items) != 2 {
		t.Fatalf("after the blueprint: %+v", d.Plan)
	}
}

func TestPlanDrop(t *testing.T) {
	e := newPlanEnv(t)
	e.planMsg(Approver, "", "a pantry list")
	e.planMsg(Approver, "", "a garden plan, low priority")
	cs := e.calls()
	if out := e.command(Approver, "plans"); !strings.Contains(out, "P2 P4 a garden plan — brief, the agent's turn") || !strings.Contains(out, "P1 P3 a pantry list") {
		t.Fatalf("/loop plans %q", out)
	}
	// The header's Drop button.
	e.pressPlan(other, cs, "plan_drop")
	if d := e.plan(2); d.Plan.Stage != pipeline.PlanBrief {
		t.Fatalf("dropped by someone else: %s", d.Plan.Stage)
	}
	e.pressPlan(Approver, cs, "plan_drop")
	if d := e.plan(2); d.Plan.Stage != pipeline.PlanDropped {
		t.Fatalf("Drop button: %s", d.Plan.Stage)
	}
	cs = e.calls()
	if posts := only(t, cs, "chat.postMessage"); len(posts) == 0 || !strings.Contains(allText(posts[len(posts)-1]), "Dropped by you: 'Drop button'.") {
		t.Fatalf("dropped line %+v", posts)
	}
	if ups := only(t, cs, "chat.update"); len(ups) == 0 || slices.Contains(actionIDs(ups[len(ups)-1]), "plan_drop") {
		t.Fatalf("header still has Drop: %+v", ups)
	}
	// /loop drop P<n>; a number alone is still an item.
	if out := e.command(other, "drop P1"); !strings.Contains(out, "only the approver") {
		t.Fatalf("/loop drop P1 by someone else: %q", out)
	}
	if out := e.command(Approver, "drop P1 not now"); out != "P1 dropped." {
		t.Fatalf("/loop drop P1: %q", out)
	}
	if out := e.command(Approver, "drop P1"); !strings.Contains(out, "plan P1 is dropped") {
		t.Fatalf("drop again: %q", out)
	}
	if out := e.command(Approver, "plans"); !strings.Contains(out, "No open plans") {
		t.Fatalf("/loop plans %q", out)
	}
}

func TestLoopPlanCommandStartsAThread(t *testing.T) {
	e := newPlanEnv(t)
	out := e.command(Approver, "plan a pantry list in jarvis-loop, urgent")
	if !strings.Contains(out, "Started P1 (P1): a pantry list.") {
		t.Fatalf("/loop plan %q", out)
	}
	if out := e.command(other, "plan mine"); !strings.Contains(out, "only the approver") {
		t.Fatalf("/loop plan by someone else %q", out)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["channel"] != ChannelPlan || posts[0].Body["thread_ts"] != nil {
		t.Fatalf("parent %+v", posts)
	}
	d := e.plan(1)
	if d.Plan.SlackChannel != ChannelPlan || d.Plan.SlackTS == "" || d.Plan.SlackHeader != d.Plan.SlackTS {
		t.Fatalf("thread %+v", d.Plan)
	}
	// Replies in that thread talk to the plan.
	e.planMsg(Approver, d.Plan.SlackTS, "for the kitchen")
	if d := e.plan(1); len(d.Turns[0].Messages) != 2 {
		t.Fatalf("turn %+v", d.Turns[0])
	}
}

func TestPlanLinksToItsPage(t *testing.T) {
	e := newPlanEnv(t)
	e.b.c.LinkBase = "http://loopd.lan:7780"
	e.planMsg(Approver, "", "a pantry list")
	e.turn(1, pipeline.TurnResult{Reply: "Ready.", Brief: "# Pantry", Ready: true})
	var all string
	for _, c := range e.calls() {
		all += allText(c)
	}
	for _, want := range []string{"<http://loopd.lan:7780/board/plans/1|plan page>", "<http://loopd.lan:7780/board/plans/1|brief v1>"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in %s", want, all)
		}
	}
}

func TestPlanReplyAfterTheConversationEnded(t *testing.T) {
	e := newPlanEnv(t)
	top := e.planMsg(Approver, "", "a pantry list")
	e.command(Approver, "drop P1")
	e.calls()
	e.planMsg(Approver, top, "wait, one more thing")
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != top ||
		posts[0].Body["text"] != "Refused message from you: plan P1 is dropped: there is no conversation to add to" {
		t.Fatalf("refusal %+v", posts)
	}
}
