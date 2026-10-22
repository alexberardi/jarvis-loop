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
	// Kinds are the kinds of work it takes (ClaimRequest.RunnerKinds, else the last claim's Kinds
	// that named any; none: items only).
	Kinds []string
	// Repos are the repos it has a checkout of (ClaimRequest.Repos; none: any).
	Repos    []string
	LastSeen time.Time
}

// Unservable is what of resources, and the item's repo, no runner seen within RunnerSeenWindow of
// now can serve, sorted; nil when one of them offers all of resources and has the repo (the
// claim's rule, HasRepo). When each is offered by some runner but none offers them all, it is all
// of them. The repo alone is judged only against runners seen (none seen: nothing is said).
func Unservable(resources []string, repo string, runners []RunnerSeen, now time.Time) []string {
	if len(resources) == 0 && repo == "" {
		return nil
	}
	var recent []RunnerSeen
	for _, r := range runners {
		if now.Sub(r.LastSeen) <= RunnerSeenWindow {
			if subset(resources, r.Capabilities) && HasRepo(r.Repos, repo) {
				return nil
			}
			recent = append(recent, r)
		}
	}
	if len(resources) == 0 && len(recent) == 0 {
		return nil
	}
	var missing []string
	for _, res := range resources {
		if !slices.ContainsFunc(recent, func(r RunnerSeen) bool { return slices.Contains(r.Capabilities, res) }) {
			missing = append(missing, res)
		}
	}
	if repo != "" && !slices.ContainsFunc(recent, func(r RunnerSeen) bool { return HasRepo(r.Repos, repo) }) {
		missing = append(missing, repo)
	}
	if len(missing) == 0 {
		missing = append(missing, resources...)
		if repo != "" {
			missing = append(missing, repo)
		}
	}
	sort.Strings(missing)
	return slices.Compact(missing)
}

// UnservableRoot is how the board shows an unservable item's wait: "no runner offers macos".
func UnservableRoot(missing []string) string {
	return "no runner offers " + strings.Join(missing, " + ")
}
