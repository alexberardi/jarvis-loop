package pipeline

import "time"

// Land's review_pending (docs/decisions/2026-10-24-land-waits-for-github-review.md): a PR whose
// base requires an approving GitHub review it lacks would be refused by gh pr merge, and the loop
// cannot give that review. The land run gives back its lease instead; the item stays in land and is
// checked again after ReviewRecheckAfter until the user approves on GitHub. Unlike ci_pending it
// never gives up: a review may take hours.

// ReviewRecheckAfter is how long a land item waiting for a GitHub review waits before its next claim.
const ReviewRecheckAfter = 10 * time.Minute

// applyReviewPending records a review_pending: the head that waits, since when (the first one on
// that head; first in the event data says it is that one, so the thread gets the PR link once),
// and when the item may be claimed again.
func applyReviewPending(it *Item, f fields, data map[string]any, now time.Time) error {
	if _, err := f.need("review"); err != nil {
		return err
	}
	head, err := f.sha("head_sha")
	if err != nil {
		return err
	}
	if it.ReviewPendingSHA != head || it.ReviewPendingSince.IsZero() {
		it.ReviewPendingSHA, it.ReviewPendingSince = head, now.UTC()
		data["first"] = true
	}
	it.NotBefore = now.Add(ReviewRecheckAfter)
	data["since"] = it.ReviewPendingSince.UTC().Format(time.RFC3339)
	data["not_before"] = it.NotBefore.UTC().Format(time.RFC3339)
	it.Stage = Land
	return nil
}
