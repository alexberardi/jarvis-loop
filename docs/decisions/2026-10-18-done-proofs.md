# Done proofs: every done item proves itself, with a morning report

2026-10-18 · item #216. **The user's goal (2026-10-18):** automate checking everything that has hit
done; doing it by hand would take hours. "Done" only meant merged and main's CI green. That day
134 items were done (jarvis-server 74, jarvis-loop 47, the rest across command-sdk, node-setup,
node-mobile, jarvis-docs and six `jarvis-cmd-*` packages), and the user had verified one. This
extends [QA proofs](2026-10-18-qa-proofs.md) (#194), which covered only the qa column. Each point is
enforced by a test (`internal/pipeline/doneproof_test.go`, `internal/store/doneproof_test.go`,
`TestStandupText`, `TestBoardVerifyFilter`) or a scenario (`scenarios/74-done-proof-sweep.json`,
`scenarios/75-done-proof-fails.json`).

## A verification status, separate from the stage

- **`items.verify`** (migration `0032_done_proofs.sql`, `pipeline.Item.Verify`, with
  `verify_reason`): `unproven`, `proving`, `verified`, `failed`, `needs-you`, `waiting`; `''` on an
  item that is not done. **A done item stays done**; only its status moves.
- **Set at the transition into done:** merged straight to done → `unproven`, or the status its
  build's proof gives (a command to run → `proving`); verified in qa (by the user or the proof) →
  `verified`; reopened in qa → `failed`.
- **Backfill:** every done item → `unproven`; one with a `verified` event → `verified` ("verified in
  qa"); one with a `reopened` event → `failed` ("reopened in qa as #N").
- **From a proof** (`pipeline.VerifyFromProof`): a check still to pass (or a command that failed
  once) → `proving`; only pi or e2e left → `waiting`; only manual left → `needs-you`; all passed →
  `verified`.

## The done sweep

- At loopd's tick (`store.OrchTick` → `doneSweep`):
  1. **Main-red items are verified by CI**: a done, unproven item the runner filed for main going
     red (`pipeline.MainRedItem`: source finding, title `main red in …` / `main red after #N`) is
     marked `verified`, reason "verified by CI: main's CI is its check", with no proof and no turn.
  2. **Batches of 10** (`DoneProofBatch`): one `done_proof_needed` trigger per item, all in one
     orchestrate turn, **lowest risk first**: no risk hold, then no hold-list paths, then size S, M,
     L, then by repo and id.
  3. **The next batch waits** until every item of the last one has its proof written and its first
     check run, or 6 hours passed (`DoneProofAskWait`): an item the orchestrator wrote no proof for
     stays `unproven` with the reason "the done sweep wrote no proof" and is not asked again.
     About 13 turns for 134 items, against the 150 a day turns cap.
- **The orchestrator** (`skills/orchestrator`, "Proofs for done items") writes the proof with the
  same `proof` action, which now also takes an unproven done item. It reads the ask, the PR, the
  **merged diff** and the evidence, and prefers checks that exercise the real thing **on main as
  deployed**: loopd usage events for jarvis-loop (events since the merge count, however old the
  merge is); pipeline scenarios, harness or sandbox commands and test names for jarvis-server and
  the app, with `e2e` for app flows; a test-install for `jarvis-cmd-*`; `pi` checks for the
  **jarvis-pi4** lab node only, never the kitchen or any prod node.
- **Checks run as for qa** (`pipeline.ProofDue` and `ApplyProofChecked` accept a `proving` done
  item): loopd evaluates the usage checks and the land-lane runner runs the commands, fenced, in
  a checkout of main, in its own proof loop (`runner.ProofEvery`), which takes no build slot.
  **At most 3 done items per pass** (`DoneProofsPerPass`, least recently checked first; every due qa
  item still goes first), so ten old items' 10-minute commands cannot hold the pass for an hour.
- **Outcomes**, actor `loop (proof)`, the stage untouched: all automatic checks passed →
  `verified` (`done_verified`); a command **failing twice in a row** → `failed` (`done_failed`) and a
  **new P1 item** in triage (`reopen_of` the done item, the failure and its evidence as the ask; the
  done item's reason ends "(filed #N)"); otherwise `done_proof_checked` with the new status. Pi
  and e2e checks stay `waiting` until a Pi runner and #195's E2E results exist.
- **The user can still verify** a done item that is not yet verified (`/loop verified N`, the
  same route as qa); it stays done.

## What the user sees

- **The morning report**: the 09:00 standup in `#loop-digest` gains a **Done items** section (only
  when there are done items): the counts per status, each failure with the item filed to fix it,
  and **Your checks**, the needs-you items, one line each on what to look at. It is in the
  standup rather than a message of its own: one morning post, no extra noise.
- **Threads stay quiet** on old items: a passing or still-proving check posts nothing. A proof the
  sweep writes posts one line ("Orchestrator: wrote its proof (the done sweep): …"), and a failure
  says what failed and which P1 fixes it.
- **The board**: each done card's label is its status, its line the reason; the Done column lists
  every status with its count (all done items, not only the last 14 days), and
  `/board?verify=<status>` shows every done item in that status, however old. The item page has a
  "Verified?" line above the proof.

## Also in this change

- `0025_proofs.sql` (#194) and `0025_usage.sql` (#199) merged with one number, so loopd refused to
  start on main. Main renamed usage to `0026_usage.sql` (#90); this change also keys both
  migrations on what they create (the `items.proof` column, the `usage_runs` table) rather than on
  version 25, which a database may have recorded for either one.

## Not done

- No report message of its own when a batch finishes; the standup carries it.
- Nothing runs pi checks yet (the Pi runner), and a regression run decides only qa items' e2e checks
  (#195), so done items with those checks stay `waiting`; deciding them from regression runs is a
  follow-up.
- The risk order uses triage's risk and size and the hold-list reason; it does not read the diff size.
- `loop show` and `/loop status` do not show the status (the board and the events do).
