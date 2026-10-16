# Phase 6: the land script

**As built (2026-10-15):** `docs/decisions/2026-10-15-land.md`. In short: the gates are a pure
`runner.Decide` the runner re-feeds from loopd, GitHub and git on every pass; CI comes from the
PR's status rollup; land checks loopd's hold rules on the paths it computed *before* merging;
main's CI is watched in the background after the lease ends (not on it); docs-only conflicts are
resolved with git's union driver; jarvis-loop's rules add `tokens`; scenarios 16–25 (21 is two
files, plus `ci-timeout`, `merge-refused`, `merged-by-hand`). Notes below mark where the text
changed.

Goal (DESIGN.md, Rollout 6): an approved item merges without anyone in the middle. Today an item
that review approved, or that the user approved in Slack, waits in `land` until someone merges by
hand. **Proves:** end to end, from intake to a green `main`, with the user only pressing a button
for held items.

Found on the first real run (item #2, 2026-10-15): the user pressed **Approve merge** and nothing
merged, because no code claims `land`. And the main session cannot stand in for it: Claude Code's
permission classifier refuses a merge whose approval is not in the conversation, which is right for
an agent. Landing is therefore **plain code, no agent**: no `claude` process, no model, nothing an
agent writes is read.

## Where it runs: the runner, stage `land`

- `loop-runner` claims `land` when its config `stages` include it (default stays `triage`, `build`,
  `review`; the dev box's config adds `land`). It holds the GitHub login (gh) already; loopd still
  holds none.
- loopd's limit for land stays 1 at a time.
- Land does not take one of the runner's agent `slots`: it has its own lane, one item at a time,
  next to them, and a paused runner still lands
  ([docs/decisions/2026-10-16-land-lane.md](../decisions/2026-10-16-land-lane.md)).
- A land claim runs `Land(ctx, item)` in-process: no worktree for an agent, no settings, no guard.
  It uses the runner's bare mirror for git and `gh` for GitHub, outside any sandbox.

## The gates, checked in this order, every time

Everything is re-derived from GitHub and git, never from an agent's words.

1. **The PR:** `gh pr view <n> --json state,headRefOid,baseRefName,mergeable,mergeStateStatus,isDraft`.
   Open, not draft, base = the repo's configured base. Merged already (by hand) → advance `merged`
   with its merge commit (idempotent). Closed → `failed` ("PR closed").
2. **Review approval for this head:** the item's last `approved` review event names `head_sha`; it
   must equal the PR's `headRefOid`. A head that moved since review → `back_to_build` is wrong (the
   build may be fine); add a land outcome **`back_to_review`** (loopd: land → review, notes required)
   so a new head gets a fresh review.
3. **Changed paths:** `git diff --name-status origin/<base>...<head>` in the mirror after a fetch
   (the runner computes them, as at `pr_opened`). Sent with every land outcome; loopd recomputes risk.
   *As built:* with every outcome but `failed`; a head missing from the mirror is fetched from
   `refs/pull/<n>/head`.
4. **CI:** every check on the head (`gh pr checks <n> --json name,state,bucket` or the status
   rollup; *as built:* the rollup, in the same `gh pr view` as the head, bucketed as gh does;
   the failing checks go in `back_to_build`'s `reason`, which loopd requires). All `pass` → go on. Any `fail`/`cancel` → `back_to_build` with the failing check names
   and links as notes. *As built (2026-10-15, item #18): it also sends `ci_head_sha`, the head that failed;
   loopd counts it as a bounce, and a build that reports `pr_opened` with that same head goes to
   needs-you instead of review (`docs/decisions/2026-10-15-ci-red-unchanged-head.md`).* Any pending → wait: poll every 30 s with heartbeats, up to `land_ci_wait`
   (config, default 30 min), then `failed` ("CI still pending after 30m"). No checks at all → wait
   the same way (a repo with no CI configured is a config error, not a pass).
5. **Harness evidence:** the item has at least one evidence file from build (`pr_opened`) and the
   PR body contains the runner's Verification footer. Missing → `back_to_build` with that note.
   *As built:* the body must **end** with the footer's trailer line.
6. **Hold:** if loopd's risk is `hold` (paths, triage, `needs_review`, or an earlier land hold)
   *or, as built, loopd's hold rules (from `GET /control`) match the paths land computed; loopd's
   own check at `merged` comes after GitHub merged, so this one is what stops a merge*:
   - an approval whose `head_sha` equals the PR head → go on;
   - otherwise → `hold` with `head_sha` and the reason (loopd's hold reasons), and stop. The bot
     already posts the held message with the buttons; the user's approval (after the 2-minute
     Cancel window) moves the item back to land, and the next claim finds the approval.
7. **Mergeable:** `mergeable == CONFLICTING` → if every conflicting path is under `docs/`, merge
   `origin/<base>` into the branch in a scratch worktree, resolve by taking both sides only if git
   resolves it (`-X` is not used; a real conflict anywhere → `back_to_build`; *as built: a
   conflict only under `docs/` is resolved by git's union driver, both sides kept*), push the branch
   (the runner's push rules: only `loop/<item>-…`, never forced), and report `back_to_review`
   (new head). `UNKNOWN` → poll like CI. Behind but clean → merge as is (no required up-to-date
   branch in these repos; main's CI after the merge catches semantic breaks).

## Merge

- `gh pr merge <n> --repo <slug> --squash --delete-branch --match-head-commit <head>`. Never
  `--auto`, never `--admin`. A refusal (head moved, not mergeable) → re-run the gates once from
  the top; a second refusal → `failed` with GitHub's message.
- Read the merge commit (`gh pr view --json mergeCommit`), advance `merged` with `head_sha`,
  `merge_sha` and the changed paths. loopd already refuses `merged` for a hold item without a
  matching approval (409 `approval_required`); keep that test.

## After the merge: main's CI

- Poll the workflow runs on the merge commit (`gh run list --commit <merge_sha> --json
  status,conclusion,url`) until all complete, up to `land_main_wait` (default 30 min), heartbeating
  on the item's lease. *As built: loopd ends the lease at `merged`, so the watch runs in the
  background with no lease; "merged by a run of this runner" is checked against the `merged`
  event's actor.* The item is already `done`; record the result as an event (`main_ci`:
  `green`/`red`/`timeout`, with the run URLs) through a new runner-allowed endpoint
  (`POST /items/{id}/events` with kind `main_ci`, runner token, only for items this runner merged).
- **Red:** the runner creates a **P1** item (`POST /items`, source `finding`, repo the same, title
  "main red after #<pr>: <failing job names>", ask with the run URLs and "caused by item #N /
  merge <sha>"). The bot posts the P1 item thread with an @mention of the approver (only the
  approver, as the must-not rules allow).
- Timeout → the same event with `timeout`, and a digest line; no item.

## Per-repo hold rules (needed before the loop lands its own PRs)

Today's hold rules are jarvis-server's paths, applied to every repo. jarvis-loop's own merge rule
(its CLAUDE.md) must hold PRs that change who may approve or merge, token handling and the landing
gates, or the land script would merge changes to itself.

- `HoldRule` gains optional `repo` (slug); a rule with a repo applies only to that repo's items, a
  rule without one to all. Existing rules get `repo: alexberardi/jarvis-server`.
- Defaults for `alexberardi/jarvis-loop` (name, glob):
  - `approval`: `internal/pipeline/**` (the stage rules, approvals, hold risk)
  - `land`: `internal/runner/land*.go`, `internal/runner/pr.go`, `internal/runner/git.go`
  - `api-auth`: `internal/api/**`
  - `store`: `internal/store/**` (approvals, hold rules and their migrations are stored there); `loopd-wiring`: `cmd/loopd/**` (token loading, `--repos`) — added in review
  - `slack-approver`: `internal/slack/**` *only* where the approver check and the reply
    classifier live; if they are not in their own files yet, move them (`internal/slack/approve.go`)
    in this phase so the rule can be narrow (*as built: `internal/slack/approve*.go`*)
  - *as built, added:* `tokens`: `internal/slack/env.go`, `internal/runner/config.go`,
    `internal/runner/client.go`
  - `guard`: `internal/guard/**`, `cmd/loop-guard/**`, `internal/runner/settings.go`
  - `ci`: `.github/workflows/**`
  - `deleted-test`: `**/*_test.go` (D, R); `scenario-removed`: `scenarios/**` (D, R)
- jarvis-loop's CLAUDE.md "Merging" rule is replaced by "the loop lands its own PRs under these
  rules" once this merges.
- A test: item #2's actual paths (`cmd/loop/main.go`, `internal/slack/bot.go`, `commands.go`, two
  scenarios) are **not** held; a change to `internal/pipeline/approve.go` (or wherever the approval
  check lives) **is**.

## Slack

- Land outcomes post in the item's thread: "Merged #7 (ff0f006); watching main's CI", then "main
  green" or the red P1 link. *As built: "Merged <PR link> as `ff0f006`; watching main's CI.", then
  "main green on `ff0f006`." or "main red on `ff0f006` (failing: test): filed #12 (P1)."* `back_to_review` / `back_to_build` from land say why in one line.
  Pending CI does not post (no noise); a wait over 10 minutes edits the parent's stage line.

## loopd changes

- Outcome `back_to_review` (land → review, `notes` and `head_sha` required).
- `POST /items/{id}/events` kind `main_ci` (runner token; the item must be `done` and merged by a
  run of this runner; data `result`, `urls`), shown in `loop show` and the digest.
- Per-repo hold rules as above, with a migration that tags the existing rows.

## Harness (scenarios first)

The fake GitHub gains checks per head (pending/pass/fail, scriptable over time), mergeable state,
merge (with `--match-head-commit` semantics: refuses on a moved head), merge commits, and workflow
runs on `main` per commit.

16. **Auto land:** review approves → CI pending → pass (fake clock) → merged → main green → done,
    thread shows both lines; `remote.main` moved to the squash commit.
17. **Held then approved:** an auth-path PR → land holds → approver presses Approve → 2-minute
    window → land claims again → merged. *(As built: approves by reply; the button applies at
    once, with no window.)* No agent run at land (fake claude never called for land).
18. **Approval for an old head:** approve, then a new commit lands on the branch → land →
    `back_to_review`, the approval is void, no merge.
19. **CI red** → `back_to_build` with the check name in the notes.
20. **Main red after merge** → item done + `main_ci red` event + a new P1 item whose thread
    mentions the approver.
21. **Conflict outside docs** → `back_to_build`; **conflict only in docs** → branch updated →
    `back_to_review`. *(As built: two files, `conflict-code` and `conflict-docs`.)*
22. **Someone else's approval** never lands (re-check of scenario 4 through land).
23. **Self-hold:** a jarvis-loop PR touching `internal/pipeline/` is held even with review approved.

Unit tests: the gate order as a table (PR state × review head × CI × evidence × risk × approval ×
mergeable → outcome); the CI rollup parser on recorded `gh` JSON; per-repo rule matching.

## Rollout on the dev box (main session, after the user's "merge it")

This PR changes the landing gates: **held for the user.** After merge: rebuild, add `land` to the
runner config's `stages`, run the runner as a systemd user service (`loop-runner.service`, so
nothing merges from inside a Claude Code session), and replace jarvis-loop's CLAUDE.md merge rule.
