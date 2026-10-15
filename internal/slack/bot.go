package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // America/New_York for the digest, even on a box without zoneinfo

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Config wires a Bot.
type Config struct {
	Store *store.Store
	// API is the Web API client with the bot token, limited to ChannelLoop and ChannelDigest.
	API   *API
	Clock clock.Clock
	Log   *slog.Logger
	// Sleep waits between retries of a failed Slack call; tests inject one that does not sleep.
	Sleep func(context.Context, time.Duration) error
	// LinkBase, when set, is loopd's address as seen from where the user reads Slack (LAN); long
	// text then links to the item on it. Empty: long text points at `loop show <id>`.
	LinkBase string
	// Repos are the configured repo slugs (loopd's --repos): /loop add sets one when the text
	// names it.
	Repos []string
	// GitHubOwner owns the repos PR links point at (default alexberardi).
	GitHubOwner string
	// DigestHours is the digest interval in hours, a divisor of 24 (default 4), aligned to
	// midnight in Location (default America/New_York).
	DigestHours int
	Location    *time.Location
	// ApprovalDelay is the window in which a reply approval can be cancelled (default 2 minutes).
	ApprovalDelay time.Duration
	// TickEvery is how often Run applies due reply approvals and checks the digest (default 5 s).
	// The loop's harness, on a fake clock, makes it short so a moved clock takes effect at once.
	TickEvery time.Duration
}

// Bot turns pipeline events into Slack messages and Slack actions into pipeline calls.
type Bot struct {
	c      Config
	events chan store.Event

	mu       sync.Mutex
	pending  map[string]*pendingApproval // by id
	digestTS time.Time                   // the boundary the failures below are for
	digestNG int                         // failed attempts to post that digest
}

type pendingApproval struct {
	id      string
	itemID  int64
	sha     string
	words   string
	due     time.Time
	echoTS  string
	channel string
}

const (
	actorPrefix  = "slack:"
	fanoutTries  = 3
	digestTries  = 3
	eventsBuffer = 4096
)

// New checks the configuration and fills in the defaults.
func New(c Config) (*Bot, error) {
	if c.Store == nil || c.API == nil || c.Clock == nil || c.Log == nil {
		return nil, errors.New("slack: Store, API, Clock and Log are required")
	}
	if c.Sleep == nil {
		c.Sleep = realSleep
	}
	if c.GitHubOwner == "" {
		c.GitHubOwner = "alexberardi"
	}
	if c.DigestHours == 0 {
		c.DigestHours = 4
	}
	if c.DigestHours < 1 || c.DigestHours > 24 || 24%c.DigestHours != 0 {
		return nil, fmt.Errorf("slack: digest interval %dh does not divide a day", c.DigestHours)
	}
	if c.Location == nil {
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			return nil, err
		}
		c.Location = loc
	}
	if c.ApprovalDelay == 0 {
		c.ApprovalDelay = 2 * time.Minute
	}
	if c.TickEvery <= 0 {
		c.TickEvery = 5 * time.Second
	}
	c.LinkBase = strings.TrimRight(c.LinkBase, "/")
	return &Bot{c: c, events: make(chan store.Event, eventsBuffer), pending: map[string]*pendingApproval{}}, nil
}

// Attach subscribes the bot to the store's events. The subscriber only queues: Slack never slows
// or fails a pipeline action.
func (b *Bot) Attach() {
	b.c.Store.Subscribe(func(e store.Event) {
		select {
		case b.events <- e:
		default:
			b.c.Log.Error("slack: event queue full; event not posted", "event", e.ID, "kind", e.Kind)
		}
	})
}

// Run posts queued events, runs the timers (reply approvals, digest) every few seconds, and makes
// sure the control message exists. It returns when ctx ends.
func (b *Bot) Run(ctx context.Context) {
	if err := b.EnsureControl(ctx); err != nil {
		b.c.Log.Error("slack: control message", "err", err.Error())
	}
	t := time.NewTicker(b.c.TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-b.events:
			b.handleEvent(ctx, e)
		case <-t.C:
			b.Tick(ctx)
		}
	}
}

// Drain posts every queued event now (tests; Run does this as events arrive).
func (b *Bot) Drain(ctx context.Context) {
	for {
		select {
		case e := <-b.events:
			b.handleEvent(ctx, e)
		default:
			return
		}
	}
}

// Tick applies reply approvals whose window has passed and posts the digest when one is due.
func (b *Bot) Tick(ctx context.Context) {
	b.applyDue(ctx)
	b.digest(ctx)
}

// ---- Slack calls with bounded retries ----

func retryable(err error) bool {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code == "rate_limited" || ae.Code == "" && ae.Status >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

// try runs a Slack call, retrying transient failures a bounded number of times. Failures are
// logged (never with a token: errors only carry Slack's code or the HTTP status).
func (b *Bot) try(ctx context.Context, what string, fn func() error) error {
	var err error
	for i := 0; i < fanoutTries; i++ {
		if err = fn(); err == nil || !retryable(err) {
			break
		}
		if i < fanoutTries-1 && b.c.Sleep(ctx, time.Duration(1<<i)*time.Second) != nil {
			break
		}
	}
	if err != nil {
		b.c.Log.Error("slack: call failed", "what", what, "err", err.Error())
	}
	return err
}

func (b *Bot) post(ctx context.Context, what string, m Message) (string, error) {
	var ts string
	err := b.try(ctx, what, func() error {
		var err error
		ts, err = b.c.API.PostMessage(ctx, m)
		return err
	})
	return ts, err
}

func (b *Bot) update(ctx context.Context, what string, m Message) error {
	return b.try(ctx, what, func() error { return b.c.API.Update(ctx, m) })
}

// ephemeral tells one user something, only in a channel the bot may post in.
func (b *Bot) ephemeral(ctx context.Context, channel, user, text string) {
	if channel != ChannelLoop && channel != ChannelDigest {
		return
	}
	b.try(ctx, "ephemeral", func() error {
		return b.c.API.PostEphemeral(ctx, Message{Channel: channel, User: user, Text: text})
	})
}

// ---- events → threads ----

func (b *Bot) handleEvent(ctx context.Context, e store.Event) {
	if e.ItemID == 0 {
		if e.Kind == "control" {
			if err := b.updateControl(ctx); err != nil {
				b.c.Log.Error("slack: control message", "err", err.Error())
			}
			if str(e.Data, "reason") == "usage_limit" {
				b.post(ctx, "usage limit", b.usageLimitMessage(e))
			}
		}
		return
	}
	d, err := b.c.Store.GetItem(ctx, e.ItemID)
	if err != nil {
		b.c.Log.Error("slack: load item", "item", e.ItemID, "err", err.Error())
		return
	}
	_, ts, err := b.c.Store.SlackThread(ctx, e.ItemID)
	if err != nil {
		return
	}
	if ts == "" {
		// A new item, or one whose parent never made it: post the parent (its current state).
		ts, err = b.post(ctx, "parent", b.parentMessage(d))
		if err != nil {
			return
		}
		if err := b.c.Store.SetSlackThread(ctx, e.ItemID, ChannelLoop, ts); err != nil {
			b.c.Log.Error("slack: record thread", "item", e.ItemID, "err", err.Error())
			return
		}
		if e.Kind == "created" {
			return
		}
	} else if e.Kind == "created" {
		return
	}
	if m, ok := b.describe(e, d); ok {
		m.Channel, m.ThreadTS = ChannelLoop, ts
		b.post(ctx, "reply", m)
	}
	pm := b.parentMessage(d)
	pm.TS = ts
	b.update(ctx, "parent update", pm)
}

func (b *Bot) hint(id int64) string {
	if b.c.LinkBase != "" {
		return fmt.Sprintf("%s/items/%d", b.c.LinkBase, id)
	}
	return fmt.Sprintf("loop show %d", id)
}

// field is a piece of agent or user text made safe and short for a message.
func (b *Bot) field(s string, max int, id int64) string {
	return Escape(Clip(strings.TrimSpace(s), max, b.hint(id)))
}

func (b *Bot) prLink(it pipeline.Item) string {
	if it.PR == 0 {
		return ""
	}
	repo := it.Repo
	if repo != "" && !strings.Contains(repo, "/") {
		repo = b.c.GitHubOwner + "/" + repo
	}
	ok := repo != ""
	for _, r := range repo {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/", r)) {
			ok = false
		}
	}
	if !ok {
		return fmt.Sprintf("PR #%d", it.PR)
	}
	return fmt.Sprintf("<https://github.com/%s/pull/%d|%s#%d>", repo, it.PR, it.Repo, it.PR)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func plain(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

func mrkdwn(s string) map[string]any { return map[string]any{"type": "mrkdwn", "text": s} }

func section(text string) map[string]any {
	return map[string]any{"type": "section", "text": mrkdwn(Clip(text, MaxChars, ""))}
}

func button(text, actionID, value, style string) map[string]any {
	b := map[string]any{"type": "button", "text": plain(text), "action_id": actionID, "value": value}
	if style != "" {
		b["style"] = style
	}
	return b
}

var priorityNames = []string{"P1 highest", "P2 high", "P3 medium", "P4 low", "P5 lowest"}

func selectMenu(actionID string, labels, values []string, current string) map[string]any {
	var opts []any
	var initial any
	for i, l := range labels {
		o := map[string]any{"text": plain(l), "value": values[i]}
		opts = append(opts, o)
		if values[i] == current {
			initial = o
		}
	}
	m := map[string]any{"type": "static_select", "action_id": actionID, "options": opts, "placeholder": plain("Choose")}
	if initial != nil {
		m["initial_option"] = initial
	}
	return m
}

// parentMessage is the item's first message in #loop: what it is, where it is, and the priority
// dropdown.
func (b *Bot) parentMessage(d store.Detail) Message {
	it := d.Item
	head := fmt.Sprintf("#%d %s", it.ID, b.field(it.Title, 300, it.ID))
	line := fmt.Sprintf("Priority *%s* · Stage *%s* · Source %s · Repo %s", it.Priority, d.Display, Escape(it.Source), repoText(it))
	if pr := b.prLink(it); pr != "" {
		line += " · " + pr
	}
	text := "*" + head + "*\n" + line
	if it.Risk == pipeline.RiskHold && it.HoldReason != "" {
		text += "\nOn the hold list: " + b.field(it.HoldReason, 400, it.ID)
	}
	sec := section(text)
	sec["block_id"] = fmt.Sprintf("item:%d", it.ID)
	if it.Stage != pipeline.Done && it.Stage != pipeline.Closed {
		sec["accessory"] = selectMenu("priority", priorityNames, []string{"P1", "P2", "P3", "P4", "P5"}, it.Priority)
	}
	return Message{Channel: ChannelLoop, Text: fmt.Sprintf("%s — %s, %s", head, it.Priority, d.Display), Blocks: []map[string]any{sec}}
}

// repoText is the item's repo for a message, or why there is none: until triage has run, triage
// picks it.
func repoText(it pipeline.Item) string {
	switch {
	case it.Repo != "":
		return Escape(it.Repo)
	case it.Stage == pipeline.Triage:
		return "not set, triage will pick"
	}
	return "not set"
}

// who names an actor in a message without pinging anyone or naming a machine.
func who(actor string) string {
	switch {
	case actor == actorPrefix+Approver:
		return "you"
	case strings.HasPrefix(actor, actorPrefix):
		return "someone else (" + Escape(strings.TrimPrefix(actor, actorPrefix)) + ")"
	case strings.HasPrefix(actor, "user:"):
		return "you (" + Escape(strings.TrimPrefix(actor, "user:")) + ", CLI)"
	case strings.HasPrefix(actor, "runner:"):
		return "a runner"
	}
	return Escape(actor)
}

func str(d map[string]any, k string) string {
	s, _ := d[k].(string)
	return s
}

// describe is the thread reply for an event: one or two lines, or the question and held
// messages, which mention the approver.
func (b *Bot) describe(e store.Event, d store.Detail) (Message, bool) {
	it := d.Item
	id := it.ID
	f := func(k string, max int) string { return b.field(str(e.Data, k), max, id) }
	to := pipeline.Stage(e.To)

	if to == pipeline.NeedsYou && e.From != string(pipeline.NeedsYou) {
		q := it.Question
		if it.Stage != pipeline.NeedsYou {
			q = str(e.Data, "question")
		}
		text := fmt.Sprintf("<@%s> #%d needs you: %s\nReply in this thread to answer.", Approver, id, b.field(q, 1500, id))
		return Message{Text: text, Blocks: []map[string]any{section(text)}}, true
	}
	if e.Kind == "advanced" && pipeline.Outcome(e.Outcome) == pipeline.Hold {
		return b.heldMessage(e, d), true
	}

	var text string
	switch e.Kind {
	case "claimed":
		text = fmt.Sprintf("Claimed for %s.", e.From)
	case "evidence":
		// Never the contents or the file name: a count and a link-free pointer.
		text = fmt.Sprintf("Evidence %v attached (`loop show %d`).", e.Data["n"], id)
	case "lease_expired":
		text = fmt.Sprintf("Lease expired; back in the %s queue.", e.From)
	case "stopped":
		text = fmt.Sprintf("Stopped; back in the %s queue.", e.From)
	case "usage_limit":
		text = "Usage limit hit: " + f("error", 300)
		if r := str(e.Data, "reset"); r != "" {
			text += " (resets " + f("reset", 100) + ")"
		}
		text += ". Active agents set to 0; back in the queue."
	case "answered":
		text = fmt.Sprintf("Answered by %s: %s → %s.", who(e.Actor), f("answer", 600), e.To)
	case "approved":
		text = fmt.Sprintf("Approved by %s for `%s`: '%s' → land.", who(e.Actor), short(str(e.Data, "head_sha")), f("words", 300))
	case "held":
		text = fmt.Sprintf("Kept holding (%s).", who(e.Actor))
	case "patched":
		var parts []string
		if p, ok := e.Data["priority"].(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("Priority %s → %s", str(p, "from"), str(p, "to")))
		}
		if t, ok := e.Data["title"].(map[string]any); ok {
			parts = append(parts, "Title: "+b.field(str(t, "to"), 300, id))
		}
		text = strings.Join(parts, "; ") + fmt.Sprintf(" (%s).", who(e.Actor))
	case "refused":
		text = fmt.Sprintf("Refused %s from %s: %s", Escape(str(e.Data, "action")), who(e.Actor), f("message", 300))
	case "guard_refused":
		// The rule and the tool only: the refused command or path can hold anything.
		text = fmt.Sprintf("Guard refused a %s call (%s); details on loopd (`loop show %d`).", Escape(str(e.Data, "tool")), Escape(str(e.Data, "rule")), id)
	case "low_cache_hit":
		text = fmt.Sprintf("Low prompt-cache hit ratio on this run (%s over %s turns).", Escape(str(e.Data, "cache_hit_ratio")), Escape(str(e.Data, "num_turns")))
	case "approval_cancelled":
		text = fmt.Sprintf("Reply approval cancelled by %s; still held.", who(e.Actor))
	case "advanced":
		switch pipeline.Outcome(e.Outcome) {
		case pipeline.Reproduced:
			// From the event, not the item: the item may have moved on by the time this posts.
			text = fmt.Sprintf("Reproduced: area %s, size %s, risk %s → ready.", f("area", 100), f("size", 10), f("risk", 10))
			if r := str(e.Data, "repo"); r != "" {
				text = fmt.Sprintf("Reproduced in %s: area %s, size %s, risk %s → ready.", f("repo", 100), f("area", 100), f("size", 10), f("risk", 10))
			}
		case pipeline.NotReproducible:
			text = "Closed, not reproducible: " + f("summary", 600)
		case pipeline.PROpened:
			verb := "opened"
			if str(e.Data, "pr_action") == "updated" {
				verb = "updated" // a re-run after a bounce: the same PR
			}
			text = fmt.Sprintf("%s %s, head `%s` → review.", b.prLink(it), verb, short(str(e.Data, "head_sha")))
			if it.Risk == pipeline.RiskHold {
				text += "\nOn the hold list: " + b.field(it.HoldReason, 400, id)
			}
		case pipeline.Approved:
			text = "Review approved → land: " + f("summary", 600)
		case pipeline.Bounce:
			text = fmt.Sprintf("Bounced back to build (%v): %s", e.Data["bounce_count"], f("notes", 600))
		case pipeline.Merged:
			text = fmt.Sprintf("Merged %s as `%s`. Done.", b.prLink(it), short(str(e.Data, "merge_sha")))
		case pipeline.BackToBuild:
			text = "Back to build: " + f("reason", 400)
		default:
			text = fmt.Sprintf("%s → %s.", Escape(e.Outcome), e.To)
		}
	default:
		text = Escape(e.Kind) + "."
	}
	return Message{Text: text}, true
}

// heldMessage asks the approver to decide a held merge, for the head the land stage reported.
func (b *Bot) heldMessage(e store.Event, d store.Detail) Message {
	it := d.Item
	sha := str(e.Data, "head_sha")
	if sha == "" {
		sha = it.HeadSHA
	}
	reasons := it.HoldReason
	text := fmt.Sprintf("<@%s> #%d is held for your approval: %s at `%s`.\nWhy: %s\nApprove with the button or a reply here (\"approve\", \"lgtm\", …).",
		Approver, it.ID, b.prLink(it), short(sha), b.field(reasons, 800, it.ID))
	value := fmt.Sprintf("%d:%s", it.ID, sha)
	return Message{Text: fmt.Sprintf("<@%s> #%d is held for your approval (%s)", Approver, it.ID, short(sha)), Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "held", "elements": []any{
			button("Approve merge", "approve", value, "primary"),
			button("Keep holding", "keep_holding", value, ""),
		}},
	}}
}

// usageLimitMessage tells the approver in #loop-digest that a usage limit set the active agents
// to 0, with the reset time when the run reported one. Nothing raises the number again on its own.
func (b *Bot) usageLimitMessage(e store.Event) Message {
	var id int64
	switch v := e.Data["item"].(type) {
	case float64:
		id = int64(v)
	case int64:
		id = v
	case int:
		id = int64(v)
	}
	text := fmt.Sprintf("<@%s> Usage limit hit", Approver)
	if id != 0 {
		text += fmt.Sprintf(" on #%d", id)
	}
	text += ": " + b.field(str(e.Data, "error"), 300, id)
	if r := str(e.Data, "reset"); r != "" {
		text += " (resets " + b.field(r, 100, id) + ")"
	}
	text += ".\nActive agents set to 0. Raise it with `/loop agents <n>` or the control message once the limit resets."
	return Message{Channel: ChannelDigest, Text: text, Blocks: []map[string]any{section(text)}}
}

// ---- the control message ----

func controlMessage(c pipeline.Control) Message {
	head := fmt.Sprintf("Active agents: %d of 6", c.ActiveAgents)
	if c.Stopped {
		head += " (stopped)"
	}
	labels := []string{"0 (paused)", "1", "2", "3", "4", "5", "6"}
	values := []string{"0", "1", "2", "3", "4", "5", "6"}
	return Message{Channel: ChannelDigest, Text: head, Blocks: []map[string]any{
		section("*" + head + "*\nThe most agents the loop may run at once. `/loop stop` and `/loop resume` stop and resume them."),
		{"type": "actions", "block_id": "control", "elements": []any{
			selectMenu("agents_select", labels, values, strconv.Itoa(c.ActiveAgents)),
			button("−", "agents_minus", "-1", ""),
			button("+", "agents_plus", "+1", ""),
		}},
	}}
}

// EnsureControl posts the active-agents message once (and pins it), or brings the existing one up
// to date. A failed pin is logged, not fatal: everything else works without it.
func (b *Bot) EnsureControl(ctx context.Context) error {
	st, err := b.c.Store.SlackState(ctx)
	if err != nil {
		return err
	}
	ctl, err := b.c.Store.Control(ctx)
	if err != nil {
		return err
	}
	m := controlMessage(ctl)
	ts := st.ControlTS
	if ts != "" {
		m.TS = ts
		err := b.update(ctx, "control update", m)
		var ae *Error
		if errors.As(err, &ae) && ae.Code == "message_not_found" {
			ts = "" // deleted: post a new one
		} else if err != nil {
			return err
		}
	}
	if ts == "" {
		m.TS = ""
		ts, err = b.post(ctx, "control post", m)
		if err != nil {
			return err
		}
		if err := b.c.Store.SetSlackControlTS(ctx, ts); err != nil {
			return err
		}
	}
	if err := b.c.API.PinsAdd(ctx, ChannelDigest, ts); err != nil {
		b.c.Log.Warn("slack: could not pin the control message; it works unpinned", "err", err.Error())
	}
	return nil
}

func (b *Bot) updateControl(ctx context.Context) error {
	st, err := b.c.Store.SlackState(ctx)
	if err != nil {
		return err
	}
	if st.ControlTS == "" {
		return b.EnsureControl(ctx)
	}
	ctl, err := b.c.Store.Control(ctx)
	if err != nil {
		return err
	}
	m := controlMessage(ctl)
	m.TS = st.ControlTS
	return b.update(ctx, "control update", m)
}

// ---- Slack → pipeline ----

type slackEvent struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

type interaction struct {
	Type    string              `json:"type"`
	User    struct{ ID string } `json:"user"`
	Channel struct{ ID string } `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID       string `json:"action_id"`
		BlockID        string `json:"block_id"`
		Value          string `json:"value"`
		SelectedOption struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	} `json:"actions"`
}

// HandleEnvelope handles an events_api or interactive envelope (already acked).
func (b *Bot) HandleEnvelope(ctx context.Context, env Envelope) {
	switch env.Type {
	case "events_api":
		var p struct {
			Event slackEvent `json:"event"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		switch p.Event.Type {
		case "message":
			b.onMessage(ctx, p.Event)
		case "app_mention":
			b.onMention(ctx, p.Event)
		}
	case "interactive":
		var p interaction
		if json.Unmarshal(env.Payload, &p) != nil || p.Type != "block_actions" {
			return
		}
		b.onActions(ctx, p)
	}
}

// refuse tells someone other than the approver that they cannot do this, and records who tried.
func (b *Bot) refuse(ctx context.Context, channel, user string, itemID int64, action string) {
	b.ephemeral(ctx, channel, user, "Sorry, only the approver can do that.")
	err := b.c.Store.Record(ctx, store.Event{ItemID: itemID, Actor: actorPrefix + user, Kind: "refused",
		Data: map[string]any{"action": action, "code": "not_approver", "message": "only the approver can do that"}})
	if err != nil {
		b.c.Log.Error("slack: record refusal", "err", err.Error())
	}
}

func (b *Bot) threadReply(ctx context.Context, threadTS, text string) {
	b.post(ctx, "reply", Message{Channel: ChannelLoop, ThreadTS: threadTS, Text: text})
}

func errMessage(err error) string {
	var pe *pipeline.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return "something went wrong; see loopd's log"
}

func (b *Bot) onMessage(ctx context.Context, m slackEvent) {
	if m.BotID != "" || m.Subtype != "" || m.User == "" || m.Channel != ChannelLoop || m.ThreadTS == "" || m.ThreadTS == m.TS {
		return
	}
	id, err := b.c.Store.ItemByThread(ctx, ChannelLoop, m.ThreadTS)
	if err != nil || id == 0 {
		return
	}
	d, err := b.c.Store.GetItem(ctx, id)
	if err != nil {
		return
	}
	text := stripMentions(m.Text)
	switch d.Item.Stage {
	case pipeline.NeedsYou:
		if m.User != Approver {
			b.refuse(ctx, m.Channel, m.User, id, "answer")
			return
		}
		if text == "" {
			return
		}
		if _, err := b.c.Store.Answer(ctx, actorPrefix+m.User, id, text); err != nil {
			b.threadReply(ctx, m.ThreadTS, "Could not record that answer: "+Escape(errMessage(err)))
		}
	case pipeline.Held:
		if m.User != Approver {
			b.refuse(ctx, m.Channel, m.User, id, "approve")
			return
		}
		if !ClassifyReply(text) {
			b.threadReply(ctx, m.ThreadTS, "Not taken as an approval; the item stays held. Reply 'approve' or use the button.")
			return
		}
		b.startApproval(ctx, d.Item, text, m.ThreadTS)
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startApproval echoes a reply approval with a Cancel button; it applies when the window ends.
func (b *Bot) startApproval(ctx context.Context, it pipeline.Item, words, threadTS string) {
	b.mu.Lock()
	for _, p := range b.pending {
		if p.itemID == it.ID {
			b.mu.Unlock()
			b.threadReply(ctx, threadTS, "An approval is already waiting to apply; use its Cancel button to stop it.")
			return
		}
	}
	p := &pendingApproval{id: newID(), itemID: it.ID, sha: it.HeadSHA, words: words, due: b.c.Clock.Now().Add(b.c.ApprovalDelay), channel: ChannelLoop}
	b.pending[p.id] = p
	b.mu.Unlock()

	mins := int(b.c.ApprovalDelay.Round(time.Minute) / time.Minute)
	text := fmt.Sprintf("Approved by you: '%s' for `%s`. Merging in %d minutes.", b.field(words, 500, it.ID), short(p.sha), mins)
	ts, err := b.post(ctx, "approval echo", Message{Channel: ChannelLoop, ThreadTS: threadTS, Text: text, Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "pending", "elements": []any{button("Cancel", "cancel_approval", p.id, "danger")}},
	}})
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		// The user cannot see a Cancel button, so do not apply it behind their back.
		delete(b.pending, p.id)
		return
	}
	p.echoTS = ts
}

// takePending removes and returns pending approvals matching keep.
func (b *Bot) takePending(keep func(*pendingApproval) bool) []*pendingApproval {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*pendingApproval
	for id, p := range b.pending {
		if keep(p) {
			out = append(out, p)
			delete(b.pending, id)
		}
	}
	return out
}

func (b *Bot) applyDue(ctx context.Context) {
	now := b.c.Clock.Now()
	for _, p := range b.takePending(func(p *pendingApproval) bool { return !now.Before(p.due) }) {
		_, ts, _ := b.c.Store.SlackThread(ctx, p.itemID)
		_, err := b.c.Store.Approve(ctx, actorPrefix+Approver, p.itemID, p.sha, p.words)
		var pe *pipeline.Error
		switch {
		case err == nil:
			if p.echoTS != "" {
				b.update(ctx, "approval echo", Message{Channel: p.channel, TS: p.echoTS,
					Text: fmt.Sprintf("Approved by you: '%s' for `%s`. Applied.", b.field(p.words, 500, p.itemID), short(p.sha))})
			}
		case errors.As(err, &pe) && pe.Code == pipeline.CodeSHAMismatch:
			d, _ := b.c.Store.GetItem(ctx, p.itemID)
			b.threadReply(ctx, ts, fmt.Sprintf("Not approved: your reply was for `%s`, but the PR head is now `%s`. Approve again for the new commit.",
				short(p.sha), short(d.Item.HeadSHA)))
		default:
			b.threadReply(ctx, ts, "Not approved: "+Escape(errMessage(err)))
		}
	}
}

func (b *Bot) onMention(ctx context.Context, m slackEvent) {
	if m.BotID != "" || m.User == "" {
		return
	}
	// A mention inside an item's thread is a reply; onMessage handles it.
	if m.Channel == ChannelLoop && m.ThreadTS != "" {
		if id, _ := b.c.Store.ItemByThread(ctx, ChannelLoop, m.ThreadTS); id != 0 {
			return
		}
	}
	if m.User != Approver {
		b.refuse(ctx, m.Channel, m.User, 0, "add")
		return
	}
	text := stripMentions(m.Text)
	if _, err := b.add(ctx, m.User, text); err != nil {
		b.ephemeral(ctx, m.Channel, m.User, Escape(errMessage(err))+" Try: @jarvis-loop <what to do>")
	}
}

func (b *Bot) add(ctx context.Context, user, text string) (pipeline.Item, error) {
	repo, rest := ParseRepo(text, b.c.Repos)
	pri, title := ParsePriority(rest)
	if title == "" {
		return pipeline.Item{}, &pipeline.Error{Code: pipeline.CodeMissingField, Message: "Say what to add."}
	}
	return b.c.Store.AddItem(ctx, actorPrefix+user, store.NewItem{Title: title, Ask: text, Repo: repo, Priority: pri, Source: "user"})
}

func (b *Bot) onActions(ctx context.Context, p interaction) {
	channel := p.Channel.ID
	if channel == "" {
		channel = p.Container.ChannelID
	}
	msgTS := p.Message.TS
	if msgTS == "" {
		msgTS = p.Container.MessageTS
	}
	user := p.User.ID
	actor := actorPrefix + user
	for _, a := range p.Actions {
		itemID := actionItem(a.ActionID, a.BlockID, a.Value)
		if itemID == 0 && a.ActionID == "cancel_approval" {
			b.mu.Lock()
			if pa := b.pending[a.Value]; pa != nil {
				itemID = pa.itemID
			}
			b.mu.Unlock()
		}
		if user != Approver {
			b.refuse(ctx, channel, user, itemID, a.ActionID)
			continue
		}
		switch a.ActionID {
		case "priority":
			pri := a.SelectedOption.Value
			if _, err := b.c.Store.PatchItem(ctx, actor, itemID, &pri, nil); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		case "approve":
			_, sha, _ := strings.Cut(a.Value, ":")
			b.cancelPending(itemID)
			_, err := b.c.Store.Approve(ctx, actor, itemID, sha, "Approve merge")
			var pe *pipeline.Error
			switch {
			case err == nil:
				b.update(ctx, "held message", Message{Channel: channel, TS: msgTS,
					Text: fmt.Sprintf("#%d: approved by you for `%s` (button).", itemID, short(sha))})
			case errors.As(err, &pe) && pe.Code == pipeline.CodeSHAMismatch:
				d, _ := b.c.Store.GetItem(ctx, itemID)
				_, ts, _ := b.c.Store.SlackThread(ctx, itemID)
				b.threadReply(ctx, ts, fmt.Sprintf("Not approved: that button was for `%s`, but the PR head is now `%s`. Use the latest held message.",
					short(sha), short(d.Item.HeadSHA)))
			default:
				b.ephemeral(ctx, channel, user, "Not approved: "+Escape(errMessage(err)))
			}
		case "keep_holding":
			b.cancelPending(itemID)
			if _, err := b.c.Store.Hold(ctx, actor, itemID, "Keep holding"); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		case "cancel_approval":
			ps := b.takePending(func(pa *pendingApproval) bool { return pa.id == a.Value })
			if len(ps) == 0 {
				b.ephemeral(ctx, channel, user, "Nothing to cancel: that approval was already applied or cancelled.")
				continue
			}
			pa := ps[0]
			b.c.Store.Record(ctx, store.Event{ItemID: pa.itemID, Actor: actor, Kind: "approval_cancelled",
				Data: map[string]any{"words": pa.words, "head_sha": pa.sha}})
			b.update(ctx, "approval echo", Message{Channel: channel, TS: msgTS,
				Text: fmt.Sprintf("Cancelled: '%s' was not applied. #%d stays held.", b.field(pa.words, 500, pa.itemID), pa.itemID)})
		case "agents_select", "agents_minus", "agents_plus":
			ctl, err := b.c.Store.Control(ctx)
			if err != nil {
				continue
			}
			n := ctl.ActiveAgents
			switch a.ActionID {
			case "agents_select":
				n, err = strconv.Atoi(a.SelectedOption.Value)
				if err != nil {
					continue
				}
			case "agents_minus":
				n--
			case "agents_plus":
				n++
			}
			n = max(0, min(6, n))
			if n == ctl.ActiveAgents {
				continue
			}
			if _, err := b.c.Store.SetControl(ctx, actor, store.ControlPatch{ActiveAgents: &n}); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		}
	}
}

// cancelPending drops a reply approval waiting on an item (a button decided it instead).
func (b *Bot) cancelPending(itemID int64) {
	b.takePending(func(p *pendingApproval) bool { return p.itemID == itemID })
}

// actionItem finds the item an action is about: the parent's block_id "item:N", or a held
// button's value "N:sha".
func actionItem(actionID, blockID, value string) int64 {
	if rest, ok := strings.CutPrefix(blockID, "item:"); ok {
		id, _ := strconv.ParseInt(rest, 10, 64)
		return id
	}
	if actionID == "approve" || actionID == "keep_holding" {
		n, _, _ := strings.Cut(value, ":")
		id, _ := strconv.ParseInt(n, 10, 64)
		return id
	}
	return 0
}
