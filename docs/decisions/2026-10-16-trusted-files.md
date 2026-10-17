# Trusted files: the approver's OK lets verify run one exact commit

2026-10-16 · item #70. Seen on #35: a change to one of jarvis-server's `deny_write` paths (the
harness its verify runs, e.g. `tools/pipeline`) made the runner's verify refuse ("the user must
look") and the item went to needs-you, with no way forward: an answer sent it back to build, which
made the same commit and hit the same refusal. 18 autonomy items depend on #35. Extends the verify
rule in [2026-10-15-all-repos.md](2026-10-15-all-repos.md); each point is enforced by a test or by
`scenarios/56-trusted-files.json`.

## What was chosen

- **The refusal names the commit.** When verify refuses, the runner reports `failed` with
  `trust_sha` (the head), `trusted_paths` (the trusted files it changes, old and new names) and
  `trusted_diffstat` (`git diff --stat` of those files against the base), and attaches the full
  diff of those files as evidence `trusted-files.diff` (cut at 1 MB). Nothing is pushed, as before.
  loopd records them on the item as `trust` (JSON column, migration `0013_trust.sql`,
  `pipeline.Trust`), replacing any earlier one.
- **The OK is the approver's, for that sha.** `ApplyTrust` (pipeline/trust.go) accepts it only in
  needs-you, only while a refusal waits for an OK, and only for the refused sha (`sha_mismatch`
  otherwise). The surfaces: the **Trust this commit** button on the needs-you message (its value
  binds the sha), a reply `trust` (or `trust it`, `trust <sha>`) in that thread (a bare `trust`
  OKs the latest refusal; a named sha must be a prefix, 7 or more characters, of the refused one,
  or the reply is refused and nothing is recorded), and
  `loop trust <id> --sha <sha>` (`POST /items/{id}/trust`, a user token only). Slack refuses anyone
  but the approver (`IsApprover`) and records the refusal. The OK moves the item back to build.
- **Once.** The runner's verify skips the refusal only when the item's trust has an OK, is not
  used, and is for exactly the head being verified. It then reports `trust_used: <sha>` with its
  outcome (`pr_opened`, `verify_failed` or `failed`), and loopd marks the OK used; loopd refuses a
  `trust_used` that is not the item's unused OK, or (on `pr_opened`) not the PR head. Any other
  head, including a later commit on the same branch, is refused again and needs a new OK.
- **The rebuild adds nothing.** The build after the OK runs the agent again (it writes the result
  the runner needs for the PR); its brief says the user OK'd that commit and to add no commit, since
  a new one voids the OK. The guard still refuses the agent's Write/Edit on `deny_write` paths: the
  OK only lets verify run a commit that already exists.
- **Land holds it.** The OK raises the item's risk to hold with the reason "trusted files changed
  (verify ran the agent's version on the user's OK)", so land holds the PR for the user's approval
  whatever the path rules say. The hold rules also gain `trusted-file` rules: jarvis-server's
  `deny_write` paths (`scripts/sandbox`, `scripts/screenshots`, `scripts/mobile-shot`,
  `tools/pipeline`) and jarvis-loop's verify gate (`internal/runner/verify.go`). They are a list of
  their own, so the built-in sets an earlier loopd wrote out are still recognised.
- **The needs-you message** shows the sha, the trusted files' diff stat in a code block, and where
  to look (`loop show <id>`, evidence `trusted-files.diff`); `loop show` prints the trust state and
  the diff stat.

## Not chosen

- A verify-only rerun without the agent: the runner needs a result (PR title and body) to open the
  PR, and the worktree's `.loop/` does not survive the run. The agent's rerun is cheap, and a
  commit it adds only costs a new OK.
- Overlaying the base's version of the trusted files: the point of the change is the agent's
  version, which the user looked at.
