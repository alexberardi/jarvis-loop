# Land waits for a required GitHub review instead of failing the merge (2026-10-24)

Item #588. **Seen:** #547, then #583 (jarvis-server PR #474) and #524 (jarvis-node-mobile PR #134)
on 2026-10-24: land waited for green CI, ran `gh pr merge`, got "Pull request … is not mergeable:
the base branch policy prohibits the merge" twice and reported `failed`, so each item went to
needs_you as a failed run. Those repos' base requires an approving GitHub review, which the loop
cannot give (it holds no second login, and approving its own PRs is not its call). `Decide` never
read the review state: `mergeStateStatus` BLOCKED with `mergeable` MERGEABLE reached the merge gate.

## Decision

- **Land reads `reviewDecision`** (added to `gh pr view --json`, `LandPR.ReviewDecision`). A new
  gate 6b, after CI, evidence and the loop's own hold, before the merge:
  - `REVIEW_REQUIRED` → a new land outcome, **`review_pending`** (`review`, `head_sha`).
  - `CHANGES_REQUESTED` → `failed`: someone asked for changes on GitHub; that is the user's call,
    not something to wait out.
  - `APPROVED` or `""` (no review required) → on to the merge, as before.
- **`review_pending` is modelled on `ci_pending`** (2026-10-19-ci-pending-requeue.md): allowed only
  from land; the item stays in land, unleased, not a failure (no needs_you, bounce or retry);
  `not_before` = now + 10 min (`pipeline.ReviewRecheckAfter`); each later land claim re-reads the PR
  and merges once the review is in. **No give-up**, unlike CI's `land_ci_give_up`: the user may take
  hours, and nothing is broken while they do. loopd records the waiting head and since when
  (`review_pending_sha`, `review_pending_since`, migration `0042_review_pending.sql`); any other
  outcome, or a park in `waiting`, clears them.
- **Slack:** the first `review_pending` on a head (event data `first`) posts once in the thread,
  mentioning the user, with the PR link: "#N … waits for your GitHub review: <PR> (head `abc1234`)
  needs an approving review its base requires, which the loop cannot give. Approve it on GitHub;
  land checks again every 10 minutes and merges it then." Later ones and their claims (`claimed`
  with `review_pending`) post nothing; the parent's stage line says "waiting for a GitHub review
  since 13:00 UTC, checking again at 13:10 UTC". The flow panel names the wait "waiting for a
  GitHub review".
- **Not the loop's `hold`:** a hold waits for the user's loop Approve, bound to the head; this waits
  on GitHub's own review state, which land re-derives on every pass. A held PR in such a repo needs
  both: the hold gate comes first, then this one.
- **Not the `waiting` stage** (2026-10-22-waiting-stage.md): it releases on merges and closes only;
  a review would need a new wait kind and loopd reading GitHub, which only the runner does.

## Tests

- `TestDecide` (runner, gate 6b rows), `TestParseLandView` (`reviewDecision` parsed);
  `TestReviewPendingStaysInLand`, `TestReviewPendingRefusals` (pipeline);
  `TestReviewPendingRequeuesLand` (store: round trip, flow wait, no claim before `not_before`, the
  claim marked).
- Scenario `98-land-waits-for-github-review`: the fake GitHub's `require_review` makes an unapproved
  PR REVIEW_REQUIRED and BLOCKED and refuses its merge with GitHub's wording; land parks it (no
  merge call, no needs_you), posts the link once, checks again quietly, and merges after the
  scenario's `approve_review`.
