package pipeline

import (
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
}

// outcomeStages are the stages each outcome may finish.
var outcomeStages = map[Outcome][]Stage{
	Reproduced: {Triage}, Routed: {Triage}, NotReproducible: {Triage}, Question: {Triage, Build},
	PROpened: {Build}, VerifyFailed: {Build}, Approved: {Review}, Bounce: {Review},
	Merged: {Land}, Hold: {Land}, BackToBuild: {Land}, BackToReview: {Land},
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

// voidApproval drops an approval that is not for head: an approval is for the commit the user saw.
func voidApproval(it *Item, data map[string]any, head string) {
	if it.Approval != nil && it.Approval.HeadSHA != head {
		it.Approval = nil
		data["approval_voided"] = true
	}
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
	return Transition{Item: it, From: Held, To: Held, Kind: "held", Data: map[string]any{"words": words}}, nil
}
