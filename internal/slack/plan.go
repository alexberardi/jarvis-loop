package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// #plan (docs/phases/07-planning.md, "Slack"): one thread per plan. The approver's top-level
// message starts a plan; their replies in its thread are the conversation with the planning
// agents (each queues a turn); the agent's reply comes back in the thread. A header message (the
// bot's first reply, or the thread's parent when the bot started it) shows the plan's stage and
// holds the Drop button. Approve buttons on a ready reply take the same 2-minute window as a
// reply approval of a merge, bound to the document's sha256.

// planReplyMax is how much of an agent's reply (at most 3,000 characters) a message shows.
const planReplyMax = 1800

// planHint is where a plan's full text is, for clipped messages.
func (b *Bot) planHint(id int64) string {
	if b.c.LinkBase != "" {
		return fmt.Sprintf("%s/board/plans/%d", b.c.LinkBase, id)
	}
	return fmt.Sprintf("GET /plans/P%d", id)
}

func (b *Bot) planField(s string, max int, id int64) string {
	return Escape(Clip(strings.TrimSpace(s), max, b.planHint(id)))
}

// docLink names a document version, linking to the plan's page on the board when loopd has a
// LinkBase (the page is the board's, docs/phases/07-planning.md "The board").
func (b *Bot) docLink(id int64, kind string, version int) string {
	name := fmt.Sprintf("%s v%d", kind, version)
	if b.c.LinkBase != "" {
		return fmt.Sprintf("<%s/board/plans/%d|%s>", b.c.LinkBase, id, name)
	}
	return fmt.Sprintf("%s (`GET /plans/P%d/docs/%s?v=%d` on loopd)", name, id, kind, version)
}

// ---- Slack → plans ----

// startPlan starts a plan from the user's words, as /loop add reads them: priority words and the
// repo anywhere; the title is the first line without them.
func (b *Bot) startPlan(ctx context.Context, actor, text, channel, ts string) (pipeline.Plan, error) {
	repo, rest := ParseRepo(text, b.c.Repos, b.c.Aliases)
	pri, all := ParsePriority(rest)
	first, _, _ := strings.Cut(text, "\n")
	_, first = ParseRepo(first, b.c.Repos, b.c.Aliases)
	_, title := ParsePriority(first)
	if title == "" {
		title = all
	}
	if title == "" {
		return pipeline.Plan{}, &pipeline.Error{Code: pipeline.CodeMissingField, Message: "Say what the plan is for."}
	}
	var repos []string
	if repo != "" {
		repos = []string{repo}
	}
	return b.c.Store.CreatePlan(ctx, actor, store.NewPlan{Title: Clip(title, 200, ""), Priority: pri, Repos: repos, Text: text,
		SlackChannel: channel, SlackTS: ts})
}

// parsePlanRef reads "P3" or "p3" (a plan, not an item: "3" is item #3).
func parsePlanRef(s string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.ToUpper(strings.TrimSpace(s)), "P")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// ---- plan events → the thread ----

func (b *Bot) handlePlanEvent(ctx context.Context, e store.Event) {
	d, err := b.c.Store.GetPlan(ctx, e.PlanID)
	if err != nil {
		b.c.Log.Error("slack: load plan", "plan", e.PlanID, "err", err.Error())
		return
	}
	if !b.ensurePlanThread(ctx, &d) {
		return
	}
	p := d.Plan
	if m, ok := b.describePlan(e, d); ok {
		m.Channel, m.ThreadTS = ChannelPlan, p.SlackTS
		b.post(ctx, "plan reply", m)
	}
	switch e.Kind {
	case "plan_turn_claimed":
		b.react(ctx, d, e, ReactionRunning, "")
	case "plan_turn_done":
		b.react(ctx, d, e, ReactionDone, ReactionRunning)
	case "plan_turn_failed", "plan_turn_expired", "plan_turn_stopped":
		b.react(ctx, d, e, "", ReactionRunning)
	}
	hm := b.planHeader(d)
	hm.TS = p.SlackHeader
	if b.headerChanged(p.ID, hm) && b.update(ctx, "plan header", hm) != nil {
		b.headerChanged(p.ID, Message{}) // unknown: update it next time
	}
}

// headerChanged records m as the plan's header and reports whether it differs from the last one.
func (b *Bot) headerChanged(id int64, m Message) bool {
	raw, _ := json.Marshal(m.Blocks)
	key := m.Text + "\n" + string(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.planHeaders[id] == key {
		return false
	}
	b.planHeaders[id] = key
	return true
}

// ensurePlanThread makes sure the plan has its thread in #plan and the header message in it,
// posting what is missing: the parent, for a plan started outside #plan (/loop plan, the API),
// or the header reply under the user's own message. It reports whether the plan's thread is
// usable.
func (b *Bot) ensurePlanThread(ctx context.Context, d *store.PlanDetail) bool {
	p := &d.Plan
	switch {
	case p.SlackTS == "":
		ts, err := b.post(ctx, "plan parent", b.planHeader(*d))
		if err != nil {
			return false
		}
		p.SlackChannel, p.SlackTS, p.SlackHeader = ChannelPlan, ts, ts
		b.headerChanged(p.ID, b.planHeader(*d))
	case p.SlackChannel != ChannelPlan:
		return false // a thread somewhere the bot does not post
	case p.SlackHeader == "":
		hm := b.planHeader(*d)
		hm.ThreadTS = p.SlackTS
		ts, err := b.post(ctx, "plan header", hm)
		if err != nil {
			return false
		}
		p.SlackHeader = ts
		b.headerChanged(p.ID, hm)
	default:
		return true
	}
	if err := b.c.Store.SetPlanSlack(ctx, p.ID, p.SlackChannel, p.SlackTS, p.SlackHeader); err != nil {
		b.c.Log.Error("slack: record plan thread", "plan", p.ID, "err", err.Error())
	}
	return true
}

// react puts add on the messages of the event's turn and takes remove off them. Reactions are a
// nicety: a failure (no reactions:write scope, a deleted message) is logged and the rest goes on.
func (b *Bot) react(ctx context.Context, d store.PlanDetail, e store.Event, add, remove string) {
	turnID := eventInt(e.Data, "turn")
	for _, t := range d.Turns {
		if t.ID != turnID {
			continue
		}
		for _, ts := range t.SlackTS {
			if remove != "" {
				if err := b.c.API.ReactionsRemove(ctx, ChannelPlan, ts, remove); err != nil {
					b.c.Log.Warn("slack: could not remove a reaction", "plan", d.Plan.ID, "err", err.Error())
				}
			}
			if add != "" {
				if err := b.c.API.ReactionsAdd(ctx, ChannelPlan, ts, add); err != nil {
					b.c.Log.Warn("slack: could not add a reaction", "plan", d.Plan.ID, "err", err.Error())
				}
			}
		}
	}
}

func eventInt(data map[string]any, k string) int64 {
	switch v := data[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func planTurn(d store.PlanDetail, id int64) (pipeline.PlanTurn, bool) {
	for _, t := range d.Turns {
		if t.ID == id {
			return t, true
		}
	}
	return pipeline.PlanTurn{}, false
}

// planDropButton drops the plan, after Slack's own confirmation dialog.
func planDropButton(id int64) map[string]any {
	b := button("Drop", "plan_drop", fmt.Sprintf("P%d", id), "danger")
	b["confirm"] = map[string]any{"title": plain(fmt.Sprintf("Drop P%d?", id)),
		"text":    plain("The conversation ends and a running turn stops. Items already filed stay."),
		"confirm": plain("Drop"), "deny": plain("Cancel"), "style": "danger"}
	return b
}

// planHeader is the plan's header message: what it is, where it is, whose turn, and Drop.
func (b *Bot) planHeader(d store.PlanDetail) Message {
	p := d.Plan
	head := fmt.Sprintf("P%d %s", p.ID, b.planField(p.Title, 300, p.ID))
	repos := "not set, the product agent asks"
	if len(p.Repos) > 0 {
		repos = Escape(strings.Join(p.Repos, ", "))
	}
	line := fmt.Sprintf("Plan · Stage *%s* · Priority *%s* · Repos %s", p.Stage, p.Priority, repos)
	switch d.Turn {
	case "agent":
		line += " · the agent's turn"
	case "you":
		line += " · your turn: reply in this thread"
	}
	if len(d.Items) > 0 {
		ids := make([]int64, len(d.Items))
		for i, it := range d.Items {
			ids[i] = it.ID
		}
		line += " · Items " + itemRefs(ids)
	}
	if b.c.LinkBase != "" {
		line += fmt.Sprintf(" · <%s/board/plans/%d|plan page>", b.c.LinkBase, p.ID)
	}
	text := "*" + head + "*\n" + line
	sec := section(text)
	sec["block_id"] = fmt.Sprintf("plan:%d", p.ID)
	blocks := []map[string]any{sec}
	if p.Stage.Open() {
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "plan", "elements": []any{planDropButton(p.ID)}})
	}
	return Message{Channel: ChannelPlan, Text: fmt.Sprintf("%s — %s, %s", head, p.Priority, p.Stage), Blocks: blocks}
}

// describePlan is the thread reply for a plan event, if it gets one.
func (b *Bot) describePlan(e store.Event, d store.PlanDetail) (Message, bool) {
	p := d.Plan
	var text string
	switch e.Kind {
	case "plan_turn_done":
		t, ok := planTurn(d, eventInt(e.Data, "turn"))
		if !ok {
			return Message{}, false
		}
		text = b.planField(t.Reply, planReplyMax, p.ID)
		if t.Ready {
			if m, ok := b.readyMessage(d, t, text); ok {
				return m, true
			}
		}
	case "plan_turn_failed":
		text = "That turn failed: " + b.planField(str(e.Data, "error"), 400, p.ID)
		if p.Stage.Talking() {
			text += ". Reply to try again."
		} else {
			text += "."
		}
	case "back_to_brief":
		text = "The engineering agent sent the plan back to the brief: " + b.planField(str(e.Data, "reason"), 600, p.ID) +
			"\nThe product agent picks it up from there."
	case "plan_approved":
		kind := str(e.Data, "kind")
		text = fmt.Sprintf("%s v%d approved by %s → %s.", capitalize(kind), eventInt(e.Data, "version"), who(e.Actor), e.To)
		if kind == pipeline.DocBrief {
			text += " The engineering agent writes the blueprint next."
		}
	case "split":
		return b.splitMessage(d), true
	case "plan_dropped":
		text = "Dropped by " + who(e.Actor)
		if w := str(e.Data, "words"); w != "" {
			text += ": '" + b.planField(w, 300, p.ID) + "'"
		}
		text += "."
		if len(d.Items) > 0 {
			text += " The items it filed stay."
		}
	case "plan_done":
		text = "All its items are settled: the plan is done."
	case "refused":
		text = fmt.Sprintf("Refused %s from %s: %s", Escape(str(e.Data, "action")), who(e.Actor), b.planField(str(e.Data, "message"), 300, p.ID))
	default:
		return Message{}, false
	}
	return Message{Text: text}, true
}

// readyMessage is the agent's reply with the Approve button for the document it says is ready:
// the version this turn wrote, or the latest when it wrote none.
func (b *Bot) readyMessage(d store.PlanDetail, t pipeline.PlanTurn, reply string) (Message, bool) {
	kind := pipeline.DocBrief
	if t.Persona == pipeline.PersonaEngineering {
		kind = pipeline.DocBlueprint
	}
	var doc *pipeline.PlanDoc
	for i, x := range d.Docs {
		if x.Kind == kind {
			doc = &d.Docs[i] // oldest first: the last one is the latest
		}
	}
	for _, ev := range d.Events {
		if ev.Kind == "plan_doc" && str(ev.Data, "kind") == kind && eventInt(ev.Data, "turn") == t.ID {
			for i, x := range d.Docs {
				if x.Kind == kind && int64(x.Version) == eventInt(ev.Data, "version") {
					doc = &d.Docs[i]
				}
			}
		}
	}
	if doc == nil {
		return Message{}, false
	}
	label := fmt.Sprintf("Approve %s v%d", kind, doc.Version)
	text := reply + "\n\n" + fmt.Sprintf("Ready for your approval: %s (`%s`).", b.docLink(d.Plan.ID, kind, doc.Version), short(doc.SHA256))
	value := fmt.Sprintf("P%d:%s:%d:%s", d.Plan.ID, kind, doc.Version, doc.SHA256)
	return Message{Text: fmt.Sprintf("%s v%d of P%d is ready for your approval", kind, doc.Version, d.Plan.ID), Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "plan_ready", "elements": []any{button(label, "plan_approve", value, "primary")}},
	}}, true
}

// splitMessage lists the items the blueprint's hand-off filed.
func (b *Bot) splitMessage(d store.PlanDetail) Message {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Filed %d items from the blueprint (their threads are in <#%s>):", len(d.Items), ChannelLoop)
	for _, it := range d.Items {
		fmt.Fprintf(&sb, "\n• #%d %s", it.ID, b.planField(it.Title, 200, d.Plan.ID))
		if len(it.DependsOn) > 0 {
			sb.WriteString(" (after " + itemRefs(it.DependsOn) + ")")
		}
	}
	text := sb.String()
	return Message{Text: text, Blocks: []map[string]any{section(text)}}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- /loop plans, /loop plan, /loop drop P<n> ----

func (b *Bot) plansList(ctx context.Context) string {
	rows, err := b.c.Store.ListPlans(ctx, true)
	if err != nil {
		return "Could not read the plans."
	}
	if len(rows) == 0 {
		return fmt.Sprintf("No open plans. Start one with a message in <#%s>.", ChannelPlan)
	}
	var sb strings.Builder
	sb.WriteString("*Open plans*")
	for _, r := range rows {
		turn := ""
		switch r.Turn {
		case "agent":
			turn = ", the agent's turn"
		case "you":
			turn = ", your turn"
		}
		fmt.Fprintf(&sb, "\n• P%d %s %s — %s%s", r.ID, r.Priority, b.planField(r.Title, 120, r.ID), r.Stage, turn)
	}
	return sb.String()
}

func (b *Bot) planCommand(ctx context.Context, user, rest string) string {
	p, err := b.startPlan(ctx, actorPrefix+user, rest, "", "")
	if err != nil {
		return Escape(errMessage(err)) + " Try: `/loop plan <the idea> [in <repo>]`, or a message in <#" + ChannelPlan + ">."
	}
	return fmt.Sprintf("Started P%d (%s): %s. Its thread is in <#%s>.", p.ID, p.Priority, Escape(p.Title), ChannelPlan)
}

func (b *Bot) dropPlanCommand(ctx context.Context, actor string, id int64, words string) string {
	p, err := b.c.Store.DropPlan(ctx, actor, id, words)
	if err != nil {
		return Escape(errMessage(err))
	}
	b.cancelPlanPending(p.ID)
	return fmt.Sprintf("P%d dropped.", p.ID)
}
