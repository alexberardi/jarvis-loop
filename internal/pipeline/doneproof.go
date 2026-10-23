package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Done proofs (docs/decisions/2026-10-18-done-proofs.md): "done" means merged and main's CI green;
// whether the merged change works is the item's verification status (Item.Verify), separate from
// its stage. A done item gets it at the transition into done; the done sweep has the orchestrator
// write a proof for each unproven one, and the proof is checked as a qa item's is
// (ApplyProofChecked), except that the item never leaves done: a failure files a new P1 item.

// A done item's verification status ("" on an item not done).
const (
	DoneUnproven = "unproven"  // no proof yet (or the sweep wrote none)
	DoneProving  = "proving"   // an automatic check (usage, command) still to pass
	DoneVerified = "verified"  // checked: by the user, the proof, or main's CI
	DoneFailed   = "failed"    // reopened in qa, or a done proof's command failed twice
	DoneNeedsYou = "needs-you" // only manual checks left
	DoneWaiting  = "waiting"   // only pi or e2e checks left, which nothing runs yet
)

// VerifyStatuses are the statuses in the report's and the board's order.
var VerifyStatuses = []string{DoneUnproven, DoneProving, DoneVerified, DoneFailed, DoneNeedsYou, DoneWaiting}

// The done path's transition kinds (the item stays done).
const (
	KindDoneProofChecked = "done_proof_checked"
	KindDoneVerified     = "done_verified"
	KindDoneFailed       = "done_failed"
)

// DoneProofReopenPriority is the priority of the item a done proof's failure files: the change was
// thought done and is not.
const DoneProofReopenPriority = "P1"

// VerifiedByCI is the reason of a main-red item's verification: main's CI going green again is
// its proof.
const VerifiedByCI = "verified by CI: main's CI is its check"

// VerifyFromProof is the status a done item's proof leaves it in.
func VerifyFromProof(p *Proof) string {
	switch p.Status() {
	case ProofNone:
		return DoneUnproven
	case CheckPassed:
		return DoneVerified
	case CheckManual:
		return DoneNeedsYou
	case CheckWaiting:
		return DoneWaiting
	}
	return DoneProving // pending, or a command that failed once
}

// MainRedItem reports whether an item is a main-red P1 the runner filed (MainRedCommit's titles):
// main's own CI checks it, so the done sweep verifies it without a proof.
func MainRedItem(it Item) bool {
	return it.Source == "finding" && (strings.HasPrefix(it.Title, "main red in ") || strings.HasPrefix(it.Title, "main red after #"))
}

// ApplyVerifiedByCI verifies an unproven main-red done item.
func ApplyVerifiedByCI(it Item) (Transition, error) {
	if it.Stage != Done || it.Verify != DoneUnproven {
		return Transition{}, errf(CodeWrongStage, "item is %s (%s), not an unproven done item", it.Stage, it.Verify)
	}
	it.Verify, it.VerifyReason = DoneVerified, VerifiedByCI
	return Transition{Item: it, From: Done, To: Done, Kind: KindDoneVerified,
		Data: map[string]any{"verify": it.Verify, "reason": it.VerifyReason}}, nil
}

// applyDoneProofChecked is ApplyProofChecked's end for a done item, whose checks p already holds.
func applyDoneProofChecked(it Item, p Proof, before string) (Transition, error) {
	it.Proof = &p
	data := map[string]any{"status": p.Status(), "by": p.By, "checks": proofSummary(p)}
	for _, c := range p.Checks {
		if c.Kind == ProofCommand && c.State == CheckFailed && c.Fails >= ProofFailsToReopen {
			words := fmt.Sprintf("Proof of the merged change failed %d times in a row: %s: %s", c.Fails, c.String(), firstLine(c.Evidence))
			it.Verify, it.VerifyReason = DoneFailed, clipRunes(words, 400)
			data["words"], data["verify"] = words, it.Verify
			return Transition{Item: it, From: Done, To: Done, Kind: KindDoneFailed, Data: data}, nil
		}
	}
	it.Verify = VerifyFromProof(&p)
	data["verify"] = it.Verify
	if it.Verify == DoneVerified {
		it.VerifyReason = clipRunes("Proof passed: "+strings.Join(proofSummary(p), "; "), 400)
		data["words"] = it.VerifyReason
		return Transition{Item: it, From: Done, To: Done, Kind: KindDoneVerified, Data: data}, nil
	}
	it.VerifyReason = doneReason(&p)
	data["changed"] = it.Verify != before
	return Transition{Item: it, From: Done, To: Done, Kind: KindDoneProofChecked, Data: data}, nil
}

// doneReason is the line that says where a done item's proof stands, as QAGroup's does for qa.
func doneReason(p *Proof) string {
	_, why := QAGroup(p, false)
	return clipRunes(why, 400)
}

// KindDoneReproof is a failed done item sent back to unproven: the reopen its failure filed was
// closed not reproducible (#415), so the failure was the proof's environment, not the change.
const KindDoneReproof = "done_reproof"

// ApplyReopenNotReproducible is a failed done item's reopen (reopenID) closed not reproducible
// with summary: the item goes back to unproven, without its proof, and the done sweep asks for a
// new one. A reopen closed as a duplicate is a real failure tracked elsewhere: the item stays
// failed.
func ApplyReopenNotReproducible(it Item, reopenID int64, summary string) (Transition, error) {
	if it.Stage != Done || it.Verify != DoneFailed {
		return Transition{}, errf(CodeWrongStage, "item is %s (%s), not a failed done item", it.Stage, it.Verify)
	}
	summary = strings.TrimSpace(summary)
	if _, dup := DuplicateOf(summary); dup {
		return Transition{}, errf(CodeWrongStage, "reopen #%d closed as a duplicate: the failure stands", reopenID)
	}
	it.Verify, it.Proof = DoneUnproven, nil
	it.VerifyReason = clipRunes(fmt.Sprintf("reopen #%d closed not reproducible: %s", reopenID, firstLine(summary)), 400)
	return Transition{Item: it, From: Done, To: Done, Kind: KindDoneReproof,
		Data: map[string]any{"verify": it.Verify, "reason": it.VerifyReason, "reopen_item": reopenID}}, nil
}

// dupOfRe reads the item a duplicate closure names out of its summary ("duplicate of #545, ...",
// as the triage brief asks for it).
var dupOfRe = regexp.MustCompile(`(?i)^duplicate\s+(?:of\s+)?#(\d+)`)

// DuplicateOf reads a closing summary: whether it closes the item as a duplicate (its first word,
// which is what the triage brief asks for) and, when it names one, the item it duplicates (0 when
// it names none). A duplicate closure is not the same as "not reproducible": the ask stands, it is
// just tracked elsewhere, so a freeze hands over to that item instead of lifting (#558).
func DuplicateOf(summary string) (int64, bool) {
	summary = strings.TrimSpace(summary)
	if !strings.HasPrefix(strings.ToLower(summary), "duplicate") {
		return 0, false
	}
	m := dupOfRe.FindStringSubmatch(summary)
	if m == nil {
		return 0, true
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, true
	}
	return id, true
}
