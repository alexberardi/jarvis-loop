# A PR that does only part of the ask does not go to done

2026-10-20 · item #353. **Seen:** #299 (MLX: engine, HF directory install, admin UI, catalog)
merged PR #337, titled "part 1 of 3: decision, install, supervision", and went to done. Parts 2
and 3 were never filed; the user found out by trying MLX on rc17 for a real user. Nothing in the
loop read the title: review approved what the PR did, land checked review, CI, evidence and hold.
What was chosen, each covered by a test or a scenario:

- **Land gate 3c, "part of the ask"** (`runner.Decide`, `PRPart`). Land reads the PR's title
  (`landViewFields` now asks gh for it). A title, or the agent's part of the body (before the
  runner's Verification footer, outside quoted lines and code blocks), saying "part N of M" or
  "part N/M" with N<M is part of the ask. It merges only when the item has at least M−N open items
  filed after it (`loop add --after`; GET /items/{id} now carries `followed_by`). Otherwise land
  reports `back_to_build` with "the PR says part N of M, and the remaining … are not filed": the
  build finishes the ask, or returns a question naming the parts to file. The gate comes after
  a conflict (which never merges anyway) and before CI, so a PR with pending CI is not waited on.
  The title wins over the body; in the body the first mention counts. A PR merged by hand that
  says it is part of the ask, with the rest unfiled, is `failed` (the user decides), not `merged`.
- **Bounce, not file the parts from land.** The ask allowed either. Land is plain code with no
  agent, and filing items from it means the land lane holding a token that can add items, and
  inventing titles and asks for the remaining parts from a PR title. A bounce keeps the split a
  decision someone makes with the words in front of them: the build agent proposes the parts in
  its question, the user or the orchestrator files them `--after` the item (same priority and
  feature is theirs to set), and the next land merges. The orchestrator's note is the question
  itself, in the item's thread.
- **Review lists the ask's steps** (`ask_steps` on `approved` and `bounce`, `UnfiledSteps`,
  `BounceUnfiled`). Each step of the ask is `done`, `not_done` (with a reason, and `follow_up`:
  the item filed for it) or `out_of_scope` (with a reason). The runner checks every `not_done`
  step's follow-up against the item's `followed_by`; an approval with one not filed is posted
  and reported as a bounce, its notes listing the steps. The PR comment shows the steps as a table.
  The field is optional in the contract (older results and the scenarios' fake reviews leave it
  out); the review skill asks for it every time. The review brief lists the items filed after the
  item, so the agent can name them.
- **Proof of a user-visible ask.** The build skill: when the ask describes something the user
  does (install an MLX model from the Models page), the proof names that path, an `e2e` flow or a
  `manual` look, not only unit tests. Review checks it and bounces without it. Not checked
  mechanically: which asks are "user-visible" is a judgment.
- **Not done:** loopd's `merged` rule does not re-check the parts (the runner's gate is what stops
  the merge, as for hold: by the time loopd hears `merged`, GitHub has merged). Build's free-text
  `not_verified` is not parsed for undone steps: review's `ask_steps` is the structured check.

Scenarios 87 (`partial-pr-stays-out-of-done`, `review-undone-step-bounces`).
