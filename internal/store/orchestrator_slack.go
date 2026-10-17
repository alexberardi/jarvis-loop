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
// any, is in data.item_id). A message seen before (Slack's retries) records nothing: fresh is false.
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
		return s.recordEvent(tx, &e, Event{Actor: "loopd", Kind: KindOrchMessage, Data: data})
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

// OrchReply records the orchestrator's answer in a Slack thread (or a DM); its Slack app posts it.
// Where it may post is the API's and the Slack client's to check.
func (s *Store) OrchReply(ctx context.Context, actor, channel, threadTS, text string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		text = strings.TrimSpace(text)
		if text == "" {
			return errf(pipeline.CodeMissingField, "text is required")
		}
		if channel == "" {
			return errf(pipeline.CodeMissingField, "channel is required")
		}
		return s.recordEvent(tx, &out, Event{Actor: actor, Kind: KindOrchReply,
			Data: map[string]any{"channel": channel, "thread_ts": threadTS, "text": text}})
	})
	return out, err
}

// OrchAskApproval has the approval message of an item held for approval posted again, with its
// sha-bound button: what the orchestrator does when the user asks it to approve (it never may).
func (s *Store) OrchAskApproval(ctx context.Context, actor string, id int64, reason string) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		if it.Stage != pipeline.Held {
			return errf(pipeline.CodeWrongStage, "item is %s, not held for approval", it.Stage)
		}
		r, err := pipeline.NeedReason(reason)
		if err != nil {
			return err
		}
		return s.recordEvent(tx, &out, Event{ItemID: id, Actor: actor, Kind: KindOrchApprovalAsked,
			Data: map[string]any{"head_sha": it.HeadSHA, "reason": r}})
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, "approval", err)
	}
	return out, nil
}

// AcceptRecommendation answers an item's question with the recommendation of the orchestrator's
// escalation (event escalationID), as actor (the approver: the Slack layer checks): the Accept
// button. The answer event says which escalation it accepted (data.accepted). An item that has
// left needs_you, or asks something new since the escalation, is refused.
func (s *Store) AcceptRecommendation(ctx context.Context, actor string, escalationID int64) (pipeline.Item, error) {
	var out pipeline.Item
	var itemID int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var esc Event
		evs, err := scanEventRows(tx.Query(`SELECT `+eventCols+` FROM events WHERE id = ?`, escalationID))
		if err != nil {
			return err
		}
		if len(evs) == 1 {
			esc = evs[0]
		}
		rec := strings.TrimSpace(str(esc.Data, "recommendation"))
		if esc.Kind != KindOrchEscalated || esc.ItemID == 0 || rec == "" {
			return errf(pipeline.CodeInvalidField, "no recommendation %d about an item to accept", escalationID)
		}
		itemID = esc.ItemID
		it, err := loadItem(tx, esc.ItemID)
		if err != nil {
			return err
		}
		if it.Stage != pipeline.NeedsYou {
			return errf(pipeline.CodeWrongStage, "item is %s, not needs_you: nothing to accept", it.Stage)
		}
		all, err := itemEvents(tx, esc.ItemID)
		if err != nil {
			return err
		}
		if last, ok := lastNeedsYou(all); ok && last.ID > esc.ID {
			return errf(pipeline.CodeWrongStage, "the item asks something new since that recommendation: answer it in the thread")
		}
		tr, err := pipeline.ApplyAnswer(it, rec)
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
