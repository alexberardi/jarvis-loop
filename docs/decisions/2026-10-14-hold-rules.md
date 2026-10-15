# loopd computes hold risk from the PR's changed paths

2026-10-14 · phase 2 (loopd core), after the review of PR #1

**Problem.** A PR on jarvis-server's hold list needs the user's approval before it merges. In the
first cut, `risk=hold` was whatever the triage agent reported. A mislabelled auth change would
then merge without the user. Also, the merge guard compared the approval with the head loopd last
recorded, but the land stage may push (for example merging `main` in) without telling loopd, so an
approval of commit A could cover commit B.

**Decision.**

- `pr_opened`, and land's `merged` and `hold`, carry `changed_paths`: the PR's files in
  `git diff --name-status` form (`M path`, `A path`, `D path`, `R[score] old new`; the words
  added/modified/deleted/renamed also work). Paths must stay inside the repo.
- Control holds **hold rules** `{name, glob, statuses?}`. `PUT /control {"hold_rules": [...]}`
  replaces the list and is persisted. An empty column means the built-in defaults, so the defaults
  live in one place (`pipeline.DefaultHoldRules`).
- **Glob matcher:** paths are split on `/`. A whole `**` segment matches zero or more segments, `*`
  matches any run of characters within one segment, and everything else is literal. A rename matches
  on its old or its new path.
- **Risk only ratchets up.** An item is `hold` if any of these holds, and it never goes back:
  triage said `hold`, a rule matches, `needs_review: true` came with `pr_opened`, or land held it.
  Each reason is appended to `hold_reason` (for the Slack message). The rule matches are recorded
  on the event (`hold_matches`).
- `merged` must name the `head_sha` it merged. loopd recomputes risk on the merged paths. For a hold
  item it requires `approval.head_sha == head_sha`; otherwise 409 `approval_required` and the item
  stays in land under its lease.
- Land's `hold` carries `head_sha`. It becomes the item's head, and an approval for another head is
  void, so the held message always shows the commit the user would approve.

**Default rules** (jarvis-server's CLAUDE.md hold list):

| name | glob | statuses | why |
| --- | --- | --- | --- |
| `auth` | `internal/modules/auth/**` | any | auth or access |
| `authn` | `internal/platform/authn/**` | any | auth or access |
| `update` | `internal/update/**` | any | signing and the update helper |
| `upgrade` | `cmd/jarvisd/upgrade*.go` | any | the upgrade path |
| `release` | `.github/workflows/release.yml` | any | signing / releases |
| `ci` | `.github/workflows/**` | any | CI checks |
| `deleted-test` | `**/*_test.go` | D, R | a weakened test |
| `contract-removed` | `contract/**` | D, R | a weakened contract check |

`deleted-test` also counts **renames**, which is stricter than "a deleted test": renaming
`x_test.go` to `x.go` removes a test just as well. Renaming one test file to another is held too;
that costs a button press, which is the safe direction. Prod changes and the "needs review" mark
have no path signature: prod is outside the repo, and the mark is the `needs_review` flag.

**Limits.** loopd trusts the runner to report the paths truthfully. Runners hold the GitHub
credentials and loopd holds none, so loopd cannot check the diff itself. A runner that lies about
the diff could still skip the hold. The land script (rollout phase 6) should compute `changed_paths`
from `gh pr diff --name-only` / `git diff --name-status` rather than from an agent's words.
