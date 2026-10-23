package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A repo whose main CI is red is frozen until main is green again
// (docs/decisions/2026-10-18-main-red-freeze.md): nothing in it is claimed but its main-red P1, the
// runs already going finish and their advances wait (queued_advances), and an advance applies in
// the order the runs finished once the freeze lifts.

// Event kinds of a freeze. frozen and unfrozen are loopd's, with no item (the bot posts them in
// #loop); advance_queued and queued_advance_dropped are on the item whose run finished; land_frozen
// is a land run stopped before it could merge.
const (
	KindFrozen        = "frozen"
	KindUnfrozen      = "unfrozen"
	KindAdvanceQueued = "advance_queued"
	KindQueuedDropped = "queued_advance_dropped"
	KindLandFrozen    = "land_frozen"
	// KindFreezeHandover is a freeze given to another item, because the one it named closed or was
	// dropped: on the item that takes it over (#558).
	KindFreezeHandover = "freeze_handover"
)

// CodeNotFrozen refuses lifting a freeze that is not there.
const CodeNotFrozen = "not_frozen"

// Freeze is a repo frozen while its main is red.
type Freeze struct {
	ID   int64  `json:"id"`
	Repo string `json:"repo"`
	// ItemID is the main-red P1, the one item of the repo that still moves.
	ItemID    int64  `json:"item_id"`
	ItemTitle string `json:"item_title"`
	// Since is when the freeze began; RedSince when the culprit merged (the red window's start).
	Since    time.Time `json:"since"`
	RedSince time.Time `json:"red_since"`
	Jobs     []string  `json:"jobs"`
	// Queued is how many finished runs' advances wait for the freeze to lift.
	Queued int `json:"queued"`
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func liveFreezes(q querier) ([]Freeze, error) {
	rows, err := q.Query(`SELECT f.id, f.repo, f.item_id, i.title, f.since, f.red_since, f.jobs,
		(SELECT COUNT(*) FROM queued_advances qa WHERE qa.freeze_id = f.id AND qa.applied_at IS NULL)
		FROM repo_freezes f JOIN items i ON i.id = f.item_id WHERE f.lifted_at IS NULL ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Freeze{}
	for rows.Next() {
		var f Freeze
		var since, red int64
		var jobs string
		if err := rows.Scan(&f.ID, &f.Repo, &f.ItemID, &f.ItemTitle, &since, &red, &jobs, &f.Queued); err != nil {
			return nil, err
		}
		f.Since, f.RedSince, f.Jobs = fromNanos(since), fromNanos(red), splitList(jobs)
		out = append(out, f)
	}
	return out, rows.Err()
}

func liveFreeze(q querier, repo string) (*Freeze, error) {
	all, err := liveFreezes(q)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Repo == repo {
			return &all[i], nil
		}
	}
	return nil, nil
}

func frozenRepos(q querier) (pipeline.Frozen, error) {
	all, err := liveFreezes(q)
	if err != nil {
		return nil, err
	}
	f := pipeline.Frozen{}
	for _, x := range all {
		f[x.Repo] = x.ItemID
	}
	return f, nil
}

// Freezes are the repos frozen now.
func (s *Store) Freezes(ctx context.Context) ([]Freeze, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return liveFreezes(tx)
}

// mainRed handles a red main_ci on culprit (the item whose merge main went red on): it files the
// repo's main-red P1 and freezes the repo, or reuses the P1 of the freeze already there while that
// item is open, or the repo's own open main-red P1 (#558). It returns the P1.
func (s *Store) mainRed(tx *sql.Tx, actor string, culprit pipeline.Item, data map[string]any, now time.Time) (int64, error) {
	repo := culprit.Repo
	f, err := liveFreeze(tx, repo)
	if err != nil {
		return 0, err
	}
	if f != nil {
		p1, err := loadItem(tx, f.ItemID)
		if err != nil {
			return 0, err
		}
		if p1.Stage != pipeline.Done && p1.Stage != pipeline.Closed {
			data["reused"] = true
			return f.ItemID, nil
		}
	}
	jobs := anyStrings(data["failing"])
	mergeSHA, _ := data["merge_sha"].(string)
	// An open main-red P1 of the repo takes the freeze over instead: a second one would carry the
	// same title, and triage would only close it as a duplicate of the first -- which is how a
	// freeze came to name a closed item and deadlock the repo (#558).
	existing, err := openMainRed(tx, repo, 0)
	if err != nil {
		return 0, err
	}
	itemID := int64(0)
	if existing != nil {
		itemID, data["reused"] = existing.ID, true
	} else {
		m := pipeline.MainRed{Slug: repo, PR: culprit.PR, Item: culprit.ID, Failing: jobs, URLs: anyStrings(data["urls"])}
		m.MergeSHA = mergeSHA
		m.LastGreen, _ = data["last_green"].(string)
		it, err := pipeline.NewItem(pipeline.MainRedTitle(repo, jobs), pipeline.MainRedAsk(m), repo, "P1", "finding", now)
		if err != nil {
			return 0, err
		}
		if err := s.insertItem(tx, actor, &it); err != nil {
			return 0, err
		}
		itemID = it.ID
	}
	ev := map[string]any{"repo": repo, "item": itemID, "jobs": jobs, "merge_sha": mergeSHA, "culprit_item": culprit.ID, "culprit_pr": culprit.PR}
	if existing != nil {
		ev["reused"] = true
	}
	if f != nil {
		if _, err := tx.Exec(`UPDATE repo_freezes SET item_id = ?, jobs = ? WHERE id = ?`, itemID, strings.Join(jobs, ","), f.ID); err != nil {
			return 0, err
		}
		ev["again"] = true
	} else {
		redSince := now
		var at int64
		err := tx.QueryRow(`SELECT at FROM events WHERE item_id = ? AND kind = 'advanced' AND outcome = ? ORDER BY id DESC LIMIT 1`,
			culprit.ID, string(pipeline.Merged)).Scan(&at)
		if err == nil {
			redSince = fromNanos(at)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO repo_freezes (repo, item_id, since, red_since, jobs) VALUES (?,?,?,?,?)`,
			repo, itemID, now.UnixNano(), redSince.UnixNano(), strings.Join(jobs, ",")); err != nil {
			return 0, err
		}
	}
	return itemID, s.event(tx, Event{Actor: "loopd", Kind: KindFrozen, Data: ev})
}

// openMainRed is the repo's oldest open main-red P1 (pipeline.IsMainRed) other than skip, nil when
// it has none. Open: before done, so a P1 whose fix merged is not taken up again.
func openMainRed(q querier, repo string, skip int64) (*pipeline.Item, error) {
	if repo == "" {
		return nil, nil
	}
	rows, err := q.Query(`SELECT `+itemCols+` FROM items WHERE repo = ? AND priority = 'P1'
		AND stage NOT IN ('done','closed') AND id != ? ORDER BY id`, repo, skip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		if pipeline.IsMainRed(it) {
			return &it, nil
		}
	}
	return nil, rows.Err()
}

// freezeItemSettled keeps a freeze's mover alive when the item it names stops being open: triage
// closed it, or the user dropped it. A freeze must not depend on one item id staying open (#558,
// docs/decisions/2026-10-23-freeze-without-one-item.md), so the freeze, in order:
//
//   - hands over to the item a duplicate closure names, when that one is open in the same repo
//     (the incident: the P1 was closed as a duplicate of the P1 whose fix was already in review);
//   - else hands over to another open main-red P1 of the repo;
//   - else re-files the P1 from the closed one's ask and hands over to that, keeping the red
//     window: main is still red, and lifting here would drain the queued advances and resume
//     merges onto it, the opposite of the freeze's job;
//   - and only when the re-filed one settles too does it lift, saying why.
func (s *Store) freezeItemSettled(tx *sql.Tx, it pipeline.Item, now time.Time) error {
	f, err := liveFreeze(tx, it.Repo)
	if err != nil || f == nil || f.ItemID != it.ID {
		return err
	}
	if dup, isDup := pipeline.DuplicateOf(it.Summary); isDup && dup != 0 && dup != it.ID {
		target, err := scanItem(tx.QueryRow(`SELECT `+itemCols+` FROM items WHERE id = ?`, dup))
		switch {
		case errors.Is(err, sql.ErrNoRows): // it named an item that is not there
		case err != nil:
			return err
		case target.Repo == it.Repo && !settled(target.Stage):
			return s.handFreezeTo(tx, *f, target, fmt.Sprintf("#%d closed as a duplicate of it", it.ID), false, now)
		}
	}
	other, err := openMainRed(tx, it.Repo, it.ID)
	if err != nil {
		return err
	}
	if other != nil {
		return s.handFreezeTo(tx, *f, *other, fmt.Sprintf("#%d closed; this is the repo's open main-red P1", it.ID), false, now)
	}
	refiled, err := freezeRefiled(tx, f.ID)
	if err != nil {
		return err
	}
	if refiled {
		_, err := s.unfreeze(tx, "loopd", *f,
			fmt.Sprintf("the re-filed P1 #%d closed too; no main-red item left to move", it.ID), now)
		return err
	}
	fresh, err := pipeline.NewItem(it.Title, it.Ask, it.Repo, "P1", "finding", now)
	if err != nil {
		return err
	}
	if err := s.insertItem(tx, "loopd", &fresh); err != nil {
		return err
	}
	return s.handFreezeTo(tx, *f, fresh, fmt.Sprintf("#%d closed before main went green, so its P1 is re-filed", it.ID), true, now)
}

// handFreezeTo points f at to and records it on to's thread: who moves while the repo is frozen
// changed.
func (s *Store) handFreezeTo(tx *sql.Tx, f Freeze, to pipeline.Item, reason string, refiled bool, now time.Time) error {
	if _, err := tx.Exec(`UPDATE repo_freezes SET item_id = ? WHERE id = ?`, to.ID, f.ID); err != nil {
		return err
	}
	data := map[string]any{"repo": f.Repo, "freeze": f.ID, "from": f.ItemID, "to": to.ID, "reason": reason,
		"jobs": f.Jobs, "since": f.Since.UTC().Format(time.RFC3339)}
	if refiled {
		data["refiled"] = true
	}
	return s.event(tx, Event{ItemID: to.ID, Actor: "loopd", Kind: KindFreezeHandover,
		From: string(to.Stage), To: string(to.Stage), Data: data})
}

// freezeRefiled reports whether f has already re-filed its P1 once: the guard against re-filing
// for ever when nothing ever fixes main.
func freezeRefiled(q querier, freezeID int64) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?
		AND json_extract(data, '$.freeze') = ? AND json_extract(data, '$.refiled') = 1`, KindFreezeHandover, freezeID).Scan(&n)
	return n > 0, err
}

// settled: the item is not open any more, so a freeze cannot wait on it.
func settled(st pipeline.Stage) bool { return st == pipeline.Closed || st == pipeline.Done }

// mainGreen handles a green main_ci on item: when its repo is frozen and the item merged after
// the culprit (so the green commit has the red one in it), the freeze lifts.
func (s *Store) mainGreen(tx *sql.Tx, it pipeline.Item, data map[string]any, now time.Time) error {
	f, err := liveFreeze(tx, it.Repo)
	if err != nil || f == nil {
		return err
	}
	var at int64
	err = tx.QueryRow(`SELECT at FROM events WHERE item_id = ? AND kind = 'advanced' AND outcome = ? ORDER BY id DESC LIMIT 1`,
		it.ID, string(pipeline.Merged)).Scan(&at)
	if err != nil {
		return err
	}
	if fromNanos(at).Before(f.RedSince) || fromNanos(at).Equal(f.RedSince) && it.ID != f.ItemID {
		return nil // merged before the red one: says nothing about main now
	}
	sha, _ := data["merge_sha"].(string)
	reason := fmt.Sprintf("main green on %s after #%d's merge", short(sha), it.ID)
	_, err = s.unfreeze(tx, "loopd", *f, reason, now)
	return err
}

// Unfreeze lifts repo's freeze by hand (loop unfreeze): the queued advances apply as when main
// goes green.
func (s *Store) Unfreeze(ctx context.Context, actor, repo string) (Freeze, error) {
	var out Freeze
	slug, err := s.ResolveRepo(repo)
	if err != nil {
		return out, err
	}
	if slug == "" {
		return out, errf(pipeline.CodeMissingField, "name the repo to unfreeze")
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		f, err := liveFreeze(tx, slug)
		if err != nil {
			return err
		}
		if f == nil {
			return errf(CodeNotFrozen, "%s is not frozen", slug)
		}
		out, err = s.unfreeze(tx, actor, *f, "lifted by hand", s.clock.Now())
		return err
	})
	return out, err
}

// unfreeze lifts f, then applies its queued advances in the order the runs finished, then sends
// back to build (pipeline.ApplyMainRedRebuild) the items whose PR's CI failed in the red window.
func (s *Store) unfreeze(tx *sql.Tx, actor string, f Freeze, reason string, now time.Time) (Freeze, error) {
	if _, err := tx.Exec(`UPDATE repo_freezes SET lifted_at = ?, lifted_by = ?, lift_reason = ? WHERE id = ?`, now.UnixNano(), actor, reason, f.ID); err != nil {
		return f, err
	}
	applied, dropped, err := s.drainQueue(tx, f, now)
	if err != nil {
		return f, err
	}
	rebuilt, err := s.mainRedRebuilds(tx, f, now)
	if err != nil {
		return f, err
	}
	f.Queued = 0
	return f, s.event(tx, Event{Actor: actor, Kind: KindUnfrozen, Data: map[string]any{"repo": f.Repo, "item": f.ItemID, "reason": reason,
		"applied": applied, "dropped": dropped, "rebuilt": rebuilt, "since": f.Since.UTC().Format(time.RFC3339)}})
}

// queueAdvance keeps a run's checked advance for when its repo's freeze lifts, and ends the lease:
// the item stays where it is, and no claim takes it meanwhile (waitingItems).
func (s *Store) queueAdvance(tx *sql.Tx, actor, runnerName string, it pipeline.Item, l Lease, adv pipeline.Advance, now time.Time) error {
	f, err := liveFreeze(tx, it.Repo)
	if err != nil {
		return err
	}
	fields, err := json.Marshal(adv.Fields)
	if err != nil {
		return err
	}
	paths, err := json.Marshal(adv.ChangedPaths)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO queued_advances (freeze_id, item_id, lease_id, runner, agent, stage, outcome, fields, changed_paths, queued_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, f.ID, it.ID, l.ID, runnerName, l.Agent, string(it.Stage), string(adv.Outcome), string(fields), string(paths), now.UnixNano()); err != nil {
		return err
	}
	if err := endLease(tx, l.ID, now, "queued"); err != nil {
		return err
	}
	return s.event(tx, Event{ItemID: it.ID, Actor: actor, Kind: KindAdvanceQueued, From: string(it.Stage), To: string(it.Stage), Outcome: string(adv.Outcome),
		Data: map[string]any{"lease": l.ID, "agent": l.Agent, "repo": it.Repo, "fix_item": f.ItemID}})
}

type queuedAdvance struct {
	id                   int64
	item                 int64
	lease, runner, agent string
	stage                pipeline.Stage
	outcome              pipeline.Outcome
	fields               map[string]string
	paths                []string
	queuedAt             time.Time
}

func (s *Store) drainQueue(tx *sql.Tx, f Freeze, now time.Time) (applied, dropped int, err error) {
	rows, err := tx.Query(`SELECT id, item_id, lease_id, runner, agent, stage, outcome, fields, changed_paths, queued_at
		FROM queued_advances WHERE freeze_id = ? AND applied_at IS NULL ORDER BY id`, f.ID)
	if err != nil {
		return 0, 0, err
	}
	var queue []queuedAdvance
	for rows.Next() {
		var q queuedAdvance
		var stage, outcome, fields, paths string
		var at int64
		if err := rows.Scan(&q.id, &q.item, &q.lease, &q.runner, &q.agent, &stage, &outcome, &fields, &paths, &at); err != nil {
			rows.Close()
			return 0, 0, err
		}
		q.stage, q.outcome, q.queuedAt = pipeline.Stage(stage), pipeline.Outcome(outcome), fromNanos(at)
		if err := json.Unmarshal([]byte(fields), &q.fields); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if err := json.Unmarshal([]byte(paths), &q.paths); err != nil {
			rows.Close()
			return 0, 0, err
		}
		queue = append(queue, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	ctl, err := s.loadControl(tx)
	if err != nil {
		return 0, 0, err
	}
	for _, q := range queue {
		if _, err := tx.Exec(`UPDATE queued_advances SET applied_at = ? WHERE id = ?`, now.UnixNano(), q.id); err != nil {
			return applied, dropped, err
		}
		actor := "runner:" + q.runner
		it, err := loadItem(tx, q.item)
		if err != nil {
			return applied, dropped, err
		}
		why := ""
		var live int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM leases WHERE item_id = ? AND ended_at IS NULL`, q.item).Scan(&live); err != nil {
			return applied, dropped, err
		}
		var tr pipeline.Transition
		switch {
		case it.Stage != q.stage:
			why = fmt.Sprintf("the item is %s now, not %s", it.Stage, q.stage)
		case live > 0:
			why = "the item has a live lease"
		default:
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM evidence WHERE lease_id = ?`, q.lease).Scan(&n); err != nil {
				return applied, dropped, err
			}
			tr, err = pipeline.ApplyAdvance(it, pipeline.Advance{Outcome: q.outcome, Fields: q.fields, Evidence: n, Agent: q.agent,
				ChangedPaths: q.paths, HoldRules: ctl.HoldRules, Repos: s.Repos()}, now)
			if err != nil {
				why = err.Error()
			}
		}
		if why != "" {
			dropped++
			if err := s.event(tx, Event{ItemID: q.item, Actor: "loopd", Kind: KindQueuedDropped, From: string(it.Stage), To: string(it.Stage), Outcome: string(q.outcome),
				Data: map[string]any{"lease": q.lease, "reason": why}}); err != nil {
				return applied, dropped, err
			}
			continue
		}
		tr.Data["queued_at"] = q.queuedAt.UTC().Format(time.RFC3339)
		if err := s.applyAdvance(tx, actor, q.item, q.lease, tr, q.fields, now); err != nil {
			return applied, dropped, err
		}
		applied++
	}
	return applied, dropped, nil
}

// mainRedRebuilds sends back to build, with pipeline.MainRedNotes, the items of f's repo that land
// sent back for red CI in the red window and that have not moved since: their CI is checked again
// on a rebased head instead of on the red base.
func (s *Store) mainRedRebuilds(tx *sql.Tx, f Freeze, now time.Time) ([]int64, error) {
	rows, err := tx.Query(`SELECT `+itemCols+` FROM items WHERE repo = ? AND stage IN ('build','needs_you')
		AND id NOT IN (SELECT item_id FROM leases WHERE ended_at IS NULL) ORDER BY id`, f.Repo)
	if err != nil {
		return nil, err
	}
	var cands []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rebuilt := []int64{}
	for _, it := range cands {
		var at int64
		var kind, outcome, data string
		err := tx.QueryRow(`SELECT at, kind, outcome, data FROM events WHERE item_id = ?
			AND kind IN ('advanced','rebuild','answered','claimed','approved') ORDER BY id DESC LIMIT 1`, it.ID).Scan(&at, &kind, &outcome, &data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if kind != "advanced" || outcome != string(pipeline.BackToBuild) || fromNanos(at).Before(f.RedSince) {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return nil, err
		}
		if _, red := d["ci_head_sha"]; !red {
			continue
		}
		tr, err := pipeline.ApplyMainRedRebuild(it)
		if err != nil {
			continue // a needs_you that is not a build's question
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return nil, err
		}
		if err := s.event(tx, transitionEvent(it.ID, "loopd", tr)); err != nil {
			return nil, err
		}
		rebuilt = append(rebuilt, it.ID)
	}
	return rebuilt, nil
}

func anyStrings(v any) []string {
	out := []string{}
	switch l := v.(type) {
	case []string:
		return append(out, l...)
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
