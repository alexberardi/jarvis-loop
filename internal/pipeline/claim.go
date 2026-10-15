package pipeline

import (
	"slices"
	"strings"
	"time"
)

// MaxActiveAgents is the top of the active-agents range (0–6).
const MaxActiveAgents = 6

// Control is the user's throttle and the limits.
type Control struct {
	ActiveAgents  int            `json:"active_agents"`
	Stopped       bool           `json:"stopped"`
	MaxRunMinutes int            `json:"max_run_minutes"`
	Limits        map[string]int `json:"limits"`
	HoldRules     []HoldRule     `json:"hold_rules"`
}

// LimitKeys are the resources and stages that can carry a limit. Build has none: it is bounded
// only by the active-agents number.
var LimitKeys = []string{"gpu", "voice", "emulator", "macos", "triage", "review", "land"}

// DefaultControl is the control row of a new install.
func DefaultControl() Control {
	return Control{
		ActiveAgents:  4,
		MaxRunMinutes: 45,
		Limits:        map[string]int{"gpu": 2, "voice": 1, "emulator": 1, "triage": 1, "review": 2, "land": 1},
		HoldRules:     DefaultHoldRules(),
	}
}

// MaxRun is the per-run time cap.
func (c Control) MaxRun() time.Duration { return time.Duration(c.MaxRunMinutes) * time.Minute }

// Validate checks the ranges.
func (c Control) Validate() error {
	if c.ActiveAgents < 0 || c.ActiveAgents > MaxActiveAgents {
		return errf(CodeInvalidField, "active_agents must be 0 to %d", MaxActiveAgents)
	}
	if c.MaxRunMinutes < 1 || c.MaxRunMinutes > 24*60 {
		return errf(CodeInvalidField, "max_run_minutes must be 1 to 1440")
	}
	for k, v := range c.Limits {
		if !slices.Contains(LimitKeys, k) {
			return errf(CodeInvalidField, "no limit %q (have %s)", k, strings.Join(LimitKeys, ", "))
		}
		if v < 0 {
			return errf(CodeInvalidField, "limit %s must be 0 or more", k)
		}
	}
	names := map[string]bool{}
	for _, r := range c.HoldRules {
		if err := r.Validate(); err != nil {
			return err
		}
		if names[r.Name] {
			return errf(CodeInvalidField, "duplicate hold rule name %q", r.Name)
		}
		names[r.Name] = true
	}
	return nil
}

// LiveLease is what the claim rules need to know about a lease that is still running.
type LiveLease struct {
	ItemID    int64
	Stage     Stage
	Resources []string
}

// ClaimRequest is a runner asking for work.
type ClaimRequest struct {
	Runner       string
	Agent        string
	Capabilities []string
}

var stageOrder = map[Stage]int{Land: 0, Review: 1, Build: 2, Triage: 3}

// Choose picks the item a runner should get, or false when there is nothing for it. items are the
// candidates (callers pass the waiting items; anything claimed or not in a work stage is skipped
// here too); live are the leases still running.
func Choose(c Control, items []Item, live []LiveLease, req ClaimRequest) (Item, bool) {
	// 1. Control.
	if c.Stopped || c.ActiveAgents <= 0 || len(live) >= c.ActiveAgents {
		return Item{}, false
	}
	claimed := map[int64]bool{}
	inUse := map[string]int{}
	for _, l := range live {
		claimed[l.ItemID] = true
		inUse[string(l.Stage)]++
		for _, r := range l.Resources {
			inUse[r]++
		}
	}
	atLimit := func(key string) bool {
		limit, ok := c.Limits[key]
		return ok && inUse[key] >= limit
	}

	var best *Item
	for i := range items {
		it := &items[i]
		// 2. Eligible: waiting, in a work stage, resources ⊆ capabilities.
		if !it.Stage.IsWork() || claimed[it.ID] {
			continue
		}
		if !subset(it.Resources, req.Capabilities) {
			continue
		}
		// 3. Per-resource limits.
		if slices.ContainsFunc(it.Resources, atLimit) {
			continue
		}
		// 4. Per-stage limits.
		if atLimit(string(it.Stage)) {
			continue
		}
		// 5. Review independence.
		if it.Stage == Review && it.BuilderAgent != "" && it.BuilderAgent == req.Agent {
			continue
		}
		if best == nil || before(*it, *best) {
			best = it
		}
	}
	if best == nil {
		return Item{}, false
	}
	return *best, true
}

// before is the claim order: priority, then stage (finish work in flight first), then oldest, then id.
func before(a, b Item) bool {
	if pa, pb := PriorityRank(a.Priority), PriorityRank(b.Priority); pa != pb {
		return pa < pb
	}
	if sa, sb := stageOrder[a.Stage], stageOrder[b.Stage]; sa != sb {
		return sa < sb
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func subset(need, have []string) bool {
	for _, r := range need {
		if !slices.Contains(have, r) {
			return false
		}
	}
	return true
}

// LeaseVerdict says what to do with a lease at a given time.
type LeaseVerdict int

// The verdicts.
const (
	LeaseLive LeaseVerdict = iota
	// LeaseExpired: no heartbeat within the TTL; the item returns to waiting in its stage.
	LeaseExpired
	// LeaseOvertime: past the max run time; the item goes to needs_you.
	LeaseOvertime
)

func (v LeaseVerdict) String() string {
	return [...]string{"live", "expired", "overtime"}[v]
}

// JudgeLease decides a lease's fate. Overtime wins over expiry: a run that went too long is
// something the user should hear about either way.
func JudgeLease(claimedAt, expiresAt, now time.Time, maxRun time.Duration) LeaseVerdict {
	if !now.Before(claimedAt.Add(maxRun)) {
		return LeaseOvertime
	}
	if !now.Before(expiresAt) {
		return LeaseExpired
	}
	return LeaseLive
}
