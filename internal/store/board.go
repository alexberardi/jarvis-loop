package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// BoardItem is a card of the web board: a board row plus what the card shows beyond it.
type BoardItem struct {
	BoardRow
	Question     string
	HoldReason   string
	SlackChannel string
	SlackTS      string
	UpdatedAt    time.Time
	// WaitingOn is the items it depends on that are not done yet.
	WaitingOn []int64
	// Waits is each of those followed to the item that holds it up (pipeline.WaitChains).
	Waits []pipeline.WaitChain
	// PlanID is the plan that filed it (0: none).
	PlanID int64
	// Builder is who its plan piece says should build it ("": not from a plan).
	Builder string
	// Unservable is the resources no runner offers it, when loopd flagged it (pipeline.Unservable).
	Unservable []string
	// Proof is a qa item's proof (nil: none), and AwaitingDeploy whether it waits for its deploy:
	// the QA column groups by them (pipeline.QAGroup).
	Proof          *pipeline.Proof
	AwaitingDeploy bool
}

// BoardItems is every item as a board card, in ListItems' order.
func (s *Store) BoardItems(ctx context.Context) ([]BoardItem, error) {
	flagged, err := unservableMissing(s.r)
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT i.id, i.title, i.priority, i.stage, i.repo, i.pr, i.created_at,
		COALESCE(l.runner || '/' || l.agent, ''), i.question, i.hold_reason, i.slack_channel, i.slack_ts, i.updated_at,
		i.depends_on, i.plan_id, i.builder, i.proof, i.awaiting_deploy FROM items i LEFT JOIN leases l ON l.item_id = i.id AND l.ended_at IS NULL
		ORDER BY i.priority, i.created_at, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.clock.Now()
	var out []BoardItem
	var deps [][]int64
	stages := map[int64]pipeline.Stage{}
	nodes := map[int64]pipeline.WaitNode{}
	for rows.Next() {
		var b BoardItem
		var created, updated int64
		var dependsOn, proof string
		if err := rows.Scan(&b.ID, &b.Title, &b.Priority, &b.Stage, &b.Repo, &b.PR, &created, &b.ClaimedBy,
			&b.Question, &b.HoldReason, &b.SlackChannel, &b.SlackTS, &updated, &dependsOn, &b.PlanID, &b.Builder, &proof, &b.AwaitingDeploy); err != nil {
			return nil, err
		}
		if proof != "" {
			b.Proof = &pipeline.Proof{}
			if err := json.Unmarshal([]byte(proof), b.Proof); err != nil {
				return nil, err
			}
		}
		d, err := splitIDs(dependsOn)
		if err != nil {
			return nil, err
		}
		deps = append(deps, d)
		stages[b.ID] = pipeline.Stage(b.Stage)
		b.Unservable = flagged[b.ID]
		nodes[b.ID] = pipeline.WaitNode{Stage: pipeline.Stage(b.Stage), Claimed: b.ClaimedBy != "", DependsOn: d, Unservable: b.Unservable}
		b.CreatedAt, b.UpdatedAt = fromNanos(created), fromNanos(updated)
		b.AgeSeconds = int64(now.Sub(b.CreatedAt) / time.Second)
		b.Display = pipeline.Display(pipeline.Stage(b.Stage), b.ClaimedBy != "")
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Every item is here, so the dependencies' stages are too, and their chains to the root.
	for i := range out {
		out[i].WaitingOn = waitingOn(pipeline.Item{Stage: pipeline.Stage(out[i].Stage), DependsOn: deps[i]}, stages)
		out[i].Waits = pipeline.WaitChains(out[i].ID, nodes)
	}
	return out, nil
}
