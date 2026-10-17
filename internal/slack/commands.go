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

const helpText = "*/loop commands*\n" +
	"`/loop help` this list\n" +
	"`/loop add <what>` add work; say \"urgent\", \"high priority\", \"low priority\" or \"someday\" to set its priority (default P3), and \"in <repo>\", \"<repo>:\" or \"[<repo>]\" to name its repo, also by a short word at the start or end (\"[mobile] …\", \"nest: …\", \"… in docs\"; default: triage picks one), and \"after #12\" or \"after #12, #13\" to wait for those items to be done\n" +
	"`/loop status` the board\n" +
	"`/loop show <id>` one item\n" +
	"`/loop priority <id> P1`\u2013`P5` change a priority\n" +
	"`/loop drop <id> [why]` close an item you won't land (its PR is closed, the branch stays); `/loop drop P<n>` drops a plan\n" +
	"`/loop plans` the open plans\n" +
	"`/loop plan <idea>` start a plan (as a message in #plan does): priority and repo words as for add\n" +
	"`/loop qa <id>` check it yourself once merged: it waits in qa after it lands\n" +
	"`/loop verified <id>` your check passed (qa → done)\n" +
	"`/loop reopen <id> <what is wrong>` your check failed: files a P2 item with your words (this one → done)\n" +
	"`/loop agents <0-6>` how many agents may run at once (0 = paused)\n" +
	"`/loop stop` / `/loop resume` stop running agents / start again\n" +
	"`/loop usage <N>%` record what /usage shows\n" +
	"Answer questions and approve held merges in the item's thread in #loop; talk to a plan in its thread in #plan."

type command struct {
	Command   string `json:"command"`
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
}

func ephemeralPayload(text string) map[string]any {
	return map[string]any{"response_type": "ephemeral", "text": Clip(text, MaxChars, "loop status")}
}

// HandleCommand answers /loop; the answer rides on the envelope's ack, visible only to the user
// who typed it. Changes are the approver's alone.
func (b *Bot) HandleCommand(ctx context.Context, env Envelope) map[string]any {
	var c command
	if json.Unmarshal(env.Payload, &c) != nil {
		return ephemeralPayload(helpText)
	}
	return ephemeralPayload(b.runCommand(ctx, c))
}

// Late delivers a command answer that missed its ack (see Socket.OnLate), as an ephemeral
// message, and only in the two channels the bot may post in.
func (b *Bot) Late(ctx context.Context, env Envelope, payload map[string]any) {
	var c command
	if json.Unmarshal(env.Payload, &c) != nil {
		return
	}
	text, _ := payload["text"].(string)
	b.ephemeral(ctx, c.ChannelID, c.UserID, text)
}

func (b *Bot) runCommand(ctx context.Context, c command) string {
	c.Text = Unescape(c.Text)
	fields := strings.Fields(c.Text)
	if len(fields) == 0 {
		return helpText
	}
	verb, args := strings.ToLower(fields[0]), fields[1:]
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Text), fields[0]))
	actor := actorPrefix + c.UserID
	approverOnly := map[string]bool{"add": true, "priority": true, "agents": true, "stop": true, "resume": true, "usage": true, "drop": true,
		"qa": true, "verified": true, "reopen": true, "plan": true}
	if approverOnly[verb] && !IsApprover(c.UserID) {
		var itemID int64
		if (verb == "priority" || verb == "drop" || verb == "qa" || verb == "verified" || verb == "reopen") && len(args) > 0 {
			itemID, _ = strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
			if _, err := b.c.Store.GetItem(ctx, itemID); err != nil {
				itemID = 0
			}
		}
		b.refuse(ctx, "", c.UserID, itemID, "/loop "+verb)
		return "Sorry, only the approver can do that."
	}
	switch verb {
	case "help":
		return helpText
	case "add":
		it, err := b.add(ctx, c.UserID, rest)
		if err != nil {
			return Escape(errMessage(err)) + " " + helpText
		}
		where := it.Priority
		if it.Repo != "" {
			where += ", " + Escape(it.Repo)
		}
		if len(it.DependsOn) > 0 {
			where += ", after " + itemRefs(it.DependsOn)
		}
		return fmt.Sprintf("Added #%d (%s): %s. Its thread is in <#%s>.", it.ID, where, Escape(it.Title), ChannelLoop)
	case "status":
		return b.status(ctx)
	case "show":
		if len(args) != 1 {
			return helpText
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		if err != nil {
			return helpText
		}
		return b.show(ctx, id)
	case "priority":
		if len(args) != 2 {
			return helpText
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		if err != nil {
			return helpText
		}
		p := strings.ToUpper(args[1])
		it, err := b.c.Store.PatchItem(ctx, actor, id, &p, nil)
		if err != nil {
			return Escape(errMessage(err))
		}
		return fmt.Sprintf("%s is now %s.", ref(it.ID, it.Title), it.Priority)
	case "plans":
		return b.plansList(ctx)
	case "plan":
		return b.planCommand(ctx, c.UserID, rest)
	case "drop":
		if len(args) == 0 {
			return helpText
		}
		if pid, ok := parsePlanRef(args[0]); ok {
			return b.dropPlanCommand(ctx, actor, pid, strings.TrimSpace(strings.TrimPrefix(rest, args[0])))
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		if err != nil {
			return helpText
		}
		words := strings.TrimSpace(strings.TrimPrefix(rest, args[0]))
		it, err := b.c.Store.Drop(ctx, actor, id, words)
		if err != nil {
			return Escape(errMessage(err))
		}
		b.cancelPending(it.ID)
		return ref(it.ID, it.Title) + " dropped (closed)."
	case "qa", "verified", "reopen":
		return b.qaCommand(ctx, actor, verb, args, rest)
	case "agents":
		n, err := strconv.Atoi(strings.Join(args, ""))
		if err != nil || n < 0 || n > 6 {
			return "Give a number from 0 to 6: `/loop agents 3`."
		}
		if _, err := b.c.Store.SetControl(ctx, actor, store.ControlPatch{ActiveAgents: &n}); err != nil {
			return Escape(errMessage(err))
		}
		return fmt.Sprintf("Active agents: %d of 6.", n)
	case "stop", "resume":
		stopped := verb == "stop"
		if _, err := b.c.Store.SetControl(ctx, actor, store.ControlPatch{Stopped: &stopped}); err != nil {
			return Escape(errMessage(err))
		}
		if stopped {
			return "Stopped: running agents stop at their next heartbeat; nothing new starts until `/loop resume`."
		}
		return "Resumed."
	case "usage":
		pct, err := strconv.Atoi(strings.TrimSuffix(strings.Join(args, ""), "%"))
		if err != nil || pct < 0 || pct > 100 {
			return "Give the percentage from /usage: `/loop usage 42%`."
		}
		if err := b.c.Store.Record(ctx, store.Event{Actor: actor, Kind: "usage", Data: map[string]any{"percent": pct}}); err != nil {
			return Escape(errMessage(err))
		}
		return fmt.Sprintf("Recorded: %d%% of the weekly limit used.", pct)
	}
	return fmt.Sprintf("Unknown command `%s`.\n", Escape(Clip(verb, 40, ""))) + helpText
}

// displayOrder is the board's order of display stages.
func displayOrder() []string {
	var out []string
	for _, s := range pipeline.AllStages {
		if s == pipeline.Done || s == pipeline.Closed {
			continue
		}
		out = append(out, pipeline.Display(s, false))
		if s.IsWork() {
			out = append(out, pipeline.Display(s, true))
		}
	}
	return out
}

func (b *Bot) status(ctx context.Context) string {
	rows, err := b.c.Store.ListItems(ctx, "", "")
	if err != nil {
		return "Could not read the board."
	}
	groups := map[string][]store.BoardRow{}
	done, closed := 0, 0
	for _, r := range rows {
		switch pipeline.Stage(r.Stage) {
		case pipeline.Done:
			done++
		case pipeline.Closed:
			closed++
		default:
			groups[r.Display] = append(groups[r.Display], r)
		}
	}
	var sb strings.Builder
	sb.WriteString("*Board*\n")
	open := 0
	for _, g := range displayOrder() {
		if len(groups[g]) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "*%s* (%d)\n", g, len(groups[g]))
		for _, r := range groups[g] {
			open++
			fmt.Fprintf(&sb, "• #%d %s %s\n", r.ID, r.Priority, b.field(r.Title, 120, r.ID))
		}
	}
	if open == 0 {
		sb.WriteString("Nothing open.\n")
	}
	fmt.Fprintf(&sb, "Done: %d · Closed: %d", done, closed)
	if ctl, err := b.c.Store.Control(ctx); err == nil {
		fmt.Fprintf(&sb, " · Active agents: %d of 6", ctl.ActiveAgents)
		if ctl.Stopped {
			sb.WriteString(" (stopped)")
		}
	}
	return sb.String()
}

func (b *Bot) show(ctx context.Context, id int64) string {
	d, err := b.c.Store.GetItem(ctx, id)
	if err != nil {
		return fmt.Sprintf("There is no item %d.", id)
	}
	it := d.Item
	var sb strings.Builder
	fmt.Fprintf(&sb, "*#%d %s*\nStage *%s* · Priority *%s* · repo %s · risk %s", it.ID, b.field(it.Title, 300, id), d.Display, it.Priority, repoText(it), it.Risk)
	if it.HoldReason != "" {
		sb.WriteString(" — " + b.field(it.HoldReason, 300, id))
	}
	if len(d.WaitingOn) > 0 {
		sb.WriteString(" · waiting on " + itemRefs(d.WaitingOn))
	}
	if it.Builder != "" {
		sb.WriteString(" · builder " + it.Builder)
	}
	if pr := b.prLink(it); pr != "" {
		fmt.Fprintf(&sb, "\n%s, head `%s`", pr, short(it.HeadSHA))
	}
	if it.Stage == pipeline.NeedsYou {
		sb.WriteString("\nQuestion: " + b.field(it.Question, 500, id))
	}
	sb.WriteString("\nLast events:")
	evs := d.Events
	if len(evs) > 6 {
		evs = evs[len(evs)-6:]
	}
	for _, e := range evs {
		line := e.Kind
		if e.Outcome != "" {
			line += " " + e.Outcome
		}
		if e.From != "" && e.To != "" && e.From != e.To {
			line += fmt.Sprintf(" (%s → %s)", e.From, e.To)
		}
		for _, k := range []string{"answer", "words", "message"} {
			if v := str(e.Data, k); v != "" {
				line += ": " + b.field(v, 120, id)
			}
		}
		fmt.Fprintf(&sb, "\n• %s %s, by %s", e.At.In(b.c.Location).Format("Jan 2 15:04"), Escape(line), who(e.Actor))
	}
	return sb.String()
}
