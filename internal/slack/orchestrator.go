package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The @orchestrator app (docs/phases/08-orchestrator.md, "Its own persona in Slack"; piece 2,
// docs/decisions/2026-10-17-orchestrator-slack.md): a second Slack app with its own bot user and
// socket. It records the messages people send it (mentions and DMs), so its turns (piece 3) can
// answer them and act for the approver; it posts its recommendations with [Accept] [Other…],
// batched, and its replies. The loop bot keeps everything else, approvals included.

// OrchestratorConfig wires the @orchestrator app.
type OrchestratorConfig struct {
	Store *store.Store
	// API is the Web API client with the orchestrator's bot token, limited to ChannelLoop,
	// ChannelDigest, ChannelPlan and DMs.
	API   *API
	Clock clock.Clock
	Log   *slog.Logger
	Sleep func(context.Context, time.Duration) error
	// BatchDelay is how long an escalation waits for others to share its message (default 2
	// minutes): several items that need the user within it go in one message.
	BatchDelay time.Duration
	// TickEvery is how often Run flushes due batches (default 5 s).
	TickEvery time.Duration
}

// Orchestrator is the @orchestrator app's Slack side.
type Orchestrator struct {
	c      OrchestratorConfig
	events chan store.Event
	// bot is the Bot sharing the retry and posting helpers, over the orchestrator's API.
	bot *Bot

	mu      sync.Mutex
	user    string        // its own bot user id (auth.test), "" until known
	batch   []store.Event // escalations waiting to be posted, oldest first
	batchAt time.Time     // when the oldest arrived
}

// Action ids of a recommendation's buttons; the value is the escalation event's id.
const (
	// orchestratorPrefix starts an orchestrator token's actor.
	orchestratorPrefix = "orchestrator:"
	actionAccept       = "orch_accept"
	actionOther        = "orch_other"
	// maxBatch caps the items in one message (Slack allows 50 blocks; each item takes 2).
	maxBatch = 10
	// reactionSeen marks a message the orchestrator recorded.
	reactionSeen = "eyes"
)

// NewOrchestrator checks the configuration and fills in the defaults.
func NewOrchestrator(c OrchestratorConfig) (*Orchestrator, error) {
	if c.Store == nil || c.API == nil || c.Clock == nil || c.Log == nil {
		return nil, errors.New("slack: the orchestrator needs Store, API, Clock and Log")
	}
	if c.Sleep == nil {
		c.Sleep = realSleep
	}
	if c.BatchDelay <= 0 {
		c.BatchDelay = 2 * time.Minute
	}
	if c.TickEvery <= 0 {
		c.TickEvery = 5 * time.Second
	}
	bot, err := New(Config{Store: c.Store, API: c.API, Clock: c.Clock, Log: c.Log, Sleep: c.Sleep})
	if err != nil {
		return nil, err
	}
	return &Orchestrator{c: c, events: make(chan store.Event, eventsBuffer), bot: bot}, nil
}

// BotUser is the orchestrator's own Slack user id, or "" until auth.test has answered.
func (o *Orchestrator) BotUser() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.user
}

// Identify learns the orchestrator's bot user id (auth.test), once.
func (o *Orchestrator) Identify(ctx context.Context) error {
	if o.BotUser() != "" {
		return nil
	}
	id, err := o.c.API.AuthTest(ctx)
	if err != nil {
		return err
	}
	o.mu.Lock()
	o.user = id
	o.mu.Unlock()
	return nil
}

// Attach subscribes to the store's events the app posts: escalations, replies, and its own
// lowering of the active agents. The subscriber only queues.
func (o *Orchestrator) Attach() {
	o.c.Store.Subscribe(func(e store.Event) {
		if !orchestratorPosts(e) {
			return
		}
		select {
		case o.events <- e:
		default:
			o.c.Log.Error("slack: orchestrator event queue full; event not posted", "event", e.ID, "kind", e.Kind)
		}
	})
}

func orchestratorPosts(e store.Event) bool {
	switch e.Kind {
	case store.KindOrchEscalated, store.KindOrchReply:
		return true
	case "control":
		return e.Data["ping"] == true && strings.HasPrefix(e.Actor, orchestratorPrefix)
	}
	return false
}

// Run handles queued events and flushes due batches until ctx ends.
func (o *Orchestrator) Run(ctx context.Context) {
	if err := o.Identify(ctx); err != nil {
		o.c.Log.Error("slack: orchestrator auth.test; mentions of it are not recognised yet", "err", err.Error())
	}
	t := time.NewTicker(o.c.TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-o.events:
			o.handleEvent(ctx, e)
		case <-t.C:
			o.Tick(ctx)
		}
	}
}

// Drain handles every queued event now (tests; Run does this as events arrive).
func (o *Orchestrator) Drain(ctx context.Context) {
	for {
		select {
		case e := <-o.events:
			o.handleEvent(ctx, e)
		default:
			return
		}
	}
}

// Tick retries a failed auth.test and posts the batch once its window has passed.
func (o *Orchestrator) Tick(ctx context.Context) {
	if o.BotUser() == "" {
		if err := o.Identify(ctx); err != nil {
			o.c.Log.Warn("slack: orchestrator auth.test", "err", err.Error())
		}
	}
	o.mu.Lock()
	var due []store.Event
	if len(o.batch) > 0 && !o.c.Clock.Now().Before(o.batchAt.Add(o.c.BatchDelay)) {
		due, o.batch = o.batch, nil
	}
	o.mu.Unlock()
	for len(due) > 0 {
		n := min(len(due), maxBatch)
		o.post(ctx, due[:n])
		due = due[n:]
	}
}

func (o *Orchestrator) handleEvent(ctx context.Context, e store.Event) {
	switch e.Kind {
	case store.KindOrchEscalated:
		o.mu.Lock()
		if len(o.batch) == 0 {
			o.batchAt = e.At
		}
		o.batch = append(o.batch, e)
		o.mu.Unlock()
	case store.KindOrchReply:
		text := Escape(Clip(str(e.Data, "text"), MaxChars-100, ""))
		o.bot.post(ctx, "orchestrator reply", Message{Channel: str(e.Data, "channel"), ThreadTS: str(e.Data, "thread_ts"),
			Text: text, Blocks: []map[string]any{section(text)}})
	case "control":
		from, _ := e.Data["from"].(map[string]any)
		to, _ := e.Data["to"].(map[string]any)
		text := fmt.Sprintf("<@%s> Orchestrator: active agents %v → %v — %s", Approver, from["active_agents"], to["active_agents"],
			Escape(Clip(str(e.Data, "reason"), 300, "")))
		o.bot.post(ctx, "orchestrator agents", Message{Channel: ChannelDigest, Text: text, Blocks: []map[string]any{section(text)}})
	}
}

// recommendation is one escalation as an entry of a message: what the item needs, the
// recommendation and why, and the buttons. Accept only where it can be the answer: an item waiting
// on the user's answer, never an approval or a trusted-file OK (those stay on their own buttons).
func (o *Orchestrator) recommendation(ctx context.Context, e store.Event, mention bool) (string, []map[string]any) {
	var head string
	accept, trust := false, false
	if e.ItemID != 0 {
		if d, err := o.c.Store.GetItem(ctx, e.ItemID); err == nil {
			head = "*" + ref(e.ItemID, d.Item.Title) + "*"
			accept = d.Item.Stage == pipeline.NeedsYou
			trust = waitsForTrust(d.Item)
		} else {
			head = fmt.Sprintf("*#%d*", e.ItemID)
		}
	}
	text := ""
	if mention {
		text = fmt.Sprintf("<@%s> ", Approver)
	}
	f := func(k string, max int) string { return o.bot.field(str(e.Data, k), max, e.ItemID) }
	if head != "" {
		text += head + " needs you: " + f("text", 800)
	} else {
		text += "Orchestrator: " + f("text", 800)
	}
	rec := strings.TrimSpace(str(e.Data, "recommendation"))
	if rec != "" {
		text += "\n*Recommend:* " + f("recommendation", 600)
		if r := strings.TrimSpace(str(e.Data, "reason")); r != "" {
			text += " _Because_ " + f("reason", 400)
		}
	}
	approval := e.Data["approval"] == true
	switch {
	case approval:
		text += "\nApprove it with the button on its approval message: approvals are yours, not mine."
		accept = false
	case trust:
		// Accept would answer with the recommendation's words, not the OK verify needs.
		text += "\nOK the trusted files with the button on its trust message: trusted-file OKs are yours, not mine."
		accept = false
	case rec == "" || !accept:
		if e.ItemID != 0 && accept {
			text += "\nReply in its thread to answer."
		}
		accept = false
	}
	if at, err := time.Parse(time.RFC3339Nano, str(e.Data, "act_at")); err == nil && accept {
		text += fmt.Sprintf("\nNight shift: I act on this recommendation at %s unless you answer first.", at.In(o.bot.c.Location).Format("15:04"))
	}
	blocks := []map[string]any{section(text)}
	if accept {
		value := strconv.FormatInt(e.ID, 10)
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "orch:" + value, "elements": []any{
			button("Accept", actionAccept, value, "primary"),
			button("Other…", actionOther, value, ""),
		}})
	}
	return text, blocks
}

// post sends a batch: one escalation in its item's thread (or #loop when it is about none), several
// as one #loop message with a line in each item's thread.
func (o *Orchestrator) post(ctx context.Context, batch []store.Event) {
	if len(batch) == 1 {
		e := batch[0]
		text, blocks := o.recommendation(ctx, e, true)
		m := Message{Channel: ChannelLoop, Text: text, Blocks: blocks}
		if e.ItemID != 0 {
			if _, ts, err := o.c.Store.SlackThread(ctx, e.ItemID); err == nil && ts != "" {
				m.ThreadTS = ts
			}
		}
		o.bot.post(ctx, "orchestrator escalation", m)
		return
	}
	head := fmt.Sprintf("<@%s> %d items need you:", Approver, len(batch))
	blocks := []map[string]any{section(head)}
	var refs []string
	for _, e := range batch {
		_, bl := o.recommendation(ctx, e, false)
		blocks = append(blocks, bl...)
		if e.ItemID != 0 {
			refs = append(refs, fmt.Sprintf("#%d", e.ItemID))
		}
	}
	if _, err := o.bot.post(ctx, "orchestrator batch", Message{Channel: ChannelLoop, Text: head + " " + strings.Join(refs, ", "), Blocks: blocks}); err != nil {
		return
	}
	for _, e := range batch {
		if e.ItemID == 0 {
			continue
		}
		if _, ts, err := o.c.Store.SlackThread(ctx, e.ItemID); err == nil && ts != "" {
			o.bot.post(ctx, "orchestrator batch line", Message{Channel: ChannelLoop, ThreadTS: ts,
				Text: fmt.Sprintf("Orchestrator: asked you about this in #loop, with %d other items.", len(batch)-1)})
		}
	}
}

// HandleEnvelope handles an events_api or interactive envelope on the orchestrator's socket.
func (o *Orchestrator) HandleEnvelope(ctx context.Context, env Envelope) {
	switch env.Type {
	case "events_api":
		var p struct {
			Event slackEvent `json:"event"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		// A mention arrives twice, as app_mention and as message.channels: only the message counts.
		if p.Event.Type == "message" {
			o.onMessage(ctx, p.Event)
		}
	case "interactive":
		var p interaction
		if json.Unmarshal(env.Payload, &p) != nil || p.Type != "block_actions" {
			return
		}
		o.onActions(ctx, p)
	}
}

// Mentions reports whether a message mentions the orchestrator's bot user.
func (o *Orchestrator) Mentions(text string) bool {
	u := o.BotUser()
	return u != "" && strings.Contains(text, "<@"+u+">")
}

// onMessage records a DM to the orchestrator, or a message mentioning it in one of the loop's
// channels, from anyone: only the approver's can make it act (store.UseApproverMessage), anyone
// else's it may only answer. The 👀 says it was heard.
func (o *Orchestrator) onMessage(ctx context.Context, m slackEvent) {
	if m.BotID != "" || m.Subtype != "" || m.User == "" || m.User == o.BotUser() {
		return
	}
	loopChannel := m.Channel == ChannelLoop || m.Channel == ChannelDigest || m.Channel == ChannelPlan
	if !(IsDM(m.Channel) || loopChannel && o.Mentions(m.Text)) {
		return
	}
	if o.nightWord(ctx, m) {
		return
	}
	msg := store.OrchestratorMessage{Channel: m.Channel, TS: m.TS, User: m.User, Text: incoming(m.Text), Approver: IsApprover(m.User)}
	if m.ThreadTS != m.TS {
		msg.ThreadTS = m.ThreadTS
	}
	if m.Channel == ChannelLoop && msg.ThreadTS != "" {
		msg.ItemID, _ = o.c.Store.ItemByThread(ctx, ChannelLoop, msg.ThreadTS)
	}
	_, fresh, err := o.c.Store.OrchMessage(ctx, msg)
	if err != nil {
		o.c.Log.Error("slack: record a message to the orchestrator", "err", err.Error())
		return
	}
	if fresh {
		o.bot.try(ctx, "orchestrator seen", func() error { return o.c.API.ReactionsAdd(ctx, m.Channel, m.TS, reactionSeen) })
	}
}

func (o *Orchestrator) ephemeral(ctx context.Context, channel, user, text string) {
	o.bot.try(ctx, "orchestrator ephemeral", func() error {
		return o.c.API.PostEphemeral(ctx, Message{Channel: channel, User: user, Text: text})
	})
}

func (o *Orchestrator) onActions(ctx context.Context, p interaction) {
	channel := p.Channel.ID
	if channel == "" {
		channel = p.Container.ChannelID
	}
	msgTS := p.Message.TS
	if msgTS == "" {
		msgTS = p.Container.MessageTS
	}
	user := p.User.ID
	for _, a := range p.Actions {
		if a.ActionID != actionAccept && a.ActionID != actionOther {
			continue
		}
		evID, _ := strconv.ParseInt(a.Value, 10, 64)
		if !IsApprover(user) {
			o.ephemeral(ctx, channel, user, "Sorry, only the approver can do that.")
			var itemID int64
			if id, err := o.escalationItem(ctx, evID); err == nil {
				itemID = id
			}
			if err := o.c.Store.Record(ctx, store.Event{ItemID: itemID, Actor: actorPrefix + user, Kind: "refused",
				Data: map[string]any{"action": strings.TrimPrefix(a.ActionID, "orch_"), "code": "not_approver", "message": "only the approver can do that"}}); err != nil {
				o.c.Log.Error("slack: record refusal", "err", err.Error())
			}
			continue
		}
		switch a.ActionID {
		case actionAccept:
			if _, err := o.c.Store.AcceptRecommendation(ctx, actorPrefix+user, evID); err != nil {
				o.ephemeral(ctx, channel, user, "Not accepted: "+Escape(errMessage(err)))
				continue
			}
			o.settle(ctx, channel, msgTS, a.BlockID, p.Message.Blocks, "Accepted by you.")
		case actionOther:
			text := "Tell me what to do instead: mention me here or DM me."
			if id, err := o.escalationItem(ctx, evID); err == nil && id != 0 {
				if d, err := o.c.Store.GetItem(ctx, id); err == nil && d.Item.Stage == pipeline.NeedsYou {
					text = fmt.Sprintf("Reply in #%d's thread with what to do instead: your reply there is the answer.", id)
				}
			}
			o.ephemeral(ctx, channel, user, text)
		}
	}
}

// escalationItem is the item an escalation event is about.
func (o *Orchestrator) escalationItem(ctx context.Context, evID int64) (int64, error) {
	e, err := o.c.Store.GetEvent(ctx, evID)
	if err != nil {
		return 0, err
	}
	if e.Kind != store.KindOrchEscalated {
		return 0, errors.New("not an escalation")
	}
	return e.ItemID, nil
}

// settle replaces an answered recommendation's buttons (the actions block blockID) with what was
// done, in the message as Slack sent it with the press.
func (o *Orchestrator) settle(ctx context.Context, channel, ts, blockID string, blocks []map[string]any, done string) {
	if len(blocks) == 0 || ts == "" {
		return
	}
	out := make([]map[string]any, 0, len(blocks))
	text := ""
	for _, b := range blocks {
		if b["block_id"] == blockID {
			b = map[string]any{"type": "context", "block_id": blockID, "elements": []any{mrkdwn(done)}}
		}
		if t, ok := b["text"].(map[string]any); ok && text == "" {
			text, _ = t["text"].(string)
		}
		out = append(out, b)
	}
	o.bot.update(ctx, "orchestrator settle", Message{Channel: channel, TS: ts, Text: text, Blocks: out})
}

// nightWord handles the approver's "@orchestrator night" or "morning" itself, without a turn: in
// auto mode it turns the night shift on or off until the next 23:00 or 08:00
// (docs/decisions/2026-10-20-night-shift.md). It reports whether the message was one.
func (o *Orchestrator) nightWord(ctx context.Context, m slackEvent) bool {
	word := strings.ToLower(strings.Trim(strings.TrimSpace(stripMentions(m.Text)), ".!"))
	if (word != pipeline.OverrideNight && word != pipeline.OverrideMorning) || !IsApprover(m.User) {
		return false
	}
	st, err := o.c.Store.SetNightOverride(ctx, actorPrefix+m.User, word, m.Channel, m.TS)
	if err != nil {
		o.c.Log.Error("slack: night shift override", "err", err.Error())
		return true
	}
	var text string
	switch {
	case st.Mode != pipeline.NightAuto:
		mode := st.Mode
		if mode == "" {
			mode = pipeline.NightOff
		}
		text = fmt.Sprintf("The night shift is %s, so %s changes nothing: `loop night auto` lets you switch it with night and morning.", mode, word)
	case st.On:
		text = fmt.Sprintf("Night shift on until %s: I act on low-risk escalations after 15 minutes, and list each in the standup with its undo.",
			st.Until.In(o.bot.c.Location).Format("15:04"))
	default:
		text = fmt.Sprintf("Night shift off until %s: everything waits for you.", st.Until.In(o.bot.c.Location).Format("15:04"))
	}
	thread := m.ThreadTS
	if thread == "" && !IsDM(m.Channel) {
		thread = m.TS
	}
	if _, err := o.c.Store.OrchReply(ctx, "loopd", m.Channel, thread, text, "the user's "+word); err != nil {
		o.c.Log.Error("slack: night shift reply", "err", err.Error())
	}
	return true
}
