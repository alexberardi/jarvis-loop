package store

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Item dependencies (docs/phases/07-planning.md, "Item dependencies and the plan link").

func dedupeIDs(ids []int64) []int64 {
	out := []int64{}
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// checkDependencies refuses an item whose dependencies name an item that does not exist or is
// closed, or make a cycle. it is already inserted (or saved) in tx.
func checkDependencies(tx *sql.Tx, it pipeline.Item) error {
	if len(it.DependsOn) == 0 {
		return nil
	}
	for _, d := range it.DependsOn {
		// The new item's own id: to the user, that item does not exist yet.
		if d == it.ID {
			return errf(pipeline.CodeInvalidField, "depends_on: no item #%d", d)
		}
		var stage pipeline.Stage
		err := tx.QueryRow(`SELECT stage FROM items WHERE id = ?`, d).Scan(&stage)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(pipeline.CodeInvalidField, "depends_on: no item #%d", d)
		}
		if err != nil {
			return err
		}
		// A closed item never becomes done, so the new item would wait forever.
		if stage == pipeline.Closed {
			return errf(pipeline.CodeInvalidField, "depends_on: #%d is closed", d)
		}
	}
	rows, err := tx.Query(`SELECT id, depends_on FROM items WHERE depends_on != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	graph := map[int64][]int64{}
	for rows.Next() {
		var id int64
		var deps string
		if err := rows.Scan(&id, &deps); err != nil {
			return err
		}
		if graph[id], err = splitIDs(deps); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if c := pipeline.DependencyCycle(graph); c != nil {
		path := make([]string, len(c))
		for i, id := range c {
			path[i] = "#" + strconv.FormatInt(id, 10)
		}
		return errf(pipeline.CodeInvalidField, "depends_on makes a cycle: %s", strings.Join(path, " → "))
	}
	return nil
}

// dependencyStages is the stages of the items that items depend on.
func dependencyStages(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, items []pipeline.Item) (map[int64]pipeline.Stage, error) {
	var ids []any
	for _, it := range items {
		for _, d := range it.DependsOn {
			ids = append(ids, d)
		}
	}
	out := map[int64]pipeline.Stage{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(`SELECT id, stage FROM items WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = pipeline.Stage(st)
	}
	return out, rows.Err()
}

// waitingOn is what "waiting on #12" shows: the dependencies not merged yet, while the item is open.
func waitingOn(it pipeline.Item, stages map[int64]pipeline.Stage) []int64 {
	if it.Stage == pipeline.Done || it.Stage == pipeline.Closed {
		return []int64{}
	}
	w := pipeline.WaitingOn(it, stages)
	if w == nil {
		w = []int64{}
	}
	return w
}

// dependencyClosed moves the open items that depend on dep, just dropped or closed, to needs_you
// (pipeline.ApplyDependencyClosed), in the transaction that closed it.
func (s *Store) dependencyClosed(tx *sql.Tx, dep int64) error {
	rows, err := tx.Query(`SELECT `+itemCols+` FROM items WHERE ',' || depends_on || ',' LIKE ? ORDER BY id`,
		"%,"+strconv.FormatInt(dep, 10)+",%")
	if err != nil {
		return err
	}
	var deps []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.clock.Now()
	for _, it := range deps {
		tr, ok := pipeline.ApplyDependencyClosed(it, dep)
		if !ok {
			continue
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		if err := s.event(tx, transitionEvent(it.ID, "loopd", tr)); err != nil {
			return err
		}
	}
	return nil
}
