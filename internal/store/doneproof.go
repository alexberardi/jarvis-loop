package store

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Done proofs (docs/decisions/2026-10-18-done-proofs.md): the done sweep asks the orchestrator for
// a proof of each unproven done item, about ten at a time, lowest risk first; the runner's proof
// pass checks them as it checks qa items (a few per pass), and loopd moves only their verification
// status. A main-red item is verified by main's CI itself and needs no proof.

const (
	// DoneProofBatch is how many done items one sweep batch asks proofs for (one orchestrate turn).
	DoneProofBatch = 10
	// DoneProofAskWait is how long a batch waits for its proofs to be written and checked once
	// before the next batch is asked: an item the orchestrator wrote none for stays unproven.
	DoneProofAskWait = 6 * time.Hour
	// DoneProofsPerPass is how many done items one proof pass of the runner checks (qa items are
	// all checked): old items' commands must not hold the pass for an hour.
	DoneProofsPerPass = 3
	// DoneNoProof is the reason of an item the sweep asked for and got no proof.
	DoneNoProof = "the done sweep wrote no proof"
)

// doneSweep runs at loopd's tick (OrchTick): main-red items verified by CI, then the next batch,
// once the last one's proofs are written and checked (or DoneProofAskWait passed).
func (s *Store) doneSweep(tx *sql.Tx, now time.Time) error {
	reds, err := queryItems(tx, `SELECT `+itemCols+` FROM items WHERE stage = 'done' AND verify = ?
		AND source = 'finding' AND title LIKE 'main red %' ORDER BY id`, pipeline.DoneUnproven)
	if err != nil {
		return err
	}
	for _, it := range reds {
		if !pipeline.MainRedItem(it) {
			continue
		}
		tr, err := pipeline.ApplyVerifiedByCI(it)
		if err != nil {
			return err
		}
		tr.Item.UpdatedAt = now
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		if err := s.event(tx, transitionEvent(it.ID, pipeline.ActorProof, tr)); err != nil {
			return err
		}
	}
	cutoff := now.Add(-DoneProofAskWait).UnixNano()
	var open int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM items WHERE stage = 'done' AND verify_asked > ?
		AND (verify = ? OR (verify = ? AND json_extract(proof, '$.checked_at') IS NULL))`,
		cutoff, pipeline.DoneUnproven, pipeline.DoneProving).Scan(&open); err != nil || open > 0 {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET verify_reason = ? WHERE stage = 'done' AND verify = ? AND verify_asked > 0
		AND verify_reason = ''`, DoneNoProof, pipeline.DoneUnproven); err != nil {
		return err
	}
	// Lowest risk first: no risk hold, no hold-list paths, small before large; then by repo.
	ids, err := queryIDs(tx, `SELECT id FROM items WHERE stage = 'done' AND verify = ? AND verify_asked = 0
		ORDER BY risk = 'hold', hold_reason != '', CASE size WHEN 'S' THEN 0 WHEN 'M' THEN 1 WHEN 'L' THEN 2 ELSE 3 END, repo, id
		LIMIT ?`, pipeline.DoneUnproven, DoneProofBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.queueOrch(tx, pipeline.Trigger{Reason: pipeline.TriggerDoneProof, ItemID: id}); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET verify_asked = ? WHERE id = ?`, now.UnixNano(), id); err != nil {
			return err
		}
	}
	return nil
}

func queryItems(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]pipeline.Item, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func queryIDs(tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// doneProofsDue are the proving done items due a check, least recently checked first, at most
// DoneProofsPerPass.
func (s *Store) doneProofsDue(ctx context.Context) ([]BoardRow, error) {
	its, err := queryItems(s.r, `SELECT `+itemCols+` FROM items WHERE stage = 'done' AND verify = ? ORDER BY id`, pipeline.DoneProving)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	its = slices.DeleteFunc(its, func(it pipeline.Item) bool { return !pipeline.ProofDue(it, now) })
	slices.SortStableFunc(its, func(a, b pipeline.Item) int {
		switch ca, cb := a.Proof.CheckedAt, b.Proof.CheckedAt; {
		case ca == nil && cb == nil:
			return 0
		case ca == nil:
			return -1
		case cb == nil:
			return 1
		}
		return a.Proof.CheckedAt.Compare(*b.Proof.CheckedAt)
	})
	out := []BoardRow{}
	for _, it := range its[:min(len(its), DoneProofsPerPass)] {
		out = append(out, BoardRow{ID: it.ID, Title: it.Title, Priority: it.Priority, Stage: string(it.Stage),
			Display: pipeline.Display(it.Stage, false), Repo: it.Repo, PR: it.PR, CreatedAt: it.CreatedAt,
			AgeSeconds: int64(now.Sub(it.CreatedAt) / time.Second)})
	}
	return out, nil
}

// DoneReport is where the done items' verification stands (the morning report and the board).
type DoneReport struct {
	// Counts is the number of done items in each status (pipeline.VerifyStatuses).
	Counts map[string]int
	Total  int
	// Failed are the done items whose check failed, each with the item that fixes it.
	Failed []DoneReportItem
	// NeedsYou are the done items only the user can check, with what to look at.
	NeedsYou []DoneReportItem
}

// DoneReportItem is one done item in the report.
type DoneReportItem struct {
	ID     int64
	Title  string
	Repo   string
	Reason string
	// Reopen is the item filed for its failure (0: none).
	Reopen int64
	// Looks are a needs-you item's manual checks.
	Looks []string
}

// DoneReport counts the done items per verification status and lists the failed and needs-you ones.
func (s *Store) DoneReport(ctx context.Context) (DoneReport, error) {
	r := DoneReport{Counts: map[string]int{}}
	its, err := queryItems(s.r, `SELECT `+itemCols+` FROM items WHERE stage = 'done' ORDER BY id`)
	if err != nil {
		return r, err
	}
	for _, it := range its {
		v := it.Verify
		if v == "" {
			v = pipeline.DoneUnproven
		}
		r.Counts[v]++
		r.Total++
		ri := DoneReportItem{ID: it.ID, Title: it.Title, Repo: it.Repo, Reason: it.VerifyReason}
		switch v {
		case pipeline.DoneFailed:
			if err := s.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM items WHERE reopen_of = ?`, it.ID).Scan(&ri.Reopen); err != nil {
				return r, err
			}
			r.Failed = append(r.Failed, ri)
		case pipeline.DoneNeedsYou:
			ri.Looks = it.Proof.Looks()
			r.NeedsYou = append(r.NeedsYou, ri)
		}
	}
	return r, nil
}

// Line is the report's counts in one line, in VerifyStatuses' order: "134 done: 12 verified, …".
func (r DoneReport) Line() string {
	parts := []string{}
	for _, st := range pipeline.VerifyStatuses {
		if n := r.Counts[st]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+st)
		}
	}
	if len(parts) == 0 {
		return strconv.Itoa(r.Total) + " done"
	}
	return strconv.Itoa(r.Total) + " done: " + strings.Join(parts, ", ")
}

// reopenNotReproducible is a reopen closed not reproducible (#415): the failed done item it was
// filed for goes back to unproven and the done sweep asks for a new proof, unless a newer reopen of
// it is still open or this one was a duplicate (ApplyReopenNotReproducible). The event is the
// original item's.
func (s *Store) reopenNotReproducible(tx *sql.Tx, actor string, reopen pipeline.Item, now time.Time) error {
	var newest int64
	if err := tx.QueryRow(`SELECT MAX(id) FROM items WHERE reopen_of = ?`, reopen.ReopenOf).Scan(&newest); err != nil {
		return err
	}
	if newest != reopen.ID {
		return nil
	}
	orig, err := loadItem(tx, reopen.ReopenOf)
	if err != nil {
		return err
	}
	tr, err := pipeline.ApplyReopenNotReproducible(orig, reopen.ID, reopen.Summary)
	if err != nil {
		return nil // not a failed done item, or a duplicate: the failure stands
	}
	tr.Item.UpdatedAt = now
	if err := saveItem(tx, tr.Item); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET verify_asked = 0 WHERE id = ?`, orig.ID); err != nil {
		return err
	}
	return s.event(tx, transitionEvent(orig.ID, actor, tr))
}
