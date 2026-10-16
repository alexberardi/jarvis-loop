# Red CI the build cannot change must not cycle

2026-10-15 · item #18. Seen on item #17: CI failed on head `e012241` because of a flaky scenario
outside the PR's code. Land sent the item back to build; the build agent found nothing to fix and
reported `pr_opened` with the same head; review approved it again; land saw the same red CI and
sent it back again. Three rounds (about $1 each) before the user noticed, with `bounce_count` still
at 1, because land's `back_to_build` was not counted. What was chosen, each enforced by a test or a
scenario (`scenarios/35-ci-red-unchanged.json`).

## Red CI is a counted bounce

- Land's CI-red `back_to_build` carries **`ci_head_sha`**, the full head that failed
  (`runner.Decide`, `TestDecide` "CI failed names the head"). loopd checks it is a sha
  (`invalid_field` otherwise) and, when it is present, sends the item through `bounce()`: the same
  count as a review bounce or a failed verify, so the second one sends the item to needs-you with
  the CI reason as the question, return stage build (`TestCIRedBackToBuildCounts`).
- **Conflicts and missing evidence are not counted** (`back_to_build` without `ci_head_sha`). A
  merge conflict comes from `main` moving, not from the build's work, and the next build run
  usually resolves it; the missing-evidence and missing-footer gates are the runner's own checks on
  what it wrote. Counting them would send work to the user that the loop can still finish. If they
  turn out to cycle too, the same field-based switch is where to add them.

## The same head after a CI bounce → needs-you

- The item remembers the failed head: **`ci_red_sha`** (a column, migration `0006_ci_red.sql`).
- A `pr_opened` whose `head_sha` equals it moves the item to **needs-you**, return stage **land**,
  with the question "CI failed on `<sha7>` and the build changed nothing: rerun CI, fix main, or
  drop" (`TestPROpenedOnTheCIRedHead`). Another review and land would only see the same red CI. The
  return stage is land, not build: once the user reruns CI or fixes main, an answer sends the item
  back to land, which reads CI again on that head; if it is still red, that is the second counted
  bounce and the item comes back to the user.
- A `pr_opened` with a different head clears `ci_red_sha` and goes to review as before.
- This catches the #17 case after one build round, where the bounce limit alone would have taken
  two, and works whatever the bounce count is.

## The harness

`scenarios/35-ci-red-unchanged.json`: CI fails → back to build with `bounce_count` 1 → the fake
build's new `no_change` play (commits nothing on a resumed branch) reports the same head → the item
is in needs-you with the question above, no second review, nothing merged.
`scenarios/19-ci-red.json` is unchanged and still passes.
