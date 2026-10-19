package slack

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	testOrchToken = "xoxb-orch-test-not-a-real-token"
	orchUser      = "U0ORCHESTRATOR"
	orchActor     = "orchestrator:loop"
)

// orchEnv is a botEnv with the @orchestrator app beside the loop bot.
type orchEnv struct {
	*botEnv
	o *Orchestrator
}

func newOrchEnv(t *testing.T) *orchEnv {
	t.Helper()
	e := newUnidentifiedOrchEnv(t)
	if err := e.o.Identify(e.ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

// newUnidentifiedOrchEnv is an orchEnv whose orchestrator has not had auth.test's answer yet.
func newUnidentifiedOrchEnv(t *testing.T) *orchEnv {
	t.Helper()
	e := newBotEnv(t)
	e.f.AddApp(slackfake.App{Name: "orchestrator", BotToken: testOrchToken, AppToken: "xapp-orch-test", BotUser: orchUser})
	api := NewAPI(e.f.baseURL(), testOrchToken, Channels{ChannelLoop, ChannelDigest, ChannelPlan, DMs})
	api.Sleep = e.sl.Sleep
	o, err := NewOrchestrator(OrchestratorConfig{Store: e.st, API: api, Clock: e.clk, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Sleep: e.sl.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	o.Attach()
	e.b.c.Orchestrator = o
	return &orchEnv{botEnv: e, o: o}
}

// orchCalls drains both apps and returns the Slack calls made, split by app.
func (e *orchEnv) orchCalls() (loop, orch []call) {
	e.t.Helper()
	e.drain()
	e.o.Drain(e.ctx)
	for _, c := range e.f.take() {
		if c.Method == "auth.test" {
			continue
		}
		if c.Auth == "Bearer "+testOrchToken {
			orch = append(orch, c)
		} else {
			loop = append(loop, c)
		}
	}
	return loop, orch
}

// message is a message event from user on the given socket's handler.
func (e *orchEnv) message(handle func(Envelope), typ, user, channel, threadTS, text string) string {
	eventSeq++
	ts := fmt.Sprintf("1800000000.%06d", eventSeq)
	ev := map[string]any{"type": typ, "channel": channel, "user": user, "text": text, "ts": ts}
	if threadTS != "" {
		ev["thread_ts"] = threadTS
	}
	handle(envelope("events_api", map[string]any{"event_id": fmt.Sprintf("Ev%d", eventSeq), "event": ev}))
	return ts
}

func (e *orchEnv) toLoop(user, threadTS, text string) {
	e.message(func(env Envelope) { e.b.HandleEnvelope(e.ctx, env) }, "message", user, ChannelLoop, threadTS, text)
}

func (e *orchEnv) toOrch(typ, user, channel, threadTS, text string) string {
	return e.message(func(env Envelope) { e.o.HandleEnvelope(e.ctx, env) }, typ, user, channel, threadTS, text)
}

func (e *orchEnv) orchPress(user, channel, msgTS, actionID, value string, blocks []map[string]any) {
	e.o.HandleEnvelope(e.ctx, envelope("interactive", map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user}, "channel": map[string]any{"id": channel},
		"container": map[string]any{"channel_id": channel, "message_ts": msgTS},
		"message":   map[string]any{"ts": msgTS, "blocks": blocks},
		"actions":   []any{map[string]any{"action_id": actionID, "block_id": "orch:" + value, "value": value}}}))
}

func (e *orchEnv) messages() []store.Event {
	e.t.Helper()
	evs, err := e.st.OrchMessages(e.ctx, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// A message mentioning @orchestrator in an item's thread is the orchestrator's: the loop bot
// neither takes it as the answer nor as an approval, and the orchestrator records it once.
func TestMentionOfTheOrchestratorIsNotTheLoopBots(t *testing.T) {
	e := newOrchEnv(t)
	id := e.add("x")
	e.toNeedsYou(id, "skip the screenshots?")
	e.orchCalls()
	ts := e.thread(id)
	text := "<@" + orchUser + "> answer #1: open the PR without the PNGs"
	e.toLoop(Approver, ts, text)
	msgTS := e.toOrch("message", Approver, ChannelLoop, ts, text)
	e.o.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": "EvMention", "event": map[string]any{
		"type": "app_mention", "channel": ChannelLoop, "user": Approver, "text": text, "ts": msgTS, "thread_ts": ts}}))
	if d := e.item(id); d.Item.Stage != pipeline.NeedsYou {
		t.Fatalf("the loop bot answered it: %s %v", d.Item.Stage, e.kinds(id))
	}
	msgs := e.messages()
	if len(msgs) != 1 {
		t.Fatalf("recorded %d times", len(msgs))
	}
	m := msgs[0]
	if m.Data["text"] != "answer #1: open the PR without the PNGs" || m.Data["approver"] != true || m.Data["item_id"] != float64(id) ||
		m.Data["thread_ts"] != ts || m.Data["channel"] != ChannelLoop {
		t.Fatalf("message %+v", m.Data)
	}
	loop, orch := e.orchCalls()
	if len(loop) != 0 {
		t.Fatalf("the loop bot said something: %+v", loop)
	}
	if r := only(t, orch, "reactions.add"); len(r) != 1 || r[0].Body["name"] != "eyes" || r[0].Body["timestamp"] != msgTS {
		t.Fatalf("seen reaction: %+v", orch)
	}
	// A plain reply is still the answer.
	e.toLoop(Approver, ts, "open the PR without them")
	if d := e.item(id); d.Item.Stage == pipeline.NeedsYou {
		t.Fatal("a plain reply did not answer")
	}

}

// In a held thread, "@orchestrator approve" is not a reply approval.
func TestMentionOfTheOrchestratorIsNotAnApproval(t *testing.T) {
	e := newOrchEnv(t)
	h := e.add("held")
	e.toHeld(h, shaA)
	e.orchCalls()
	e.toLoop(Approver, e.thread(h), "<@"+orchUser+"> approve #1")
	if len(e.b.pending) != 0 {
		t.Fatal("a mention of the orchestrator started a reply approval")
	}
}

// Until the orchestrator's auth.test answers, the loop bot cannot tell its mention: a message
// mentioning anyone but the loop bot is left alone, so "@orchestrator approve" is no approval and
// "@orchestrator trust" no OK. A mention of the loop bot itself is still its own.
func TestMentionBeforeTheOrchestratorIsIdentified(t *testing.T) {
	e := newUnidentifiedOrchEnv(t)
	if e.o.BotUser() != "" {
		t.Fatal("identified")
	}
	h := e.add("held")
	e.toHeld(h, shaA)
	tr := e.add("trusted")
	e.toTrust(tr, shaB)
	q := e.add("question")
	e.toNeedsYou(q, "skip the screenshots?")
	e.orchCalls()
	e.toLoop(Approver, e.thread(h), "<@"+orchUser+"> approve #1")
	if len(e.b.pending) != 0 {
		t.Fatal("a mention of the orchestrator started a reply approval")
	}
	e.toLoop(Approver, e.thread(tr), "<@"+orchUser+"> trust")
	if d := e.item(tr); d.Item.Stage != pipeline.NeedsYou || d.Item.Trust.By != "" {
		t.Fatalf("a mention of the orchestrator trusted: %s %+v", d.Item.Stage, d.Item.Trust)
	}
	e.toLoop(Approver, e.thread(q), "<@"+slackfake.BotUser+"> open the PR without them")
	if d := e.item(q); d.Item.Stage == pipeline.NeedsYou {
		t.Fatal("a mention of the loop bot did not answer")
	}
}

// DMs from anyone are recorded (only the approver's can make it act); channel messages only
// when they mention it; its own and other bots' never.
func TestOrchestratorRecordsDMsAndMentions(t *testing.T) {
	e := newOrchEnv(t)
	e.toOrch("message", Approver, "D0DM1", "", "drop #61 and #63")
	e.toOrch("message", other, "D0DM2", "", "drop #61")
	e.toOrch("message", Approver, ChannelLoop, "", "no mention here")
	e.toOrch("message", Approver, "C0ELSEWHERE", "", "<@"+orchUser+"> outside the loop's channels")
	e.toOrch("message", orchUser, "D0DM1", "", "its own")
	e.o.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": "EvBot", "event": map[string]any{
		"type": "message", "channel": "D0DM1", "user": "U0BOT", "bot_id": "B1", "text": "a bot", "ts": "1.9"}}))
	e.toOrch("message", Approver, ChannelDigest, "", "<@"+orchUser+"> what's stuck?")
	msgs := e.messages()
	if len(msgs) != 3 {
		t.Fatalf("recorded %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Data["approver"] != true || msgs[1].Data["approver"] != false || msgs[2].Data["text"] != "what's stuck?" {
		t.Fatalf("messages %+v", msgs)
	}
	if msgs[0].Data["thread_ts"] != nil {
		t.Fatalf("a top-level DM has no thread: %+v", msgs[0].Data)
	}
}

func (e *orchEnv) escalate(id int64, text, rec string) store.Event {
	e.t.Helper()
	ev, err := e.st.OrchEscalate(e.ctx, orchActor, id, text, rec, "the code and its tests pass")
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

// A recommendation waits out the batch window, then goes in the item's thread, mentioning the
// approver, with [Accept] [Other…]. Accept is the approver's: anyone else is refused; the
// approver's records the recommendation as their answer and settles the buttons.
func TestRecommendationAccept(t *testing.T) {
	e := newOrchEnv(t)
	id := e.add("mobile-shot: the newest inbox item's detail")
	e.toNeedsYou(id, "can't take the screenshots")
	e.orchCalls()
	esc := e.escalate(id, "the agent can't take the screenshots the item asks for", "open the PR without them")
	loop, orch := e.orchCalls()
	if len(loop) != 0 || len(orch) != 0 {
		t.Fatalf("posted inside the batch window: loop %+v orch %+v", loop, orch)
	}
	e.clk.Advance(2 * time.Minute)
	e.o.Tick(e.ctx)
	_, orch = e.orchCalls()
	posts := only(t, orch, "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", orch)
	}
	p := posts[0]
	txt := allText(p)
	if p.Body["thread_ts"] != e.thread(id) || !strings.Contains(txt, "<@"+Approver+">") || !strings.Contains(txt, "needs you: the agent can't take") ||
		!strings.Contains(txt, "*Recommend:* open the PR without them") || !strings.Contains(txt, "_Because_ the code and its tests pass") {
		t.Fatalf("message %s", txt)
	}
	if ids := actionIDs(p); len(ids) != 2 || ids[0] != actionAccept || ids[1] != actionOther {
		t.Fatalf("buttons %v", ids)
	}
	value := elementValue(p, actionAccept)
	if value != fmt.Sprint(esc.ID) {
		t.Fatalf("accept value %q, escalation %d", value, esc.ID)
	}

	e.orchPress(other, ChannelLoop, "1.1", actionAccept, value, blocksOf(p))
	if e.item(id).Item.Stage != pipeline.NeedsYou {
		t.Fatal("someone else's Accept answered")
	}
	_, orch = e.orchCalls()
	if eph := only(t, orch, "chat.postEphemeral"); len(eph) != 1 || eph[0].Body["user"] != other {
		t.Fatalf("refusal %+v", orch)
	}
	if ev := lastEvent(e.item(id)); ev.Kind != "refused" || ev.Data["action"] != "accept" {
		t.Fatalf("refusal event %+v", ev)
	}

	e.orchPress(Approver, ChannelLoop, "1.1", actionOther, value, blocksOf(p))
	_, orch = e.orchCalls()
	if eph := only(t, orch, "chat.postEphemeral"); len(eph) != 1 || !strings.Contains(fmt.Sprint(eph[0].Body["text"]), "Reply in #1's thread") {
		t.Fatalf("other %+v", orch)
	}

	e.orchPress(Approver, ChannelLoop, "1.1", actionAccept, value, blocksOf(p))
	d := e.item(id)
	ev := lastEvent(d)
	if d.Item.Stage == pipeline.NeedsYou || ev.Kind != "answered" || ev.Actor != actorPrefix+Approver || ev.Data["answer"] != "open the PR without them" {
		t.Fatalf("accept: %s %+v", d.Item.Stage, ev)
	}
	loop, orch = e.orchCalls()
	up := only(t, orch, "chat.update")
	if len(up) != 1 || !strings.Contains(allText(up[0]), "Accepted by you.") || len(actionIDs(up[0])) != 0 {
		t.Fatalf("settle %+v", orch)
	}
	replies := only(t, loop, "chat.postMessage")
	if len(replies) != 1 || !strings.Contains(fmt.Sprint(replies[0].Body["text"]), "Answered by you, accepting the orchestrator's recommendation: open the PR without them") {
		t.Fatalf("thread line %+v", replies)
	}

	// Pressed again: refused, nothing answered twice.
	e.orchPress(Approver, ChannelLoop, "1.1", actionAccept, value, blocksOf(p))
	_, orch = e.orchCalls()
	if eph := only(t, orch, "chat.postEphemeral"); len(eph) != 1 || !strings.Contains(fmt.Sprint(eph[0].Body["text"]), "Not accepted") {
		t.Fatalf("twice %+v", orch)
	}
}

// Escalations inside one window share a message in #loop (one section and Accept each) with a
// line in each item's thread; an approval's has no Accept (approvals stay on their button), nor
// one with no recommendation.
func TestEscalationsAreBatched(t *testing.T) {
	e := newOrchEnv(t)
	a := e.add("a")
	e.toNeedsYou(a, "which layout?")
	h := e.add("held")
	e.toHeld(h, shaA)
	b := e.add("b")
	e.toNeedsYou(b, "retry?")
	e.orchCalls()
	e.escalate(a, "a product call", "the list layout")
	e.clk.Advance(time.Minute)
	e.escalate(h, "merge #2? it is held on auth", "approve it")
	if err := e.st.Record(e.ctx, store.Event{ItemID: b, Actor: "loopd", Kind: store.KindOrchEscalated,
		Data: map[string]any{"text": "failure 3 on the same head", "reason": "the third failure on one head is the user's"}}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(59 * time.Second)
	e.o.Drain(e.ctx)
	e.o.Tick(e.ctx)
	if _, orch := e.orchCalls(); len(orch) != 0 {
		t.Fatalf("posted before the first one's window ended: %+v", orch)
	}
	e.clk.Advance(time.Second)
	e.o.Tick(e.ctx)
	_, orch := e.orchCalls()
	posts := only(t, orch, "chat.postMessage")
	if len(posts) != 4 {
		t.Fatalf("want one batch and three thread lines, got %d: %+v", len(posts), posts)
	}
	batch := posts[0]
	txt := allText(batch)
	if batch.Body["thread_ts"] != nil || !strings.Contains(txt, "<@"+Approver+"> 3 items need you") || strings.Count(txt, "<@") != 2 { // the fallback text and the header only
		t.Fatalf("batch %s", txt)
	}
	if ids := actionIDs(batch); len(ids) != 2 || ids[0] != actionAccept {
		t.Fatalf("only the product call gets Accept: %v", ids)
	}
	if !strings.Contains(txt, "Approve it with the button on its approval message") {
		t.Fatalf("approval entry %s", txt)
	}
	for i, id := range []int64{a, h, b} {
		if posts[i+1].Body["thread_ts"] != e.thread(id) || !strings.Contains(fmt.Sprint(posts[i+1].Body["text"]), "with 2 other items") {
			t.Fatalf("thread line %d: %+v", i, posts[i+1].Body)
		}
	}
}

// Its replies go where it was asked, DMs included; lowering the active agents pings the user in
// #loop-digest.
func TestOrchestratorRepliesAndAgentsPing(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.st.OrchReply(e.ctx, orchActor, "D0DM1", "1.1", "#35 waits on #34 <!channel>", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.OrchAgentsDown(e.ctx, orchActor, 1, "every run fails on the git lock"); err != nil {
		t.Fatal(err)
	}
	_, orch := e.orchCalls()
	posts := only(t, orch, "chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("posts %+v", orch)
	}
	if posts[0].Body["channel"] != "D0DM1" || posts[0].Body["thread_ts"] != "1.1" || strings.Contains(allText(posts[0]), "<!channel>") {
		t.Fatalf("reply %+v", posts[0].Body)
	}
	if posts[1].Body["channel"] != ChannelDigest || !strings.Contains(allText(posts[1]), "<@"+Approver+"> Orchestrator: active agents 4 → 1 — every run fails on the git lock") {
		t.Fatalf("agents ping %s", allText(posts[1]))
	}
}

// Asked to approve, the orchestrator has the loop bot post the item's approval message again,
// with its sha-bound buttons; nothing is approved.
func TestApprovalAskedPostsTheButton(t *testing.T) {
	e := newOrchEnv(t)
	id := e.add("x")
	e.toHeld(id, shaA)
	e.orchCalls()
	if _, err := e.st.OrchAskApproval(e.ctx, orchActor, id, "you said approve #1"); err != nil {
		t.Fatal(err)
	}
	loop, _ := e.orchCalls()
	posts := only(t, loop, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != e.thread(id) {
		t.Fatalf("posts %+v", loop)
	}
	if v := elementValue(posts[0], "approve"); v != fmt.Sprintf("%d:%s", id, shaA) {
		t.Fatalf("approve button %q in %s", v, allText(posts[0]))
	}
	if !strings.Contains(allText(posts[0]), "Orchestrator: approvals are yours") {
		t.Fatalf("lead %s", allText(posts[0]))
	}
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("approved")
	}
}

// Asked to OK trusted files, the orchestrator has the loop bot post the item's trust message again,
// with its button bound to the refused commit; nothing is trusted.
func TestTrustAskedPostsTheButton(t *testing.T) {
	e := newOrchEnv(t)
	id := e.add("x")
	e.toTrust(id, shaA)
	e.orchCalls()
	if _, err := e.st.OrchAskApproval(e.ctx, orchActor, id, "you said OK the trusted files"); err != nil {
		t.Fatal(err)
	}
	loop, _ := e.orchCalls()
	posts := only(t, loop, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["thread_ts"] != e.thread(id) {
		t.Fatalf("posts %+v", loop)
	}
	if v := elementValue(posts[0], "trust"); v != fmt.Sprintf("%d:%s", id, shaA) {
		t.Fatalf("trust button %q in %s", v, allText(posts[0]))
	}
	if !strings.Contains(allText(posts[0]), "Orchestrator: trusted-file OKs are yours") || !strings.Contains(allText(posts[0]), "tools/pipeline") {
		t.Fatalf("message %s", allText(posts[0]))
	}
	if d := e.item(id); d.Item.Stage != pipeline.NeedsYou || d.Item.Trust.By != "" {
		t.Fatal("trusted")
	}
}

// A recommendation about an item waiting on a trusted-file OK has no Accept: its words are not the
// OK verify needs. It points to the trust message's button.
func TestRecommendationNoAcceptOnTrust(t *testing.T) {
	e := newOrchEnv(t)
	id := e.add("x")
	e.toTrust(id, shaA)
	e.orchCalls()
	e.escalate(id, "verify refused a change to tools/pipeline", "OK it: the change is the item's ask")
	e.orchCalls()
	e.clk.Advance(2 * time.Minute)
	e.o.Tick(e.ctx)
	_, orch := e.orchCalls()
	posts := only(t, orch, "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", orch)
	}
	if v := elementValue(posts[0], actionAccept); v != "" {
		t.Fatalf("Accept on a trusted-file question: %s", allText(posts[0]))
	}
	if !strings.Contains(allText(posts[0]), "button on its trust message") {
		t.Fatalf("no pointer to the trust button: %s", allText(posts[0]))
	}
}

// toTrust has verify refuse id's build commit sha for changing a trusted file.
func (e *orchEnv) toTrust(id int64, sha string) {
	e.t.Helper()
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "harness", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.Failed, map[string]string{"error": "the change touches tools/pipeline", pipeline.FieldTrustSHA: sha,
		pipeline.FieldTrustPaths: "tools/pipeline", pipeline.FieldTrustDiffstat: "tools/pipeline | 2 +-"})
}

// The orchestrator's own actions read "Orchestrator: <what> — <reason>"; the user's actions it
// made for them read "… you via orchestrator".
func TestOrchestratorLines(t *testing.T) {
	e := newOrchEnv(t)
	dep := e.add("the dependency")
	id := e.add("x")
	it := e.item(id).Item
	for _, c := range []struct {
		ev   store.Event
		want string
	}{
		{store.Event{Kind: "answered", To: "build", Data: map[string]any{"answer_kind": "harness", "answer": "list it under not verified", "reason": "the standing answer"}},
			"Orchestrator: answered (harness) 'list it under not verified' → build — the standing answer."},
		{store.Event{Kind: "retried", To: "build", Data: map[string]any{"failure": float64(1), "reason": "index.lock left by a killed run"}},
			"Orchestrator: retried the run (failure 1) → build — index.lock left by a killed run."},
		{store.Event{Kind: "patched", Data: map[string]any{"priority": map[string]any{"from": "P3", "to": "P1"}, "reason": "blocks three items"}},
			"Orchestrator: priority P3 → P1 — blocks three items."},
		{store.Event{Kind: "linked", Data: map[string]any{"after": float64(dep), "reason": "needs its API"}},
			"Orchestrator: now waits on #1 the dependency — needs its API."},
		{store.Event{Kind: store.KindOrchAdded, Data: map[string]any{"reason": "git lock on 2 items"}},
			"Orchestrator: filed this item — git lock on 2 items."},
		{store.Event{Kind: store.KindOrchSeenAgain, Data: map[string]any{"reason": "again on #9"}},
			"Orchestrator: saw this again — again on #9."},
		{store.Event{Kind: "refused", Data: map[string]any{"action": "approve", "message": "forbidden"}},
			"Orchestrator: tried to approve; refused: forbidden"},
	} {
		c.ev.ItemID, c.ev.Actor = id, orchActor
		m, ok := e.b.describe(e.ctx, c.ev, store.Detail{Item: it})
		if !ok || m.Text != c.want {
			t.Errorf("%s: got %q, want %q", c.ev.Kind, m.Text, c.want)
		}
	}
	via := map[string]any{"via": orchActor, "words": "drop #2"}
	m, _ := e.b.describe(e.ctx, store.Event{ItemID: id, Actor: actorPrefix + Approver, Kind: "dropped", Data: via}, store.Detail{Item: it})
	if !strings.HasPrefix(m.Text, "Dropped by you via orchestrator: 'drop #2'") {
		t.Errorf("via: %q", m.Text)
	}
	m, _ = e.b.describe(e.ctx, store.Event{ItemID: id, Actor: actorPrefix + Approver, Kind: "answered", To: "build",
		Data: map[string]any{"via": orchActor, "answer": "skip it"}}, store.Detail{Item: it})
	if m.Text != "Answered by you via orchestrator: skip it → build." {
		t.Errorf("via answer: %q", m.Text)
	}
}

// Without the @orchestrator app, the loop bot posts an escalation itself (no Accept button), and
// a mention of some other user is just a reply.
func TestEscalationWithoutTheOrchestratorApp(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("x")
	e.toNeedsYou(id, "which layout?")
	e.calls()
	if _, err := e.st.OrchEscalate(e.ctx, orchActor, id, "a product call", "the list layout", "matches the inbox"); err != nil {
		t.Fatal(err)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || !strings.Contains(allText(posts[0]), "<@"+Approver+"> Orchestrator: #1 x needs you: a product call") ||
		!strings.Contains(allText(posts[0]), "*Recommend:* the list layout _Because_ matches the inbox") || len(actionIDs(posts[0])) != 0 {
		t.Fatalf("posts %+v", posts)
	}
	e.reply(Approver, e.thread(id), "<@"+orchUser+"> the list layout")
	if e.item(id).Item.Stage == pipeline.NeedsYou {
		t.Fatal("with the orchestrator off, a reply mentioning anyone is still the answer")
	}
}
