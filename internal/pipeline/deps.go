package pipeline

import (
	"fmt"
	"slices"
)

// Item dependencies (docs/phases/07-planning.md, "Item dependencies and the plan link"): an item
// waits until every item it depends on is done. qa does not count: the piece it depends on is
// merged but not verified yet.

// WaitingOn is the dependencies of it that are not done yet, in its order. stages holds the
// dependencies' stages; one missing from it counts as not done.
func WaitingOn(it Item, stages map[int64]Stage) []int64 {
	var out []int64
	for _, d := range it.DependsOn {
		if st, ok := stages[d]; !ok || st != Done {
			out = append(out, d)
		}
	}
	return out
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
