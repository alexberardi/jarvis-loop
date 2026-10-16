package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Who may approve, and what counts as an approval. These live in their own file so jarvis-loop's
// hold rule for them ("slack-approver": internal/slack/approve*.go) can be narrow: a change here
// changes who may approve or merge, and waits for the user (docs/phases/06-land.md).

// Approver is the only member whose answers, approvals and control changes count. It is a
// constant, not a flag: changing who may approve is a code change and goes through review.
const Approver = "U0C9AFPAMD4"

// IsApprover reports whether a Slack member may answer, approve and change the loop.
func IsApprover(user string) bool { return user == Approver }

var (
	approvalWords   = []string{"approve", "approved", "lgtm", "fine", "ok", "okay", "yes", "yep", "yea", "yeah", "sure"}
	approvalPhrases = [][]string{{"ship", "it"}, {"go", "ahead"}, {"merge", "it"}, {"do", "it"}}
	negationWords   = []string{"not", "don't", "dont", "no", "nope", "nah", "never", "wait", "hold", "holding", "stop", "cancel"}
	// changeVerbs open a note for the builder ("rebuild", "fix the typo"); leadWords may come first.
	changeVerbs = []string{"rebuild", "rebase", "fix", "rename", "change", "add", "remove", "drop", "update", "use",
		"make", "move", "revert", "resolve", "rework", "rewrite", "refactor", "replace", "delete", "split",
		"tune", "fine-tune", "tweak", "adjust", "redo", "retry", "reword", "bump", "simplify", "test", "document"}
	leadWords      = []string{"please", "pls", "also", "just", "then", "ok", "okay", "actually", "yes", "yeah", "yep", "sure", "but"}
	questionStarts = []string{"is", "are", "can", "could", "should", "would", "will", "does", "did", "why", "what", "how", "when", "where", "who", "which", "shall"}
)

// ClassifyReply decides whether a reply in a held item's thread approves the merge. It is
// deliberately strict and has no model: consent is an approval word or phrase, with no question
// mark, no question opening and no negation anywhere in the reply. Anything else is not an
// approval, and the item stays held.
func ClassifyReply(text string) bool {
	text = stripMentions(text)
	if strings.Contains(text, "?") {
		return false
	}
	ws := words(text)
	if len(ws) == 0 || slices.Contains(questionStarts, ws[0]) {
		return false
	}
	consent := false
	for i, w := range ws {
		if slices.Contains(negationWords, w) || strings.HasSuffix(w, "n't") {
			return false
		}
		if slices.Contains(approvalWords, w) {
			consent = true
		}
		for _, p := range approvalPhrases {
			if i+len(p) <= len(ws) && slices.Equal(ws[i:i+len(p)], p) {
				consent = true
			}
		}
	}
	return consent
}

// IsHeldNote decides whether a reply in a held item's thread is the user's note for the builder
// ("rebuild", "rebase and fix X"), which sends the item back to build. The bot checks it before
// ClassifyReply, so a change request wins over consent words in it ("ok, rebuild"). Like
// ClassifyReply it is strict and has no model, and a match must be positive: the reply (after
// mentions and leading "please", "ok", "yes", "but", "actually") opens with a change verb, or is
// "merge main …", or "merge …" with a change verb later ("merge it after adding a test"). A
// question (a question mark or a question opening) or any negation or "wait" word is never a
// note. Anything else ("looks good", "let me check", "later", "merge it please") is not a note,
// and the item stays held.
func IsHeldNote(text string) bool {
	text = stripMentions(text)
	if strings.Contains(text, "?") {
		return false
	}
	ws := words(text)
	for _, w := range ws {
		if slices.Contains(negationWords, w) || strings.HasSuffix(w, "n't") {
			return false
		}
	}
	for len(ws) > 0 && slices.Contains(leadWords, ws[0]) {
		ws = ws[1:]
	}
	if len(ws) == 0 || slices.Contains(questionStarts, ws[0]) {
		return false
	}
	if isChangeVerb(ws[0]) {
		return true
	}
	if ws[0] != "merge" {
		return false
	}
	if len(ws) > 1 && ws[1] == "main" {
		return true
	}
	return slices.ContainsFunc(ws[1:], isChangeVerb)
}

// isChangeVerb is a change verb or its -ing form ("adding", "fixing", "rebasing").
func isChangeVerb(w string) bool {
	if slices.Contains(changeVerbs, w) {
		return true
	}
	stem, ok := strings.CutSuffix(w, "ing")
	if !ok || stem == "" {
		return false
	}
	// "adding", "removing" → "remove", "dropping" → "drop"
	return slices.Contains(changeVerbs, stem) || slices.Contains(changeVerbs, stem+"e") ||
		len(stem) >= 2 && stem[len(stem)-1] == stem[len(stem)-2] && slices.Contains(changeVerbs, stem[:len(stem)-1])
}

type pendingApproval struct {
	id     string
	itemID int64
	// A plan's approval (plan.go) names the plan, the document and its version instead of an item.
	planID  int64
	kind    string
	version int
	sha     string
	words   string
	due     time.Time
	echoTS  string
	channel string
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
		if p.planID != 0 {
			b.applyPlanApproval(ctx, p)
			continue
		}
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

// cancelPending drops a reply approval waiting on an item (a button decided it instead).
func (b *Bot) cancelPending(itemID int64) {
	b.takePending(func(p *pendingApproval) bool { return p.itemID == itemID && p.planID == 0 })
}

// cancelPlanPending drops an approval waiting on a plan (it was dropped).
func (b *Bot) cancelPlanPending(planID int64) {
	b.takePending(func(p *pendingApproval) bool { return p.planID == planID })
}

// approveButton applies the Approve merge button at once, for the head the held message showed.
func (b *Bot) approveButton(ctx context.Context, channel, msgTS, user string, itemID int64, value string) {
	_, sha, _ := strings.Cut(value, ":")
	b.cancelPending(itemID)
	_, err := b.c.Store.Approve(ctx, actorPrefix+user, itemID, sha, "Approve merge")
	var pe *pipeline.Error
	switch {
	case err == nil:
		b.update(ctx, "held message", Message{Channel: channel, TS: msgTS,
			Text: fmt.Sprintf("%s: approved by you for `%s` (button).", b.refID(ctx, itemID), short(sha))})
	case errors.As(err, &pe) && pe.Code == pipeline.CodeSHAMismatch:
		d, _ := b.c.Store.GetItem(ctx, itemID)
		_, ts, _ := b.c.Store.SlackThread(ctx, itemID)
		b.threadReply(ctx, ts, fmt.Sprintf("Not approved: that button was for `%s`, but the PR head is now `%s`. Use the latest held message.",
			short(sha), short(d.Item.HeadSHA)))
	default:
		b.ephemeral(ctx, channel, user, "Not approved: "+Escape(errMessage(err)))
	}
}

// cancelApproval stops a reply approval still in its window.
func (b *Bot) cancelApproval(ctx context.Context, channel, msgTS, user, id string) {
	ps := b.takePending(func(pa *pendingApproval) bool { return pa.id == id })
	if len(ps) == 0 {
		b.ephemeral(ctx, channel, user, "Nothing to cancel: that approval was already applied or cancelled.")
		return
	}
	pa := ps[0]
	if pa.planID != 0 {
		b.c.Store.Record(ctx, store.Event{PlanID: pa.planID, Actor: actorPrefix + user, Kind: "approval_cancelled",
			Data: map[string]any{"kind": pa.kind, "version": pa.version, "sha256": pa.sha}})
		b.update(ctx, "approval echo", Message{Channel: channel, TS: msgTS,
			Text: fmt.Sprintf("Cancelled: %s v%d was not approved.", pa.kind, pa.version)})
		return
	}
	b.c.Store.Record(ctx, store.Event{ItemID: pa.itemID, Actor: actorPrefix + user, Kind: "approval_cancelled",
		Data: map[string]any{"words": pa.words, "head_sha": pa.sha}})
	b.update(ctx, "approval echo", Message{Channel: channel, TS: msgTS,
		Text: fmt.Sprintf("Cancelled: '%s' was not applied. %s stays held.", b.field(pa.words, 500, pa.itemID), b.refID(ctx, pa.itemID))})
}
