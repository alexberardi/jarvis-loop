# Trust carries to a new head whose trusted files' diff is unchanged

2026-10-19 · item #277. Amends the "Until a verdict" point of
[2026-10-16-trusted-files.md](2026-10-16-trusted-files.md). Seen on #272: Trust was asked three
times for the same change to `tools/pipeline/scenarios/computer_control.json` (at f9219af, 2e2fe0a
and 28e2f20). Each `trusted-files.diff` had the same sha256, so only code outside the trusted files
had changed between heads (review fixes, merges of main). #221 was asked twice the same way. Each
re-ask parked a finished build in needs-you until the user pressed Trust again. Each point is
enforced by a test (`pipeline/trust_test.go`, `runner/trustcarry_test.go`) or by a scenario
(`77-trust-carried-unchanged-diff`, `77-trust-carried-after-merge-of-main`,
`77-trust-asks-again-changed-diff`).

## What was chosen

- **The refusal records the digest of what the user looks at.** The runner's refusal also sends
  `trusted_diff_sha256`: the sha256 of `git diff --full-index --binary --no-textconv --no-ext-diff
  origin/<base>...<head> -- <trusted paths>`, the uncut diff it attaches as `trusted-files.diff`
  (that file is cut at 1 MB; the digest is not). The flags make the diff bind the bytes: for a
  binary trusted file, or one the branch marks `-diff` (or gives a textconv driver) in
  `.gitattributes`, git's default diff prints only "Binary files differ" and abbreviated (7-hex)
  blob ids, which an agent could brute-force to carry the OK to content the user never saw. Full
  40-hex ids, a real binary patch and no driver leave nothing to collide. loopd stores it on the item's trust as `diff_sha256` (it must be 64 lowercase hex).
- **The runner carries the OK only to the same bytes.** When verify meets a trusted-file change on a
  head the OK does not allow (another head, or the OK was used), it carries the OK when the
  approver gave one, it has a digest, the head changes the same trusted paths, and the diff of
  those paths against the merge base has the same sha256. It then runs verify and reports
  `trust_carried: <head>` and `trusted_diff_sha256: <digest>` beside `trust_used: <head>`. Any other
  case (no OK yet, an OK from before digests, other paths, one byte different) refuses as before.
  The three-dot range compares against the merge base, so a merge of main, or a rebase onto it,
  carries the OK unless main changed the same trusted files.
- **loopd checks the digest itself.** It accepts `trust_carried` only from build, only with
  `trust_used` for the same commit, only when the item has an OK with a digest, and only when the
  digest the runner sent equals the one recorded at the refusal (`invalid_field` otherwise). It
  then rebinds the OK to the new head (`head_sha`; `carried_from` keeps the old one; `by`, `at`
  and the digest stay), unused, and `trust_used` is applied as before (`pr_opened` or
  `verify_failed` use it up). The transition records a `trust_carried` event
  `{from, to, diff_sha256, by}`; Slack posts one line in the thread and `loop show` prints where the
  OK carried from. The item's risk stays hold, so land still holds the PR for the user.
- The scenarios `74-rebase-after-trust-pushes` and `74-foreign-remote-commit-asks` relied on a
  plain rebase asking for Trust again; their fake build now also changes the trusted file after the
  rebase (`rebase_trusted_edit`), so they still test what they were written for.

## Not chosen

- Trusting the runner's word that the diff is unchanged: loopd compares the digest with the one
  it recorded at the refusal.
- Comparing the diff stat, or the files' contents at the head: the stat hides the lines, and the
  contents ignore what the base had. The diff against the merge base is exactly what the user saw.
- Carrying an OK across a different diff of the same files (a smaller or "harmless" change): any
  change to the bytes the user looked at asks again.
