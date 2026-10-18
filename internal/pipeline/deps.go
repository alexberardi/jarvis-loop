package pipeline

import (
	"fmt"
	"slices"
	"strings"
)

// Item dependencies (docs/phases/07-planning.md, "Item dependencies and the plan link"): an item
// waits until every item it depends on is merged: in qa or done
// (docs/decisions/2026-10-18-deps-at-merge.md). The user's check of a qa item goes on in parallel;
// when it fails (Reopen), loopd flags the open items that depended on it rather than stopping them.

// DependencyMet is whether a dependency in stage st satisfies the items that depend on it.
func DependencyMet(st Stage) bool { return st == QA || st == Done }

// WaitingOn is the dependencies of it that are not merged yet, in its order. stages holds the
// dependencies' stages; one missing from it counts as not merged.
func WaitingOn(it Item, stages map[int64]Stage) []int64 {
	var out []int64
	for _, d := range it.DependsOn {
		if st, ok := stages[d]; !ok || !DependencyMet(st) {
			out = append(out, d)
		}
	}
	return out
}

// WaitNode is what a wait chain needs to know of an item.
type WaitNode struct {
	Stage     Stage
	Claimed   bool
	DependsOn []int64
	// Unservable is the resources no runner offers it (Unservable), when loopd flagged it.
	Unservable []string
}

// WaitChain is what an item waits on, followed to its root (docs/phases/08-orchestrator.md, "Talking
// to it"): Path is a dependency of the item not merged yet, then that one's, and so on; Root is the
// state of the last one ("needs you", "in build"), which holds up the rest.
type WaitChain struct {
	Path []int64
	Root string
}

// String is the board's "#12 → #35 → needs you".
func (c WaitChain) String() string {
	var b strings.Builder
	for _, id := range c.Path {
		fmt.Fprintf(&b, "#%d → ", id)
	}
	return b.String() + c.Root
}

// WaitChains is one chain per dependency of item id not merged yet, in its order; none for a done or
// closed item. Past the first link a chain follows the first dependency not merged of each item (that
// item's own card shows the rest). A dependency missing from items ends its chain as "not found",
// one already on the path as "a cycle".
func WaitChains(id int64, items map[int64]WaitNode) []WaitChain {
	it, ok := items[id]
	if !ok || it.Stage == Done || it.Stage == Closed {
		return nil
	}
	var out []WaitChain
	for _, d := range it.DependsOn {
		if n, ok := items[d]; ok && DependencyMet(n.Stage) {
			continue
		}
		c := WaitChain{Path: []int64{d}}
		for cur := d; ; {
			n, ok := items[cur]
			if !ok {
				c.Root = "not found"
				break
			}
			next := int64(0)
			if n.Stage != Closed {
				for _, dd := range n.DependsOn {
					if nn, ok := items[dd]; !ok || !DependencyMet(nn.Stage) {
						next = dd
						break
					}
				}
			}
			if next == 0 {
				c.Root = WaitRoot(n.Stage, n.Claimed)
				if len(n.Unservable) > 0 && !n.Claimed {
					c.Root = UnservableRoot(n.Unservable)
				}
				break
			}
			loop := next == id || slices.Contains(c.Path, next)
			c.Path = append(c.Path, next)
			if loop {
				c.Root = "a cycle"
				break
			}
			cur = next
		}
		out = append(out, c)
	}
	return out
}

// WaitRoot is how a chain's root is shown: what the items behind it wait for.
func WaitRoot(st Stage, claimed bool) string {
	switch st {
	case NeedsYou:
		return "needs you"
	case Held:
		return "held for approval"
	case Triage:
		if claimed {
			return "in triage"
		}
		return "waiting for triage"
	case Build:
		if claimed {
			return "in build"
		}
		return "ready, not claimed"
	case Review:
		if claimed {
			return "in review"
		}
		return "queued for review"
	case Land:
		if claimed {
			return "landing"
		}
		return "queued to land"
	case QA:
		return "in qa"
	}
	return string(st)
}

// DependencyCycle finds a cycle in a dependency graph (item id -> the ids it depends on) and
// returns it as a path that starts and ends on the same id, or nil when there is none.
func DependencyCycle(graph map[int64][]int64) []int64 {
	const (
		unseen = iota
		onPath
		finished
	)
	state := map[int64]int{}
	var path []int64
	var visit func(id int64) []int64
	visit = func(id int64) []int64 {
		state[id] = onPath
		path = append(path, id)
		for _, d := range graph[id] {
			switch state[d] {
			case onPath:
				i := slices.Index(path, d)
				return append(append([]int64{}, path[i:]...), d)
			case unseen:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = finished
		return nil
	}
	ids := make([]int64, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if state[id] == unseen {
			if c := visit(id); c != nil {
				return c
			}
		}
	}
	return nil
}

// DependencyClosedQuestion is what a dependent item asks when an item it depends on is dropped
// or closed.
func DependencyClosedQuestion(dep int64) string {
	return fmt.Sprintf("#%d it depends on was dropped: keep, change or drop this one?", dep)
}

// ApplyDependencyClosed is an item it depends on (dep) being dropped or closed: dep leaves its
// dependencies (it can never be done, so the item would wait forever), and an open item goes to
// needs_you to ask whether to keep, change or drop it. An item already in needs_you keeps its
// question. false: nothing to do (dep is not a dependency, or the item is past the point where it
// matters: qa, done, closed).
func ApplyDependencyClosed(it Item, dep int64) (Transition, bool) {
	if !slices.Contains(it.DependsOn, dep) {
		return Transition{}, false
	}
	switch it.Stage {
	case QA, Done, Closed:
		return Transition{}, false
	}
	from := it.Stage
	it.DependsOn = slices.DeleteFunc(slices.Clone(it.DependsOn), func(d int64) bool { return d == dep })
	data := map[string]any{"dependency": dep}
	if from != NeedsYou {
		q := DependencyClosedQuestion(dep)
		it = toNeedsYou(it, from, q)
		data["question"] = q
	}
	return Transition{Item: it, From: from, To: it.Stage, Kind: "dependency_closed", Data: data}, true
}
