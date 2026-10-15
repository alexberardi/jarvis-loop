package store

import (
	"context"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// BoardItem is a card of the web board: a board row plus what the card shows beyond it.
type BoardItem struct {
	BoardRow
	Question     string
	HoldReason   string
	SlackChannel string
	SlackTS      string
	UpdatedAt    time.Time
}

// BoardItems is every item as a board card, in ListItems' order.
func (s *Store) BoardItems(ctx context.Context) ([]BoardItem, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT i.id, i.title, i.priority, i.stage, i.repo, i.pr, i.created_at,
		COALESCE(l.runner || '/' || l.agent, ''), i.question, i.hold_reason, i.slack_channel, i.slack_ts, i.updated_at
		FROM items i LEFT JOIN leases l ON l.item_id = i.id AND l.ended_at IS NULL
		ORDER BY i.priority, i.created_at, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.clock.Now()
	var out []BoardItem
	for rows.Next() {
		var b BoardItem
		var created, updated int64
		if err := rows.Scan(&b.ID, &b.Title, &b.Priority, &b.Stage, &b.Repo, &b.PR, &created, &b.ClaimedBy,
			&b.Question, &b.HoldReason, &b.SlackChannel, &b.SlackTS, &updated); err != nil {
			return nil, err
		}
		b.CreatedAt, b.UpdatedAt = fromNanos(created), fromNanos(updated)
		b.AgeSeconds = int64(now.Sub(b.CreatedAt) / time.Second)
		b.Display = pipeline.Display(pipeline.Stage(b.Stage), b.ClaimedBy != "")
		out = append(out, b)
	}
	return out, rows.Err()
}
