package pipeline

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Unservable items (docs/decisions/2026-10-17-unservable-items.md): an item whose resources no
// runner offers waits forever, as no claim can take it (claim.go: resources ⊆ capabilities).
// loopd flags it at once instead of letting it idle until the orchestrator notices.

// RunnerSeenWindow is how recently a runner must have been seen for its capabilities to count.
const RunnerSeenWindow = 24 * time.Hour

// RunnerSeen is a runner as loopd last saw it (its last claim or heartbeat).
type RunnerSeen struct {
	Name         string
	Capabilities []string
	// OnlyResources, PollSeconds and Stages are as it last claimed (slot holds, slothold.go);
	// Stages are its work stages in all lanes (none: it did not say, so any).
	OnlyResources []string
	PollSeconds   int
	Stages        []Stage
	// Builders are the other builders it said it can run (BuilderQwen), for the flow panel's
	// usage-paced reason (WaitReason).
	Builders []string
	// Kinds are the kinds of work it last claimed (ClaimRequest.Kinds; none: items only).
	Kinds    []string
	LastSeen time.Time
}

// Unservable is what of resources no runner seen within RunnerSeenWindow of now can serve, sorted;
// nil when one of them offers all of resources (the claim's rule). When each resource is offered
// by some runner but none offers them all, it is all of resources.
func Unservable(resources []string, runners []RunnerSeen, now time.Time) []string {
	if len(resources) == 0 {
		return nil
	}
	var recent []RunnerSeen
	for _, r := range runners {
		if now.Sub(r.LastSeen) <= RunnerSeenWindow {
			if subset(resources, r.Capabilities) {
				return nil
			}
			recent = append(recent, r)
		}
	}
	var missing []string
	for _, res := range resources {
		if !slices.ContainsFunc(recent, func(r RunnerSeen) bool { return slices.Contains(r.Capabilities, res) }) {
			missing = append(missing, res)
		}
	}
	if len(missing) == 0 {
		missing = append(missing, resources...)
	}
	sort.Strings(missing)
	return slices.Compact(missing)
}

// UnservableRoot is how the board shows an unservable item's wait: "no runner offers macos".
func UnservableRoot(missing []string) string {
	return "no runner offers " + strings.Join(missing, " + ")
}
