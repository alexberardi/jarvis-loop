package slack

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The QA lane (docs/decisions/2026-10-15-qa.md): a merged item marked needs_qa waits in qa for the
// approver's own check. Verified moves it to done; Reopen asks for what is wrong in the thread, and
// the approver's next reply there files a new P2 item with those words.

// VerifiedWords are the words a Verified button press records.
const VerifiedWords = "Verified button"

// qaMessage asks the approver to check a merged item, or, for a repo whose change runs only once
// deployed (the loop itself), says the ask comes then, with no buttons
// (docs/decisions/2026-10-16-qa-after-deploy.md).
func (b *Bot) qaMessage(e store.Event, d store.Detail) Message {
	it := d.Item
	merged := fmt.Sprintf("%s merged as `%s`", b.prLink(it), short(str(e.Data, "merge_sha")))
	if str(e.Data, "merged_outside") == "true" {
		merged += " on GitHub, outside the loop"
	}
	if str(e.Data, "main_ci") == "watching" {
		merged += "; watching main's CI"
	}
	if e.Data["awaiting_deploy"] == true {
		head := fmt.Sprintf("<@%s> #%d: Merged; I'll ask for your check once it's deployed", Approver, it.ID)
		text := fmt.Sprintf("%s. %s.\nNothing to check until a loop build that contains it is running.", head, merged)
		return Message{Text: head, Blocks: []map[string]any{section(text)}}
	}
	return b.qaAsk(it, "Merged", merged)
}

// deployedMessage is the ask once a runner reports a build that contains the merge.
func (b *Bot) deployedMessage(e store.Event, d store.Detail) Message {
	it := d.Item
	deployed := fmt.Sprintf("%s (merged as `%s`) is running in the build `%s`", b.prLink(it), short(str(e.Data, "merge_sha")), short(str(e.Data, "revision")))
	return b.qaAsk(it, "Deployed", deployed)
}

// qaAsk is the ask with the Verified and Reopen buttons.
func (b *Bot) qaAsk(it pipeline.Item, what, detail string) Message {
	text := fmt.Sprintf("<@%s> #%d: %s — waiting for your check. %s.\nPress Verified when it works; Reopen, then reply here with what is wrong, files a new item.",
		Approver, it.ID, what, detail)
	value := strconv.FormatInt(it.ID, 10)
	return Message{Text: fmt.Sprintf("<@%s> #%d: %s — waiting for your check", Approver, it.ID, what), Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "qa", "elements": []any{
			button("Verified", "qa_verified", value, "primary"),
			button("Reopen", "qa_reopen", value, "danger"),
		}},
	}}
}

// reopenButton is the approver's Reopen: the words come in the next reply in the item's thread.
func (b *Bot) reopenButton(ctx context.Context, channel, user string, id int64) {
	d, err := b.c.Store.GetItem(ctx, id)
	if err != nil || d.Item.Stage != pipeline.QA {
		msg := fmt.Sprintf("#%d is not waiting for your check.", id)
		if err != nil {
			msg = Escape(errMessage(err))
		}
		b.ephemeral(ctx, channel, user, msg)
		return
	}
	_, ts, err := b.c.Store.SlackThread(ctx, id)
	if err != nil || ts == "" {
		b.ephemeral(ctx, channel, user, fmt.Sprintf("Use `/loop reopen %d <what is wrong>`.", id))
		return
	}
	b.mu.Lock()
	b.reopening[id] = true
	b.mu.Unlock()
	b.threadReply(ctx, ts, fmt.Sprintf("Reply in this thread with what is wrong: your reply files a new P2 item with those words, and #%d is done. (Or `/loop reopen %d <words>`.)", id, id))
}

func (b *Bot) clearReopen(id int64) {
	b.mu.Lock()
	delete(b.reopening, id)
	b.mu.Unlock()
}

// qaReply is a reply in a qa item's thread: the Reopen words, once the approver pressed Reopen.
func (b *Bot) qaReply(ctx context.Context, m slackEvent, id int64, text string) {
	if !IsApprover(m.User) {
		b.refuse(ctx, m.Channel, m.User, id, "reopen")
		return
	}
	if text == "" {
		return
	}
	b.mu.Lock()
	asked := b.reopening[id]
	b.mu.Unlock()
	if !asked {
		if d, err := b.c.Store.GetItem(ctx, id); err == nil && d.Item.AwaitingDeploy {
			b.threadReply(ctx, m.ThreadTS, fmt.Sprintf("Not taken: I'll ask for your check once it's deployed (or `/loop verified %d`, `/loop reopen %d <what is wrong>`).", id, id))
			return
		}
		b.threadReply(ctx, m.ThreadTS, "Not taken: press Verified if it works, or Reopen and then reply with what is wrong.")
		return
	}
	if _, _, err := b.c.Store.Reopen(ctx, actorPrefix+m.User, id, text); err != nil {
		b.threadReply(ctx, m.ThreadTS, "Could not reopen: "+Escape(errMessage(err)))
		return
	}
	b.clearReopen(id)
}

// qaCommand is /loop qa N, /loop verified N and /loop reopen N <words>.
func (b *Bot) qaCommand(ctx context.Context, actor, verb string, args []string, rest string) string {
	if len(args) == 0 {
		return helpText
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil {
		return helpText
	}
	words := strings.TrimSpace(strings.TrimPrefix(rest, args[0]))
	switch verb {
	case "qa":
		it, err := b.c.Store.SetQA(ctx, actor, id)
		if err != nil {
			return Escape(errMessage(err))
		}
		return fmt.Sprintf("#%d waits in qa for your check once merged.", it.ID)
	case "verified":
		b.clearReopen(id)
		it, err := b.c.Store.Verified(ctx, actor, id, words)
		if err != nil {
			return Escape(errMessage(err))
		}
		return fmt.Sprintf("#%d verified (done).", it.ID)
	}
	if words == "" {
		return "Say what is wrong: `/loop reopen <id> <what is wrong>`."
	}
	old, filed, err := b.c.Store.Reopen(ctx, actor, id, words)
	if err != nil {
		return Escape(errMessage(err))
	}
	b.clearReopen(id)
	return fmt.Sprintf("#%d done; filed #%d (%s): %s.", old.ID, filed.ID, filed.Priority, Escape(filed.Title))
}
