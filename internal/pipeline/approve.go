package pipeline

import (
	"fmt"
	"strings"
	"time"
)

// Who may approve or merge: the stage each outcome may come from, the review approval, the
// merge gate and the user's approval of a held merge. This file is on jarvis-loop's hold list
// (loopRules, docs/decisions/2026-10-18-narrow-loop-holds.md); the rest of ApplyAdvance is not.

// Approval is the user's approval of a held merge, bound to the commit they saw.
type Approval struct {
	By      string    `json:"by"`
	Words   string    `json:"words"`
	HeadSHA string    `json:"head_sha"`
	At      time.Time `json:"at"`
	// CarriedFrom is the head the user approved when review carried the approval to HeadSHA over
	// merges of the base (carryApproval); "" for the user's own approval of HeadSHA.
	CarriedFrom string `json:"carried_from,omitempty"`
}

// outcomeStages are the stages each outcome may finish.
var outcomeStages = map[Outcome][]Stage{
	Reproduced: {Triage}, Routed: {Triage}, NotReproducible: {Triage}, Question: {Triage, Build},
	PROpened: {Build}, VerifyFailed: {Build}, Approved: {Review}, Bounce: {Review},
	Merged: {Land}, Hold: {Land}, BackToBuild: {Land}, BackToReview: {Land}, CIPending: {Land}, ReviewPending: {Land},
	UsageLimit: {Triage, Build, Review, Land}, Failed: {Triage, Build, Review, Land},
}

// needsReview reads an agent's needs_review field: the builder or reviewer holding what a path
// rule cannot see.
func needsReview(f fields) (bool, error) {
	switch strings.TrimSpace(f["needs_review"]) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errf(CodeInvalidField, "needs_review must be true or false")
	}
}

// reviewApproves checks a review's approval: of the PR's head, by an agent other than the
// builder. A reviewer's needs_review holds the merge.
func reviewApproves(it *Item, a Advance, f fields) error {
	sha, err := f.sha("head_sha")
	if err != nil {
		return err
	}
	if sha != it.HeadSHA {
		return errf(CodeSHAMismatch, "reviewed %s but the PR head is %s", sha, it.HeadSHA)
	}
	if a.Agent != "" && a.Agent == it.BuilderAgent {
		return errf(CodeSameAgent, "the builder cannot approve its own work")
	}
	// The reviewer holds what a path rule cannot see: a curl | sh pin edited in a docs page, a
	// new secret used in a package.
	nr, err := needsReview(f)
	if err != nil {
		return err
	}
	if nr {
		raiseRisk(it, []string{"review: marked needs review"})
	}
	return nil
}

// checkMerge is the merge gate: risk is recomputed on what was actually merged (reasons, the hold
// rules' hits on its paths), and a held item's approval must be for that head.
func checkMerge(it *Item, head string, reasons []string) error {
	raiseRisk(it, reasons)
	if it.Risk == RiskHold && (it.Approval == nil || it.Approval.HeadSHA != head) {
		return errf(CodeApprovalRequired, "on the hold list (%s): merged head %s needs the user's approval for that commit",
			it.HoldReason, head)
	}
	return nil
}

// voidApproval marks an approval that is not for head void: an approval is for the commit the user
// saw, and checkMerge lets only that head through. The approval stays on the item as the one
// review may carry to the new head when the only change is merges of the base (carryApproval).
func voidApproval(it *Item, data map[string]any, head string) {
	if it.Approval != nil && it.Approval.HeadSHA != head {
		data["approval_voided"] = true
	}
}

// Event kinds of carryApproval's decision.
const (
	KindApprovalCarried    = "approval_carried"
	KindApprovalNotCarried = "approval_not_carried"
)

// carryApproval decides, when review approves a new head of an item the user approved at an older
// one, whether the approval moves to the new head (docs/decisions/2026-10-18-approval-carry.md).
// The runner sends the delta it computed from git: carry_from (the approved commit), carry_merges
// (the merges of the base since), carry_conflicts and carry_changed (newline-separated files: the
// merges' conflicted files, and the files where a merge is not git's own outside the conflict
// hunks), carry_problem (why the change is not merges of the base at all). The approval moves only
// when the change is merges of the base and nothing else, no conflict was in a file on the hold
// list and the reviewer did not mark it needs review; otherwise it is dropped and the user
// approves the new head. hits are the hold rules' matches on paths.
func carryApproval(it *Item, f fields, needsReview bool, hits func([]ChangedPath) []HoldMatch) *Note {
	a := it.Approval
	if a == nil || a.HeadSHA == it.HeadSHA {
		return nil
	}
	lines := func(k string) []string {
		var out []string
		for _, l := range strings.Split(f[k], "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	from := strings.TrimSpace(f["carry_from"])
	merges := strings.Fields(f["carry_merges"])
	conflicts, changed := lines("carry_conflicts"), lines("carry_changed")
	data := map[string]any{"from_sha": a.HeadSHA, "to_sha": it.HeadSHA, "by": a.By}
	if len(merges) > 0 {
		data["merges"] = merges
	}
	if len(conflicts) > 0 {
		data["conflicts"] = conflicts
	}
	var why string
	switch {
	case from == "":
		why = "the review sent no delta from the approved commit"
	case from != a.HeadSHA:
		why = fmt.Sprintf("the review's delta is from %s, not the approved commit", short7(from))
	case strings.TrimSpace(f["carry_problem"]) != "":
		why = strings.TrimSpace(f["carry_problem"])
	case len(changed) > 0:
		why = "the PR's own lines changed outside the conflict hunks: " + strings.Join(changed, ", ")
		data["changed"] = changed
	case len(merges) == 0:
		why = "no merge of the base since the approval"
	case needsReview:
		why = "the review marked the new head needs review"
	}
	if why == "" && len(conflicts) > 0 {
		paths := make([]ChangedPath, 0, len(conflicts))
		for _, c := range conflicts {
			paths = append(paths, ChangedPath{Status: "M", Path: c})
		}
		if m := hits(paths); len(m) > 0 {
			var s []string
			for _, x := range m {
				s = append(s, x.String())
			}
			why = "a conflict was resolved in a file on the hold list (" + strings.Join(s, "; ") + ")"
			data["hold_matches"] = m
		}
	}
	if why != "" {
		it.Approval = nil
		data["reason"] = why
		return &Note{Kind: KindApprovalNotCarried, Data: data}
	}
	it.Approval = &Approval{By: a.By, Words: a.Words, HeadSHA: it.HeadSHA, At: a.At, CarriedFrom: a.HeadSHA}
	return &Note{Kind: KindApprovalCarried, Data: data}
}

// KindHeldMerge is the runner merging the base into a held PR (ApplyHeldMerge).
const KindHeldMerge = "held_merge"

// ApplyHeldMerge records the runner's merge of the base into a held item's PR, done while it waits
// for the user so that it is mergeable once approved: the new head goes to review. Any approval
// stays for review to carry (an approval of the old head moves only if the change is that merge).
func ApplyHeldMerge(it Item, head, notes string) (Transition, error) {
	if it.Stage != Held {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held", it.Stage)
	}
	head = strings.TrimSpace(head)
	if !shaRe.MatchString(head) {
		return Transition{}, errf(CodeInvalidField, "head_sha must be a lowercase hex commit sha")
	}
	if head == it.HeadSHA {
		return Transition{}, errf(CodeSHAMismatch, "the merge %s is the held head already", head)
	}
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return Transition{}, errf(CodeMissingField, "notes are required")
	}
	data := map[string]any{"from_sha": it.HeadSHA, "head_sha": head, "notes": notes}
	voidApproval(&it, data, head)
	it.HeadSHA = head
	it.Stage = Review
	return Transition{Item: it, From: Held, To: Review, Kind: KindHeldMerge, Data: data}, nil
}

// ApplyApprove records the user's approval of a held merge for the head they saw.
func ApplyApprove(it Item, by, headSHA, words string, now time.Time) (Transition, error) {
	if it.Stage != Held {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held", it.Stage)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return Transition{}, errf(CodeMissingField, "head_sha is required")
	}
	if headSHA != it.HeadSHA {
		return Transition{}, errf(CodeSHAMismatch, "approval is for %s but the PR head is now %s", headSHA, it.HeadSHA)
	}
	it.Approval = &Approval{By: by, Words: words, HeadSHA: headSHA, At: now}
	it.Stage = Land
	it.UpdatedAt = now
	return Transition{Item: it, From: Held, To: Land, Kind: "approved",
		Data: map[string]any{"by": by, "words": words, "head_sha": headSHA}}, nil
}

// ApplyHold records the user keeping a held merge held.
func ApplyHold(it Item, words string) (Transition, error) {
	if it.Stage != Held {
		return Transition{}, errf(CodeWrongStage, "item is %s, not held", it.Stage)
	}
	// Keeping it held also drops an approval of an older head, which review could otherwise carry.
	it.Approval = nil
	return Transition{Item: it, From: Held, To: Held, Kind: "held", Data: map[string]any{"words": words}}, nil
}
