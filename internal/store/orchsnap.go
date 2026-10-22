package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The orchestrator's snapshot (docs/phases/08-orchestrator.md, "What each turn sees"): the board as
// data, the items that triggered the turn in full, its notebook, its open escalations and the
// user's replies since its last turn.

// OrchSnapItem is an open item as the orchestrator sees it.
type OrchSnapItem struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Repo        string  `json:"repo,omitempty"`
	Priority    string  `json:"priority"`
	Stage       string  `json:"stage"`
	Leased      bool    `json:"leased,omitempty"`
	DependsOn   []int64 `json:"depends_on,omitempty"`
	WaitingOn   []int64 `json:"waiting_on,omitempty"`
	BounceCount int     `json:"bounce_count,omitempty"`
	// AgeInStage is how long since the item last changed (seconds).
	AgeInStage int64  `json:"age_in_stage_seconds"`
	Question   string `json:"question,omitempty"`
	HoldReason string `json:"hold_reason,omitempty"`
	PR         int    `json:"pr,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	// Resources are what it needs from a runner (its capabilities).
	Resources []string `json:"resources,omitempty"`
	// Unservable is the resources no runner offers it, when loopd flagged it (it told the user).
	Unservable []string `json:"unservable,omitempty"`
	// ReopenedDeps is its dependencies that failed the user's check, while the fix is open.
	ReopenedDeps []ReopenedDependency `json:"reopened_deps,omitempty"`
}

// OrchSnapRunner is a runner's health.
type OrchSnapRunner struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	// Repos are the names of the repos it has a checkout of, as it last claimed (none: it did not
	// say, so any); it is given only items on them (#467).
	Repos     []string  `json:"repos,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
	LastClaim time.Time `json:"last_claim,omitempty"`
	// State is "draining" while the runner claims nothing new before a restart ("" working).
	State string `json:"state,omitempty"`
}

// OrchSnapTriggered is an item that triggered the turn, in full.
type OrchSnapTriggered struct {
	Item     pipeline.Item `json:"item"`
	Events   []Event       `json:"events"` // the newest OrchSnapEvents
	Evidence []Evidence    `json:"evidence"`
}

// OrchSnapshot is what an orchestrate turn sees.
type OrchSnapshot struct {
	At           time.Time           `json:"at"`
	Day          string              `json:"day"`
	TurnsToday   int                 `json:"turns_today"`
	TurnsPerDay  int                 `json:"turns_per_day"`
	ActiveAgents int                 `json:"active_agents"`
	Stopped      bool                `json:"stopped"`
	Turn         *OrchTurn           `json:"turn,omitempty"`      // the running turn: its triggers
	LastTurn     *OrchTurn           `json:"last_turn,omitempty"` // the one before: how its actions went
	Items        []OrchSnapItem      `json:"items"`
	Working      []Working           `json:"working"`
	Runners      []OrchSnapRunner    `json:"runners"`
	Triggered    []OrchSnapTriggered `json:"triggered"`
	Notebook     []Event             `json:"notebook"`
	Escalations  []Event             `json:"escalations"`
	Replies      []Event             `json:"replies"`
	// Runway is how much the loop can do without the user, and what of theirs frees the most
	// (#205): what it answers "runway" with.
	Runway RunwayReport `json:"runway"`
}

// Snapshot sizes.
const (
	OrchSnapEvents = 30
	OrchSnapNotes  = 30
)

// OrchSnapshot is the board for the running orchestrate turn (or, with none, for the next).
func (s *Store) OrchSnapshot(ctx context.Context) (OrchSnapshot, error) {
	now := s.clock.Now()
	snap := OrchSnapshot{At: now, Day: s.orchDay(now), Items: []OrchSnapItem{}, Runners: []OrchSnapRunner{},
		Triggered: []OrchSnapTriggered{}, Escalations: []Event{}, Replies: []Event{}}
	ctl, err := s.loadControl(s.r)
	if err != nil {
		return snap, err
	}
	snap.TurnsPerDay, snap.ActiveAgents, snap.Stopped = ctl.OrchTurnsPerDay, ctl.ActiveAgents, ctl.Stopped
	if snap.Runway, _, err = s.runway(s.r); err != nil {
		return snap, err
	}
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM orch_turns WHERE day = ? AND status != ?`, snap.Day, OrchQueued).
		Scan(&snap.TurnsToday); err != nil {
		return snap, err
	}
	if snap.Turn, err = orchTurnWith(s.r, OrchRunning); err != nil {
		return snap, err
	}
	q := `SELECT ` + orchTurnCols + ` FROM orch_turns WHERE status IN ('done','failed')`
	args := []any{}
	if snap.Turn != nil {
		q += ` AND id < ?`
		args = append(args, snap.Turn.ID)
	}
	last, err := scanOrchTurn(s.r.QueryRowContext(ctx, q+` ORDER BY id DESC LIMIT 1`, args...))
	switch {
	case err == nil:
		snap.LastTurn = &last
	case !errors.Is(err, sql.ErrNoRows):
		return snap, err
	}

	// The open items.
	rows, err := s.r.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE stage NOT IN ('done','closed') ORDER BY priority, created_at, id`)
	if err != nil {
		return snap, err
	}
	var open []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return snap, err
		}
		open = append(open, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	deps, err := dependencyStages(s.r, open)
	if err != nil {
		return snap, err
	}
	leased := map[int64]bool{}
	if snap.Working, err = s.Working(ctx, 0); err != nil {
		return snap, err
	}
	for _, w := range snap.Working {
		leased[w.ItemID] = true
	}
	flagged, err := unservableMissing(s.r)
	if err != nil {
		return snap, err
	}
	reopened, err := reopenedDependencies(s.r)
	if err != nil {
		return snap, err
	}
	for _, it := range open {
		snap.Items = append(snap.Items, OrchSnapItem{ID: it.ID, Title: it.Title, Repo: it.Repo, Priority: it.Priority, Stage: string(it.Stage),
			Leased: leased[it.ID], DependsOn: it.DependsOn, WaitingOn: waitingOn(it, deps), BounceCount: it.BounceCount,
			AgeInStage: int64(now.Sub(it.UpdatedAt) / time.Second), Question: it.Question, HoldReason: it.HoldReason, PR: it.PR, HeadSHA: it.HeadSHA, Resources: it.Resources,
			Unservable: flagged[it.ID], ReopenedDeps: reopened[it.ID]})
	}

	// The runners.
	rrows, err := s.r.QueryContext(ctx, `SELECT r.name, r.capabilities, r.repos, r.last_seen, r.state,
		COALESCE((SELECT MAX(e.at) FROM events e WHERE e.actor = 'runner:' || r.name AND e.kind IN ('claimed','plan_turn_claimed','orchestrate_claimed')), 0)
		FROM runners r ORDER BY r.name`)
	if err != nil {
		return snap, err
	}
	for rrows.Next() {
		var r OrchSnapRunner
		var caps, repos string
		var seen, claim int64
		if err := rrows.Scan(&r.Name, &caps, &repos, &seen, &r.State, &claim); err != nil {
			rrows.Close()
			return snap, err
		}
		r.Capabilities, r.Repos, r.LastSeen = splitList(caps), splitList(repos), fromNanos(seen)
		if claim != 0 {
			r.LastClaim = fromNanos(claim)
		}
		snap.Runners = append(snap.Runners, r)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return snap, err
	}

	// The items that triggered the turn.
	if snap.Turn != nil {
		seen := map[int64]bool{}
		for _, tr := range snap.Turn.Triggers {
			if tr.ItemID == 0 || seen[tr.ItemID] {
				continue
			}
			seen[tr.ItemID] = true
			d, err := s.GetItem(ctx, tr.ItemID)
			var pe *pipeline.Error
			if errors.As(err, &pe) && pe.Code == CodeNotFound {
				continue
			}
			if err != nil {
				return snap, err
			}
			evs := d.Events
			if len(evs) > OrchSnapEvents {
				evs = evs[len(evs)-OrchSnapEvents:]
			}
			snap.Triggered = append(snap.Triggered, OrchSnapTriggered{Item: d.Item, Events: evs, Evidence: d.Evidence})
		}
	}

	if snap.Notebook, err = s.Notebook(ctx, OrchSnapNotes); err != nil {
		return snap, err
	}
	// Open escalations: its (and loopd's on its behalf) on items still in the stage they were in.
	erows, err := s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events e WHERE e.kind = ? AND e.at >= ?
		AND (e.item_id IS NULL OR NOT EXISTS (SELECT 1 FROM events l WHERE l.item_id = e.item_id AND l.id > e.id AND l.to_stage != l.from_stage))
		ORDER BY e.id`, KindOrchEscalated, now.Add(-48*time.Hour).UnixNano())
	if err != nil {
		return snap, err
	}
	if snap.Escalations, err = scanEvents(erows); err != nil {
		return snap, err
	}
	// The user's replies (their own actions, in Slack or the CLI) since its last turn started.
	var since int64
	if snap.LastTurn != nil && snap.LastTurn.ClaimedAt != nil {
		since = snap.LastTurn.ClaimedAt.UnixNano()
	} else {
		since = now.Add(-24 * time.Hour).UnixNano()
	}
	urows, err := s.r.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE at > ? AND (actor LIKE 'user:%' OR actor LIKE 'slack:%')
		ORDER BY id DESC LIMIT 50`, since)
	if err != nil {
		return snap, err
	}
	replies, err := scanEvents(urows)
	if err != nil {
		return snap, err
	}
	for i := len(replies) - 1; i >= 0; i-- {
		if !strings.HasPrefix(replies[i].Kind, "orchestrat") {
			snap.Replies = append(snap.Replies, replies[i])
		}
	}
	return snap, nil
}
