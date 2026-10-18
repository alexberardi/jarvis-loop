package slack

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The Rebuild button (on needs_you and held messages) opens a modal for the builder's note;
// submitting it sends the item back to build with the note (docs/decisions/2026-10-17-needs-you-rebuild.md).
const (
	// RebuildDefaultNote is the modal's note until the approver edits it, and the note when they
	// empty it.
	RebuildDefaultNote = "rebase on main and fix what CI shows"
	rebuildCallback    = "rebuild"
	rebuildBlock       = "note" // the input's block_id and action_id
)

func rebuildButton(value string) map[string]any { return button("Rebuild…", "rebuild", value, "") }

// rebuildModal is the Rebuild form for an item: one optional note, the default filled in.
func rebuildModal(id int64) map[string]any {
	return map[string]any{
		"type": "modal", "callback_id": rebuildCallback, "private_metadata": strconv.FormatInt(id, 10),
		"title": plain(fmt.Sprintf("Rebuild #%d", id)), "submit": plain("Rebuild"), "close": plain("Cancel"),
		"blocks": []map[string]any{
			section(fmt.Sprintf("Sends #%d back to build on its branch, with this note for the builder.", id)),
			{"type": "input", "block_id": rebuildBlock, "optional": true, "label": plain("Note for the builder"),
				"element": map[string]any{"type": "plain_text_input", "action_id": rebuildBlock, "multiline": true, "initial_value": RebuildDefaultNote}},
		},
	}
}

// openRebuild opens the Rebuild modal for the approver's click (the caller checked who clicked).
func (b *Bot) openRebuild(ctx context.Context, channel, user, triggerID string, itemID int64) {
	if itemID == 0 {
		return
	}
	err := fmt.Errorf("Slack sent no trigger_id")
	if triggerID != "" {
		err = b.c.API.OpenView(ctx, triggerID, rebuildModal(itemID))
	}
	if err != nil {
		b.c.Log.Warn("slack: open the Rebuild modal", "item", itemID, "err", err.Error())
		b.ephemeral(ctx, channel, user, `Could not open the Rebuild form; reply "rebuild …" in the thread instead.`)
	}
}

// onViewSubmission handles a submitted modal: the Rebuild form sends the item back to build with
// the note (the default when it is empty), the approver's only.
func (b *Bot) onViewSubmission(ctx context.Context, p interaction) {
	if p.View.CallbackID != rebuildCallback {
		return
	}
	id, err := strconv.ParseInt(p.View.PrivateMetadata, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	user := p.User.ID
	if !IsApprover(user) {
		b.refuse(ctx, ChannelLoop, user, id, "rebuild")
		return
	}
	note := ""
	if v := p.View.State.Values[rebuildBlock][rebuildBlock].Value; v != nil {
		note = strings.TrimSpace(*v)
	}
	if note == "" {
		note = RebuildDefaultNote
	}
	b.cancelPending(id)
	// A refusal (the item moved on meanwhile) is recorded, and its event's line says why in the thread.
	b.c.Store.Rebuild(ctx, actorPrefix+user, id, note)
}
