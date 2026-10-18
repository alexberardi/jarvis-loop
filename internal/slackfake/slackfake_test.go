package slackfake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
)

const (
	loopCh = slack.ChannelLoop
	bot    = "xoxb-fake-bot-token"
)

// client is the real Socket Mode client against the fake, recording what it was handed.
type client struct {
	mu   sync.Mutex
	envs []slack.Envelope
}

func (c *client) got() []slack.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slack.Envelope(nil), c.envs...)
}

func start(t *testing.T) (*slackfake.Server, *slack.API, *client) {
	t.Helper()
	f := slackfake.New()
	t.Cleanup(f.Close)
	c := &client{}
	ctx, cancel := context.WithCancel(context.Background())
	sock := &slack.Socket{
		Open: slack.NewAPI(f.BaseURL(), "xapp-fake", nil).OpenConnection,
		OnEnvelope: func(_ context.Context, e slack.Envelope) {
			c.mu.Lock()
			c.envs = append(c.envs, e)
			c.mu.Unlock()
		},
		OnCommand: func(_ context.Context, e slack.Envelope) map[string]any {
			var p map[string]any
			json.Unmarshal(e.Payload, &p)
			return map[string]any{"response_type": "ephemeral", "text": "you said " + p["text"].(string)}
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan struct{})
	go func() { defer close(done); sock.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := f.WaitConnected(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	return f, slack.NewAPI(f.BaseURL(), bot, slack.Channels{loopCh, slack.ChannelDigest}), c
}

// waitEnvs waits (real time, bounded) for the client to have handled n envelopes: the ack comes
// before handling.
func waitEnvs(t *testing.T, c *client, n int) []slack.Envelope {
	t.Helper()
	for i := 0; i < 500; i++ {
		if g := c.got(); len(g) >= n {
			return g
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handled %d envelopes, want %d", len(c.got()), n)
	return nil
}

func TestRecordsTheWorkspace(t *testing.T) {
	f, api, _ := start(t)
	ctx := context.Background()
	parent, err := api.PostMessage(ctx, slack.Message{Channel: loopCh, Text: "#1 parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.PostMessage(ctx, slack.Message{Channel: loopCh, ThreadTS: parent, Text: "a reply"}); err != nil {
		t.Fatal(err)
	}
	if err := api.Update(ctx, slack.Message{Channel: loopCh, TS: parent, Text: "#1 parent, edited"}); err != nil {
		t.Fatal(err)
	}
	if err := api.PostEphemeral(ctx, slack.Message{Channel: loopCh, User: "U1", Text: "just you"}); err != nil {
		t.Fatal(err)
	}
	if err := api.PinsAdd(ctx, loopCh, parent); err != nil {
		t.Fatal(err)
	}
	if err := api.PinsAdd(ctx, loopCh, parent); err != nil { // already pinned is fine
		t.Fatal(err)
	}
	var ae *slack.Error
	if err := api.Update(ctx, slack.Message{Channel: loopCh, TS: "1.2", Text: "x"}); !errors.As(err, &ae) || ae.Code != "message_not_found" {
		t.Fatalf("updating a missing message: %v", err)
	}

	ms := f.Messages()
	if len(ms) != 3 {
		t.Fatalf("messages %+v", ms)
	}
	p, r, e := ms[0], ms[1], ms[2]
	if p.Text != "#1 parent, edited" || p.Edits != 1 || p.History[0] != "#1 parent" || !p.Pinned || p.User != slackfake.BotUser {
		t.Fatalf("parent %+v", p)
	}
	if r.ThreadTS != parent || r.Kind != "message" {
		t.Fatalf("reply %+v", r)
	}
	if e.Kind != "ephemeral" || e.To != "U1" {
		t.Fatalf("ephemeral %+v", e)
	}
	for _, c := range f.Calls() {
		if c.Method != "apps.connections.open" && c.Auth != "Bearer "+bot {
			t.Fatalf("%s with %q", c.Method, c.Auth)
		}
	}
}

func TestInjectsAReply(t *testing.T) {
	f, api, c := start(t)
	parent, _ := api.PostMessage(context.Background(), slack.Message{Channel: loopCh, Text: "#1"})
	ts, err := f.Reply("U0C9AFPAMD4", loopCh, parent, "lgtm")
	if err != nil {
		t.Fatal(err)
	}
	env := waitEnvs(t, c, 1)[0]
	var p struct {
		Event map[string]string `json:"event"`
	}
	json.Unmarshal(env.Payload, &p)
	if env.Type != "events_api" || p.Event["type"] != "message" || p.Event["thread_ts"] != parent || p.Event["ts"] != ts ||
		p.Event["user"] != "U0C9AFPAMD4" || p.Event["text"] != "lgtm" || p.Event["channel"] != loopCh {
		t.Fatalf("envelope %s %s", env.Type, env.Payload)
	}
	ms := f.Messages()
	if last := ms[len(ms)-1]; last.User != "U0C9AFPAMD4" || last.ThreadTS != parent || last.Text != "lgtm" {
		t.Fatalf("the reply is not in the thread: %+v", last)
	}
	if _, err := f.Mention("U2", loopCh, "", "<@bot> add a thing"); err != nil {
		t.Fatal(err)
	}
	if env := waitEnvs(t, c, 2)[1]; !json.Valid(env.Payload) || string(env.Type) != "events_api" {
		t.Fatalf("mention %+v", env)
	}
}

func TestPressesWhatIsThere(t *testing.T) {
	f, api, c := start(t)
	ctx := context.Background()
	ts, _ := api.PostMessage(ctx, slack.Message{Channel: loopCh, Text: "held", Blocks: []map[string]any{
		{"type": "section", "block_id": "item:7", "text": map[string]any{"type": "mrkdwn", "text": "held"},
			"accessory": map[string]any{"type": "static_select", "action_id": "priority", "options": []any{
				map[string]any{"text": map[string]any{"type": "plain_text", "text": "P1"}, "value": "P1"}}}},
		{"type": "actions", "block_id": "held", "elements": []any{
			map[string]any{"type": "button", "action_id": "approve", "value": "7:abc", "text": map[string]any{"type": "plain_text", "text": "Approve merge"}}}},
	}})
	if err := f.Press("U3", loopCh, ts, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.Choose("U3", loopCh, ts, "priority", "P1"); err != nil {
		t.Fatal(err)
	}
	envs := waitEnvs(t, c, 2)
	var p struct {
		Type    string            `json:"type"`
		User    map[string]string `json:"user"`
		Actions []map[string]any  `json:"actions"`
	}
	json.Unmarshal(envs[0].Payload, &p)
	if envs[0].Type != "interactive" || p.Type != "block_actions" || p.User["id"] != "U3" ||
		p.Actions[0]["value"] != "7:abc" || p.Actions[0]["block_id"] != "held" {
		t.Fatalf("press %s", envs[0].Payload)
	}
	json.Unmarshal(envs[1].Payload, &p)
	if sel, _ := p.Actions[0]["selected_option"].(map[string]any); sel["value"] != "P1" || p.Actions[0]["block_id"] != "item:7" {
		t.Fatalf("choose %s", envs[1].Payload)
	}
	if err := f.Press("U3", loopCh, ts, "cancel_approval"); !errors.Is(err, slackfake.ErrNoElement) {
		t.Fatalf("pressing a button that is not there: %v", err)
	}
	if err := f.Choose("U3", loopCh, ts, "priority", "P9"); !errors.Is(err, slackfake.ErrNoElement) {
		t.Fatalf("choosing an option that is not there: %v", err)
	}
}

// A press carries a trigger_id; views.open with it shows a modal (recorded, once per trigger), and
// Submit sends that person's open modal back as a view_submission with what they typed.
func TestModalOpensAndSubmits(t *testing.T) {
	f, api, c := start(t)
	ctx := context.Background()
	ts, _ := api.PostMessage(ctx, slack.Message{Channel: loopCh, Text: "q", Blocks: []map[string]any{
		{"type": "actions", "block_id": "needs_you", "elements": []any{
			map[string]any{"type": "button", "action_id": "rebuild", "value": "7", "text": map[string]any{"type": "plain_text", "text": "Rebuild…"}}}},
	}})
	if err := f.Submit("U3", nil); !errors.Is(err, slackfake.ErrNoView) {
		t.Fatalf("submit without a modal: %v", err)
	}
	if err := f.Press("U3", loopCh, ts, "rebuild"); err != nil {
		t.Fatal(err)
	}
	var p struct {
		TriggerID string `json:"trigger_id"`
		View      struct {
			CallbackID      string `json:"callback_id"`
			PrivateMetadata string `json:"private_metadata"`
			State           struct {
				Values map[string]map[string]map[string]any
			} `json:"state"`
		} `json:"view"`
	}
	json.Unmarshal(waitEnvs(t, c, 1)[0].Payload, &p)
	if p.TriggerID == "" {
		t.Fatal("no trigger_id")
	}
	view := map[string]any{"type": "modal", "callback_id": "rebuild", "private_metadata": "7",
		"title": map[string]any{"type": "plain_text", "text": "Rebuild #7"}, "submit": map[string]any{"type": "plain_text", "text": "Rebuild"},
		"close": map[string]any{"type": "plain_text", "text": "Cancel"},
		"blocks": []any{map[string]any{"type": "input", "block_id": "note", "label": map[string]any{"type": "plain_text", "text": "Note"},
			"element": map[string]any{"type": "plain_text_input", "action_id": "note", "initial_value": "rebase"}}}}
	if err := api.OpenView(ctx, p.TriggerID, view); err != nil {
		t.Fatal(err)
	}
	if err := api.OpenView(ctx, p.TriggerID, view); err == nil {
		t.Fatal("a trigger opened two views")
	}
	msgs := f.Messages()
	if m := msgs[len(msgs)-1]; m.Kind != "modal" || m.To != "U3" || m.Text != "[Rebuild #7]\nNote: rebase\n[Rebuild] [Cancel]" {
		t.Fatalf("modal %+v", m)
	}
	if err := f.Submit("U3", map[string]string{"note": "rebase and fix the lint"}); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(waitEnvs(t, c, 2)[1].Payload, &p)
	if p.View.CallbackID != "rebuild" || p.View.PrivateMetadata != "7" || p.View.State.Values["note"]["note"]["value"] != "rebase and fix the lint" {
		t.Fatalf("submission %+v", p.View)
	}
	msgs = f.Messages()
	if m := msgs[len(msgs)-1]; m.Kind != "submit" || m.User != "U3" || m.Text != "[Rebuild #7]\nNote: rebase and fix the lint" {
		t.Fatalf("submit %+v", m)
	}
	if err := f.Submit("U3", nil); !errors.Is(err, slackfake.ErrNoView) {
		t.Fatalf("a modal submitted twice: %v", err)
	}
}

func TestCommandRecordsTheAnswer(t *testing.T) {
	f, _, _ := start(t)
	answer, err := f.Command("U0C9AFPAMD4", loopCh, "/loop", "status")
	if err != nil || answer != "you said status" {
		t.Fatalf("%q %v", answer, err)
	}
	ms := f.Messages()
	if len(ms) != 2 || ms[0].Kind != "command" || ms[0].Text != "/loop status" || ms[1].Kind != "response" || ms[1].To != "U0C9AFPAMD4" {
		t.Fatalf("%+v", ms)
	}
}

func TestRecordsReactions(t *testing.T) {
	f, _, _ := start(t)
	ctx := context.Background()
	api := slack.NewAPI(f.BaseURL(), bot, slack.Channels{slack.ChannelPlan})
	ts, err := f.Reply(slack.Approver, slack.ChannelPlan, "", "a plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return api.ReactionsAdd(ctx, slack.ChannelPlan, ts, slack.ReactionRunning) },
		func() error { return api.ReactionsAdd(ctx, slack.ChannelPlan, ts, slack.ReactionRunning) }, // already there: fine
		func() error { return api.ReactionsAdd(ctx, slack.ChannelPlan, ts, slack.ReactionDone) },
		func() error { return api.ReactionsRemove(ctx, slack.ChannelPlan, ts, slack.ReactionRunning) },
		func() error { return api.ReactionsRemove(ctx, slack.ChannelPlan, ts, slack.ReactionRunning) }, // gone already: fine
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	msgs := f.Messages()
	if got := msgs[len(msgs)-1].Reactions; len(got) != 1 || got[0] != slack.ReactionDone {
		t.Fatalf("reactions %v", got)
	}
	if err := api.ReactionsAdd(ctx, slack.ChannelLoop, ts, slack.ReactionDone); !errors.Is(err, slack.ErrChannelNotAllowed) {
		t.Fatalf("reaction outside the allowed channels: %v", err)
	}
	if err := api.ReactionsAdd(ctx, slack.ChannelPlan, "1.2", slack.ReactionDone); err == nil {
		t.Fatal("reaction on a message that does not exist")
	}
}

// A second app (the @orchestrator): its own socket and bot user. A mention reaches the loop bot
// as a message and the app as an app_mention and a message, each its own event; a DM only the app;
// a press the app that posted the message; auth.test names each token's bot user.
func TestSecondApp(t *testing.T) {
	f := slackfake.New()
	t.Cleanup(f.Close)
	f.AddApp(slackfake.App{Name: "orch", BotToken: "xoxb-orch", AppToken: "xapp-orch", BotUser: "U0ORCH"})
	listen := func(appToken string) *client {
		c := &client{}
		ctx, cancel := context.WithCancel(context.Background())
		sock := &slack.Socket{Open: slack.NewAPI(f.BaseURL(), appToken, nil).OpenConnection, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			OnEnvelope: func(_ context.Context, e slack.Envelope) {
				c.mu.Lock()
				c.envs = append(c.envs, e)
				c.mu.Unlock()
			}}
		done := make(chan struct{})
		go func() { defer close(done); sock.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		return c
	}
	loop, orch := listen("xapp-fake"), listen("xapp-orch")
	if err := f.WaitConnected(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.WaitAppConnected("orch", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for tok, want := range map[string]string{bot: slackfake.BotUser, "xoxb-orch": "U0ORCH"} {
		if id, err := slack.NewAPI(f.BaseURL(), tok, nil).AuthTest(ctx); err != nil || id != want {
			t.Fatalf("auth.test %s: %q %v", tok, id, err)
		}
	}

	if _, err := f.MentionApp("orch", slack.Approver, loopCh, "", "what's stuck?"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DM("orch", slack.Approver, "", "drop #61"); err != nil {
		t.Fatal(err)
	}
	type ev struct{ Type, Channel, Text string }
	events := func(envs []slack.Envelope) (out []ev, ids map[string]bool) {
		ids = map[string]bool{}
		for _, e := range envs {
			var p struct {
				EventID string `json:"event_id"`
				Event   ev     `json:"event"`
			}
			json.Unmarshal(e.Payload, &p)
			ids[p.EventID] = true
			out = append(out, p.Event)
		}
		return out, ids
	}
	got, _ := events(waitEnvs(t, loop, 1))
	if len(got) != 1 || got[0].Type != "message" || got[0].Text != "<@U0ORCH> what's stuck?" {
		t.Fatalf("loop bot got %+v", got)
	}
	got, ids := events(waitEnvs(t, orch, 3))
	dm := slackfake.DMChannel("orch", slack.Approver)
	if len(got) != 3 || got[0].Type != "app_mention" || got[1].Type != "message" || got[2].Channel != dm || len(ids) != 3 {
		t.Fatalf("app got %+v (ids %v)", got, ids)
	}

	oapi := slack.NewAPI(f.BaseURL(), "xoxb-orch", slack.Channels{loopCh, slack.DMs})
	ts, err := oapi.PostMessage(ctx, slack.Message{Channel: dm, Text: "on it", Blocks: []map[string]any{
		{"type": "actions", "block_id": "b", "elements": []any{map[string]any{"type": "button", "action_id": "orch_accept", "value": "7", "text": map[string]any{"type": "plain_text", "text": "Accept"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ms := f.Messages()
	if last := ms[len(ms)-1]; last.User != "U0ORCH" || last.Channel != dm {
		t.Fatalf("posted as %+v", last)
	}
	if err := f.Press(slack.Approver, dm, ts, "orch_accept"); err != nil {
		t.Fatal(err)
	}
	envs := waitEnvs(t, orch, 4)
	if envs[3].Type != "interactive" || !strings.Contains(string(envs[3].Payload), `"blocks"`) {
		t.Fatalf("press %+v", envs[3])
	}
	if n := len(loop.got()); n != 1 {
		t.Fatalf("the loop bot got %d envelopes", n)
	}
}
