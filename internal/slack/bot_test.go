package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	other = "U0OTHERUSER"
	shaA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// 2026-10-14 12:00 UTC is 08:00 in New York: a digest boundary.
var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

type botEnv struct {
	t    *testing.T
	ctx  context.Context
	f    *fakeSlack
	st   *store.Store
	clk  *clock.Fake
	b    *Bot
	sl   *sleeper
	logs *syncBuffer
	dir  string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newBotEnv(t *testing.T) *botEnv {
	t.Helper()
	clk := clock.NewFake(t0)
	dir := t.TempDir()
	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &botEnv{t: t, ctx: context.Background(), f: newFakeSlack(t), st: st, clk: clk, sl: &sleeper{clk: clk}, logs: &syncBuffer{}, dir: dir}
	e.b = e.newBot()
	e.b.Attach()
	return e
}

// newBot builds a bot over the same store and fake Slack, as a restarted loopd would.
func (e *botEnv) newBot() *Bot {
	api := NewAPI(e.f.baseURL(), testBotToken, Channels{ChannelLoop, ChannelDigest, ChannelPlan})
	api.Sleep = e.sl.Sleep
	b, err := New(Config{Store: e.st, API: api, Clock: e.clk, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Sleep: e.sl.Sleep})
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *botEnv) drain() { e.b.Drain(e.ctx) }

// calls drains the fan-out and returns the Slack calls made, checking the rules every message
// must keep: an allowed channel, at most 2,000 characters, no token.
func (e *botEnv) calls() []call {
	e.t.Helper()
	e.drain()
	cs := e.f.take()
	for _, c := range cs {
		if c.Method == "apps.connections.open" {
			continue
		}
		ch, _ := c.Body["channel"].(string)
		if c.Method != "views.open" && ch != ChannelLoop && ch != ChannelDigest && ch != ChannelPlan {
			e.t.Errorf("%s to channel %q", c.Method, ch)
		}
		raw, _ := json.Marshal(c.Body)
		if strings.Contains(string(raw), testBotToken) {
			e.t.Errorf("%s carries the token", c.Method)
		}
		if txt, _ := c.Body["text"].(string); utf8.RuneCountInString(txt) > MaxChars {
			e.t.Errorf("%s text has %d characters", c.Method, utf8.RuneCountInString(txt))
		}
		for _, blk := range blocksOf(c) {
			if t, ok := blk["text"].(map[string]any); ok {
				if s, _ := t["text"].(string); utf8.RuneCountInString(s) > MaxChars {
					e.t.Errorf("%s block has %d characters", c.Method, utf8.RuneCountInString(s))
				}
			}
		}
	}
	return cs
}

func blocksOf(c call) []map[string]any {
	raw, _ := json.Marshal(c.Body["blocks"])
	var out []map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// allText is a message's text and the text of its blocks.
func allText(c call) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprint(c.Body["text"]))
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.Encode(c.Body["blocks"])
	return sb.String()
}

func only(t *testing.T, cs []call, method string) []call {
	t.Helper()
	var out []call
	for _, c := range cs {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// actionIDs lists the action_ids of a message's interactive elements.
func actionIDs(c call) []string {
	var ids []string
	for _, b := range blocksOf(c) {
		if acc, ok := b["accessory"].(map[string]any); ok {
			ids = append(ids, fmt.Sprint(acc["action_id"]))
		}
		if els, ok := b["elements"].([]any); ok {
			for _, el := range els {
				if m, ok := el.(map[string]any); ok && m["action_id"] != nil {
					ids = append(ids, fmt.Sprint(m["action_id"]))
				}
			}
		}
	}
	return ids
}

func elementValue(c call, actionID string) string {
	for _, b := range blocksOf(c) {
		if els, ok := b["elements"].([]any); ok {
			for _, el := range els {
				if m, ok := el.(map[string]any); ok && m["action_id"] == actionID {
					return fmt.Sprint(m["value"])
				}
			}
		}
	}
	return ""
}

func (e *botEnv) item(id int64) store.Detail {
	e.t.Helper()
	d, err := e.st.GetItem(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *botEnv) add(title string) int64 {
	e.t.Helper()
	it, err := e.st.AddItem(e.ctx, "user:alex", store.NewItem{Title: title, Repo: "jarvis-server"})
	if err != nil {
		e.t.Fatal(err)
	}
	return it.ID
}

// runner claims the item's stage and finishes it with an outcome.
func (e *botEnv) runner(id int64, agent string, outcome pipeline.Outcome, fields map[string]string, paths ...string) {
	e.t.Helper()
	cl, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: agent, Capabilities: pipeline.Resources})
	if err != nil || cl == nil || cl.Item.ID != id {
		e.t.Fatalf("claim: %+v %v", cl, err)
	}
	if _, err := e.st.AddEvidence(e.ctx, "devbox", id, "", "note.txt", strings.NewReader("evidence")); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Advance(e.ctx, "devbox", id, outcome, fields, paths); err != nil {
		e.t.Fatalf("advance %s: %v", outcome, err)
	}
}

func (e *botEnv) toNeedsYou(id int64, question string) {
	e.runner(id, "a1", pipeline.Question, map[string]string{"question": question})
}

func (e *botEnv) toHeld(id int64, sha string) {
	e.t.Helper()
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "auth", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.PROpened, map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": sha},
		"M internal/modules/auth/session.go")
	e.runner(id, "reviewer", pipeline.Approved, map[string]string{"summary": "tests cover it", "head_sha": sha})
	e.runner(id, "lander", pipeline.Hold, map[string]string{"reason": "on the hold list", "head_sha": sha}, "M internal/modules/auth/session.go")
}

func (e *botEnv) thread(id int64) string {
	e.t.Helper()
	ch, ts, err := e.st.SlackThread(e.ctx, id)
	if err != nil || ch != ChannelLoop || ts == "" {
		e.t.Fatalf("item %d thread %q %q %v", id, ch, ts, err)
	}
	return ts
}

func envelope(typ string, payload any) Envelope {
	raw, _ := json.Marshal(payload)
	return Envelope{EnvelopeID: "env", Type: typ, Payload: raw}
}

var eventSeq int

// reply is a thread reply in #loop from user.
func (e *botEnv) reply(user, threadTS, text string) {
	eventSeq++
	e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": fmt.Sprintf("Ev%d", eventSeq), "event": map[string]any{
		"type": "message", "channel": ChannelLoop, "user": user, "text": text, "ts": fmt.Sprintf("1800000000.%06d", eventSeq), "thread_ts": threadTS}}))
}

// press is a block action from user on a message.
func (e *botEnv) press(user, channel, msgTS, actionID, blockID, value string) {
	act := map[string]any{"action_id": actionID, "block_id": blockID, "value": value}
	if strings.HasPrefix(actionID, "priority") || actionID == "agents_select" {
		act = map[string]any{"action_id": actionID, "block_id": blockID, "selected_option": map[string]any{"value": value}}
	}
	e.b.HandleEnvelope(e.ctx, envelope("interactive", map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user}, "channel": map[string]any{"id": channel}, "trigger_id": "trig-" + user,
		"container": map[string]any{"channel_id": channel, "message_ts": msgTS},
		"message":   map[string]any{"ts": msgTS}, "actions": []any{act}}))
}

// command runs /loop and returns the ephemeral answer.
func (e *botEnv) command(user, text string) string {
	p := e.b.HandleCommand(e.ctx, envelope("slash_commands", map[string]any{"command": "/loop", "text": text, "user_id": user, "channel_id": ChannelLoop}))
	if p["response_type"] != "ephemeral" {
		e.t.Errorf("/loop %s answered %v, want ephemeral", text, p["response_type"])
	}
	return fmt.Sprint(p["text"])
}

func (e *botEnv) kinds(id int64) []string {
	var out []string
	for _, ev := range e.item(id).Events {
		out = append(out, ev.Kind)
	}
	return out
}

func lastEvent(d store.Detail) store.Event { return d.Events[len(d.Events)-1] }

// ---- threads ----

func TestItemThread(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("timers <!channel> drift")

	cs := e.calls()
	posts := only(t, cs, "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("calls %+v", cs)
	}
	p := posts[0]
	if p.Body["channel"] != ChannelLoop || p.Body["thread_ts"] != nil {
		t.Fatalf("parent %+v", p.Body)
	}
	txt := allText(p)
	for _, want := range []string{fmt.Sprintf("#%d", id), "P3", "intake", "user", "Repo jarvis-server", "&lt;!channel&gt;"} {
		if !strings.Contains(txt, want) {
			t.Errorf("parent lacks %q: %s", want, txt)
		}
	}
	if strings.Contains(txt, "<!channel>") {
		t.Error("a title became a channel ping")
	}
	if ids := actionIDs(p); len(ids) != 1 || ids[0] != "priority" {
		t.Fatalf("parent actions %v", ids)
	}
	if !strings.Contains(fmt.Sprint(blocksOf(p)[0]["accessory"]), "initial_option:map[text:map[text:P3") {
		t.Fatalf("dropdown not at P3: %v", blocksOf(p)[0]["accessory"])
	}
	ts := e.thread(id)

	// needs_you: replies in the thread (the claim, the evidence, then the question with a
	// mention); the parent's stage line is kept current.
	e.toNeedsYou(id, "Which timer?")
	cs = e.calls()
	replies := only(t, cs, "chat.postMessage")
	if len(replies) != 3 {
		t.Fatalf("replies %+v", replies)
	}
	for _, r := range replies {
		if r.Body["thread_ts"] != ts {
			t.Fatalf("reply outside the thread: %+v", r.Body)
		}
	}
	if !strings.Contains(allText(replies[1]), "Evidence 1 attached") || strings.Contains(allText(replies[1]), "note.txt") {
		t.Fatalf("evidence reply %s", allText(replies[1]))
	}
	q := allText(replies[2])
	if !strings.Contains(q, "<@"+Approver+">") || !strings.Contains(q, "Which timer?") {
		t.Fatalf("question reply %s", q)
	}
	ups := only(t, cs, "chat.update")
	if len(ups) == 0 || ups[len(ups)-1].Body["ts"] != ts || !strings.Contains(allText(ups[len(ups)-1]), "needs-you") {
		t.Fatalf("parent updates %+v", ups)
	}
}

func TestLongTextIsClipped(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("long")
	e.calls()
	e.toNeedsYou(id, strings.Repeat("why ", 1000))
	cs := e.calls() // calls() checks the 2,000-character limit
	q := allText(only(t, cs, "chat.postMessage")[2])
	if !strings.Contains(q, fmt.Sprintf("full text on loopd: loop show %d", id)) {
		t.Fatalf("no pointer to the full text: %s", q)
	}
}

// A run the runner could not accept goes to the user as a runner error to retry or drop, not as
// a question to answer.
func TestFailedRunIsARunnerErrorNotAQuestion(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("refused")
	e.calls()
	e.runner(id, "a1", pipeline.Failed, map[string]string{"error": "the triage's result couldn't be accepted: evidence: evidence key.txt is not a regular file (again, after one retry). Last output: done"})
	q := allText(only(t, e.calls(), "chat.postMessage")[2])
	want := fmt.Sprintf("<@%s> #%d refused needs you: the triage's result couldn't be accepted: evidence: evidence key.txt is not a regular file (again, after one retry). Last output: done\nReply \"retry\" to run it again, or drop.", Approver, id)
	if !strings.HasPrefix(q, want) || strings.Contains(q, "to answer") {
		t.Fatalf("needs-you reply\n%s\nwant\n%s", q, want)
	}
}

func TestThreadIsCreatedForItemsThatHaveNone(t *testing.T) {
	e := newBotEnv(t)
	e.f.failNext("chat.postMessage", "channel_not_found") // the parent post fails once
	id := e.add("orphan")
	if _, err := e.st.GetItem(e.ctx, id); err != nil {
		t.Fatal("a Slack failure failed the pipeline action")
	}
	e.calls()
	if _, ts, _ := e.st.SlackThread(e.ctx, id); ts != "" {
		t.Fatal("thread recorded for a failed post")
	}
	if !strings.Contains(e.logs.String(), "channel_not_found") {
		t.Fatalf("failure not logged: %s", e.logs.String())
	}
	// The next event posts the parent first, then its reply.
	e.toNeedsYou(id, "q?")
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) < 2 || cs[0].Body["thread_ts"] != nil {
		t.Fatalf("calls %+v", cs)
	}
	e.thread(id)
}

func TestTransientFailuresAreRetriedBounded(t *testing.T) {
	e := newBotEnv(t)
	e.f.failNext("chat.postMessage", "http500", "http500")
	id := e.add("flaky")
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 3 {
		t.Fatalf("%d attempts, want 3", len(cs))
	}
	e.thread(id)
	// Always failing: gives up after a bounded number of attempts; the pipeline is unaffected.
	e.f.failNext("chat.postMessage", "http500", "http500", "http500", "http500", "http500")
	id2 := e.add("down")
	if n := len(only(t, e.calls(), "chat.postMessage")); n != 3 {
		t.Fatalf("%d attempts", n)
	}
	if e.item(id2).Item.Stage != pipeline.Triage {
		t.Fatal("pipeline changed")
	}
	if strings.Contains(e.logs.String(), testBotToken) {
		t.Fatal("token in the logs")
	}
}

// ---- needs_you answers ----

func TestAnswerByThreadReply(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("answer me")
	e.calls()
	ts := e.thread(id)
	e.toNeedsYou(id, "Which timer?")
	e.calls()

	// Someone else: refused, ephemeral, recorded; nothing changes.
	e.reply(other, ts, "the kitchen one")
	cs := e.calls()
	eph := only(t, cs, "chat.postEphemeral")
	if len(eph) != 1 || eph[0].Body["user"] != other || !strings.Contains(allText(eph[0]), "only the approver") {
		t.Fatalf("ephemeral %+v", cs)
	}
	d := e.item(id)
	if d.Item.Stage != pipeline.NeedsYou || lastEvent(d).Kind != "refused" || lastEvent(d).Actor != "slack:"+other {
		t.Fatalf("after refusal: %s %+v", d.Item.Stage, lastEvent(d))
	}

	// The bot's own messages and edits are ignored.
	e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": "EvBot", "event": map[string]any{
		"type": "message", "channel": ChannelLoop, "bot_id": "B0CA6SRV664", "text": "the bot", "ts": "1.5", "thread_ts": ts}}))
	if e.item(id).Item.Stage != pipeline.NeedsYou {
		t.Fatal("the bot answered itself")
	}

	// The approver answers.
	e.reply(Approver, ts, "<@U0BOT> the kitchen one")
	d = e.item(id)
	if d.Item.Stage != pipeline.Triage {
		t.Fatalf("stage %s", d.Item.Stage)
	}
	ev := lastEvent(d)
	if ev.Kind != "answered" || ev.Actor != "slack:"+Approver || ev.Data["answer"] != "the kitchen one" {
		t.Fatalf("event %+v", ev)
	}
	if r := only(t, e.calls(), "chat.postMessage"); len(r) != 1 || !strings.Contains(allText(r[0]), "the kitchen one") {
		t.Fatalf("answered reply %+v", r)
	}

	// Replies in a thread that is not waiting on anything are just conversation.
	e.reply(other, ts, "nice")
	if cs := e.calls(); len(cs) != 0 {
		t.Fatalf("chatter answered: %+v", cs)
	}
}

// ---- held ----

func heldSetup(t *testing.T) (*botEnv, int64, string) {
	e := newBotEnv(t)
	id := e.add("auth fix")
	e.calls()
	ts := e.thread(id)
	e.toHeld(id, shaA)
	return e, id, ts
}

func TestHeldMessageHasButtons(t *testing.T) {
	e, id, ts := heldSetup(t)
	cs := only(t, e.calls(), "chat.postMessage")
	// Replies describe the event as it happened, even when posted after the item moved on.
	var sawTriage bool
	for _, c := range cs {
		if txt := allText(c); strings.HasPrefix(txt, "Reproduced:") {
			sawTriage = true
			if !strings.Contains(txt, "risk normal") {
				t.Errorf("triage reply shows the later risk: %s", txt)
			}
		}
	}
	if !sawTriage {
		t.Error("no triage reply")
	}
	held := cs[len(cs)-1]
	if held.Body["thread_ts"] != ts {
		t.Fatalf("held message outside the thread")
	}
	txt := allText(held)
	for _, want := range []string{"<@" + Approver + ">", "jarvis-server/pull/142", "aaaaaaa", "auth: M internal/modules/auth/session.go"} {
		if !strings.Contains(txt, want) {
			t.Errorf("held message lacks %q: %s", want, txt)
		}
	}
	ids := actionIDs(held)
	if strings.Join(ids, ",") != "approve,keep_holding,rebuild,drop" {
		t.Fatalf("buttons %v", ids)
	}
	if v := elementValue(held, "approve"); v != fmt.Sprintf("%d:%s", id, shaA) {
		t.Fatalf("approve value %q", v)
	}
}

func TestReplyApprovalWaitsTwoMinutes(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	e.b.Tick(e.ctx) // first tick: starts the digest clock, posts nothing

	e.reply(Approver, ts, "yea, removing that old test is fine")
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 {
		t.Fatalf("echo %+v", cs)
	}
	echo := allText(cs[0])
	if !strings.Contains(echo, "Approved by you: 'yea, removing that old test is fine' for `aaaaaaa`. Merging in 2 minutes.") {
		t.Fatalf("echo %s", echo)
	}
	if ids := actionIDs(cs[0]); len(ids) != 1 || ids[0] != "cancel_approval" {
		t.Fatalf("echo buttons %v", ids)
	}
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("approved before the window ended")
	}
	e.clk.Advance(2*time.Minute - time.Second)
	e.b.Tick(e.ctx)
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("approved early")
	}
	e.clk.Advance(time.Second)
	e.b.Tick(e.ctx)
	d := e.item(id)
	if d.Item.Stage != pipeline.Land || d.Item.Approval == nil || d.Item.Approval.HeadSHA != shaA ||
		d.Item.Approval.Words != "yea, removing that old test is fine" || d.Item.Approval.By != "slack:"+Approver {
		t.Fatalf("after the window: %s %+v", d.Item.Stage, d.Item.Approval)
	}
	cs = e.calls()
	if len(only(t, cs, "chat.update")) == 0 {
		t.Fatal("the echo kept its Cancel button")
	}
	// It fires once.
	e.clk.Advance(time.Hour)
	e.b.Tick(e.ctx)
	if n := strings.Count(strings.Join(e.kinds(id), ","), "approved"); n != 1 {
		t.Fatalf("approved %d times", n)
	}
}

func TestCancelStopsTheReplyApproval(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	e.reply(Approver, ts, "ship it")
	echo := only(t, e.calls(), "chat.postMessage")[0]
	pid := elementValue(echo, "cancel_approval")
	echoTS := "echo-ts"

	// Someone else cannot cancel it.
	e.press(other, ChannelLoop, echoTS, "cancel_approval", "pending", pid)
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 {
		t.Fatalf("no refusal")
	}
	e.press(Approver, ChannelLoop, echoTS, "cancel_approval", "pending", pid)
	if k := lastEvent(e.item(id)).Kind; k != "approval_cancelled" {
		t.Fatalf("last event %s", k)
	}
	e.calls()
	e.clk.Advance(5 * time.Minute)
	e.b.Tick(e.ctx)
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("approved after Cancel")
	}
}

func TestKeepHoldingCancelsAPendingReplyApproval(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	e.reply(Approver, ts, "ok")
	e.calls()
	e.press(Approver, ChannelLoop, "held-ts", "keep_holding", "held", fmt.Sprintf("%d:%s", id, shaA))
	if k := lastEvent(e.item(id)).Kind; k != "held" {
		t.Fatalf("last event %s", k)
	}
	e.clk.Advance(3 * time.Minute)
	e.b.Tick(e.ctx)
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("Keep holding did not cancel the pending approval")
	}
}

func TestNonApprovalReplies(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	for _, r := range []string{"looks fine?", "don't merge yet", "ok wait", "no", "is the test still there", "hold on", "looks good", "let me check"} {
		e.reply(Approver, ts, r)
		cs := only(t, e.calls(), "chat.postMessage")
		if len(cs) != 1 || !strings.Contains(allText(cs[0]), "Not taken as an approval; the item stays held. Reply 'approve' or use the button, or say what to change to send it back to build.") {
			t.Fatalf("%q: %+v", r, cs)
		}
	}
	// Someone else's "approve": refused and recorded.
	e.reply(other, ts, "approve")
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 {
		t.Fatal("no refusal")
	}
	e.clk.Advance(time.Hour)
	e.b.Tick(e.ctx)
	if d := e.item(id); d.Item.Stage != pipeline.Held || lastEvent(d).Kind != "refused" {
		t.Fatalf("stage %s, last %s", d.Item.Stage, lastEvent(d).Kind)
	}
}

// A reply that is neither an approval nor a question or a "wait" is the user's note for the
// builder: the item goes back to build with it, the PR and the hold kept.
func TestHeldReplySendsBackToBuild(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	e.reply(other, ts, "rebuild it")
	if d := e.item(id); d.Item.Stage != pipeline.Held || lastEvent(d).Kind != "refused" {
		t.Fatalf("someone else sent it back: %s", d.Item.Stage)
	}
	e.calls()
	e.reply(Approver, ts, "rebase and fix the typo in the heading")
	d := e.item(id)
	ev := lastEvent(d)
	if d.Item.Stage != pipeline.Build || d.Item.PR != 142 || ev.Kind != "rebuild" || ev.Actor != "slack:"+Approver ||
		ev.Data["notes"] != "rebase and fix the typo in the heading" {
		t.Fatalf("stage %s, last %+v", d.Item.Stage, ev)
	}
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 || cs[0].Body["text"] != "Sent back to build by you: 'rebase and fix the typo in the heading'." || cs[0].Body["thread_ts"] != ts {
		t.Fatalf("posts %+v", cs)
	}
}

// A change request wins over consent words: "merge it after adding a test" and "ok, rebuild" go
// back to build with the reply as the notes, and no approval starts; "merge it please" and "lgtm"
// still start one.
func TestHeldChangeRequestBeatsConsent(t *testing.T) {
	for _, r := range []string{"merge it after adding a test", "ok, rebuild", "okay fix the typo", "yes, but rebase first", "sure, use the store's clock"} {
		e, id, ts := heldSetup(t)
		e.calls()
		e.reply(Approver, ts, r)
		d := e.item(id)
		if ev := lastEvent(d); d.Item.Stage != pipeline.Build || ev.Kind != "rebuild" || ev.Data["notes"] != r {
			t.Fatalf("%q: stage %s, last %+v", r, d.Item.Stage, ev)
		}
		for _, c := range only(t, e.calls(), "chat.postMessage") {
			if strings.Contains(allText(c), "Approved by you") {
				t.Fatalf("%q: approval echo %+v", r, c)
			}
		}
		e.clk.Advance(time.Hour)
		e.b.Tick(e.ctx)
		if d := e.item(id); d.Item.Stage != pipeline.Build || d.Item.Approval != nil {
			t.Fatalf("%q: stage %s approval %+v", r, d.Item.Stage, d.Item.Approval)
		}
	}
	for _, r := range []string{"merge it please", "lgtm", "merge it", "ship it", "approve"} {
		e, id, ts := heldSetup(t)
		e.calls()
		e.reply(Approver, ts, r)
		var echo bool
		for _, c := range only(t, e.calls(), "chat.postMessage") {
			if strings.Contains(allText(c), "Approved by you") {
				echo = true
			}
		}
		if !echo {
			t.Fatalf("%q: no approval echo", r)
		}
		if d := e.item(id); d.Item.Stage != pipeline.Held {
			t.Fatalf("%q: stage %s", r, d.Item.Stage)
		}
	}
}

// A pending reply approval is cancelled when a note sends the item back to build.
func TestHeldNoteCancelsPendingApproval(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.reply(Approver, ts, "lgtm")
	e.reply(Approver, ts, "actually, rebuild with the new name")
	e.clk.Advance(3 * time.Minute)
	e.b.Tick(e.ctx)
	if d := e.item(id); d.Item.Stage != pipeline.Build || d.Item.Approval != nil {
		t.Fatalf("stage %s approval %+v", d.Item.Stage, d.Item.Approval)
	}
}

func TestButtonApprovalIsImmediate(t *testing.T) {
	e, id, _ := heldSetup(t)
	e.calls()
	val := fmt.Sprintf("%d:%s", id, shaA)
	e.press(other, ChannelLoop, "held-ts", "approve", "held", val)
	if d := e.item(id); d.Item.Stage != pipeline.Held || lastEvent(d).Kind != "refused" {
		t.Fatalf("someone else approved: %s", d.Item.Stage)
	}
	e.calls()
	e.press(Approver, ChannelLoop, "held-ts", "approve", "held", val)
	d := e.item(id)
	if d.Item.Stage != pipeline.Land || d.Item.Approval.Words != "Approve merge" || d.Item.Approval.HeadSHA != shaA {
		t.Fatalf("%s %+v", d.Item.Stage, d.Item.Approval)
	}
	cs := e.calls()
	if up := only(t, cs, "chat.update"); len(up) == 0 || up[0].Body["ts"] != "held-ts" {
		t.Fatalf("held message not updated: %+v", cs)
	}
}

func TestHeadMovedDuringTheWindow(t *testing.T) {
	e, id, ts := heldSetup(t)
	e.calls()
	e.reply(Approver, ts, "lgtm")
	e.calls()
	// Meanwhile: approved at A from the CLI, and land holds again at a new head B.
	if _, err := e.st.Approve(e.ctx, "user:alex", id, shaA, "cli"); err != nil {
		t.Fatal(err)
	}
	e.runner(id, "lander", pipeline.Hold, map[string]string{"reason": "main moved", "head_sha": shaB}, "M internal/modules/auth/session.go")
	e.calls()
	e.clk.Advance(2 * time.Minute)
	e.b.Tick(e.ctx)
	d := e.item(id)
	if d.Item.Stage != pipeline.Held || d.Item.HeadSHA != shaB || lastEvent(d).Kind != "refused" {
		t.Fatalf("stage %s head %s last %s", d.Item.Stage, d.Item.HeadSHA, lastEvent(d).Kind)
	}
	var said bool
	for _, c := range only(t, e.calls(), "chat.postMessage") {
		if txt := allText(c); strings.Contains(txt, "Not approved") && strings.Contains(txt, "bbbbbbb") {
			said = true
		}
	}
	if !said {
		t.Fatal("the bot did not say the head moved")
	}
	// An old button for A is refused the same way.
	e.press(Approver, ChannelLoop, "old", "approve", "held", fmt.Sprintf("%d:%s", id, shaA))
	if e.item(id).Item.Stage != pipeline.Held {
		t.Fatal("approved a stale head")
	}
}

// ---- priority ----

func TestPriorityDropdown(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("prio")
	e.calls()
	ts := e.thread(id)
	block := fmt.Sprintf("item:%d", id)
	e.press(other, ChannelLoop, ts, "priority", block, "P1")
	if d := e.item(id); d.Item.Priority != "P3" || lastEvent(d).Kind != "refused" {
		t.Fatalf("someone else changed it: %s", d.Item.Priority)
	}
	e.calls()
	e.press(Approver, ChannelLoop, ts, "priority", block, "P1")
	if p := e.item(id).Item.Priority; p != "P1" {
		t.Fatalf("priority %s", p)
	}
	ups := only(t, e.calls(), "chat.update")
	if len(ups) == 0 || !strings.Contains(fmt.Sprint(blocksOf(ups[len(ups)-1])[0]["accessory"]), "initial_option:map[text:map[text:P1") {
		t.Fatalf("parent not updated: %+v", ups)
	}
}

// ---- control ----

func TestControlMessage(t *testing.T) {
	e := newBotEnv(t)
	if err := e.b.EnsureControl(e.ctx); err != nil {
		t.Fatal(err)
	}
	cs := e.calls()
	posts := only(t, cs, "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["channel"] != ChannelDigest || !strings.Contains(allText(posts[0]), "Active agents: 4 of 6") {
		t.Fatalf("control post %+v", cs)
	}
	if ids := actionIDs(posts[0]); strings.Join(ids, ",") != "agents_select,agents_minus,agents_plus" {
		t.Fatalf("control actions %v", ids)
	}
	pins := only(t, cs, "pins.add")
	st, _ := e.st.SlackState(e.ctx)
	if len(pins) != 1 || st.ControlTS == "" || pins[0].Body["timestamp"] != st.ControlTS {
		t.Fatalf("pins %+v state %+v", pins, st)
	}

	// A restart updates the same message.
	b2 := e.newBot()
	if err := b2.EnsureControl(e.ctx); err != nil {
		t.Fatal(err)
	}
	cs = e.f.take()
	if len(only(t, cs, "chat.postMessage")) != 0 || len(only(t, cs, "chat.update")) != 1 || only(t, cs, "chat.update")[0].Body["ts"] != st.ControlTS {
		t.Fatalf("restart %+v", cs)
	}

	// Any control change updates it: the CLI...
	two := 2
	if _, err := e.st.SetControl(e.ctx, "user:alex", store.ControlPatch{ActiveAgents: &two}); err != nil {
		t.Fatal(err)
	}
	ups := only(t, e.calls(), "chat.update")
	if len(ups) != 1 || ups[0].Body["ts"] != st.ControlTS || !strings.Contains(allText(ups[0]), "Active agents: 2 of 6") {
		t.Fatalf("after CLI %+v", ups)
	}
	// ...the buttons and the dropdown (approver only)...
	e.press(Approver, ChannelDigest, st.ControlTS, "agents_plus", "control", "")
	e.press(Approver, ChannelDigest, st.ControlTS, "agents_plus", "control", "")
	e.press(Approver, ChannelDigest, st.ControlTS, "agents_minus", "control", "")
	if c, _ := e.st.Control(e.ctx); c.ActiveAgents != 3 {
		t.Fatalf("after +/+/-: %d", c.ActiveAgents)
	}
	e.press(Approver, ChannelDigest, st.ControlTS, "agents_select", "control", "6")
	e.press(Approver, ChannelDigest, st.ControlTS, "agents_plus", "control", "") // stays at 6
	if c, _ := e.st.Control(e.ctx); c.ActiveAgents != 6 {
		t.Fatalf("after select: %d", c.ActiveAgents)
	}
	e.calls()
	e.press(other, ChannelDigest, st.ControlTS, "agents_select", "control", "0")
	if c, _ := e.st.Control(e.ctx); c.ActiveAgents != 6 {
		t.Fatal("someone else changed active agents")
	}
	if ev, ok, _ := e.st.LastEvent(e.ctx, "refused"); !ok || ev.Actor != "slack:"+other {
		t.Fatalf("refusal not recorded: %+v", ev)
	}
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 {
		t.Fatal("no ephemeral refusal")
	}
	// ...and stop.
	e.command(Approver, "stop")
	ups = only(t, e.calls(), "chat.update")
	if len(ups) != 1 || !strings.Contains(allText(ups[0]), "Active agents: 6 of 6 (stopped)") {
		t.Fatalf("after stop %+v", ups)
	}
}

func TestControlMessageWithoutPinScope(t *testing.T) {
	e := newBotEnv(t)
	e.f.failNext("pins.add", "missing_scope")
	if err := e.b.EnsureControl(e.ctx); err != nil {
		t.Fatalf("a failed pin failed the control message: %v", err)
	}
	if st, _ := e.st.SlackState(e.ctx); st.ControlTS == "" {
		t.Fatal("control message not recorded")
	}
	if !strings.Contains(e.logs.String(), "missing_scope") {
		t.Fatalf("pin failure not logged: %s", e.logs.String())
	}
	// Everything else works.
	id := e.add("still works")
	e.calls()
	e.thread(id)
}

func TestControlMessageDeletedIsPostedAgain(t *testing.T) {
	e := newBotEnv(t)
	e.b.EnsureControl(e.ctx)
	e.f.take()
	e.f.failNext("chat.update", "message_not_found")
	if err := e.newBot().EnsureControl(e.ctx); err != nil {
		t.Fatal(err)
	}
	if posts := only(t, e.f.take(), "chat.postMessage"); len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
}

// ---- /loop and mentions ----

// A usage limit turns the active agents to 0; the user hears about it in #loop-digest with the
// reset time and a mention (DESIGN.md, Usage control), and the control message shows 0.
func TestUsageLimitIsAnnouncedInTheDigestChannel(t *testing.T) {
	e := newBotEnv(t)
	if err := e.b.EnsureControl(e.ctx); err != nil {
		t.Fatal(err)
	}
	id := e.add("limited")
	e.calls()
	e.runner(id, "a1", pipeline.UsageLimit, map[string]string{"error": "You've hit your session limit", "reset": "3:45pm"})
	cs := e.calls()
	var digest []call
	for _, c := range only(t, cs, "chat.postMessage") {
		if c.Body["channel"] == ChannelDigest {
			digest = append(digest, c)
		}
	}
	if len(digest) != 1 {
		t.Fatalf("digest-channel posts %+v", digest)
	}
	txt := allText(digest[0])
	for _, want := range []string{"<@" + Approver + ">", "Usage limit", "session limit", "resets 3:45pm", fmt.Sprintf("#%d", id), "0"} {
		if !strings.Contains(txt, want) {
			t.Errorf("usage-limit post lacks %q: %s", want, txt)
		}
	}
	st, _ := e.st.SlackState(e.ctx)
	var shown bool
	for _, u := range only(t, cs, "chat.update") {
		if u.Body["ts"] == st.ControlTS && strings.Contains(allText(u), "Active agents: 0 of 6") {
			shown = true
		}
	}
	if !shown {
		t.Fatalf("the control message does not show 0: %+v", only(t, cs, "chat.update"))
	}
	// Other control changes are not announced.
	two := 2
	e.st.SetControl(e.ctx, "user:alex", store.ControlPatch{ActiveAgents: &two})
	for _, c := range only(t, e.calls(), "chat.postMessage") {
		t.Fatalf("a plain control change posted %+v", c.Body)
	}
}

func TestLoopCommands(t *testing.T) {
	e := newBotEnv(t)
	out := e.command(Approver, "add this is a high priority feature")
	rows, _ := e.st.ListItems(e.ctx, "", "")
	if len(rows) != 1 || rows[0].Priority != "P2" || rows[0].Title != "this is a feature" || !strings.Contains(out, "#1") {
		t.Fatalf("add: %q %+v", out, rows)
	}
	if d := e.item(rows[0].ID); d.Item.Source != "user" || lastEvent(d).Actor != "slack:"+Approver {
		t.Fatalf("add event %+v", lastEvent(d))
	}
	e.command(Approver, "add urgent: main is red")

	if out := e.command(other, "status"); !strings.Contains(out, "this is a feature") || !strings.Contains(out, "intake") {
		t.Fatalf("status %q", out)
	}
	if out := e.command(other, "show 1"); !strings.Contains(out, "#1") || !strings.Contains(out, "P2") || !strings.Contains(out, "risk normal") || !strings.Contains(out, "repo not set, triage will pick") {
		t.Fatalf("show %q", out)
	}
	if out := e.command(other, "show 99"); !strings.Contains(out, "no item 99") {
		t.Fatalf("show missing %q", out)
	}
	if out := e.command(Approver, "priority 1 P5"); e.item(1).Item.Priority != "P5" {
		t.Fatalf("priority %q", out)
	}
	if out := e.command(Approver, "priority 1 P9"); !strings.Contains(out, "P1 to P5") {
		t.Fatalf("bad priority %q", out)
	}
	e.command(Approver, "agents 5")
	if c, _ := e.st.Control(e.ctx); c.ActiveAgents != 5 {
		t.Fatal("agents")
	}
	if out := e.command(Approver, "agents 7"); !strings.Contains(out, "0 to 6") {
		t.Fatalf("agents 7 %q", out)
	}
	e.command(Approver, "stop")
	if c, _ := e.st.Control(e.ctx); !c.Stopped {
		t.Fatal("stop")
	}
	e.command(Approver, "resume")
	if c, _ := e.st.Control(e.ctx); c.Stopped {
		t.Fatal("resume")
	}
	e.command(Approver, "usage 42%")
	if ev, ok, _ := e.st.LastEvent(e.ctx, "usage"); !ok || ev.Data["percent"] != float64(42) {
		t.Fatalf("usage %+v", ev)
	}

	// help lists every command; anything unknown says so, then shows the same help. Anyone may ask.
	help := e.command(other, "help")
	for _, want := range []string{"/loop help", "/loop add", "/loop status", "/loop show", "/loop priority", "/loop agents", "/loop stop", "/loop resume", "/loop usage"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %s: %q", want, help)
		}
	}
	if strings.Contains(help, "Unknown") {
		t.Errorf("help says unknown: %q", help)
	}
	if out := e.command(other, ""); out != help {
		t.Errorf("bare /loop %q, want the help", out)
	}
	if out := e.command(other, "bogus"); !strings.HasPrefix(out, "Unknown command `bogus`.") || !strings.Contains(out, "/loop help") {
		t.Errorf("unknown %q", out)
	}

	// The approver-only commands, from someone else.
	for _, c := range []string{"add sneaky", "priority 1 P1", "agents 6", "stop", "resume", "usage 10%"} {
		before, _ := e.st.Control(e.ctx)
		out := e.command(other, c)
		if !strings.Contains(out, "only the approver") {
			t.Errorf("%s by someone else: %q", c, out)
		}
		after, _ := e.st.Control(e.ctx)
		if before.ActiveAgents != after.ActiveAgents || before.Stopped != after.Stopped {
			t.Errorf("%s changed control", c)
		}
		if ev, ok, _ := e.st.LastEvent(e.ctx, "refused"); !ok || ev.Actor != "slack:"+other {
			t.Errorf("%s: refusal not recorded", c)
		}
	}
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 2 || e.item(1).Item.Priority != "P5" {
		t.Fatal("someone else changed items")
	}
	if out := e.command(other, "dance"); !strings.Contains(out, "/loop add") {
		t.Fatalf("help %q", out)
	}
	if out := e.command(Approver, "add urgent"); !strings.Contains(out, "/loop add") {
		t.Fatalf("empty add %q", out)
	}
}

func TestMentionAddsAnItem(t *testing.T) {
	e := newBotEnv(t)
	mention := func(user, channel, text string) {
		eventSeq++
		e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": fmt.Sprintf("Ev%d", eventSeq), "event": map[string]any{
			"type": "app_mention", "channel": channel, "user": user, "text": text, "ts": fmt.Sprintf("1.%d", eventSeq)}}))
	}
	mention(Approver, "CELSEWHERE", "<@U0BOT> low priority: tidy the logs")
	rows, _ := e.st.ListItems(e.ctx, "", "")
	if len(rows) != 1 || rows[0].Priority != "P4" || rows[0].Title != "tidy the logs" {
		t.Fatalf("rows %+v", rows)
	}
	// Slack's escapes are undone once: the title and the ask are the words as typed.
	mention(Approver, "CELSEWHERE", "<@U0BOT> fix the &lt;tool_call&gt; parsing &amp; retries")
	rows, _ = e.st.ListItems(e.ctx, "", "")
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		if d, _ := e.st.GetItem(e.ctx, r.ID); r.Title != "tidy the logs" && (d.Item.Title != "fix the <tool_call> parsing & retries" || d.Item.Ask != d.Item.Title) {
			t.Fatalf("escaped add: %+v", d.Item)
		}
	}
	// Someone else: refused and recorded; the ephemeral goes only to an allowed channel.
	mention(other, "CELSEWHERE", "<@U0BOT> add a thing")
	mention(other, ChannelLoop, "<@U0BOT> add a thing")
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 2 {
		t.Fatal("someone else added an item")
	}
	eph := only(t, e.calls(), "chat.postEphemeral") // calls() fails any post outside the two channels
	if len(eph) != 1 {
		t.Fatalf("ephemerals %+v", eph)
	}
	if ev, ok, _ := e.st.LastEvent(e.ctx, "refused"); !ok || ev.Actor != "slack:"+other {
		t.Fatal("refusal not recorded")
	}
}

// ---- digest ----

func TestDigest(t *testing.T) {
	e := newBotEnv(t)
	e.clk.Advance(30 * time.Minute) // 08:30 in New York
	e.b.Tick(e.ctx)                 // the first tick starts the clock at 08:00 and posts nothing
	if st, _ := e.st.SlackState(e.ctx); !st.LastDigest.Equal(t0) {
		t.Fatalf("last digest %v", st.LastDigest)
	}
	// One at a time, so each runner claim picks the item meant.
	idHeld := e.add("held item")
	e.toHeld(idHeld, shaA)
	idQ := e.add("asks")
	e.toNeedsYou(idQ, "what?")
	idNew := e.add("fresh item")
	e.calls()

	e.clk.Advance(3 * time.Hour) // 11:30: no boundary yet
	e.b.Tick(e.ctx)
	if posts := only(t, e.calls(), "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("early digest %+v", posts)
	}
	e.clk.Advance(30 * time.Minute) // 12:00
	e.b.Tick(e.ctx)
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || posts[0].Body["channel"] != ChannelDigest {
		t.Fatalf("digest %+v", posts)
	}
	txt := allText(posts[0])
	for _, want := range []string{"New (3)", fmt.Sprintf("#%d fresh item", idNew), "Held (1)", fmt.Sprintf("#%d held item", idHeld),
		"Needs you (1)", "Active agents: 4 of 6", "08:00–12:00"} {
		if !strings.Contains(txt, want) {
			t.Errorf("digest lacks %q: %s", want, txt)
		}
	}
	if strings.Contains(txt, "Merged") || strings.Contains(txt, "Failing") {
		t.Errorf("empty sections shown: %s", txt)
	}

	// No double post: again in the same window, or after a restart.
	e.clk.Advance(time.Minute)
	e.b.Tick(e.ctx)
	e.newBot().Tick(e.ctx)
	if posts := only(t, e.calls(), "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("double digest %+v", posts)
	}

	// Nothing to report: no message, but the window moves on.
	e.clk.Advance(4 * time.Hour)
	e.b.Tick(e.ctx)
	if posts := only(t, e.calls(), "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("empty digest %+v", posts)
	}
	if st, _ := e.st.SlackState(e.ctx); !st.LastDigest.Equal(t0.Add(8 * time.Hour)) {
		t.Fatalf("last digest %v", st.LastDigest)
	}

	// Merged, failing and the usage reading.
	e.command(Approver, "usage 42%")
	e.runner(idNew, "a1", pipeline.Failed, map[string]string{"error": "boom"})
	e.calls()
	e.clk.Advance(4 * time.Hour)
	e.b.Tick(e.ctx)
	posts = only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
	txt = allText(posts[0])
	for _, want := range []string{"Failing (1)", "Last /loop usage: 42%"} {
		if !strings.Contains(txt, want) {
			t.Errorf("digest lacks %q: %s", want, txt)
		}
	}
}

func TestDigestAlignsToNewYork(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 10, 14, 9, 59, 0, 0, ny), time.Date(2026, 10, 14, 8, 0, 0, 0, ny)},
		{time.Date(2026, 10, 14, 0, 0, 0, 0, ny), time.Date(2026, 10, 14, 0, 0, 0, 0, ny)},
		{time.Date(2026, 10, 14, 23, 59, 0, 0, ny), time.Date(2026, 10, 14, 20, 0, 0, 0, ny)},
		// The day the clocks go back (2026-11-01): boundaries stay on local hours.
		{time.Date(2026, 11, 1, 5, 0, 0, 0, ny), time.Date(2026, 11, 1, 4, 0, 0, 0, ny)},
	}
	for _, c := range cases {
		if got := digestBoundary(c.now, ny, 4); !got.Equal(c.want) {
			t.Errorf("boundary(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

// ---- through the socket ----

func TestAnswerThroughSocketMode(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("over the wire")
	e.calls()
	ts := e.thread(id)
	e.toNeedsYou(id, "which?")
	e.calls()

	handled := make(chan struct{}, 4)
	s := &Socket{
		Open: NewAPI(e.f.baseURL(), "xapp-test", nil).OpenConnection,
		OnEnvelope: func(ctx context.Context, env Envelope) {
			e.b.HandleEnvelope(ctx, env)
			handled <- struct{}{}
		},
		OnCommand: e.b.HandleCommand,
		Log:       slog.New(slog.NewTextHandler(e.logs, nil)),
		Sleep:     (&sleeper{}).Sleep,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	c := e.f.conn()
	e.f.send(c, map[string]any{"type": "hello"})
	msg := map[string]any{"envelope_id": "env-a", "type": "events_api", "payload": map[string]any{"event_id": "EvA", "event": map[string]any{
		"type": "message", "channel": ChannelLoop, "user": Approver, "text": "the left one", "ts": "2.0", "thread_ts": ts}}}
	e.f.send(c, msg)
	e.f.send(c, msg) // Slack's retry: acked, not handled again
	e.f.ack()
	e.f.ack()
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply was not handled")
	}
	if d := e.item(id); d.Item.Stage != pipeline.Triage || lastEvent(d).Data["answer"] != "the left one" {
		t.Fatalf("stage %s", d.Item.Stage)
	}
	e.f.send(c, map[string]any{"envelope_id": "env-s", "type": "slash_commands", "payload": map[string]any{
		"command": "/loop", "text": fmt.Sprintf("show %d", id), "user_id": Approver, "channel_id": ChannelLoop}})
	a := e.f.ack()
	if a["envelope_id"] != "env-s" || !strings.Contains(fmt.Sprint(a["payload"]), "the left one") {
		t.Fatalf("command ack %v", a)
	}
	select {
	case <-handled:
		t.Fatal("the duplicate was handled")
	default:
	}
}

func TestDigestThatCannotBePostedIsSkippedAfterRetries(t *testing.T) {
	e := newBotEnv(t)
	e.b.Tick(e.ctx)
	e.clk.Advance(time.Hour)
	e.add("something")
	e.calls()
	e.clk.Advance(3 * time.Hour)
	e.f.failNext("chat.postMessage", "not_in_channel", "not_in_channel", "not_in_channel")
	for i := 0; i < 3; i++ {
		if st, _ := e.st.SlackState(e.ctx); !st.LastDigest.Equal(t0) {
			t.Fatalf("attempt %d: window moved on before giving up", i)
		}
		e.b.Tick(e.ctx)
	}
	if st, _ := e.st.SlackState(e.ctx); !st.LastDigest.Equal(t0.Add(4 * time.Hour)) {
		t.Fatalf("last digest %v", st.LastDigest)
	}
	if !strings.Contains(e.logs.String(), "digest not posted") {
		t.Fatal("not logged")
	}
}

func TestGuardRefusedLineHidesTheCommand(t *testing.T) {
	e := newBotEnv(t)
	it, err := e.st.AddItem(e.ctx, "user:alex", store.NewItem{Title: "x", Repo: "jarvis-server"})
	if err != nil {
		t.Fatal(err)
	}
	ev := store.Event{ItemID: it.ID, Actor: "runner:devbox", Kind: "guard_refused",
		Data: map[string]any{"rule": "protected_path", "tool": "Bash", "target": "cat /home/alex/secret-host.lan"}}
	m, ok := e.b.describe(e.ctx, ev, store.Detail{Item: it})
	if !ok || !strings.Contains(m.Text, "Guard refused a Bash call (protected_path)") || strings.Contains(m.Text, "secret-host") {
		t.Fatalf("got %q", m.Text)
	}
}

// builder_fallback is one line in the thread, never a second needs-you message.
func TestBuilderFallbackLine(t *testing.T) {
	e := newBotEnv(t)
	it, err := e.st.AddItem(e.ctx, "user:alex", store.NewItem{Title: "x", Repo: "jarvis-server"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		data map[string]any
		want string
	}{
		{map[string]any{"from": "qwen", "to": "claude", "bounce_count": 2}, "Builder: qwen → claude for the rest of this item (second strike)."},
		{map[string]any{"from": "qwen", "to": "claude", "this_run_only": true, "reason": "runner devbox has no qwen config"}, "No qwen builder on this runner: this build runs with claude."},
	} {
		ev := store.Event{ItemID: it.ID, Actor: "runner:devbox", Kind: "builder_fallback", From: "needs_you", To: "needs_you", Data: c.data}
		m, ok := e.b.describe(e.ctx, ev, store.Detail{Item: it})
		if !ok || m.Text != c.want {
			t.Errorf("got %q, want %q", m.Text, c.want)
		}
	}
}

// unservable mentions the approver once with what no runner offers; servable is one line.
func TestUnservableLines(t *testing.T) {
	e := newBotEnv(t)
	it, err := e.st.AddItem(e.ctx, "user:alex", store.NewItem{Title: "Mac app", Repo: "jarvis-server"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind string
		want string
	}{
		{store.KindUnservable, "<@" + Approver + "> #1 Mac app waits in build, but no runner seen in the last 24h offers macos. Bring up a runner with it, or change the item's resources; it is claimed as soon as a runner offers it."},
		{store.KindServable, "A runner offers macos now; it can claim this from the build queue."},
	} {
		ev := store.Event{ItemID: it.ID, Actor: "loopd", Kind: c.kind, From: "build", To: "build", Data: map[string]any{"missing": []any{"macos"}}}
		m, ok := e.b.describe(e.ctx, ev, store.Detail{Item: it})
		if !ok || m.Text != c.want {
			t.Errorf("got %q, want %q", m.Text, c.want)
		}
	}
}

// /loop add sets the repo when the text names a configured one, with the priority words too;
// otherwise the repo stays empty for triage.
func TestAddRepoWords(t *testing.T) {
	e := newBotEnv(t)
	e.b.c.Repos = []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}
	out := e.command(Approver, "add jarvis-loop: show the repo in /loop show, high priority")
	d := e.item(1)
	if d.Item.Repo != "alexberardi/jarvis-loop" || d.Item.Title != "show the repo in /loop show" || d.Item.Priority != "P2" {
		t.Fatalf("add: %q %+v", out, d.Item)
	}
	if !strings.Contains(out, "Added #1 (P2, alexberardi/jarvis-loop)") {
		t.Fatalf("answer %q", out)
	}
	if out := e.command(other, "show 1"); !strings.Contains(out, "repo alexberardi/jarvis-loop") {
		t.Fatalf("show with a repo %q", out)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) == 0 || !strings.Contains(allText(posts[0]), "Repo alexberardi/jarvis-loop") {
		t.Fatalf("parent lacks the repo: %+v", posts)
	}
	e.command(Approver, "add timers drift")
	if d := e.item(2); d.Item.Repo != "" || d.Item.Title != "timers drift" {
		t.Fatalf("no repo words: %+v", d.Item)
	}
	if posts := only(t, e.calls(), "chat.postMessage"); len(posts) == 0 || !strings.Contains(allText(posts[0]), "Repo not set, triage will pick") {
		t.Fatalf("parent lacks the unset repo: %+v", posts)
	}
}

func TestRepoText(t *testing.T) {
	for _, c := range []struct {
		repo  string
		stage pipeline.Stage
		want  string
	}{
		{"alexberardi/jarvis-loop", pipeline.Build, "alexberardi/jarvis-loop"},
		{"a<b>", pipeline.Triage, "a&lt;b&gt;"},
		{"", pipeline.Triage, "not set, triage will pick"},
		{"", pipeline.NeedsYou, "not set"},
		{"", pipeline.Closed, "not set"},
	} {
		if got := repoText(pipeline.Item{Repo: c.repo, Stage: c.stage}); got != c.want {
			t.Errorf("repoText(%q, %s) = %q, want %q", c.repo, c.stage, got, c.want)
		}
	}
}

// "/loop add … after #1": the item waits on #1, says so in its thread header and /loop show, and
// goes to needs-you when #1 is dropped. An unknown id is refused.
func TestAddAfter(t *testing.T) {
	e := newBotEnv(t)
	e.add("a")
	out := e.command(Approver, "add show the plan link after #1, high priority")
	d := e.item(2)
	if d.Item.Title != "show the plan link" || d.Item.Priority != "P2" || len(d.Item.DependsOn) != 1 || d.Item.DependsOn[0] != 1 {
		t.Fatalf("add: %q %+v", out, d.Item)
	}
	if !strings.Contains(out, "Added #2 (P2, after #1)") {
		t.Fatalf("answer %q", out)
	}
	if out := e.command(other, "show 2"); !strings.Contains(out, "waiting on #1") {
		t.Fatalf("show %q", out)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) < 2 || !strings.Contains(allText(posts[1]), "Waiting on #1") || strings.Contains(allText(posts[0]), "Waiting on") {
		t.Fatalf("parent lacks waiting on: %+v", posts)
	}
	if out := e.command(Approver, "add more after #9"); !strings.Contains(out, "no item #9") {
		t.Fatalf("unknown dependency %q", out)
	}

	if _, err := e.st.Drop(e.ctx, "user:alex", 1, ""); err != nil {
		t.Fatal(err)
	}
	e.drain()
	ts := e.thread(2)
	var asked bool
	for _, c := range only(t, e.calls(), "chat.postMessage") {
		if c.Body["thread_ts"] == ts && strings.Contains(allText(c), "#2 show the plan link needs you: #1 it depends on was dropped: keep, change or drop this one?") {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("no needs-you question in #2's thread: %+v", e.calls())
	}
	if out := e.command(Approver, "add more after #1"); !strings.Contains(out, "#1 is closed") {
		t.Fatalf("closed dependency %q", out)
	}
}
