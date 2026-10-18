package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/store"
)

// A repo frozen while its main is red (docs/decisions/2026-10-18-main-red-freeze.md): loopd's
// frozen and unfrozen events have no item; the bot posts them in #loop.

func idOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func idsOf(v any) []int64 {
	list, _ := v.([]any)
	var out []int64
	for _, x := range list {
		if id := idOf(x); id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// freezeMessage is the #loop post for a frozen or unfrozen event.
func (b *Bot) freezeMessage(ctx context.Context, e store.Event) Message {
	repo := Escape(Clip(str(e.Data, "repo"), 200, ""))
	fix := idOf(e.Data["item"])
	var text string
	switch e.Kind {
	case store.KindFrozen:
		jobs := Escape(Clip(strings.Join(listOf(e.Data, "jobs"), ", "), 300, ""))
		lead := fmt.Sprintf(":rotating_light: *main is red in %s* (failing: %s, still red after a re-run): *frozen*.", repo, jobs)
		if e.Data["again"] == true {
			lead = fmt.Sprintf(":rotating_light: *main is still red in %s* (failing: %s) after the fix merged: still *frozen*, now for a new P1.", repo, jobs)
		}
		text = lead + fmt.Sprintf(" Nothing else in it moves stages (no claims, no advances, no merges) until main is green; runs already going finish and their results wait. %s goes first. Likely culprit: %s (PR #%d). Lift it by hand: `loop unfreeze %s`.",
			b.refID(ctx, fix), b.refID(ctx, idOf(e.Data["culprit_item"])), idOf(e.Data["culprit_pr"]), repo)
	case store.KindUnfrozen:
		text = fmt.Sprintf(":large_green_circle: *%s unfrozen* (%s).", repo, Escape(Clip(str(e.Data, "reason"), 200, "")))
		if n := idOf(e.Data["applied"]); n > 0 {
			text += fmt.Sprintf(" Applied %d result(s) that waited, in the order they finished.", n)
		}
		if n := idOf(e.Data["dropped"]); n > 0 {
			text += fmt.Sprintf(" Dropped %d (the item had moved meanwhile).", n)
		}
		if ids := idsOf(e.Data["rebuilt"]); len(ids) > 0 {
			refs := make([]string, len(ids))
			for i, id := range ids {
				refs[i] = b.refID(ctx, id)
			}
			text += " CI failed on these while main was red, so they go back to build to rebase and re-run: " + strings.Join(refs, ", ") + "."
		}
	}
	return Message{Channel: ChannelLoop, Text: text, Blocks: []map[string]any{section(text)}}
}

// freezeLine is an item thread's line for the freeze's item events ("" for any other event).
func (b *Bot) freezeLine(ctx context.Context, e store.Event, id int64) string {
	repo := b.field(str(e.Data, "repo"), 200, id)
	switch e.Kind {
	case store.KindAdvanceQueued:
		return fmt.Sprintf("Finished %s (%s); main is red in %s, so the result waits for %s and applies once main is green.",
			e.From, e.Outcome, repo, b.refID(ctx, idOf(e.Data["fix_item"])))
	case store.KindLandFrozen:
		return fmt.Sprintf("Land stopped before merging: main is red in %s (%s first). Waits in land until main is green.",
			repo, b.refID(ctx, idOf(e.Data["fix_item"])))
	case store.KindQueuedDropped:
		return fmt.Sprintf("The %s result that waited while main was red was dropped: %s.", e.Outcome, b.field(str(e.Data, "reason"), 300, id))
	}
	return ""
}
