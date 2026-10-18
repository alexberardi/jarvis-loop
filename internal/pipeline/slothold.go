package pipeline

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Held slots (#192, docs/runbooks/mac-runner.md "Limits"): a runner with only_resources (a Mac's
// [macos]) takes only the work that needs them, so when the dev box fills the last agent slot
// that work waits even though its runner is idle. loopd holds the last free slot for such a
// resource R while (a) a runner taking only R has been seen within two of its poll intervals,
// (b) an item needing R is claimable now (by that runner's claim, in the stages it takes: waiting,
// dependencies done, not leased, limits allow) and (c) the claim at hand would not take it. Each R holds at most
// one slot; the slots held never outnumber the free ones.

// DefaultPollSeconds is the poll interval of a runner that did not say (the runner's default).
const DefaultPollSeconds = 30

// SlotHold is a free slot kept for Resource work: Runner would take ItemID now.
type SlotHold struct {
	Resource string `json:"resource"`
	Runner   string `json:"runner"`
	ItemID   int64  `json:"item_id"`
}

// Around says whether r has been seen within two of its poll intervals of now.
func (r RunnerSeen) Around(now time.Time) bool {
	poll := r.PollSeconds
	if poll <= 0 {
		poll = DefaultPollSeconds
	}
	return now.Sub(r.LastSeen) <= 2*time.Duration(poll)*time.Second
}

// SlotHolds is the slots held now, by resource. items, deps, live and frozen are as for Choose.
func SlotHolds(c Control, items []Item, deps map[int64]Stage, live []LiveLease, runners []RunnerSeen, frozen Frozen, now time.Time) []SlotHold {
	free := c.ActiveAgents - AgentLeases(live)
	if c.Stopped || free <= 0 {
		return nil
	}
	rs := slices.Clone(runners)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	byRes := map[string]SlotHold{}
	for _, r := range rs {
		if len(r.OnlyResources) == 0 || !r.Around(now) {
			continue
		}
		for _, res := range r.OnlyResources {
			if _, ok := byRes[res]; ok {
				continue
			}
			// Land takes no agent slot, so it holds none.
			stages := agentStages(r.Stages)
			if len(stages) == 0 {
				continue
			}
			req := ClaimRequest{Runner: r.Name, Capabilities: r.Capabilities, OnlyResources: []string{res}, Stages: stages}
			if it, ok := Choose(c, items, deps, live, req, frozen); ok {
				byRes[res] = SlotHold{Resource: res, Runner: r.Name, ItemID: it.ID}
			}
		}
	}
	var out []SlotHold
	for _, h := range byRes {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	if len(out) > free {
		out = out[:free]
	}
	return out
}

// HeldReason is why a claim got nothing: "slot held for macos work".
func HeldReason(holds []SlotHold) string {
	var res []string
	for _, h := range holds {
		res = append(res, h.Resource)
	}
	if len(holds) == 1 {
		return "slot held for " + res[0] + " work"
	}
	return "slots held for " + strings.Join(res, ", ") + " work"
}

// serves says whether w takes an item needing one of the held resources.
func serves(w Work, holds []SlotHold) bool {
	var res []string
	switch {
	case w.Item != nil:
		res = w.Item.Needs()
	case w.Reg != nil:
		res = w.Reg.Resources
	}
	return slices.ContainsFunc(holds, func(h SlotHold) bool { return slices.Contains(res, h.Resource) })
}

// agentStages is stages without land (none: every agent stage).
func agentStages(stages []Stage) []Stage {
	if len(stages) == 0 {
		return []Stage{Triage, Build, Review}
	}
	return slices.DeleteFunc(slices.Clone(stages), func(s Stage) bool { return s == Land })
}
