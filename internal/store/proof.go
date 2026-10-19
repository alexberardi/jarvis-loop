package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md): the runner reports its command checks of a
// due proof (kind proof_checked); loopd evaluates the usage checks on its own event log in the
// same transaction and applies pipeline.ApplyProofChecked: verified or reopened by the loop, or
// still waiting. The proof sweep asks the orchestrator, once per item, for a proof of an item in qa
// that has none; its proof action is OrchProof.

// KindProofChecked is a runner's check of a qa item's proof.
const KindProofChecked = "proof_checked"

// ProofChecked records a runner's check of a qa item's due proof: data {results: [{check, state,
// evidence}]}, one for each command check it ran. A proof that is not due (another runner checked
// it, the user already verified it) is wrong_stage and not recorded as a refusal.
func (s *Store) ProofChecked(ctx context.Context, runnerName string, id int64, data map[string]any) (Event, error) {
	var out Event
	for k := range data {
		if k != "results" {
			return out, errf(pipeline.CodeInvalidField, "proof_checked data may hold results, not %q", k)
		}
	}
	var results []pipeline.CheckResult
	if raw, ok := data["results"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &results); err != nil {
			return out, errf(pipeline.CodeInvalidField, "results must be a list of {check, state, evidence}: %v", err)
		}
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		usage := map[int]pipeline.UsageSeen{}
		if it.Proof != nil {
			since, err := proofSince(tx, id)
			if err != nil {
				return err
			}
			for i, c := range it.Proof.Checks {
				if c.Kind != pipeline.ProofUsage || c.State == pipeline.CheckPassed {
					continue
				}
				if usage[i], err = usageSeen(tx, id, c, since); err != nil {
					return err
				}
			}
		}
		tr, err := pipeline.ApplyProofChecked(it, results, usage, s.clock.Now())
		if err != nil {
			return err
		}
		tr.Data["runner"] = runnerName
		actor := "runner:" + runnerName
		switch tr.Kind {
		case "reopened":
			_, _, err := s.reopenTx(tx, pipeline.ActorProof, it, tr)
			out = transitionEvent(id, pipeline.ActorProof, tr)
			return err
		case "verified":
			actor = pipeline.ActorProof
		}
		tr.Item.UpdatedAt = s.clock.Now()
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = transitionEvent(id, actor, tr)
		if err := s.event(tx, out); err != nil {
			return err
		}
		return s.planItemSettled(tx, tr.Item)
	})
	if err != nil {
		return out, err
	}
	out.At = s.clock.Now()
	return out, nil
}

// proofSince is the id of the item's newest merge or deploy event: usage counts only after it
// (the change was not in use before).
func proofSince(tx *sql.Tx, id int64) (int64, error) {
	var since int64
	err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE item_id = ?
		AND ((kind = 'advanced' AND outcome = ?) OR kind = 'deployed')`, id, string(pipeline.Merged)).Scan(&since)
	return since, err
}

// usageSeen counts the events after since that match a usage check: its event kind, one of its
// actors (when set), and for each match key one of its values in the event's data. The item's own
// proof records (proof_set, proof_checked) are not use: a proof cannot pass on its own checks.
func usageSeen(tx *sql.Tx, id int64, c pipeline.ProofCheck, since int64) (pipeline.UsageSeen, error) {
	var u pipeline.UsageSeen
	rows, err := tx.Query(`SELECT id, actor, data FROM events WHERE kind = ? AND id > ?
		AND NOT (COALESCE(item_id, 0) = ? AND kind IN ('proof_set', ?)) ORDER BY id`, c.Event, since, id, KindProofChecked)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eid         int64
			actor, data string
		)
		if err := rows.Scan(&eid, &actor, &data); err != nil {
			return u, err
		}
		if len(c.Actor) > 0 && !slices.Contains(c.Actor, actor) {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(data), &d)
		ok := true
		for k, vs := range c.Match {
			v, has := d[k]
			if !has || !slices.Contains(vs, fmt.Sprint(v)) {
				ok = false
				break
			}
		}
		if ok {
			u.Count++
			u.Last = eid
		}
	}
	return u, rows.Err()
}

// ProofsDue are the qa items whose proof is due a check (pipeline.ProofDue), as board rows.
func (s *Store) ProofsDue(ctx context.Context) ([]BoardRow, error) {
	rows, err := s.ListItemsFilter(ctx, ListFilter{Stage: string(pipeline.QA)})
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	out := []BoardRow{}
	for _, r := range rows {
		it, err := loadItem(s.r, r.ID)
		if err != nil {
			return nil, err
		}
		if pipeline.ProofDue(it, now) {
			out = append(out, r)
		}
	}
	return out, nil
}

// OrchProof is the orchestrator writing the proof of a qa item that has none, or replacing the
// sweep's manual-only one (the proof sweep's action): checks is the JSON list of checks, as a
// build states them.
func (s *Store) OrchProof(ctx context.Context, actor string, id int64, checks json.RawMessage, reason string) (pipeline.Item, error) {
	return s.orch(ctx, actor, id, "proof", reason, func(tx *sql.Tx, it pipeline.Item, _ []Event) (pipeline.Transition, error) {
		p, err := pipeline.ParseProof(string(checks), pipeline.ProofBySweep)
		if err != nil {
			return pipeline.Transition{}, err
		}
		return pipeline.ApplySetProof(it, p)
	})
}

// proof_asked: 0 not asked, 1 asked; 2 marks a sweep proof with only manual checks written
// before E2E results could pass a check (migration 0027), to ask once more (3).
const (
	proofAskE2E   = 2
	proofAskedE2E = 3
)

// proofSweep asks the orchestrator, once per item, to write a proof for each item in qa without
// one: on the first tick after this shipped, every item already in qa (the retroactive sweep);
// later, any item that reaches qa without a build's proof. It asks once more (proof_e2e) for the
// items migration 0027 marked: their manual-only proof may give way to E2E checks (#195).
func (s *Store) proofSweep(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, proof_asked FROM items WHERE stage = 'qa'
		AND ((proof = '' AND proof_asked = 0) OR proof_asked = ?) ORDER BY id`, proofAskE2E)
	if err != nil {
		return err
	}
	type ask struct {
		id    int64
		asked int
	}
	var asks []ask
	for rows.Next() {
		var a ask
		if err := rows.Scan(&a.id, &a.asked); err != nil {
			rows.Close()
			return err
		}
		asks = append(asks, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range asks {
		reason, next := pipeline.TriggerProof, 1
		if a.asked == proofAskE2E {
			reason, next = pipeline.TriggerProofE2E, proofAskedE2E
		}
		if err := s.queueOrch(tx, pipeline.Trigger{Reason: reason, ItemID: a.id}); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET proof_asked = ? WHERE id = ?`, next, a.id); err != nil {
			return err
		}
	}
	return nil
}
