package pipeline

import (
	"cmp"
	"slices"
	"strconv"
	"time"
)

// Runway (#205): how much work the loop can do without the user, and what of the user's would free
// the most. Runnable work is the items in a work stage (triage, build, review, land), claimed or
// not, whose dependencies are merged or will be by other runnable work not on the hold list; an unclaimed item no runner
// can serve is not. The rest waits on the user: held items, needs_you (a question or a trusted-file
// OK), and whatever depends on them. A qa item does not block: dependencies are met at merge
// (DependencyMet).

// RunwayNode is what the runway needs to know of an item.
type RunwayNode struct {
	Stage     Stage
	Claimed   bool
	DependsOn []int64
	Priority  string
	// Unservable: unclaimed, and no runner offers the resources it needs.
	Unservable bool
	// Trust: a needs_you item waiting on the approver's trusted-file OK.
	Trust bool
	// Hold: on the hold list (risk hold) with no approval for its current head. Its own work stages
	// are runnable, but it stops at land for the user, so it does not meet a dependency.
	Hold bool
}

// What the user does to unblock an item.
const (
	UnblockApprove = "approve" // a held merge
	UnblockTrust   = "trust"   // a trusted-file OK
	UnblockAnswer  = "answer"  // a question
)

// Unblocker is one thing the user can do, and how many items it would make runnable (the item
// itself included).
type Unblocker struct {
	ItemID   int64
	Action   string
	Frees    int
	Priority string
}

// Runway is the work the loop can do without the user.
type Runway struct {
	// Hours is the runnable work in agent-hours: each item's remaining work stages at the recent
	// per-stage durations.
	Hours float64
	// Runnable is the runnable items, by id.
	Runnable []int64
	// Unblockers is the user's waiting items, most freed first, then priority, then id.
	Unblockers []Unblocker
}

// DefaultStageDurations is a stage's run time when there is no recent run to measure.
var DefaultStageDurations = map[Stage]time.Duration{
	Triage: 10 * time.Minute,
	Build:  45 * time.Minute,
	Review: 15 * time.Minute,
	Land:   10 * time.Minute,
}

// runwayStages are the work stages in order: an item in one still has it and the ones after.
var runwayStages = []Stage{Triage, Build, Review, Land}

// ComputeRunway is the runway of items (every item a dependency names should be in it; one missing
// counts as not merged); a held-risk item in flight (Hold) is runnable itself but meets no
// dependency until it merges. durations is each work stage's recent run time; a stage missing from it
// takes DefaultStageDurations'.
func ComputeRunway(items map[int64]RunwayNode, durations map[Stage]time.Duration) Runway {
	var r Runway
	runnable := runnableSet(items, 0)
	for id := range runnable {
		r.Runnable = append(r.Runnable, id)
		i := slices.Index(runwayStages, items[id].Stage)
		for _, st := range runwayStages[i:] {
			d, ok := durations[st]
			if !ok {
				d = DefaultStageDurations[st]
			}
			r.Hours += d.Hours()
		}
	}
	slices.Sort(r.Runnable)
	for id, n := range items {
		var action string
		switch {
		case n.Stage == Held:
			action = UnblockApprove
		case n.Stage == NeedsYou && n.Trust:
			action = UnblockTrust
		case n.Stage == NeedsYou:
			action = UnblockAnswer
		default:
			continue
		}
		freed := runnableSet(items, id)
		r.Unblockers = append(r.Unblockers, Unblocker{ItemID: id, Action: action, Frees: len(freed) - len(runnable), Priority: n.Priority})
	}
	slices.SortFunc(r.Unblockers, func(a, b Unblocker) int {
		return cmp.Or(cmp.Compare(b.Frees, a.Frees), cmp.Compare(a.Priority, b.Priority), cmp.Compare(a.ItemID, b.ItemID))
	})
	return r
}

// runnableSet is the runnable items, with item unblocked (0: none) counted as back at work (and
// approved, so it meets its dependents' dependency): grown
// until nothing more joins, so a cycle never does.
func runnableSet(items map[int64]RunwayNode, unblocked int64) map[int64]bool {
	in := map[int64]bool{}
	for changed := true; changed; {
		changed = false
		for id, n := range items {
			if in[id] || !(id == unblocked || slices.Contains(runwayStages, n.Stage) && !n.Unservable) {
				continue
			}
			ready := true
			for _, d := range n.DependsOn {
				dn, ok := items[d]
				if !ok || !DependencyMet(dn.Stage) && (!in[d] || dn.Hold && d != unblocked) {
					ready = false
					break
				}
			}
			if ready {
				in[id], changed = true, true
			}
		}
	}
	return in
}

// RunwayAwayAfter is how long without a user action counts as away; RunwayNightHour the local hour
// from which the user counts as away anyway.
const (
	RunwayAwayAfter = 30 * time.Minute
	RunwayNightHour = 22
)

// DefaultRunwayWarnHours is the runway (agent-hours) below which loopd warns an away user.
const DefaultRunwayWarnHours = 3

// MaxRunwayWarnHours is the most the control accepts.
const MaxRunwayWarnHours = 48

// UserAway says the user is likely away: no action of theirs for RunwayAwayAfter (or none ever),
// or it is past RunwayNightHour in loc.
func UserAway(lastAction, now time.Time, loc *time.Location) bool {
	return lastAction.IsZero() || now.Sub(lastAction) >= RunwayAwayAfter || now.In(loc).Hour() >= RunwayNightHour
}

// RunwayLowStep is how far (agent-hours) below the last warning the runway must fall to be a new
// low worth another warning.
const RunwayLowStep = 1.0

// RunwayWarned is what the warnings of the current dry spell said (#227), kept between ticks:
// zero when there is no dry spell.
type RunwayWarned struct {
	// Warned: a warning went out in this dry spell.
	Warned bool `json:"warned,omitempty"`
	// Hours is the lowest runway a warning of this spell reported.
	Hours float64 `json:"hours,omitempty"`
	// Unblockers is what the warnings named and is still waiting, as "item:action", sorted.
	Unblockers []string `json:"unblockers,omitempty"`
	// OKSince is when the runway got back to the threshold, zero while it is below; the spell ends
	// once it has stayed there for RunwayAwayAfter.
	OKSince time.Time `json:"ok_since,omitzero"`
}

func unblockerKeys(us []Unblocker) []string {
	var keys []string
	for _, u := range us {
		keys = append(keys, strconv.FormatInt(u.ItemID, 10)+":"+u.Action)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// RunwayWarn decides the dry-spell warning: one per real change, not one per tick (#227). It
// sends when the runway is below threshold hours (0: off), the user is away and there is something
// they can unblock, and either no warning went out in this dry spell, the runway is a new low
// (RunwayLowStep below the last warning's), or an unblocker is one no warning of the spell named
// (by item and action; their order is not news). The spell (next) outlives a blip: it ends only
// once the runway has stayed at the threshold for RunwayAwayAfter, or the warning is turned off.
// An unblocker that is no longer waiting is forgotten, so it is news if it comes back. The caller passes
// the unblockers the warning would name, and does not call it while the loop is paused.
func RunwayWarn(r Runway, threshold int, away bool, last RunwayWarned, now time.Time) (send bool, next RunwayWarned) {
	if threshold <= 0 || !last.Warned && r.Hours >= float64(threshold) {
		return false, RunwayWarned{}
	}
	keys := unblockerKeys(r.Unblockers)
	var fresh []string
	if last.Warned {
		var kept []string
		for _, k := range keys {
			if slices.Contains(last.Unblockers, k) {
				kept = append(kept, k)
			} else {
				fresh = append(fresh, k)
			}
		}
		last.Unblockers = kept
	}
	if r.Hours >= float64(threshold) {
		if last.OKSince.IsZero() {
			last.OKSince = now
		}
		if now.Sub(last.OKSince) >= RunwayAwayAfter {
			return false, RunwayWarned{}
		}
		return false, last
	}
	last.OKSince = time.Time{}
	if !last.Warned {
		if !away || len(keys) == 0 {
			return false, RunwayWarned{}
		}
		return true, RunwayWarned{Warned: true, Hours: r.Hours, Unblockers: keys}
	}
	newLow := r.Hours <= last.Hours-RunwayLowStep+1e-9
	if !away || len(keys) == 0 || !newLow && len(fresh) == 0 {
		return false, last
	}
	return true, RunwayWarned{Warned: true, Hours: min(r.Hours, last.Hours), Unblockers: keys}
}
