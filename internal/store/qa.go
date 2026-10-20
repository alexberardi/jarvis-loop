package store

import (
	"context"
	"database/sql"
	"strconv"

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
// the new item's id), and flags the open items that depend on it (dependency_reopened). It returns
// both.
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
		old, filed, err = s.reopenTx(tx, actor, it, tr)
		return err
	})
	if err != nil {
		return old, filed, s.refuse(ctx, actor, id, "reopen", err)
	}
	return old, filed, nil
}

// reopenTx stores a reopen transition of it (the user's or a failed proof's): it files the new P2
// item with the transition's (trimmed) words, and records the event with the new item's id.
func (s *Store) reopenTx(tx *sql.Tx, actor string, it pipeline.Item, tr pipeline.Transition) (old, filed pipeline.Item, err error) {
	words := tr.Data["words"].(string)
	now := s.clock.Now()
	n, err := pipeline.NewReopen(it, words, now)
	if err != nil {
		return old, filed, err
	}
	if tr.From == pipeline.Done {
		n.Priority = pipeline.DoneProofReopenPriority // a done item's proof failed (doneproof.go)
	}
	if err := s.insertItem(tx, actor, &n); err != nil {
		return old, filed, err
	}
	if tr.From == pipeline.Done {
		tr.Item.VerifyReason += " (filed #" + strconv.FormatInt(n.ID, 10) + ")"
	}
	tr.Item.UpdatedAt = now
	if err := saveItem(tx, tr.Item); err != nil {
		return old, filed, err
	}
	tr.Data["reopen_item"] = n.ID
	if err := s.event(tx, transitionEvent(it.ID, actor, tr)); err != nil {
		return old, filed, err
	}
	if err := s.dependencyReopened(tx, it.ID, n.ID, words); err != nil {
		return old, filed, err
	}
	return tr.Item, n, s.planItemSettled(tx, tr.Item)
}

// Deployed is a runner reporting that the loop build it runs contains a qa item's merge commit
// (kind deployed, data {revision}; docs/decisions/2026-10-16-qa-after-deploy.md): the deploy wait
// ends and the Slack bot asks the user to check. Any runner may report it, once; a second report,
// or one for an item no longer waiting, is wrong_stage and not recorded as a refusal (two runners
// may race to it).
func (s *Store) Deployed(ctx context.Context, runnerName string, id int64, data map[string]any) (Event, error) {
	var out Event
	for k := range data {
		if k != "revision" {
			return out, errf(pipeline.CodeInvalidField, "deployed data may hold revision, not %q", k)
		}
	}
	rev, _ := data["revision"].(string)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		tr, err := pipeline.ApplyDeployed(it, rev)
		if err != nil {
			return err
		}
		tr.Item.UpdatedAt = s.clock.Now()
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = transitionEvent(id, "runner:"+runnerName, tr)
		return s.event(tx, out)
	})
	if err != nil {
		return out, err
	}
	out.At = s.clock.Now()
	return out, nil
}
