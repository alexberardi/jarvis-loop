package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Error codes for main_ci.
const (
	CodeNotYourMerge    = "not_your_merge"
	CodeAlreadyRecorded = "already_recorded"
)

// MainCIResults are what a main_ci event may report about main's CI on a merge commit.
var MainCIResults = []string{"green", "red", "superseded", "timeout"}

// mainCIKeys are the only data a main_ci event may carry.
var mainCIKeys = []string{"result", "urls", "failing", "merge_sha", "p1_item", "rerun", "last_green"}

// MainCI records the result of main's CI on the commit an item was merged as (kind main_ci). Only
// the runner whose land run merged the item may record it, once, after the merge (the item is done,
// or in qa waiting for the user's check, or was closed from qa): the lease ended at the merge, so
// the merged event is the proof of who watches.
func (s *Store) MainCI(ctx context.Context, runnerName string, id int64, data map[string]any) (Event, error) {
	var out Event
	actor := "runner:" + runnerName
	if err := validMainCI(data); err != nil {
		return out, err
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		if it.Stage != pipeline.Done && it.Stage != pipeline.QA && it.Stage != pipeline.Closed {
			return errf(pipeline.CodeWrongStage, "item %d is %s, not merged", id, it.Stage)
		}
		var mergedBy string
		err = tx.QueryRow(`SELECT actor FROM events WHERE item_id = ? AND kind = 'advanced' AND outcome = ? ORDER BY id DESC LIMIT 1`,
			id, string(pipeline.Merged)).Scan(&mergedBy)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(pipeline.CodeWrongStage, "item %d was not merged by a land run", id)
		}
		if err != nil {
			return err
		}
		if mergedBy != actor {
			return errf(CodeNotYourMerge, "item %d was merged by another runner", id)
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE item_id = ? AND kind = 'main_ci'`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errf(CodeAlreadyRecorded, "item %d already has main's CI result", id)
		}
		// A red main (still red after the runner re-ran the failed jobs) freezes the repo and files
		// its P1, or reuses the open one; a green one after the red merge lifts the freeze.
		switch data["result"] {
		case "red":
			delete(data, "p1_item")
			p1, err := s.mainRed(tx, actor, it, data, s.clock.Now())
			if err != nil {
				return err
			}
			data["p1_item"] = float64(p1)
		}
		out = Event{ItemID: id, Actor: actor, Kind: "main_ci", From: string(it.Stage), To: string(it.Stage), Data: data}
		if err := s.event(tx, out); err != nil {
			return err
		}
		if data["result"] == "green" {
			return s.mainGreen(tx, it, data, s.clock.Now())
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	out.At = s.clock.Now()
	return out, nil
}

func validMainCI(data map[string]any) error {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(mainCIKeys, k) {
			return errf(pipeline.CodeInvalidField, "main_ci data may hold %s, not %q", strings.Join(mainCIKeys, ", "), k)
		}
	}
	res, ok := data["result"].(string)
	if !ok || res == "" {
		return errf(pipeline.CodeMissingField, "main_ci needs a result (%s)", strings.Join(MainCIResults, ", "))
	}
	if !slices.Contains(MainCIResults, res) {
		return errf(pipeline.CodeInvalidField, "result must be one of %s", strings.Join(MainCIResults, ", "))
	}
	for _, k := range []string{"urls", "failing", "rerun"} {
		v, present := data[k]
		if !present {
			continue
		}
		list, ok := v.([]any)
		if !ok || len(list) > 50 {
			return errf(pipeline.CodeInvalidField, "%s must be a list of at most 50 strings", k)
		}
		for _, x := range list {
			if s, ok := x.(string); !ok || len(s) > 500 {
				return errf(pipeline.CodeInvalidField, "%s must be a list of strings", k)
			}
		}
	}
	for _, k := range []string{"merge_sha", "last_green"} {
		if v, present := data[k]; present {
			if s, ok := v.(string); !ok || len(s) > 64 {
				return errf(pipeline.CodeInvalidField, "%s must be a commit sha", k)
			}
		}
	}
	if v, present := data["p1_item"]; present {
		if f, ok := v.(float64); !ok || f < 1 || f != float64(int64(f)) {
			return errf(pipeline.CodeInvalidField, "p1_item must be an item id")
		}
	}
	return nil
}
