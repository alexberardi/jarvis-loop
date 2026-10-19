package store

import (
	"context"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The user's approval of a held merge, their OK for verify to run trusted files, and keeping a
// held merge held. On jarvis-loop's hold list (docs/decisions/2026-10-18-narrow-loop-holds.md).

// Approve records the user's approval of a held merge for headSHA.
func (s *Store) Approve(ctx context.Context, actor string, id int64, headSHA, words string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "approve", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyApprove(it, actor, headSHA, words, s.clock.Now())
	})
}

// Trust records the approver's OK for verify to run headSHA, the commit it refused for changing
// trusted files.
func (s *Store) Trust(ctx context.Context, actor string, id int64, headSHA string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "trust", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyTrust(it, actor, headSHA, s.clock.Now())
	})
}

// Hold records the user keeping a held merge held.
func (s *Store) Hold(ctx context.Context, actor string, id int64, words string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "hold", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyHold(it, words)
	})
}
