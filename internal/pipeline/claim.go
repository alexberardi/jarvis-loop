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
	// OrchTurnsPerDay caps the orchestrate turns started in a day (0: none).
	OrchTurnsPerDay int `json:"orchestrator_turns_per_day"`
	// Budgets are the usage budgets per provider (usage.go).
	Budgets map[string]Budget `json:"budgets"`
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
		Budgets:       DefaultBudgets(),

		OrchTurnsPerDay: DefaultOrchTurnsPerDay,
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
	if c.OrchTurnsPerDay < 0 || c.OrchTurnsPerDay > MaxOrchTurnsPerDay {
		return errf(CodeInvalidField, "orchestrator_turns_per_day must be 0 to %d", MaxOrchTurnsPerDay)
	}
	for k, v := range c.Limits {
		if !slices.Contains(LimitKeys, k) {
			return errf(CodeInvalidField, "no limit %q (have %s)", k, strings.Join(LimitKeys, ", "))
		}
		if v < 0 {
			return errf(CodeInvalidField, "limit %s must be 0 or more", k)
		}
	}
	for k, b := range c.Budgets {
		if !slices.Contains(BudgetProviders, k) {
			return errf(CodeInvalidField, "no budget for %q (have %s)", k, strings.Join(BudgetProviders, ", "))
		}
		if err := b.Validate(); err != nil {
			return err
		}
	}
	// A name may repeat within a repo for several globs (jarvis-loop's "land" rule covers three
	// files), but the same (repo, name, glob) twice is a mistake.
	seen := map[[3]string]bool{}
	for _, r := range c.HoldRules {
		if err := r.Validate(); err != nil {
			return err
		}
		k := [3]string{r.Repo, r.Name, r.Glob}
		if seen[k] {
			return errf(CodeInvalidField, "duplicate hold rule name %q (repo %q, glob %q)", r.Name, r.Repo, r.Glob)
		}
		seen[k] = true
	}
	return nil
}

// LiveLease is what the claim rules need to know about a lease that is still running.
type LiveLease struct {
	ItemID    int64
	Stage     Stage
	Resources []string
	// Claude: the run is claude's (everything but land and qwen builds), for the paced number.
	Claude bool
}

// ClaimRequest is a runner asking for work.
type ClaimRequest struct {
	Runner       string
	Agent        string
	Capabilities []string
	// OnlyResources, when set, limits the claim to items that need at least one of them: a Mac
	// runner with [macos] takes the macos items and leaves the rest to the dev box.
	OnlyResources []string
	// PollSeconds is how often the runner asks (0: it did not say); loopd keeps it with the
	// runner for the held slots (SlotHolds).
	PollSeconds int
	// RunnerStages are the work stages the runner takes in all its lanes (none: it did not say);
	// loopd keeps them with the runner so a slot is held only for work it would claim.
	RunnerStages []Stage
	// Stages, when set, limits the claim to these stages (a runner that only builds).
	Stages []Stage
	// Kinds is the work the runner takes: WorkItem, WorkPlan (plan turns), WorkOrchestrate.
	// Empty: items only, so a runner that predates plans never gets a turn.
	Kinds []string
	// Builders are the other builders the runner can run (BuilderQwen when it has a qwen block):
	// a paced claim may route a P3–P5 build to one (usage.go).
	Builders []string `json:"builders,omitempty"`
}

// The kinds of work a claim may return.
const (
	WorkItem = "item"
	WorkPlan = "plan"
)

// PlanTurnStage is the stage a running plan turn counts as among the live leases.
const PlanTurnStage Stage = "plan"

func (r ClaimRequest) wants(kind string) bool {
	if len(r.Kinds) == 0 {
		return kind == WorkItem
	}
	return slices.Contains(r.Kinds, kind)
}

// TurnCandidate is a queued plan turn a runner could take: its plan has no turn running.
type TurnCandidate struct {
	TurnID    int64
	PlanID    int64
	Priority  string // the plan's
	CreatedAt time.Time
}

// Work is what a claim gets: an item, a plan turn or an orchestrate turn.
type Work struct {
	Item *Item
	Turn *TurnCandidate
	Orch *OrchCandidate
	// RouteQwen: build the item with qwen this run (a paced claim, ChooseWorkPaced).
	RouteQwen bool
	// Held, with nothing to claim, is why: the slots left are held (SlotHolds).
	Held string
}

// ChooseWork picks an item (Choose), a queued plan turn or the queued orchestrate turn (orch, nil
// when there is none to start). Turns share the active-agents limit with items (live includes the
// running turns); a plan turn's priority is its plan's, and it beats an item of the same priority:
// the user is in the conversation. The orchestrate turn beats everything (it gets the next free
// slot), one at a time. The fix of a frozen repo (frozen) beats even that: it may take a slot
// over the active-agents number. holds are the slots held (SlotHolds): when they are all the free
// slots left, the claim gets only an item needing a held resource.
func ChooseWork(c Control, items []Item, deps map[int64]Stage, live []LiveLease, turns []TurnCandidate, orch *OrchCandidate, holds []SlotHold, req ClaimRequest, frozen Frozen) (Work, bool) {
	return ChooseWorkPaced(c, Pace{Level: PaceNormal, Agents: c.ActiveAgents}, items, deps, live, turns, orch, holds, req, frozen)
}

// ChooseWorkPaced is ChooseWork under a claude pace (PaceFor). Slow: claude's runs at once are at
// most pace.Agents (live counts the Claude leases), and claude takes only P1 and size S items,
// triage (short, and an item has no size before it) and P1 plan turns; paused: claude takes nothing. Either way land and qwen builds go on, and a P3–P5
// build of size S or M with no builder set goes to qwen this run when the runner has it
// (Work.RouteQwen). The active-agents number caps everything as before; a frozen repo's fix and
// the work of a held slot are paced like any item.
func ChooseWorkPaced(c Control, pace Pace, items []Item, deps map[int64]Stage, live []LiveLease, turns []TurnCandidate, orch *OrchCandidate, holds []SlotHold, req ClaimRequest, frozen Frozen) (Work, bool) {
	claudeLive := 0
	for _, l := range live {
		if l.Claude {
			claudeLive++
		}
	}
	p := pacing{pace: pace, claudeOK: pace.Level == PaceNormal || pace.Level == PaceSlow && claudeLive < pace.Agents}
	p.route = func(it Item) bool {
		return pace.Level != PaceNormal && routable(it) && slices.Contains(req.Builders, BuilderQwen)
	}
	p.eligible = func(it Item) bool {
		if !UsesClaude(it) || p.route(it) {
			return true
		}
		return p.claudeOK && (pace.Level == PaceNormal || it.Priority == "P1" || it.Size == "S" || it.Stage == Triage)
	}
	if req.wants(WorkItem) {
		if it, ok := chooseFix(c, items, deps, live, req, frozen, p.eligible); ok {
			return Work{Item: &it, RouteQwen: p.route(it)}, true
		}
	}
	if c.Stopped || c.ActiveAgents <= 0 || len(live) >= c.ActiveAgents {
		return Work{}, false
	}
	w, ok := chooseWork(c, p, items, deps, live, turns, orch, req, frozen)
	if len(holds) == 0 || c.ActiveAgents-len(live) > len(holds) || (ok && serves(w, holds)) {
		return w, ok
	}
	// The slots left are held: only held work, if this runner can take some.
	if req.wants(WorkItem) {
		held := req
		held.OnlyResources = nil
		for _, h := range holds {
			if len(req.OnlyResources) == 0 || slices.Contains(req.OnlyResources, h.Resource) {
				held.OnlyResources = append(held.OnlyResources, h.Resource)
			}
		}
		if len(held.OnlyResources) > 0 {
			if it, ok := choosePaced(c, items, deps, live, held, frozen, p.eligible); ok {
				return Work{Item: &it, RouteQwen: p.route(it)}, true
			}
		}
	}
	return Work{Held: HeldReason(holds)}, false
}

// pacing is a claim's pace (ChooseWorkPaced): whether claude may take another run, which items it
// may take, and which builds go to qwen this run.
type pacing struct {
	pace            Pace
	claudeOK        bool
	eligible, route func(Item) bool
}

func chooseWork(c Control, p pacing, items []Item, deps map[int64]Stage, live []LiveLease, turns []TurnCandidate, orch *OrchCandidate, req ClaimRequest, frozen Frozen) (Work, bool) {
	if orch != nil && p.claudeOK && req.wants(WorkOrchestrate) && !slices.ContainsFunc(live, func(l LiveLease) bool { return l.Stage == OrchestrateStage }) {
		return Work{Orch: orch}, true
	}
	var it Item
	okItem := false
	if req.wants(WorkItem) {
		it, okItem = choosePaced(c, items, deps, live, req, frozen, p.eligible)
	}
	var best *TurnCandidate
	if req.wants(WorkPlan) && p.claudeOK {
		for i := range turns {
			t := &turns[i]
			if p.pace.Level != PaceNormal && t.Priority != "P1" {
				continue
			}
			if best == nil || turnBefore(*t, *best) {
				best = t
			}
		}
	}
	switch {
	case best != nil && (!okItem || PriorityRank(best.Priority) <= PriorityRank(it.Priority)):
		return Work{Turn: best}, true
	case okItem:
		return Work{Item: &it, RouteQwen: p.route(it)}, true
	}
	return Work{}, false
}

// UsesClaude reports whether an item's next run is claude's: every stage but land, and a build
// unless the item's builder is qwen.
func UsesClaude(it Item) bool {
	return it.Stage != Land && !(it.Stage == Build && it.Builder == BuilderQwen)
}

// routable: a build a paced claim may send to qwen: P3–P5, size S or M (it fits Qwen's context and
// pace), and no builder set (a plan's "builder: claude" or a qwen item's fallback to claude stays).
func routable(it Item) bool {
	return it.Stage == Build && it.Builder == "" && PriorityRank(it.Priority) >= 3 && PriorityRank(it.Priority) <= 5 &&
		(it.Size == "S" || it.Size == "M")
}

func turnBefore(a, b TurnCandidate) bool {
	if pa, pb := PriorityRank(a.Priority), PriorityRank(b.Priority); pa != pb {
		return pa < pb
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.TurnID < b.TurnID
}

var stageOrder = map[Stage]int{Land: 0, Review: 1, Build: 2, Triage: 3}

// Choose picks the item a runner should get, or false when there is nothing for it. items are the
// candidates (callers pass the waiting items; anything claimed or not in a work stage is skipped
// here too); deps are the stages of the items they depend on; live are the leases still running;
// frozen are the repos whose main is red: their items are skipped, except each one's fix, which
// goes first (chooseFix).
func Choose(c Control, items []Item, deps map[int64]Stage, live []LiveLease, req ClaimRequest, frozen Frozen) (Item, bool) {
	return choosePaced(c, items, deps, live, req, frozen, nil)
}

// choosePaced is Choose with one more filter (nil: none), the pace's.
func choosePaced(c Control, items []Item, deps map[int64]Stage, live []LiveLease, req ClaimRequest, frozen Frozen, eligible func(Item) bool) (Item, bool) {
	if it, ok := chooseFix(c, items, deps, live, req, frozen, eligible); ok {
		return it, true
	}
	// 1. Control.
	if c.Stopped || c.ActiveAgents <= 0 || len(live) >= c.ActiveAgents {
		return Item{}, false
	}
	return choose(c, items, deps, live, req, func(it Item) bool { return !frozen.Blocks(it) && (eligible == nil || eligible(it)) }, true)
}

// chooseFix is the main-red item of a frozen repo, when it waits and this runner may take it: it
// goes before everything, and may take a slot over the active-agents number and its stage's limit
// (the frozen repo's running work holds slots it cannot give back); a stop, 0 active agents, the
// resource limits and review independence still apply.
func chooseFix(c Control, items []Item, deps map[int64]Stage, live []LiveLease, req ClaimRequest, frozen Frozen, eligible func(Item) bool) (Item, bool) {
	if len(frozen) == 0 || c.Stopped || c.ActiveAgents <= 0 {
		return Item{}, false
	}
	return choose(c, items, deps, live, req, func(it Item) bool { return frozen.Fixes(it) && (eligible == nil || eligible(it)) }, false)
}

func choose(c Control, items []Item, deps map[int64]Stage, live []LiveLease, req ClaimRequest, pick func(Item) bool, stageLimits bool) (Item, bool) {
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
		if !pick(*it) {
			continue
		}
		// 2. Eligible: waiting, in a work stage, resources ⊆ capabilities (and, with OnlyResources,
		// needing one of them).
		if !it.Stage.IsWork() || claimed[it.ID] {
			continue
		}
		if len(req.Stages) > 0 && !slices.Contains(req.Stages, it.Stage) {
			continue
		}
		if !subset(it.Resources, req.Capabilities) {
			continue
		}
		if len(req.OnlyResources) > 0 && !slices.ContainsFunc(it.Resources, func(r string) bool { return slices.Contains(req.OnlyResources, r) }) {
			continue
		}
		// Dependencies: every item it depends on is merged (qa or done).
		if len(WaitingOn(*it, deps)) > 0 {
			continue
		}
		// 3. Per-resource limits.
		if slices.ContainsFunc(it.Resources, atLimit) {
			continue
		}
		// 4. Per-stage limits.
		if stageLimits && atLimit(string(it.Stage)) {
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
