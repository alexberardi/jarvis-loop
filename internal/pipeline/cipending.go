package pipeline

import (
	"time"
)

// Land's ci_pending (docs/decisions/2026-10-19-ci-pending-requeue.md): a land run whose PR's CI is
// still pending (not failed) at the runner's land_ci_wait gives back its lease instead of failing.
// The item stays in land, is claimed again after LandRequeueAfter, and the runner gives it to the
// user only once CI has been pending on that head for its land_ci_give_up in all.

// LandRequeueAfter is how long a land item put back by ci_pending waits before its next claim.
const LandRequeueAfter = 10 * time.Minute

// applyCIPending records a ci_pending: the head CI is pending on, since when (the earliest of the
// runs on that head), and when the item may be claimed again.
func applyCIPending(it *Item, f fields, data map[string]any, now time.Time) error {
	if _, err := f.need("wait"); err != nil {
		return err
	}
	head, err := f.sha("head_sha")
	if err != nil {
		return err
	}
	raw, err := f.need("since")
	if err != nil {
		return err
	}
	since, perr := time.Parse(time.RFC3339, raw)
	if perr != nil || since.After(now) {
		return errf(CodeInvalidField, "since must be an RFC 3339 time not after now, not %q", raw)
	}
	if it.CIPendingSHA != head || it.CIPendingSince.IsZero() || since.Before(it.CIPendingSince) {
		it.CIPendingSince, it.CIPendingSHA = since.UTC(), head
	}
	it.NotBefore = now.Add(LandRequeueAfter)
	data["not_before"] = it.NotBefore.UTC().Format(time.RFC3339)
	it.Stage = Land
	return nil
}

// Deferred reports whether it is a land item put back by ci_pending whose next claim is not due
// at now.
func (it Item) Deferred(now time.Time) bool {
	return it.Stage == Land && now.Before(it.NotBefore)
}

// Ready is items without the deferred ones: what a claim at now chooses from.
func Ready(items []Item, now time.Time) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if !it.Deferred(now) {
			out = append(out, it)
		}
	}
	return out
}
