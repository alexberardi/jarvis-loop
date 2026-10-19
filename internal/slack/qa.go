package slack

import (
	"context"
	"fmt"
	"slices"
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
		head := fmt.Sprintf("<@%s> %s: Merged; I'll ask for your check once it's deployed", Approver, ref(it.ID, it.Title))
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

// qaAsk is the ask with the Verified and Reopen buttons. With a proof (docs/decisions/2026-10-18-qa-proofs.md)
// that still has automatic checks to pass, it says the loop checks it itself and does not mention
// the approver; with only manual checks left, it says what to look at.
func (b *Bot) qaAsk(it pipeline.Item, what, detail string) Message {
	head := fmt.Sprintf("<@%s> %s: %s — waiting for your check", Approver, ref(it.ID, it.Title), what)
	text := fmt.Sprintf("%s. %s.\nPress Verified when it works; Reopen, then reply here with what is wrong, files a new item.", head, detail)
	switch st := it.Proof.Status(); st {
	case pipeline.CheckManual:
		head = fmt.Sprintf("<@%s> %s: %s — waiting for your check: %s", Approver, ref(it.ID, it.Title), what, b.looks(it))
		text = fmt.Sprintf("%s. %s.\nPress Verified when it works; Reopen, then reply here with what is wrong, files a new item.", head, detail)
	case pipeline.CheckPending, pipeline.CheckWaiting, pipeline.CheckFailed:
		head = fmt.Sprintf("%s: %s — the loop checks it itself", ref(it.ID, it.Title), what)
		checks := append(it.Proof.Open(pipeline.CheckPending), it.Proof.Open(pipeline.CheckWaiting)...)
		text = fmt.Sprintf("%s: %s. %s.", head, b.field(strings.Join(checks, "; "), 600, it.ID), detail)
		if looks := it.Proof.Looks(); len(looks) > 0 {
			text += "\nThen your check: " + b.looks(it) + "."
		}
		reopen := "reopened if one fails twice in a row"
		if slices.ContainsFunc(it.Proof.Checks, func(c pipeline.ProofCheck) bool { return c.Kind == pipeline.ProofE2E }) {
			reopen = "reopened if a command fails twice in a row or a regression run fails one of its E2E tests"
		}
		text += "\nIt is verified when every automatic check passes, and " + reopen + "; Verified and Reopen still work by hand."
	}
	value := strconv.FormatInt(it.ID, 10)
	return Message{Text: head, Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "qa", "elements": []any{
			button("Verified", "qa_verified", value, "primary"),
			button("Reopen", "qa_reopen", value, "danger"),
		}},
	}}
}

// looks is a proof's manual checks: what the user should look at.
func (b *Bot) looks(it pipeline.Item) string {
	return "look at " + b.field(strings.Join(it.Proof.Looks(), "; "), 600, it.ID)
}

// proofCheckedMessage is a check of the proof that changed where it stands: its automatic checks
// passed and only the user's are left (the ask, with the buttons), a command check failed once
// (checked again before it reopens), or what it still waits for. A check that changed nothing
// posts nothing (they run every 30 minutes).
func (b *Bot) proofCheckedMessage(e store.Event, d store.Detail) (Message, bool) {
	it := d.Item
	if e.Data["changed"] != true || it.Proof == nil {
		return Message{}, false
	}
	switch str(e.Data, "status") {
	case pipeline.CheckManual:
		return b.qaAsk(it, "Proof passed", "Every automatic check passed"), true
	case pipeline.CheckFailed:
		_, why := pipeline.QAGroup(it.Proof, false)
		text := fmt.Sprintf("Proof check %s. I check again in %d minutes; a second failure in a row reopens it.",
			b.field(why, 800, it.ID), int(pipeline.ProofRecheck.Minutes()))
		return Message{Text: text}, true
	}
	_, why := pipeline.QAGroup(it.Proof, false)
	return Message{Text: "Proof: " + b.field(why, 800, it.ID) + "."}, true
}

// reopenButton is the approver's Reopen: the words come in the next reply in the item's thread.
func (b *Bot) reopenButton(ctx context.Context, channel, user string, id int64) {
	d, err := b.c.Store.GetItem(ctx, id)
	if err != nil || d.Item.Stage != pipeline.QA {
		msg := fmt.Sprintf("%s is not waiting for your check.", ref(id, d.Item.Title))
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
		return ref(it.ID, it.Title) + " waits in qa for your check once merged."
	case "verified":
		b.clearReopen(id)
		it, err := b.c.Store.Verified(ctx, actor, id, words)
		if err != nil {
			return Escape(errMessage(err))
		}
		return ref(it.ID, it.Title) + " verified (done)."
	}
	if words == "" {
		return "Say what is wrong: `/loop reopen <id> <what is wrong>`."
	}
	old, filed, err := b.c.Store.Reopen(ctx, actor, id, words)
	if err != nil {
		return Escape(errMessage(err))
	}
	b.clearReopen(id)
	return fmt.Sprintf("%s done; filed #%d (%s): %s.", ref(old.ID, old.Title), filed.ID, filed.Priority, Escape(filed.Title))
}
