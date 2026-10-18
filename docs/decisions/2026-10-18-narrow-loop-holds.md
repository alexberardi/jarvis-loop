# jarvis-loop's hold rules name files, not packages (2026-10-18)

Item #204. jarvis-loop's hold list (`pipeline.loopRules`) held every PR touching
`internal/pipeline/**`, `internal/api/**`, `internal/store/**` or `cmd/loopd/**`, which is nearly
every loop PR. Overnight 2026-10-17/18 six reviewed, green PRs sat held and, with QA-gated
dependents behind them, all six agent slots were idle from 02:15 to 08:04. The user approved
narrowing the list.

## Decision

A loop PR that passed review and CI lands on its own unless it changes one of these:

| Rule | Files | What |
|---|---|---|
| `approval` | `internal/pipeline/approve.go`, `internal/pipeline/trust.go`, `internal/api/approve.go`, `internal/store/approve.go` | who may approve or merge: the outcome-to-stage table, review approval (head, not the builder), the merge gate (risk on the merged paths, the approval for that head), the user's approve and hold; the trusted-file OK |
| `hold-rules` | `internal/pipeline/holdrules.go`, `internal/store/control.go` | the hold rules and the control row that keeps them |
| `api-auth` | `internal/api/auth.go`, `internal/api/onbehalf.go`, `internal/store/approver.go`, `cmd/loopd/main.go` | tokens and the bearer middleware, the runners-only listener, the orchestrator acting for the user and the approver-message check it spends, loopd's token loading and listeners |
| `slack-approver` | `internal/slack/approve*.go` | unchanged |
| `tokens` | `internal/slack/env.go`, `internal/runner/config.go`, `internal/runner/client_auth.go` | token handling |
| `guard` | `internal/guard/**`, `cmd/loop-guard/**`, `internal/runner/settings.go` | unchanged |
| `land` | `internal/runner/land*.go`, `pr.go`, `git.go` | unchanged |
| `trusted-file` | `internal/runner/verify.go` | unchanged (`trustedFileRules`) |
| `ci`, `deleted-test`, `scenario-removed` | `.github/workflows/**`, deleted or renamed `*_test.go` and `scenarios/**` | unchanged |

The approval, auth and token code sat inside the big shared files (`pipeline.go`, `api.go`,
`store.go`, `orchestrator.go`, `client.go`) that most PRs touch, so it moved out first, with no
change in behaviour:

- `ApplyAdvance`'s outcome-to-stage table, the review approval, the merge gate and voiding an
  approval for another head, and `Approval`, `ApplyApprove`, `ApplyHold` went to
  `pipeline/approve.go`. `ApplyAdvance` calls them; the rest of it (fields, evidence, the other
  outcomes) stays in `pipeline.go`.
- `api.go`'s bearer middleware (`auth`, `BearerToken`, `PrincipalFrom`, `NewRunners`) went to
  `auth.go`; the approve, trust and hold handlers to `approve.go`; `orchestrator.go`'s on-behalf
  rules (`onBehalfRoutes`, `actForUser`, `controlForUser`) to `onbehalf.go`.
- `store.go`'s `Approve`, `Trust`, `Hold` went to `approve.go`; the control row and the built-in
  hold rules (`loadControl` to `SetControl`, `SetDefaultHoldRules`) to `control.go`;
  `orchestrator.go`'s `UseApproverMessage` to `approver.go`.
- `runner/client.go`'s request with the token went to `client_auth.go` (`send`): the item named
  `client.go` as token handling, but #201 changes its claim body, and the token is set in one
  place, now that file.

With this list #193 (deps at merge), #201 (flow panel), #202 (CLI) and #192 (slot hold) land without
a hold (`TestRecentLoopPRsAreNotHeld`, their full path lists).

**The old built-in sets stay recognised.** jarvis-loop's list as it was before #204 is frozen as
`loopRulesV1` and never edited. `EarlierDefaultHoldRules` lists every built-in set an earlier loopd
shipped (jarvis-server alone, with `loopRulesV1`, the 6b list, and the 6b list with the trusted
files), so a database carrying a copy of one goes back to the live defaults when loopd opens it.

## Trade-offs

- **The route table stays unheld.** `routes()` in `api.go` names each route's token kinds, and #201,
  #202 and #192 added routes there. A PR could open a route to the wrong kind without a hold;
  `TestApprovalAndControlRoutesAreTheUsers` pins approve, trust, hold, plan approve, `PUT /control`
  and unfreeze to the user's token, and that acting for the user never reaches an approval, so such
  a change fails CI. Deleting that test is held; editing it is caught only by review.
- **Migrations are unheld.** #192 and #201 each added one. A path rule cannot tell a migration
  that touches `control` or approvals from one that adds a table; review has to.
- **Plan approvals are not on the list.** `ApplyPlanApprove` (`pipeline/plans.go`) and
  `store.ApprovePlan` approve a plan's brief or blueprint, which files items; those items go
  through every gate, the hold rules included. The route is pinned to the user above.
- **The rest of `ApplyAdvance` is unheld,** including where `pr_opened` raises hold risk. Land
  re-checks the hold rules itself (`land*.go`) and `merged` recomputes risk on the merged paths in
  `checkMerge` (held), so a dropped raise there still cannot merge a held change.
- **`cmd/loopd/main.go` stays held whole.** Token loading, the orchestrator's tokens and which
  listener serves which tokens are woven through `run`, and no recent PR needed it.
- **What a rule's name says changed:** `store` and `loopd-wiring` are gone (`hold-rules`,
  `api-auth`), so held messages read differently.
