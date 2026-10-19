package store

import (
	"context"
	"database/sql"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The user's corrections to a filed item (loop edit, link, unlink, resources, feature): each is an event,
// and a refusal is recorded as one too (human).

// Edit fixes an item's title or ask before build starts (pipeline.ApplyEdit).
func (s *Store) Edit(ctx context.Context, actor string, id int64, title, ask *string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "edit", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyEdit(it, title, ask)
	})
}

// Link makes an open item wait for more open items, as loop add --after does (kind linked,
// data.after the list). All link or none: a cycle or a closed item refuses the lot.
func (s *Store) Link(ctx context.Context, actor string, id int64, after []int64) (pipeline.Item, error) {
	return s.humanTx(ctx, actor, id, "link", func(tx *sql.Tx, it pipeline.Item) (pipeline.Transition, error) {
		if len(after) == 0 {
			return pipeline.Transition{}, errf(pipeline.CodeMissingField, "after is required: the items to wait for")
		}
		for _, a := range after {
			var err error
			if it, err = linkAfter(tx, it, a); err != nil {
				return pipeline.Transition{}, err
			}
		}
		return pipeline.Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindLinked,
			Data: map[string]any{"after": after, "depends_on": it.DependsOn}}, nil
	}, nil)
}

// Unlink stops an open item waiting for another (pipeline.ApplyUnlink).
func (s *Store) Unlink(ctx context.Context, actor string, id, after int64) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "unlink", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyUnlink(it, after)
	})
}

// SetResources replaces an item's resources after triage (pipeline.ApplyResources). Its unservable
// flag goes with the old resources (the resources event says why; a servable one would say a runner
// came), and the check runs again at once: flagged anew if no runner offers the new ones either.
func (s *Store) SetResources(ctx context.Context, actor string, id int64, resources string) (pipeline.Item, error) {
	return s.humanTx(ctx, actor, id, "resources", func(_ *sql.Tx, it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyResources(it, resources)
	}, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM unservable WHERE item_id = ?`, id); err != nil {
			return err
		}
		return s.checkUnservable(tx, s.clock.Now())
	})
}

// SetFeature tags an item with a feature, or clears it (pipeline.ApplyFeature).
func (s *Store) SetFeature(ctx context.Context, actor string, id int64, feature string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "feature", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyFeature(it, feature)
	})
}
