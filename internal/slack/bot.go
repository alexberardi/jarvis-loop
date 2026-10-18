package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
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
	// API is the Web API client with the bot token, limited to ChannelLoop, ChannelDigest and ChannelPlan.
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
	// Aliases are other words for repos (alias -> slug, from loopd's --repos-file): "mobile",
	// "node app". ParseRepo says where they count.
	Aliases map[string]string
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
	// Orchestrator is the @orchestrator app, when loopd runs it: messages that mention it are its
	// (never an answer or an approval here), and it posts the escalations. nil: off.
	Orchestrator *Orchestrator
}

// Bot turns pipeline events into Slack messages and Slack actions into pipeline calls.
type Bot struct {
	c      Config
	events chan store.Event

	mu      sync.Mutex
	pending map[string]*pendingApproval // by id
	// reopening are the qa items whose Reopen the approver pressed: their next reply in the thread
	// is the words (qa.go).
	reopening map[int64]bool
	// planHeaders are the plan header messages as last posted or updated (plan.go), so an event
	// that changes nothing on it does not edit it again.
	planHeaders map[int64]string
	digestTS    time.Time // the boundary the failures below are for
	digestNG    int       // failed attempts to post that digest
	self        string    // the loop bot's own user id, once toOrchestrator has needed it
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
	return &Bot{c: c, events: make(chan store.Event, eventsBuffer), pending: map[string]*pendingApproval{}, reopening: map[int64]bool{},
		planHeaders: map[int64]string{}}, nil
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
	if channel != ChannelLoop && channel != ChannelDigest && channel != ChannelPlan {
		return
	}
	b.try(ctx, "ephemeral", func() error {
		return b.c.API.PostEphemeral(ctx, Message{Channel: channel, User: user, Text: text})
	})
}

// ---- events → threads ----

func (b *Bot) handleEvent(ctx context.Context, e store.Event) {
	if e.PlanID != 0 {
		b.handlePlanEvent(ctx, e)
		return
	}
	if e.ItemID == 0 {
		if e.Kind == "control" {
			if err := b.updateControl(ctx); err != nil {
				b.c.Log.Error("slack: control message", "err", err.Error())
			}
			if str(e.Data, "reason") == "usage_limit" {
				b.post(ctx, "usage limit", b.usageLimitMessage(ctx, e))
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
			b.urgentFinding(ctx, d, ts)
			return
		}
	} else if e.Kind == "created" {
		return
	}
	if e.Kind == store.KindOrchEscalated && b.c.Orchestrator != nil {
		return // the @orchestrator app posts it, with its buttons
	}
	if m, ok := b.describe(ctx, e, d); ok {
		m.Channel, m.ThreadTS = ChannelLoop, ts
		b.post(ctx, "reply", m)
	}
	pm := b.parentMessage(d)
	pm.TS = ts
	b.update(ctx, "parent update", pm)
}

// urgentFinding mentions the approver in the thread of a P1 the loop filed itself (a red main):
// "drop everything" work nobody asked for must reach someone. The user's own P1s need no ping.
func (b *Bot) urgentFinding(ctx context.Context, d store.Detail, ts string) {
	it := d.Item
	if it.Priority != "P1" || it.Source == "user" {
		return
	}
	text := fmt.Sprintf("<@%s> P1, filed by the loop: %s", Approver, b.field(it.Title, 300, it.ID))
	b.post(ctx, "urgent finding", Message{Channel: ChannelLoop, ThreadTS: ts, Text: text, Blocks: []map[string]any{section(text)}})
}

func (b *Bot) hint(id int64) string {
	if b.c.LinkBase != "" {
		return fmt.Sprintf("%s/items/%d", b.c.LinkBase, id)
	}
	return fmt.Sprintf("loop show %d", id)
}

// refID is ref for an item known only by its id: the bare "#N" when it cannot be loaded.
func (b *Bot) refID(ctx context.Context, id int64) string {
	d, err := b.c.Store.GetItem(ctx, id)
	if err != nil {
		return fmt.Sprintf("#%d", id)
	}
	return ref(id, d.Item.Title)
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

// dropButton closes the item (store.Drop), after Slack's own confirmation dialog.
func dropButton(id int64, value string) map[string]any {
	b := button("Drop", "drop", value, "danger")
	b["confirm"] = map[string]any{"title": plain(fmt.Sprintf("Drop #%d?", id)),
		"text":    plain("It closes, a running agent stops, and its PR is closed (the branch stays)."),
		"confirm": plain("Drop"), "deny": plain("Cancel"), "style": "danger"}
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
	line := fmt.Sprintf("Priority *%s* · Stage *%s* · Source %s · Repo %s", it.Priority, d.Display+landWaiting(d), Escape(it.Source), repoText(it))
	if pr := b.prLink(it); pr != "" {
		line += " · " + pr
	}
	if it.ReopenOf != 0 {
		line += fmt.Sprintf(" · Reopens #%d", it.ReopenOf)
	}
	if len(d.WaitingOn) > 0 {
		line += " · Waiting on " + itemRefs(d.WaitingOn)
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

// itemRefs lists item ids as "#12, #13".
func itemRefs(ids []int64) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(out, ", ")
}

// landWaiting is what a land run has been waiting on for a while (its land_waiting event, while
// that is the item's latest), for the parent's stage line: ", waiting for CI (check) since 14:05
// UTC".
func landWaiting(d store.Detail) string {
	if d.Item.Stage != pipeline.Land || len(d.Events) == 0 {
		return ""
	}
	e := d.Events[len(d.Events)-1]
	if e.Kind != "land_waiting" {
		return ""
	}
	out := ", waiting for " + Escape(Clip(str(e.Data, "waiting_for"), 120, ""))
	if t, err := time.Parse(time.RFC3339, str(e.Data, "since")); err == nil {
		out += " since " + t.UTC().Format("15:04") + " UTC"
	}
	return out
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

// listOf is a list field of an event's data (JSON: a list of strings).
func listOf(d map[string]any, k string) []string {
	list, _ := d[k].([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func str(d map[string]any, k string) string {
	s, _ := d[k].(string)
	return s
}

// secondOpinionLine is a second_opinion event in the thread: counts only (the findings are in
// the PR comment).
func secondOpinionLine(d map[string]any) string {
	n := func(k string) int { f, _ := d[k].(float64); return int(f) }
	if str(d, "status") == "unavailable" {
		return fmt.Sprintf("Second opinion (Gemini) unavailable (%s); the review stands.", Escape(str(d, "reason")))
	}
	if n("findings") == 0 {
		return "Second opinion (Gemini): no findings."
	}
	text := fmt.Sprintf("Second opinion (Gemini): %d findings (%d high)", n("findings"), n("high"))
	switch {
	case str(d, "status") == "unjudged":
		return text + "; not judged (" + Escape(str(d, "reason")) + "); the review stands."
	case d["bounced"] == true:
		return text + fmt.Sprintf(", Claude agreed with %d (%d high); sent back to build.", n("agreed"), n("agreed_high"))
	case d["repeat"] == true:
		return text + fmt.Sprintf(", Claude agreed with %d (%d high); this head was sent back once already, so it stands.", n("agreed"), n("agreed_high"))
	case d["blocking"] == true:
		return text + fmt.Sprintf(", Claude agreed with %d (%d high); the review stands.", n("agreed"), n("agreed_high"))
	}
	return text + fmt.Sprintf(", Claude agreed with %d (%d high); advisory, recorded only.", n("agreed"), n("agreed_high"))
}

// describe is the thread reply for an event: one or two lines, or the question and held
// messages, which mention the approver.
func (b *Bot) describe(ctx context.Context, e store.Event, d store.Detail) (Message, bool) {
	it := d.Item
	id := it.ID
	f := func(k string, max int) string { return b.field(str(e.Data, k), max, id) }
	to := pipeline.Stage(e.To)

	if to == pipeline.NeedsYou && e.From != string(pipeline.NeedsYou) && str(e.Data, pipeline.FieldTrustSHA) != "" {
		return b.trustMessage(e, d), true
	}
	if to == pipeline.NeedsYou && e.From != string(pipeline.NeedsYou) {
		q := it.Question
		if it.Stage != pipeline.NeedsYou {
			q = str(e.Data, "question")
		}
		rebuildable := pipeline.Rebuildable(pipeline.Item{ReturnStage: pipeline.Stage(e.From)}) || pipeline.Rebuildable(it)
		reply := "Reply in this thread to answer."
		if rebuildable {
			reply = `Reply in this thread to answer, or start with "rebuild" (or use Rebuild…) to send it back to build with your note.`
		}
		if pipeline.Outcome(e.Outcome) == pipeline.Failed {
			// A run that ended without a usable result is a runner error, not a question; any
			// reply sends the item back to the stage that failed.
			reply = `Reply "retry" to run it again, or drop.`
		}
		text := fmt.Sprintf("<@%s> %s needs you: %s\n%s", Approver, ref(id, it.Title), b.field(q, 1500, id), reply)
		value := strconv.FormatInt(id, 10)
		buttons := []any{dropButton(id, value)}
		if rebuildable {
			buttons = append([]any{rebuildButton(value)}, buttons...)
		}
		return Message{Text: text, Blocks: []map[string]any{section(text),
			{"type": "actions", "block_id": "needs_you", "elements": buttons},
		}}, true
	}
	if e.Kind == "advanced" && pipeline.Outcome(e.Outcome) == pipeline.Hold {
		return b.heldMessage(e, d), true
	}
	if e.Kind == "advanced" && pipeline.Outcome(e.Outcome) == pipeline.Merged && to == pipeline.QA {
		return b.qaMessage(e, d), true
	}
	if e.Kind == "deployed" {
		return b.deployedMessage(e, d), true
	}
	if e.Kind == store.KindOrchApprovalAsked {
		// Asked to approve (or to OK trusted files), the orchestrator has the approval (or trust)
		// message posted again: the button is the user's, sha-bound.
		m := b.heldMessage(e, d)
		lead := "Orchestrator: approvals are yours, so here is its approval message again — " + f("reason", 300)
		if e.Data["trust"] == true {
			m = b.trustMessage(e, d)
			lead = "Orchestrator: trusted-file OKs are yours, so here is its trust message again — " + f("reason", 300)
		}
		m.Blocks = append([]map[string]any{section(lead)}, m.Blocks...)
		m.Text = lead + "\n" + m.Text
		return m, true
	}
	if e.Kind == store.KindOrchEscalated {
		// Only when loopd runs without the @orchestrator app (it posts these, with Accept).
		text := fmt.Sprintf("<@%s> Orchestrator: %s needs you: %s", Approver, ref(id, it.Title), f("text", 800))
		if r := str(e.Data, "recommendation"); r != "" {
			text += "\n*Recommend:* " + f("recommendation", 600) + " _Because_ " + f("reason", 400)
		}
		return Message{Text: text, Blocks: []map[string]any{section(text)}}, true
	}
	if e.Kind == store.KindUnservable {
		// loopd's own check (docs/decisions/2026-10-17-unservable-items.md): no claim can take it.
		text := fmt.Sprintf("<@%s> %s waits in %s, but no runner seen in the last %dh offers %s. Bring up a runner with it, or change the item's resources; it is claimed as soon as a runner offers it.",
			Approver, ref(id, it.Title), Escape(e.To), int(pipeline.RunnerSeenWindow.Hours()), b.field(strings.Join(listOf(e.Data, "missing"), " + "), 200, id))
		return Message{Text: text}, true
	}
	if e.Kind == store.KindServable {
		return Message{Text: fmt.Sprintf("A runner offers %s now; it can claim this from the %s queue.", b.field(strings.Join(listOf(e.Data, "missing"), " + "), 200, id), Escape(e.To))}, true
	}
	if strings.HasPrefix(e.Actor, orchestratorPrefix) {
		return Message{Text: b.orchestratorLine(ctx, e)}, true
	}
	by := who(e.Actor)
	if via := str(e.Data, "via"); strings.HasPrefix(via, orchestratorPrefix) {
		by += " via orchestrator"
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
		text = fmt.Sprintf("Answered by %s: %s → %s.", by, f("answer", 600), e.To)
		if e.Data["accepted"] != nil {
			text = fmt.Sprintf("Answered by %s, accepting the orchestrator's recommendation: %s → %s.", by, f("answer", 600), e.To)
		}
	case "approved":
		text = fmt.Sprintf("Approved by %s for `%s`: '%s' → land.", by, short(str(e.Data, "head_sha")), f("words", 300))
	case "trusted":
		text = fmt.Sprintf("Trusted files OK'd by %s for `%s`: verify runs that commit once → build.", by, short(str(e.Data, "head_sha")))
	case "held":
		text = fmt.Sprintf("Kept holding (%s).", by)
	case "rebuild":
		if strings.HasPrefix(e.Actor, "runner:") {
			text = "Back to build: " + f("notes", 400)
		} else {
			text = fmt.Sprintf("Sent back to build by %s: '%s'.", by, f("notes", 600))
		}
	case "patched":
		var parts []string
		if p, ok := e.Data["priority"].(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("Priority %s → %s", str(p, "from"), str(p, "to")))
		}
		if t, ok := e.Data["title"].(map[string]any); ok {
			parts = append(parts, "Title: "+b.field(str(t, "to"), 300, id))
		}
		text = strings.Join(parts, "; ") + fmt.Sprintf(" (%s).", by)
	case "refused":
		text = fmt.Sprintf("Refused %s from %s: %s", Escape(str(e.Data, "action")), by, f("message", 300))
	case "guard_refused":
		// The rule and the tool only: the refused command or path can hold anything.
		text = fmt.Sprintf("Guard refused a %s call (%s); details on loopd (`loop show %d`).", Escape(str(e.Data, "tool")), Escape(str(e.Data, "rule")), id)
	case "builder_fallback":
		if e.Data["this_run_only"] != nil {
			text = "No qwen builder on this runner: this build runs with claude."
		} else {
			text = "Builder: qwen → claude for the rest of this item (second strike)."
		}
	case store.KindSecondOpinion:
		text = secondOpinionLine(e.Data)
	case "low_cache_hit":
		text = fmt.Sprintf("Low prompt-cache hit ratio on this run (%s over %s turns).", Escape(str(e.Data, "cache_hit_ratio")), Escape(str(e.Data, "num_turns")))
	case "approval_cancelled":
		text = fmt.Sprintf("Reply approval cancelled by %s; still held.", by)
	case "land_waiting":
		return Message{}, false // pending CI does not post; the parent's stage line says it
	case "main_ci":
		text = b.mainCILine(ctx, e, id)
	case "dropped":
		text = "Dropped by " + by
		if w := str(e.Data, "words"); w != "" {
			text += ": '" + f("words", 300) + "'"
		}
		text += ". Closed"
		if pr := b.prLink(it); pr != "" {
			text += "; the runner closes " + pr + " (the branch stays)"
		}
		text += "."
	case "pr_closed":
		return Message{}, false // the dropped line already said so
	case "qa_marked":
		text = fmt.Sprintf("Marked for your check once merged (%s): it waits in qa after it lands.", by)
	case "verified":
		text = "Verified by " + by
		if w := str(e.Data, "words"); w != "" && w != VerifiedWords {
			text += ": '" + f("words", 300) + "'"
		}
		text += ". Done."
	case "reopened":
		text = fmt.Sprintf("Reopened by %s: '%s'. Filed #%v (%s) with those words; this one is done.",
			by, f("words", 300), e.Data["reopen_item"], pipeline.ReopenPriority)
	case "advanced":
		switch pipeline.Outcome(e.Outcome) {
		case pipeline.Reproduced:
			// From the event, not the item: the item may have moved on by the time this posts.
			text = fmt.Sprintf("Reproduced: area %s, size %s, risk %s → ready.", f("area", 100), f("size", 10), f("risk", 10))
			if r := str(e.Data, "repo"); r != "" {
				text = fmt.Sprintf("Reproduced in %s: area %s, size %s, risk %s → ready.", f("repo", 100), f("area", 100), f("size", 10), f("risk", 10))
			}
		case pipeline.Routed:
			text = fmt.Sprintf("Triage: this is %s — %s. Triaging it there next.", f("repo", 100), f("why", 500))
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
		case pipeline.VerifyFailed:
			// The step's name only: its output is on loopd and in the next build's notes.
			text = fmt.Sprintf("Runner verify \"%s\" failed; nothing pushed. Back to build (%v); the log is evidence (`loop show %d`).",
				f("verify_step", 100), e.Data["bounce_count"], id)
		case pipeline.Merged:
			text = fmt.Sprintf("Merged %s as `%s`. Done.", b.prLink(it), short(str(e.Data, "merge_sha")))
			if str(e.Data, "main_ci") == "watching" {
				where := ""
				if str(e.Data, "merged_outside") == "true" {
					where = " on GitHub, outside the loop"
				}
				text = fmt.Sprintf("Merged %s as `%s`%s; watching main's CI.", b.prLink(it), short(str(e.Data, "merge_sha")), where)
			}
		case pipeline.BackToBuild:
			text = "Back to build: " + f("reason", 400)
		case pipeline.BackToReview:
			text = "Back to review: " + f("notes", 400)
		default:
			text = fmt.Sprintf("%s → %s.", Escape(e.Outcome), e.To)
		}
	default:
		text = Escape(e.Kind) + "."
	}
	return Message{Text: text}, true
}

// orchestratorLine is an orchestrator's own action on the item, with its reason: "Orchestrator:
// retried the run (failure 1) → build — index.lock left by a killed run."
func (b *Bot) orchestratorLine(ctx context.Context, e store.Event) string {
	f := func(k string, max int) string { return b.field(str(e.Data, k), max, e.ItemID) }
	var what string
	switch e.Kind {
	case "answered":
		what = fmt.Sprintf("answered (%s) '%s' → %s", f("answer_kind", 20), f("answer", 600), e.To)
	case "retried":
		what = fmt.Sprintf("retried the run (failure %v) → %s", e.Data["failure"], e.To)
	case "patched":
		what = "changed the item"
		if p, ok := e.Data["priority"].(map[string]any); ok {
			what = fmt.Sprintf("priority %s → %s", str(p, "from"), str(p, "to"))
		}
	case store.KindLinked:
		what = "now waits on #?"
		if after, ok := e.Data["after"].(float64); ok {
			what = "now waits on " + b.refID(ctx, int64(after))
		}
	case store.KindOrchAdded:
		what = "filed this item"
	case store.KindOrchSeenAgain:
		what = "saw this again"
	case "refused":
		return fmt.Sprintf("Orchestrator: tried to %s; refused: %s", Escape(str(e.Data, "action")), f("message", 300))
	default:
		what = Escape(e.Kind)
	}
	if r := f("reason", 400); r != "" {
		what += " — " + r
	}
	return "Orchestrator: " + what + "."
}

// mainCILine is main's CI on an item's merge commit, as the land script recorded it.
func (b *Bot) mainCILine(ctx context.Context, e store.Event, id int64) string {
	sha := short(str(e.Data, "merge_sha"))
	switch str(e.Data, "result") {
	case "green":
		return fmt.Sprintf("main green on `%s`.", sha)
	case "red":
		var names []string
		if list, ok := e.Data["failing"].([]any); ok {
			for _, x := range list {
				names = append(names, fmt.Sprint(x))
			}
		}
		failing := b.field(strings.Join(names, ", "), 300, id)
		if p1, ok := e.Data["p1_item"].(float64); ok && p1 > 0 {
			return fmt.Sprintf("main red on `%s` (failing: %s): filed %s (P1).", sha, failing, b.refID(ctx, int64(p1)))
		}
		return fmt.Sprintf("main red on `%s` (failing: %s); no P1 item could be filed, see `loop show %d`.", sha, failing, id)
	case "superseded":
		return fmt.Sprintf("main's CI on `%s` was cancelled (likely by a newer push to main); nothing failed, no P1 filed.", sha)
	case "timeout":
		return fmt.Sprintf("main's CI on `%s` did not finish in time; see the runs on GitHub.", sha)
	}
	return "main's CI: " + Escape(str(e.Data, "result")) + "."
}

// heldMessage asks the approver to decide a held merge, for the head the land stage reported.
func (b *Bot) heldMessage(e store.Event, d store.Detail) Message {
	it := d.Item
	sha := str(e.Data, "head_sha")
	if sha == "" {
		sha = it.HeadSHA
	}
	reasons := it.HoldReason
	text := fmt.Sprintf("<@%s> %s is held for your approval: %s at `%s`.\nWhy: %s\nApprove with the button or a reply here (\"approve\", \"lgtm\", …).",
		Approver, ref(it.ID, it.Title), b.prLink(it), short(sha), b.field(reasons, 800, it.ID))
	value := fmt.Sprintf("%d:%s", it.ID, sha)
	return Message{Text: fmt.Sprintf("<@%s> %s is held for your approval (%s)", Approver, ref(it.ID, it.Title), short(sha)), Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "held", "elements": []any{
			button("Approve merge", "approve", value, "primary"),
			button("Keep holding", "keep_holding", value, ""),
			rebuildButton(value),
			dropButton(it.ID, value),
		}},
	}}
}

// usageLimitMessage tells the approver in #loop-digest that a usage limit set the active agents
// to 0, with the reset time when the run reported one. Nothing raises the number again on its own.
func (b *Bot) usageLimitMessage(ctx context.Context, e store.Event) Message {
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
		text += " on " + b.refID(ctx, id)
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
	Type      string              `json:"type"`
	User      struct{ ID string } `json:"user"`
	Channel   struct{ ID string } `json:"channel"`
	TriggerID string              `json:"trigger_id"`
	// View is a submitted modal (type view_submission).
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]struct {
				Value *string `json:"value"`
			} `json:"values"`
		} `json:"state"`
	} `json:"view"`
	Message struct {
		TS       string           `json:"ts"`
		ThreadTS string           `json:"thread_ts"`
		Blocks   []map[string]any `json:"blocks"`
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
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		switch p.Type {
		case "block_actions":
			b.onActions(ctx, p)
		case "view_submission":
			b.onViewSubmission(ctx, p)
		}
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

// toOrchestrator reports whether a message is addressed to the @orchestrator app: its own socket
// records it, so the loop bot leaves it alone ("@orchestrator answer #46: …" is not an answer,
// "@orchestrator approve #28" not an approval). Until the orchestrator's auth.test has answered,
// a message that mentions anyone but the loop bot itself counts as the orchestrator's (and any
// mention does, while the loop bot does not know its own id either): left alone, never misread.
func (b *Bot) toOrchestrator(ctx context.Context, text string) bool {
	o := b.c.Orchestrator
	if o == nil {
		return false
	}
	if o.BotUser() != "" {
		return o.Mentions(text)
	}
	users := userMentionRe.FindAllStringSubmatch(text, -1)
	if len(users) == 0 {
		return false
	}
	self := b.selfUser(ctx)
	for _, u := range users {
		if self == "" || u[1] != self {
			return true
		}
	}
	return false
}

var userMentionRe = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)

// selfUser is the loop bot's own Slack user id (auth.test, asked once it answers), or "".
func (b *Bot) selfUser(ctx context.Context) string {
	b.mu.Lock()
	self := b.self
	b.mu.Unlock()
	if self != "" {
		return self
	}
	id, err := b.c.API.AuthTest(ctx)
	if err != nil {
		b.c.Log.Warn("slack: auth.test; mentions are left to the orchestrator until it answers", "err", err.Error())
		return ""
	}
	b.mu.Lock()
	b.self = id
	b.mu.Unlock()
	return id
}

func (b *Bot) onMessage(ctx context.Context, m slackEvent) {
	if m.BotID != "" || m.Subtype != "" || m.User == "" || b.toOrchestrator(ctx, m.Text) {
		return
	}
	if m.Channel == ChannelPlan {
		b.onPlanMessage(ctx, m)
		return
	}
	if m.Channel != ChannelLoop || m.ThreadTS == "" || m.ThreadTS == m.TS {
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
	text := incoming(m.Text)
	switch d.Item.Stage {
	case pipeline.NeedsYou:
		if !IsApprover(m.User) {
			b.refuse(ctx, m.Channel, m.User, id, "answer")
			return
		}
		if text == "" {
			return
		}
		if waitsForTrust(d.Item) && IsTrustReply(text) {
			b.trustReply(ctx, m, d.Item)
			return
		}
		if pipeline.Rebuildable(d.Item) && IsRebuildNote(text) {
			// The user's note for the builder, not an answer: back to build with it (a red CI's
			// rerun would test the same merge again).
			if _, err := b.c.Store.Rebuild(ctx, actorPrefix+m.User, id, text); err != nil {
				b.threadReply(ctx, m.ThreadTS, "Could not send it back to build: "+Escape(errMessage(err)))
			}
			return
		}
		if _, err := b.c.Store.Answer(ctx, actorPrefix+m.User, id, text); err != nil {
			b.threadReply(ctx, m.ThreadTS, "Could not record that answer: "+Escape(errMessage(err)))
		}
	case pipeline.QA:
		b.qaReply(ctx, m, id, text)
	case pipeline.Held:
		if !IsApprover(m.User) {
			b.refuse(ctx, m.Channel, m.User, id, "approve")
			return
		}
		// A change request wins over consent words in it ("ok, rebuild", "merge it after adding a test").
		switch {
		case IsHeldNote(text):
			// The user's note for the builder: back to build with it (the event's line says so).
			b.cancelPending(id)
			if _, err := b.c.Store.Rebuild(ctx, actorPrefix+m.User, id, text); err != nil {
				b.threadReply(ctx, m.ThreadTS, "Could not send it back to build: "+Escape(errMessage(err)))
			}
		case ClassifyReply(text):
			b.startApproval(ctx, d.Item, text, m.ThreadTS)
		default:
			b.threadReply(ctx, m.ThreadTS, "Not taken as an approval; the item stays held. Reply 'approve' or use the button, or say what to change to send it back to build.")
		}
	}
}

func (b *Bot) onMention(ctx context.Context, m slackEvent) {
	if m.BotID != "" || m.User == "" || b.toOrchestrator(ctx, m.Text) {
		return
	}
	// In #plan every message is the conversation's (onMessage), mentioning the bot or not.
	if m.Channel == ChannelPlan {
		return
	}
	// A mention inside an item's thread is a reply; onMessage handles it.
	if m.Channel == ChannelLoop && m.ThreadTS != "" {
		if id, _ := b.c.Store.ItemByThread(ctx, ChannelLoop, m.ThreadTS); id != 0 {
			return
		}
	}
	if !IsApprover(m.User) {
		b.refuse(ctx, m.Channel, m.User, 0, "add")
		return
	}
	text := incoming(m.Text)
	if _, err := b.add(ctx, m.User, text); err != nil {
		b.ephemeral(ctx, m.Channel, m.User, Escape(errMessage(err))+" Try: @jarvis-loop <what to do>")
	}
}

func (b *Bot) add(ctx context.Context, user, text string) (pipeline.Item, error) {
	after, rest := ParseAfter(text)
	repo, rest := ParseRepo(rest, b.c.Repos, b.c.Aliases)
	pri, title := ParsePriority(rest)
	if title == "" {
		return pipeline.Item{}, &pipeline.Error{Code: pipeline.CodeMissingField, Message: "Say what to add."}
	}
	return b.c.Store.AddItem(ctx, actorPrefix+user, store.NewItem{Title: title, Ask: text, Repo: repo, Priority: pri, Source: "user", DependsOn: after})
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
		if strings.HasPrefix(a.ActionID, "plan_") {
			b.onPlanAction(ctx, channel, msgTS, user, a.ActionID, a.Value)
			continue
		}
		itemID := actionItem(a.ActionID, a.BlockID, a.Value)
		if itemID == 0 && a.ActionID == "cancel_approval" {
			b.mu.Lock()
			if pa := b.pending[a.Value]; pa != nil {
				itemID = pa.itemID
			}
			b.mu.Unlock()
		}
		if !IsApprover(user) {
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
			b.approveButton(ctx, channel, msgTS, user, itemID, a.Value)
		case "trust":
			_, sha, _ := strings.Cut(a.Value, ":")
			b.trust(ctx, channel, msgTS, user, itemID, sha, "button")
		case "drop":
			b.cancelPending(itemID)
			if _, err := b.c.Store.Drop(ctx, actor, itemID, "Drop button"); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		case "qa_verified":
			b.clearReopen(itemID)
			if _, err := b.c.Store.Verified(ctx, actor, itemID, VerifiedWords); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		case "qa_reopen":
			b.reopenButton(ctx, channel, user, itemID)
		case "rebuild":
			b.openRebuild(ctx, channel, user, p.TriggerID, itemID)
		case "keep_holding":
			b.cancelPending(itemID)
			if _, err := b.c.Store.Hold(ctx, actor, itemID, "Keep holding"); err != nil {
				b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
			}
		case "cancel_approval":
			b.cancelApproval(ctx, channel, msgTS, user, a.Value)
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

// actionItem finds the item an action is about: the parent's block_id "item:N", or a held
// button's value "N:sha", or a Drop button's "N" or "N:sha", or a qa button's "N".
func actionItem(actionID, blockID, value string) int64 {
	if rest, ok := strings.CutPrefix(blockID, "item:"); ok {
		id, _ := strconv.ParseInt(rest, 10, 64)
		return id
	}
	if actionID == "approve" || actionID == "trust" || actionID == "keep_holding" || actionID == "rebuild" || actionID == "drop" || actionID == "qa_verified" || actionID == "qa_reopen" {
		n, _, _ := strings.Cut(value, ":")
		id, _ := strconv.ParseInt(n, 10, 64)
		return id
	}
	return 0
}
