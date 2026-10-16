package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Who may plan, and how a plan's approval applies (#plan, plan.go). In an approve*.go file for the
// same reason as approve.go: jarvis-loop's "slack-approver" hold rule covers it, so a change to who
// may start or steer a plan, or approve its brief and blueprint, waits for the user.

// onPlanMessage handles a message in #plan: the approver's top-level message starts a plan, their
// reply in a plan's thread queues a turn. Anyone else's message is logged and otherwise ignored:
// no plan, no reply.
func (b *Bot) onPlanMessage(ctx context.Context, m slackEvent) {
	if !IsApprover(m.User) {
		b.c.Log.Info("slack: ignored a #plan message from someone other than the approver", "user", m.User, "ts", m.TS)
		return
	}
	text := stripMentions(m.Text)
	if text == "" {
		return
	}
	actor := actorPrefix + m.User
	if m.ThreadTS == "" || m.ThreadTS == m.TS {
		if _, err := b.startPlan(ctx, actor, text, ChannelPlan, m.TS); err != nil {
			b.post(ctx, "reply", Message{Channel: ChannelPlan, ThreadTS: m.TS, Text: "Could not start a plan: " + Escape(errMessage(err))})
		}
		return
	}
	id, err := b.c.Store.PlanByThread(ctx, ChannelPlan, m.ThreadTS)
	if err != nil || id == 0 {
		return
	}
	// A refusal (the plan left the conversation) is loopd's refused event, which the thread shows.
	var pe *pipeline.Error
	if _, err := b.c.Store.PlanSlackMessage(ctx, actor, id, text, m.TS); err != nil && !errors.As(err, &pe) {
		b.post(ctx, "reply", Message{Channel: ChannelPlan, ThreadTS: m.ThreadTS, Text: "Not sent to the agent: " + Escape(errMessage(err))})
	}
}

// onPlanAction handles the buttons on plan messages: plan_approve ("P3:brief:2:<sha256>") starts
// an approval, plan_drop ("P3") drops the plan after Slack's own confirmation.
func (b *Bot) onPlanAction(ctx context.Context, channel, msgTS, user, actionID, value string) {
	ref, rest, _ := strings.Cut(value, ":")
	id, ok := parsePlanRef(ref)
	if !ok {
		return
	}
	if !IsApprover(user) {
		b.ephemeral(ctx, channel, user, "Sorry, only the approver can do that.")
		if err := b.c.Store.Record(ctx, store.Event{PlanID: id, Actor: actorPrefix + user, Kind: "refused",
			Data: map[string]any{"action": strings.TrimPrefix(actionID, "plan_"), "code": "not_approver", "message": "only the approver can do that"}}); err != nil {
			b.c.Log.Error("slack: record refusal", "err", err.Error())
		}
		return
	}
	switch actionID {
	case "plan_approve":
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return
		}
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		b.startPlanApproval(ctx, id, parts[0], v, parts[2])
	case "plan_drop":
		b.cancelPlanPending(id)
		if _, err := b.c.Store.DropPlan(ctx, actorPrefix+user, id, "Drop button"); err != nil {
			b.ephemeral(ctx, channel, user, Escape(errMessage(err)))
		}
	}
}

// startPlanApproval echoes an Approve button press in the plan's thread with a Cancel button; it
// applies when the window ends, bound to the sha256 the button was posted for.
func (b *Bot) startPlanApproval(ctx context.Context, planID int64, kind string, version int, sha string) {
	d, err := b.c.Store.GetPlan(ctx, planID)
	if err != nil || d.Plan.SlackChannel != ChannelPlan {
		return
	}
	thread := d.Plan.SlackTS
	b.mu.Lock()
	for _, p := range b.pending {
		if p.planID == planID {
			b.mu.Unlock()
			b.post(ctx, "reply", Message{Channel: ChannelPlan, ThreadTS: thread,
				Text: "An approval is already waiting to apply; use its Cancel button to stop it."})
			return
		}
	}
	p := &pendingApproval{id: newID(), planID: planID, kind: kind, version: version, sha: sha,
		words: fmt.Sprintf("Approve %s v%d", kind, version), due: b.c.Clock.Now().Add(b.c.ApprovalDelay), channel: ChannelPlan}
	b.pending[p.id] = p
	b.mu.Unlock()

	mins := int(b.c.ApprovalDelay.Round(time.Minute) / time.Minute)
	text := fmt.Sprintf("Approved by you: %s (`%s`). Applying in %d minutes.", b.docLink(planID, kind, version), short(sha), mins)
	if kind == pipeline.DocBlueprint {
		text += " Its pieces are filed as items then."
	}
	ts, err := b.post(ctx, "approval echo", Message{Channel: ChannelPlan, ThreadTS: thread, Text: text, Blocks: []map[string]any{
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

// applyPlanApproval applies a plan approval whose window has passed. A refusal (a newer version,
// the plan moved on) is in the echo; loopd's refused event says it in the thread too.
func (b *Bot) applyPlanApproval(ctx context.Context, p *pendingApproval) {
	_, err := b.c.Store.ApprovePlan(ctx, actorPrefix+Approver, p.planID, p.kind, p.sha, p.words)
	if p.echoTS == "" {
		return
	}
	text := fmt.Sprintf("Approved by you: %s v%d (`%s`). Applied.", p.kind, p.version, short(p.sha))
	if err != nil {
		text = fmt.Sprintf("Not applied: %s", Escape(errMessage(err)))
	}
	b.update(ctx, "approval echo", Message{Channel: p.channel, TS: p.echoTS, Text: text})
}
