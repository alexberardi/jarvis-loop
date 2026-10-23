package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The orchestrator in Slack (piece 2 of docs/phases/08-orchestrator.md,
// docs/decisions/2026-10-17-orchestrator-slack.md): the messages people send it, its replies, the
// approval message it has posted instead of approving, and the approver accepting its
// recommendation.

// Event kinds of the orchestrator's Slack side.
const (
	// KindOrchMessage is a message to the orchestrator (a mention or a DM): the trigger for its turn.
	KindOrchMessage = "orchestrator.message"
	// KindOrchReply is the orchestrator's answer, which its Slack app posts.
	KindOrchReply = "orchestrator.reply"
	// KindOrchApprovalAsked has the item's approval message, with its button, posted again.
	KindOrchApprovalAsked = "orchestrator.approval_asked"
	// KindOrchAcceptPending is the approver's Accept on an escalation whose recommendation needs
	// their hand first (its step): the item stays in needs_you until 'Done — re-queue' records the
	// answer (data escalation, step).
	KindOrchAcceptPending = "orchestrator.accept_pending"
)

// recordEvent records e and sets *out to it as subscribers get it (with its id and time).
func (s *Store) recordEvent(tx *sql.Tx, out *Event, e Event) error {
	if err := s.event(tx, e); err != nil {
		return err
	}
	*out = s.pending[len(s.pending)-1]
	return nil
}

// OrchestratorMessage is a message to the orchestrator as the Slack layer saw it.
type OrchestratorMessage struct {
	Channel, TS, ThreadTS, User, Text string
	// ItemID is the item whose #loop thread it is in, or 0.
	ItemID int64
	// Approver: only the approver's messages can make it act for the user.
	Approver bool
}

// OrchMessage keeps a message to the orchestrator (RecordSlackMessage, so acting for the user can
// name it) and records it as an orchestrator.message event (no item: the item it is about, if
// any, is in data.item_id), and starts (or joins) an orchestrate turn with the message trigger. A
// message seen before (Slack's retries) records nothing: fresh is false.
func (s *Store) OrchMessage(ctx context.Context, m OrchestratorMessage) (e Event, fresh bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO slack_messages (channel, ts, user, text, at) VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
			m.Channel, m.TS, m.User, m.Text, s.clock.Now().UnixNano())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		fresh = true
		data := map[string]any{"channel": m.Channel, "ts": m.TS, "user": m.User, "text": m.Text, "approver": m.Approver}
		if m.ThreadTS != "" {
			data["thread_ts"] = m.ThreadTS
		}
		if m.ItemID != 0 {
			data["item_id"] = m.ItemID
		}
		if err := s.recordEvent(tx, &e, Event{Actor: "loopd", Kind: KindOrchMessage, Data: data}); err != nil {
			return err
		}
		return s.queueOrch(tx, pipeline.Trigger{Reason: pipeline.TriggerMessage, ItemID: m.ItemID, Text: clipText(m.Text, 2000),
			Channel: m.Channel, TS: m.TS, ThreadTS: m.ThreadTS, Approver: m.Approver})
	})
	return e, fresh, err
}

// OrchMessages are the messages to the orchestrator after event id since, oldest first (at most
// limit; all when limit is 0).
func (s *Store) OrchMessages(ctx context.Context, since int64, limit int) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM events WHERE kind = ? AND id > ? ORDER BY id`
	args := []any{KindOrchMessage, since}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// OrchReply records the orchestrator's answer in a Slack thread (or a DM), with its reason when it
// gave one; its Slack app posts it. Where it may post is the API's and the Slack client's to check.
func (s *Store) OrchReply(ctx context.Context, actor, channel, threadTS, text, reason string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		text = strings.TrimSpace(text)
		if text == "" {
			return errf(pipeline.CodeMissingField, "text is required")
		}
		if channel == "" {
			return errf(pipeline.CodeMissingField, "channel is required")
		}
		data := map[string]any{"channel": channel, "thread_ts": threadTS, "text": text}
		if r := strings.TrimSpace(reason); r != "" {
			data["reason"] = clipText(r, 500)
		}
		return s.recordEvent(tx, &out, Event{Actor: actor, Kind: KindOrchReply, Data: data})
	})
	return out, err
}

// OrchAskApproval has the approval message of an item held for approval posted again, with its
// sha-bound button: what the orchestrator does when the user asks it to approve (it never may).
// For an item waiting on the approver's OK of trusted files, it is that item's trust message, with
// its button bound to the refused commit (data.trust and the refusal's fields).
func (s *Store) OrchAskApproval(ctx context.Context, actor string, id int64, reason string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		trust := it.Stage == pipeline.NeedsYou && it.Trust != nil && it.Trust.By == ""
		if it.Stage != pipeline.Held && !trust {
			return errf(pipeline.CodeWrongStage, "item is %s, not held for approval or waiting on a trusted-file OK", it.Stage)
		}
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		data := map[string]any{"head_sha": it.HeadSHA, "reason": r}
		if trust {
			data = map[string]any{"trust": true, "reason": r, pipeline.FieldTrustSHA: it.Trust.HeadSHA,
				pipeline.FieldTrustPaths: strings.Join(it.Trust.Paths, ","), pipeline.FieldTrustDiffstat: it.Trust.Diffstat}
		}
		return s.recordEvent(tx, &out, Event{ItemID: id, Actor: actor, Kind: KindOrchApprovalAsked, Data: data})
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, "approval", err)
	}
	return out, nil
}

// acceptable loads escalation escalationID and its item, and checks its recommendation can still
// be the item's answer: an escalation with a recommendation about an item that is in needs_you,
// waits on no trusted-file OK, and asks nothing newer than the escalation. esc is returned even
// when the check fails, for the refusal's item.
func acceptable(tx *sql.Tx, escalationID int64) (esc Event, it pipeline.Item, err error) {
	evs, err := scanEventRows(tx.Query(`SELECT `+eventCols+` FROM events WHERE id = ?`, escalationID))
	if err != nil {
		return esc, it, err
	}
	if len(evs) == 1 {
		esc = evs[0]
	}
	if esc.Kind != KindOrchEscalated || esc.ItemID == 0 || strings.TrimSpace(str(esc.Data, "recommendation")) == "" {
		return esc, it, errf(pipeline.CodeInvalidField, "no recommendation %d about an item to accept", escalationID)
	}
	if it, err = loadItem(tx, esc.ItemID); err != nil {
		return esc, it, err
	}
	if it.Stage != pipeline.NeedsYou {
		return esc, it, errf(pipeline.CodeWrongStage, "item is %s, not needs_you: nothing to accept", it.Stage)
	}
	if it.Trust != nil && it.Trust.By == "" {
		return esc, it, errf(pipeline.CodeWrongStage, "the item waits on your OK of its trusted files: use the Trust button on its message")
	}
	all, err := itemEvents(tx, esc.ItemID)
	if err != nil {
		return esc, it, err
	}
	if last, ok := lastNeedsYou(all); ok && last.ID > esc.ID {
		return esc, it, errf(pipeline.CodeWrongStage, "the item asks something new since that recommendation: answer it in the thread")
	}
	return esc, it, nil
}

// hasAcceptPending reports whether escalation esc already has a pending accept (AcceptPending).
func hasAcceptPending(tx *sql.Tx, esc Event) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE item_id = ? AND kind = ? AND json_extract(data, '$.escalation') = ?`,
		esc.ItemID, KindOrchAcceptPending, esc.ID).Scan(&n)
	return n > 0, err
}

// AcceptPending records the approver's Accept on an escalation whose recommendation needs their
// hand first (data.step): the item stays in needs_you, its question standing, until
// 'Done — re-queue' records the answer (AcceptRecommendation). A second press, or one on an
// escalation without a step, is refused.
func (s *Store) AcceptPending(ctx context.Context, actor string, escalationID int64) (Event, error) {
	var out Event
	var itemID int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		esc, _, err := acceptable(tx, escalationID)
		itemID = esc.ItemID
		if err != nil {
			return err
		}
		step := str(esc.Data, "step")
		if step == "" {
			return errf(pipeline.CodeInvalidField, "the recommendation has no step to wait for: Accept answers at once")
		}
		pending, err := hasAcceptPending(tx, esc)
		if err != nil {
			return err
		}
		if pending {
			return errf(pipeline.CodeWrongStage, "already accepted: do the step (%s), then press 'Done — re-queue'", step)
		}
		return s.recordEvent(tx, &out, Event{ItemID: esc.ItemID, Actor: actor, Kind: KindOrchAcceptPending,
			Data: map[string]any{"escalation": esc.ID, "step": step}})
	})
	if err != nil {
		var pe *pipeline.Error
		if itemID != 0 && errors.As(err, &pe) {
			return out, s.refuse(ctx, actor, itemID, "accept", err)
		}
		return out, err
	}
	return out, nil
}

// AcceptRecommendation answers an item's question with the recommendation of the orchestrator's
// escalation (event escalationID), as actor (the approver: the Slack layer checks): the Accept
// button. The answer event says which escalation it accepted (data.accepted). An item that has
// left needs_you, or asks something new since the escalation, is refused; so is an escalation
// whose recommendation needs the user's hand first (data.step) until its pending accept
// (AcceptPending) is recorded, so an Accept never re-queues the item before the step is done.
func (s *Store) AcceptRecommendation(ctx context.Context, actor string, escalationID int64) (pipeline.Item, error) {
	var out pipeline.Item
	var itemID int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		esc, it, err := acceptable(tx, escalationID)
		itemID = esc.ItemID
		if err != nil {
			return err
		}
		if step := str(esc.Data, "step"); step != "" {
			pending, err := hasAcceptPending(tx, esc)
			if err != nil {
				return err
			}
			if !pending {
				return errf(pipeline.CodeWrongStage,
					"the recommendation needs your hand first (%s): Accept holds the item for it, then 'Done — re-queue' records the answer", step)
			}
		}
		tr, err := pipeline.ApplyAnswer(it, strings.TrimSpace(str(esc.Data, "recommendation")))
		if err != nil {
			return err
		}
		tr.Data["accepted"] = esc.ID
		tr.Item.UpdatedAt = s.clock.Now()
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = tr.Item
		if err := s.event(tx, transitionEvent(it.ID, actor, tr)); err != nil {
			return err
		}
		return s.planItemSettled(tx, tr.Item)
	})
	if err != nil {
		var pe *pipeline.Error
		if itemID != 0 && errors.As(err, &pe) {
			return out, s.refuse(ctx, actor, itemID, "accept", err)
		}
		return out, err
	}
	return out, nil
}

// GetEvent is one event by its id.
func (s *Store) GetEvent(ctx context.Context, id int64) (Event, error) {
	evs, err := scanEventRows(s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE id = ?`, id))
	if err != nil {
		return Event{}, err
	}
	if len(evs) == 0 {
		return Event{}, errf(CodeNotFound, "no event %d", id)
	}
	return evs[0], nil
}

func scanEventRows(rows *sql.Rows, err error) ([]Event, error) {
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}
