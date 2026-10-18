package pipeline

import (
	"math"
	"slices"
	"strconv"
	"time"
)

// The flow panel's rules (docs/decisions/2026-10-18-flow-panel.md): why an item waits right now,
// and the percentiles of a set of durations.

// Why an item waits (WaitReason). A limit reads "limit <key>:<n>" (LimitReason).
const (
	WaitDeps     = "dependencies"
	WaitQA       = "qa"
	WaitNeedsYou = "needs you"
	WaitHeld     = "held for approval"
	WaitNoRunner = "no runner for a resource"
	WaitStopped  = "stopped"
	WaitNoSlot   = "no free slot"
	WaitFrozen   = "frozen repo"
	// WaitReady: nothing holds it; it goes to the next runner that asks for its stage.
	WaitReady = "ready"
)

// LimitReason is the wait reason of an item held by the limit on key.
func LimitReason(key string, n int) string { return "limit " + key + ":" + strconv.Itoa(n) }

// usage is what the live leases hold: the items claimed, and how many runs use each stage and
// resource.
func usage(live []LiveLease) (claimed map[int64]bool, inUse map[string]int) {
	claimed, inUse = map[int64]bool{}, map[string]int{}
	for _, l := range live {
		claimed[l.ItemID] = true
		inUse[string(l.Stage)]++
		for _, r := range l.Resources {
			inUse[r]++
		}
	}
	return claimed, inUse
}

// loopBlocks is why no item can be claimed at all ("" when one can): Choose's control rule. With
// every agent slot busy, land is still claimed (AgentLeases); WaitReason checks that apart.
func loopBlocks(c Control) string {
	switch {
	case c.Stopped:
		return WaitStopped
	case c.ActiveAgents <= 0:
		return WaitNoSlot
	}
	return ""
}

// itemBlocks is what holds a waiting work item back whichever runner asks ("" for nothing), in
// Choose's order: its dependencies, then the limits of the resources it needs (Needs: land needs
// none), then its stage's (unless stageLimits is false: a frozen repo's fix is exempt from them).
func itemBlocks(c Control, it Item, deps map[int64]Stage, inUse map[string]int, stageLimits bool) string {
	if len(WaitingOn(it, deps)) > 0 {
		return WaitDeps
	}
	keys := slices.Clone(it.Needs())
	if stageLimits {
		keys = append(keys, string(it.Stage))
	}
	for _, key := range keys {
		if limit, ok := c.Limits[key]; ok && inUse[key] >= limit {
			return LimitReason(key, limit)
		}
	}
	return ""
}

// WaitReason is why it waits right now ("" when it does not: claimed, done or closed). deps are the
// stages of the items it depends on, live the running leases (turns included, as Claim counts
// them), unservable the resources no runner offers it (Unservable, nil when none), frozen the
// repos whose main is red, holds the slots held (SlotHolds). An item's own blockers (dependencies,
// no runner) come before its repo's (frozen) and the loop's (stopped, no free slot): once those
// free up it would still wait for them. The limits come next: they change with every run. Last,
// the free slots all held for work it does not need is no free slot, as ChooseWork applies them.
// Land takes no agent slot, so neither busy nor held slots hold it back. A frozen repo's fix waits only for a stop or 0 active agents and the resource limits, as
// chooseFix takes it.
func WaitReason(c Control, it Item, deps map[int64]Stage, live []LiveLease, unservable []string, frozen Frozen, holds []SlotHold) string {
	switch it.Stage {
	case NeedsYou:
		return WaitNeedsYou
	case Held:
		return WaitHeld
	case QA:
		return WaitQA
	}
	claimed, inUse := usage(live)
	if !it.Stage.IsWork() || claimed[it.ID] {
		return ""
	}
	if len(WaitingOn(it, deps)) > 0 {
		return WaitDeps
	}
	if len(unservable) > 0 {
		return WaitNoRunner
	}
	if frozen.Blocks(it) {
		return WaitFrozen
	}
	fix := frozen.Fixes(it)
	if r := loopBlocks(c); r != "" {
		return r
	}
	land := it.Stage == Land
	if !fix && !land && AgentLeases(live) >= c.ActiveAgents {
		return WaitNoSlot
	}
	if r := itemBlocks(c, it, deps, inUse, !fix); r != "" {
		return r
	}
	if !fix && !land && len(holds) > 0 && c.ActiveAgents-AgentLeases(live) <= len(holds) && !serves(Work{Item: &it}, holds) {
		return WaitNoSlot
	}
	return WaitReady
}

// Dist is a set of durations as the flow panel shows them: how many, the median and the 90th
// percentile, in whole seconds (nearest rank: the value at rank ⌈p·n⌉ of the sorted set).
type Dist struct {
	Count         int   `json:"count"`
	MedianSeconds int64 `json:"median_s"`
	P90Seconds    int64 `json:"p90_s"`
}

// DistOf is the Dist of ds (the zero Dist for none).
func DistOf(ds []time.Duration) Dist {
	if len(ds) == 0 {
		return Dist{}
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := func(p float64) int64 {
		i := int(math.Ceil(p*float64(len(s)))) - 1
		return int64(s[max(i, 0)] / time.Second)
	}
	return Dist{Count: len(s), MedianSeconds: rank(0.5), P90Seconds: rank(0.9)}
}
