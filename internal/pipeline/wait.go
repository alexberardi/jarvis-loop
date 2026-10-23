package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The waiting stage (docs/decisions/2026-10-22-waiting-stage.md): an item whose next step (a
// rebuild, a review or the merge) needs other work done first is parked in waiting, with what it
// waits on: items (its DependsOn, done once merged: qa or done) and PRs that are not loop items
// (WaitPRs, done once merged on GitHub, as a runner sees it). Nothing claims it there. loopd moves
// it back to the stage it left (ReturnStage) as soon as everything is done, with a note; a
// dependency dropped or a PR closed without merging sends it to the user instead.

// Wait is a build or review agent's outcome that parks its item (the runner reports it; loopd
// applies it as the user's wait, and sends the item to the user when the wait is refused).
const Wait Outcome = "wait"

// WaitLongAfter is how long an item waits before the standup lists it.
const WaitLongAfter = 24 * time.Hour

// WaitPR is a PR a waiting item waits on, "owner/name#444", and whether a runner saw it merged.
type WaitPR struct {
	Ref    string `json:"ref"`
	Merged bool   `json:"merged,omitempty"`
}

var waitPRRe = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#?([0-9]+)$`)

// ParseWaitPR reads a PR to wait on: "owner/name#444", or "#444" (or "444") for a PR of repo.
// It returns the ref as "owner/name#444".
func ParseWaitPR(s, repo string) (string, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "-merge")
	m := waitPRRe.FindStringSubmatch(s)
	if m == nil {
		return "", errf(CodeInvalidField, "PR %q: want owner/name#444, or #444 for one of the item's repo", s)
	}
	slug := m[1]
	if slug == "" {
		if repo == "" {
			return "", errf(CodeInvalidField, "PR %q: the item has no repo; name it as owner/name#%s", s, m[2])
		}
		slug = repo
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", errf(CodeInvalidField, "PR %q: the number must be positive", s)
	}
	return slug + "#" + strconv.Itoa(n), nil
}

// SplitWaitPR splits a ref ParseWaitPR made into the repo slug and the number.
func SplitWaitPR(ref string) (slug string, n int, ok bool) {
	i := strings.LastIndexByte(ref, '#')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	return ref[:i], n, err == nil && n > 0
}

// OpenWaitPRs are the PRs it waits on not seen merged yet.
func OpenWaitPRs(it Item) []string {
	var out []string
	for _, p := range it.WaitPRs {
		if !p.Merged {
			out = append(out, p.Ref)
		}
	}
	return out
}

// WaitsOn is what a waiting item waits on, for the board and Slack: "#472, #483,
// owner/name#444" (the items not merged yet, then the PRs not merged yet; "" for none).
func WaitsOn(it Item, stages map[int64]Stage) string {
	var parts []string
	for _, d := range WaitingOn(it, stages) {
		parts = append(parts, "#"+strconv.FormatInt(d, 10))
	}
	return strings.Join(append(parts, OpenWaitPRs(it)...), ", ")
}

// Waitable reports whether an item in this stage may be parked, and the stage it goes back to:
// build, review and land, a needs_you item whose question came from one of them, and a waiting
// item (which waits on more).
func Waitable(it Item) (Stage, bool) {
	switch it.Stage {
	case Build, Review, Land:
		return it.Stage, true
	case NeedsYou, Waiting:
		if Rebuildable(it) {
			return it.ReturnStage, true
		}
	}
	return "", false
}

// ApplyWait parks it in waiting. The caller has already added the items to wait on (added) to its
// DependsOn, checked for a cycle; stages holds their stages. prs are refs ParseWaitPR made. Refused
// for a stage it cannot wait in and when, with them, it waits on nothing not done.
func ApplyWait(it Item, added []int64, prs []string, stages map[int64]Stage, reason string, now time.Time) (Transition, error) {
	ret, ok := Waitable(it)
	if !ok {
		if it.Stage == Triage {
			return Transition{}, errf(CodeWrongStage, "#%d is in triage: link it (loop link --after) instead, triage waits for its dependencies anyway", it.ID)
		}
		return Transition{}, errf(CodeWrongStage, "#%d is %s; only an item in build, review or land (or a question from one) can wait", it.ID, it.Stage)
	}
	reason = strings.TrimSpace(reason)
	if len(added) == 0 && len(prs) == 0 {
		return Transition{}, errf(CodeMissingField, "say what it waits on: items (--on 472,483) or a PR (--pr owner/name#444)")
	}
	for _, ref := range prs {
		if !slices.ContainsFunc(it.WaitPRs, func(p WaitPR) bool { return p.Ref == ref }) {
			it.WaitPRs = append(slices.Clone(it.WaitPRs), WaitPR{Ref: ref})
		}
	}
	for _, d := range added {
		if st, ok := stages[d]; ok && DependencyMet(st) {
			return Transition{}, errf(CodeInvalidField, "#%d is already merged (%s): nothing to wait for", d, st)
		}
	}
	waits := WaitsOn(it, stages)
	if waits == "" {
		return Transition{}, errf(CodeInvalidField, "#%d waits on nothing that is not done", it.ID)
	}
	from := it.Stage
	data := map[string]any{"waits_on": waits, "return_stage": string(ret)}
	if len(added) > 0 {
		data["on"] = added
	}
	if len(prs) > 0 {
		data["prs"] = prs
	}
	if reason != "" {
		data["reason"] = reason
	}
	if from == NeedsYou {
		data["question"] = it.Question
	}
	if from != Waiting {
		it.WaitingSince = now
	}
	it.Stage, it.ReturnStage, it.Question = Waiting, ret, ""
	it.NotBefore, it.CIPendingSince, it.CIPendingSHA = time.Time{}, time.Time{}, ""
	return Transition{Item: it, From: from, To: Waiting, Kind: "waiting", Data: data}, nil
}

// ApplyRelease moves a waiting item back to the stage it left once everything it waits on is done.
// false: it is not waiting, or still waits on something.
func ApplyRelease(it Item, stages map[int64]Stage) (Transition, bool) {
	if it.Stage != Waiting || len(WaitingOn(it, stages)) > 0 || len(OpenWaitPRs(it)) > 0 {
		return Transition{}, false
	}
	ret := it.ReturnStage
	if ret != Build && ret != Review && ret != Land {
		ret = Build
	}
	var done []string
	for _, d := range it.DependsOn {
		done = append(done, fmt.Sprintf("#%d %s", d, doneWord(stages[d])))
	}
	for _, p := range it.WaitPRs {
		done = append(done, p.Ref+" merged")
	}
	note := fmt.Sprintf("back to %s", ret)
	if len(done) > 0 {
		note = strings.Join(done, ", ") + ": " + note
	}
	data := map[string]any{"note": note, "return_stage": string(ret)}
	if !it.WaitingSince.IsZero() {
		data["waited_since"] = it.WaitingSince.Format(time.RFC3339)
	}
	it.Stage, it.ReturnStage, it.WaitPRs, it.WaitingSince = ret, "", nil, time.Time{}
	return Transition{Item: it, From: Waiting, To: ret, Kind: "released", Data: data}, true
}

func doneWord(st Stage) string {
	if st == QA {
		return "merged"
	}
	return string(st)
}

// WaitPRClosedQuestion is what a waiting item asks when a PR it waits on was closed unmerged.
func WaitPRClosedQuestion(ref string) string {
	return fmt.Sprintf("%s it waits on was closed without merging: keep, change or drop this one?", ref)
}

// ApplyWaitPR is a runner seeing a PR a waiting item waits on merged (state MERGED: the PR is done,
// and the caller then tries ApplyRelease) or closed without merging (CLOSED: the item goes to the
// user, as for a dropped dependency).
func ApplyWaitPR(it Item, ref, state string) (Transition, error) {
	if it.Stage != Waiting {
		return Transition{}, errf(CodeWrongStage, "#%d is %s, not waiting", it.ID, it.Stage)
	}
	i := slices.IndexFunc(it.WaitPRs, func(p WaitPR) bool { return p.Ref == ref && !p.Merged })
	if i < 0 {
		return Transition{}, errf(CodeInvalidField, "#%d does not wait on %s", it.ID, ref)
	}
	data := map[string]any{"pr": ref, "state": state}
	switch state {
	case "MERGED":
		it.WaitPRs = slices.Clone(it.WaitPRs)
		it.WaitPRs[i].Merged = true
		return Transition{Item: it, From: Waiting, To: Waiting, Kind: "wait_pr_merged", Data: data}, nil
	case "CLOSED":
		q := WaitPRClosedQuestion(ref)
		data["question"] = q
		it = unpark(it)
		return Transition{Item: toNeedsYou(it, it.ReturnStage, q), From: Waiting, To: NeedsYou, Kind: "wait_pr_closed", Data: data}, nil
	}
	return Transition{}, errf(CodeInvalidField, "state must be MERGED or CLOSED, not %q", state)
}

// ApplyUnwait is the user ending a wait by hand: the item goes back to the stage it left and stops
// waiting on its PRs. Items it still depends on keep holding its claim (unlink them).
func ApplyUnwait(it Item) (Transition, error) {
	if it.Stage != Waiting {
		return Transition{}, errf(CodeWrongStage, "#%d is %s, not waiting", it.ID, it.Stage)
	}
	ret := it.ReturnStage
	if ret != Build && ret != Review && ret != Land {
		ret = Build
	}
	data := map[string]any{"note": "the wait was ended by hand: back to " + string(ret), "return_stage": string(ret)}
	if prs := OpenWaitPRs(it); len(prs) > 0 {
		data["prs"] = prs
	}
	it = unpark(it)
	it.Stage, it.ReturnStage = ret, ""
	return Transition{Item: it, From: Waiting, To: ret, Kind: "released", Data: data}, nil
}

// unpark clears what only a waiting item carries (its stage and ReturnStage stay).
func unpark(it Item) Item {
	it.WaitPRs, it.WaitingSince = nil, time.Time{}
	return it
}

// WaitedLong is whether a waiting item has waited at least WaitLongAfter at now.
func WaitedLong(it Item, now time.Time) bool {
	return it.Stage == Waiting && !it.WaitingSince.IsZero() && now.Sub(it.WaitingSince) >= WaitLongAfter
}
