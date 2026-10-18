package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Acting for the user: only on a recent message of the approver's, once per action and target
// (docs/decisions/2026-10-16-orchestrator-token.md). On jarvis-loop's hold list
// (docs/decisions/2026-10-18-narrow-loop-holds.md).

// Error codes of acting for the user.
const (
	CodeNotApprover = "not_approver"
	CodeUsed        = "message_used"
)

// OnBehalfWindow is how long after the approver's message the orchestrator may act on it.
const OnBehalfWindow = 10 * time.Minute

// UseApproverMessage spends the approver's message (channel, ts) on one action on one target (an
// item id, or "" for none): it must be a message the Slack layer saw from approver, at most
// OnBehalfWindow old, and not used for this action on this target before. by is who acts on it.
func (s *Store) UseApproverMessage(ctx context.Context, channel, ts, approver, action, target, by string) (SlackMessage, error) {
	m := SlackMessage{Channel: channel, TS: ts}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var at int64
		err := tx.QueryRow(`SELECT user, text, at FROM slack_messages WHERE channel = ? AND ts = ?`, channel, ts).Scan(&m.User, &m.Text, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNotApprover, "no message %s/%s from the approver", channel, ts)
		}
		if err != nil {
			return err
		}
		m.At = fromNanos(at)
		if approver == "" || m.User != approver {
			return errf(CodeNotApprover, "message %s/%s is not the approver's: only the approver's messages make it act", channel, ts)
		}
		now := s.clock.Now()
		if now.Sub(m.At) > OnBehalfWindow {
			return errf(CodeNotApprover, "message %s/%s is older than %d minutes", channel, ts, int(OnBehalfWindow.Minutes()))
		}
		res, err := tx.Exec(`INSERT INTO slack_message_uses (channel, ts, action, target, actor, at) VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			channel, ts, action, target, by, now.UnixNano())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errf(CodeUsed, "message %s/%s was already used for %s %s", channel, ts, action, target)
		}
		return nil
	})
	return m, err
}
