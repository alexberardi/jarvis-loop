# A freeze follows the red main, not one item id

2026-10-23 · item #558. **Seen (2026-10-23):** main went red twice in a row in
`alexberardi/jarvis-loop`, so two main-red P1s were filed (#539 for PR #128's merge, then #545 for
PR #130's). Triage read #545's title, saw the byte-identical #539 and closed #545
`not_reproducible` with "duplicate of #539". But the repo's freeze named **#545**, and
`Frozen.Blocks` let through exactly that one id: once it closed, *nothing* in the repo could move —
including #539, the live P1 whose fix was open as PR #133 with CI green. The repo sat deadlocked
until `loop unfreeze alexberardi/jarvis-loop` was run by hand. The freeze did the opposite of its
job: it is there to make the fix go first, and it blocked the only fix.

The freeze named one item id and nothing re-pointed or cleared it when that item stopped being
open: `store.mainRed` was the only code that ever re-pointed a freeze, and it needs a fresh red
`main_ci`, which needs a fresh merge — which the freeze itself forbids. Closing (triage) and
dropping (`loop drop`) are the two ways in; both were reproduced on `main` before the change.

What was chosen, each covered by a test or a scenario:

- **A freeze lets every main-red P1 of its repo through, not one id.** `pipeline.IsMainRed` (a P1
  whose source is `finding` and whose title is a main-red title, `MainRedItem`) is the invariant:
  `Frozen.Blocks` never holds one, and `Frozen.Fixes` is true for one, so `chooseFix` gives it the
  same first-before-everything claim as the named item. Narrowed to the loop's own main-red
  findings at P1 so no unrelated P1 slips through a freeze. A freeze can therefore no longer
  deadlock the repo's live main-red P1, whatever happens to the id it names.
- **A second red never files a twin.** `mainRed` already reused the P1 of a live freeze; it now
  also takes over the repo's own **open** main-red P1 when there is no live freeze (after a hand
  unfreeze, or a green then a red again), with `reused` on the `main_ci` and `frozen` events,
  instead of filing a second P1 with a byte-identical title. That twin was what made the duplicate
  closure inevitable (scenario `freeze-no-twin-p1`).
- **A freeze whose item settles hands over** (`store.freezeItemSettled`, called from the advance
  path's close and from `Store.Drop`; event `freeze_handover` on the item that takes it over, in
  #loop and in that item's thread). In order:
  1. **A duplicate closure hands the freeze to the item it duplicates** — the incident. The target
     is read out of the closing summary with `pipeline.DuplicateOf` ("duplicate of #N", the form
     the triage brief asks for and `ApplyReopenNotReproducible` already keyed off), and taken only
     when it is an open item of the same repo.
  2. **Else any other open main-red P1 of the repo** takes it over.
  3. **Else the P1 is re-filed** from the closed one's title and ask, and the freeze follows it,
     keeping its `red_since` and jobs. It does **not** lift: main is still red, and lifting drains
     `queued_advances` and resumes merges onto a red main, the opposite of the freeze's job.
  4. **Only when the re-filed one settles too does the freeze lift**, with the reason ("the
     re-filed P1 #N closed too; no main-red item left to move"). One re-file per freeze row
     (`freeze_handover` with `refiled`) is the guard against re-filing for ever: a second closure
     is the user or triage saying, twice, that this item is not how main gets fixed.
- **Duplicate is not "not reproducible".** The distinction now decides whether the freeze hands
  over (the ask stands, tracked elsewhere) or the P1 is re-filed (the ask does not stand), so the
  triage brief says it in as many words: close as a duplicate only with a summary that starts
  `duplicate of #N`, because loopd reads that id. A `duplicate_of` field on the `not_reproducible`
  outcome, which would remove the summary parsing, is the follow-up; the prefix is what runners
  write today.
- **Surfaces.** `#loop` gets the handover post (who moves now, why, and that the repo is still
  frozen), the new mover's thread gets its line, and `loop status` and the board banner no longer
  say "only #N moves": #N goes first, as does any other main-red P1 of the repo.

**Not done:** a freeze whose item reaches **done** while main's CI never decides (`main_ci`
reporting `superseded` or `timeout`) still holds the repo until the user lifts it by hand. Nothing
closed there, so none of the paths above fire, and re-filing a P1 would be wrong when main may well
be green. Deciding what an undecided main watch should do is its own change.
