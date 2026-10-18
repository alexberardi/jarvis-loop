package store

import (
	"database/sql"
	"strconv"
)

// KindDependencyReopened is loopd's flag on an open item whose dependency failed the user's check
// (docs/decisions/2026-10-18-deps-at-merge.md): data {dependency, reopen_item, words}. The item
// keeps its stage and its claim.
const KindDependencyReopened = "dependency_reopened"

// ReopenedDependency is a dependency of an open item that was reopened: the board and the
// orchestrator show it until the dependent is done or closed, or the reopen item is merged or closed.
type ReopenedDependency struct {
	Dependency int64  `json:"dependency"`
	ReopenItem int64  `json:"reopen_item"`
	Words      string `json:"words"`
}

// dependencyReopened flags the open items that depend on dep, just reopened as reopenItem, in the
// transaction that reopened it.
func (s *Store) dependencyReopened(tx *sql.Tx, dep, reopenItem int64, words string) error {
	rows, err := tx.Query(`SELECT id, stage FROM items WHERE ',' || depends_on || ',' LIKE ? AND stage NOT IN ('done','closed') ORDER BY id`,
		"%,"+strconv.FormatInt(dep, 10)+",%")
	if err != nil {
		return err
	}
	type open struct {
		id    int64
		stage string
	}
	var deps []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.stage); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range deps {
		if err := s.event(tx, Event{ItemID: o.id, Actor: "loopd", Kind: KindDependencyReopened, From: o.stage, To: o.stage,
			Data: map[string]any{"dependency": dep, "reopen_item": reopenItem, "words": words}}); err != nil {
			return err
		}
	}
	return nil
}

// reopenedDependencies is each open item's standing flags, oldest first.
func reopenedDependencies(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) (map[int64][]ReopenedDependency, error) {
	rows, err := q.Query(`SELECT e.item_id, json_extract(e.data, '$.dependency'), json_extract(e.data, '$.reopen_item'),
		COALESCE(json_extract(e.data, '$.words'), '')
		FROM events e JOIN items i ON i.id = e.item_id JOIN items r ON r.id = json_extract(e.data, '$.reopen_item')
		WHERE e.kind = ? AND i.stage NOT IN ('done','closed') AND r.stage NOT IN ('qa','done','closed') ORDER BY e.id`,
		KindDependencyReopened)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ReopenedDependency{}
	for rows.Next() {
		var id int64
		var f ReopenedDependency
		if err := rows.Scan(&id, &f.Dependency, &f.ReopenItem, &f.Words); err != nil {
			return nil, err
		}
		out[id] = append(out[id], f)
	}
	return out, rows.Err()
}
