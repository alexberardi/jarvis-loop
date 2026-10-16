package store

import (
	"context"
	"database/sql"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// SetQA marks an item needs_qa before it lands (docs/decisions/2026-10-15-qa.md): once merged it
// waits in qa for the user's check.
func (s *Store) SetQA(ctx context.Context, actor string, id int64) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "qa", pipeline.ApplySetQA)
}

// Verified is the user's check of a qa item passing: qa → done.
func (s *Store) Verified(ctx context.Context, actor string, id int64, words string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "verified", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyVerified(it, words)
	})
}

// Reopen is the user's check of a qa item failing: in one transaction it files a new P2 item with
// the words as its ask, linked by reopen_of, and moves the qa item to done (event reopened, with
// the new item's id). It returns both.
func (s *Store) Reopen(ctx context.Context, actor string, id int64, words string) (old, filed pipeline.Item, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		tr, err := pipeline.ApplyReopen(it, words)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		n, err := pipeline.NewReopen(it, words, now)
		if err != nil {
			return err
		}
		if err := s.insertItem(tx, actor, &n); err != nil {
			return err
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		tr.Data["reopen_item"] = n.ID
		old, filed = tr.Item, n
		if err := s.event(tx, transitionEvent(id, actor, tr)); err != nil {
			return err
		}
		return s.planItemSettled(tx, tr.Item)
	})
	if err != nil {
		return old, filed, s.refuse(ctx, actor, id, "reopen", err)
	}
	return old, filed, nil
}
