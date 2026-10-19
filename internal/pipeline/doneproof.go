package pipeline

import (
	"fmt"
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
