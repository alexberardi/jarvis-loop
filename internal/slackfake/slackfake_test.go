package slackfake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
