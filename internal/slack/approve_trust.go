package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The trust OK (docs/decisions/2026-10-16-trusted-files.md): when the runner's verify refuses a
// commit that changes files it trusts (a repo's deny_write paths), the approver may OK that one
// commit with the button or a "trust" reply; verify then runs it once. This file is under
// jarvis-loop's slack-approver hold rule: it decides whose OK counts.

// IsTrustReply reports whether a reply in a needs-you thread is the approver's trust OK: the
// word "trust" alone, or "trust it" / "trust this" / "trust <sha>" (mentions stripped).
func IsTrustReply(text string) bool {
	ws := strings.Fields(strings.ToLower(stripMentions(text)))
	if len(ws) == 0 || strings.Trim(ws[0], ".!") != "trust" || len(ws) > 2 {
		return false
	}
	return len(ws) == 1 || ws[1] == "it" || ws[1] == "this" || isHex(strings.Trim(ws[1], "`"))
}

func isHex(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// waitsForTrust reports whether a needs-you item waits on verify's refusal of a trusted-file
// change, with no OK yet.
func waitsForTrust(it pipeline.Item) bool {
	return it.Stage == pipeline.NeedsYou && it.Trust != nil && it.Trust.By == ""
}

// trustMessage asks the approver about verify's refusal: the commit, its trusted files' diff
// stat, where to look, and the Trust button bound to that commit.
func (b *Bot) trustMessage(e store.Event, d store.Detail) Message {
	it := d.Item
	sha := str(e.Data, pipeline.FieldTrustSHA)
	stat := str(e.Data, pipeline.FieldTrustDiffstat)
	if stat == "" {
		stat = strings.ReplaceAll(str(e.Data, pipeline.FieldTrustPaths), ",", "\n")
	}
	text := fmt.Sprintf("<@%s> %s needs you: verify did not run `%s`, because it changes files the runner trusts (deny_write):\n```%s```\n"+
		"Look at the change (`loop show %d`, evidence trusted-files.diff). To let verify run `%s` once, press Trust this commit or reply \"trust\"; "+
		"any later commit needs a new OK, and land holds the PR for you. Or drop.",
		Approver, ref(it.ID, it.Title), short(sha), b.field(stat, 1200, it.ID), it.ID, short(sha))
	return Message{Text: fmt.Sprintf("<@%s> %s needs you: trusted files changed (%s)", Approver, ref(it.ID, it.Title), short(sha)), Blocks: []map[string]any{
		section(text),
		{"type": "actions", "block_id": "needs_you", "elements": []any{
			button("Trust this commit", "trust", fmt.Sprintf("%d:%s", it.ID, sha), "primary"),
			dropButton(it.ID, strconv.FormatInt(it.ID, 10)),
		}},
	}}
}

// trust applies the approver's OK for sha; msgTS is the needs-you message a button was on.
func (b *Bot) trust(ctx context.Context, channel, msgTS, user string, itemID int64, sha, how string) {
	_, threadTS, _ := b.c.Store.SlackThread(ctx, itemID)
	_, err := b.c.Store.Trust(ctx, actorPrefix+user, itemID, sha)
	var pe *pipeline.Error
	switch {
	case err == nil:
		if msgTS != "" {
			b.update(ctx, "trust message", Message{Channel: channel, TS: msgTS,
				Text: fmt.Sprintf("%s: trusted files OK'd by you for `%s` (%s). Verify runs that commit once.", b.refID(ctx, itemID), short(sha), how)})
		}
	case errors.As(err, &pe) && pe.Code == pipeline.CodeSHAMismatch:
		d, _ := b.c.Store.GetItem(ctx, itemID)
		now := ""
		if d.Item.Trust != nil {
			now = d.Item.Trust.HeadSHA
		}
		b.threadReply(ctx, threadTS, fmt.Sprintf("Not trusted: that OK was for `%s`, but verify refused `%s`. Use the latest message.", short(sha), short(now)))
	default:
		b.threadReply(ctx, threadTS, "Not trusted: "+Escape(errMessage(err)))
	}
}

// trustReply is the approver's "trust" reply in a needs-you thread: the OK for the commit the
// latest refusal named. A reply that names a sha must name that commit; any other sha is refused
// and nothing is recorded, so an OK typed for an older refusal cannot trust a newer commit.
func (b *Bot) trustReply(ctx context.Context, m slackEvent, it pipeline.Item) {
	sha, ok := trustReplySHA(incoming(m.Text), it.Trust.HeadSHA)
	if !ok {
		b.threadReply(ctx, m.ThreadTS, fmt.Sprintf("Not trusted: that OK was for `%s`, but verify refused `%s`. Use the latest message.", short(sha), short(it.Trust.HeadSHA)))
		return
	}
	b.trust(ctx, m.Channel, "", m.User, it.ID, sha, "reply")
}

// trustReplySHA picks the commit a trust reply OKs: head for a bare "trust" (or "trust it" /
// "trust this"), or for "trust <sha>" whose sha is a prefix (at least 7 hex characters) of head.
// Otherwise it returns the typed sha and false.
func trustReplySHA(text, head string) (string, bool) {
	ws := strings.Fields(strings.ToLower(stripMentions(text)))
	if len(ws) < 2 {
		return head, true
	}
	typed := strings.Trim(ws[1], "`")
	if !isHex(typed) {
		return head, true
	}
	if head != "" && strings.HasPrefix(strings.ToLower(head), typed) {
		return head, true
	}
	return typed, false
}
