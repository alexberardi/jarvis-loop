# Phase 5: triage and review agents

Goal (DESIGN.md, Rollout 5): the two agent stages around build run unattended, so the main session
no longer does them by hand (as it did for item #1). **Proves:** bad fixes and stale reports get
caught before the user sees them.

**As built (2026-10-15):** `docs/decisions/2026-10-15-triage-review.md`: loopd learns the
configured repos from a `--repos` flag (the live loopd needs it before the real run), triage's
working directory (the repos checked out under `repos/<key>/`), the read-only guard rule, the
per-stage prefix goldens, the review comment, and the harness additions.

## Runner

- The runner claims `triage`, `build` and `review` (config `stages`, default all three). Same four
  boundary layers, same caching rules (one byte-identical prefix *per stage*), same result-file
  contract: agents hold no loopd or GitHub credentials; the runner does every loopd and GitHub call.
- **Context the runner gives the agent** (files under `.loop/context/`, listed in the brief, the
  item details last as today):
  - triage: the configured repos (key, slug, a one-line description from config), a snapshot of
    open loop items (id, title, stage, repo) and the last 30 merged PRs of the target repo
    (number, title, merged date, changed paths) for dedupe; the item's ask and any evidence.
  - build after a bounce: the review notes from loopd's events and the PR's review comments
    (fetched by the runner with `gh`), in `.loop/review/`.
  - review: the PR (number, title, body, head sha), the item's build evidence, the triage notes,
    and the diff (`git diff origin/<base>...<head>` from the mirror).

## Triage (`skills/triage/SKILL.md`)

The agent decides, with evidence:
- **Which repo** (from the configured list) when the item has none. This fixes items added from
  Slack, which have no repo; loopd's `reproduced` outcome gains an optional `repo` field, valid only
  when the item has none and the value is a configured repo slug.
- **Still real on `main`?** Reproduce with the target repo's harness where possible (sandbox name
  `loop-<item>`), else by reading the code; for features, check it doesn't already exist.
- **Already done or in flight?** Against the open items and recent merges. Duplicate → `not_reproducible`
  with "duplicate of #N / PR #M" and the evidence.
- Then `reproduced` with area, size, risk (its own judgement; loopd still computes hold from paths
  at build and land), resources (`gpu`, `voice`, `emulator`, `macos` as the verification needs), a
  summary, and **triage notes** (what to change, where, how to verify) as evidence.
- Unclear ask → `question`.

## Review (`skills/review/SKILL.md`)

A different agent from the builder (loopd already enforces it). The worktree is the PR's branch at
its head sha, read-only in spirit: the agent must not commit (the guard refuses `git commit` in
review runs). It:
- reads the diff, the triage notes and the PR body;
- **judges test coverage meaningfully** (the user's merge gate): do the tests exercise the new
  behaviour and its failure cases, would they fail without the change; not "do tests exist";
- re-runs the verification the PR claims (`scripts/check`, the named scenarios or shots) and
  compares;
- returns `approved` (summary, head_sha) or `bounce` (notes: specific, actionable, each with file
  and line where possible).
- The runner posts the review on the PR as a comment (`gh pr comment`, or `gh pr review --comment`;
  never `--approve`, which the user's own account cannot do on its own PR anyway).

## Slack repo default

`/loop add` and mentions: if the text names a configured repo (`in jarvis-loop`, `jarvis-loop:`,
`[jarvis-loop]`), set it and strip the words; otherwise leave the repo empty for triage to decide.
Table-tested like the priority words.

## loopd

- `reproduced` accepts `repo` (above).
- `GET /items?open=1` for the runner's snapshot (or reuse the list endpoint).
- No other changes.

## Harness (loop-sandbox scenarios, test-first)

1. Triage with no repo → the fake agent picks `jarvis-loop` → item gets the repo → build.
2. Triage duplicate → `not_reproducible` naming the duplicate → closed, thread says why.
3. Review bounce → build re-run gets `.loop/review/` notes → the runner **updates the existing PR**
   (no second PR) → review approves → land.
4. Review refuses to commit (guard) → refusal event, review still completes.
5. Second bounce → needs-you with the notes.
6. A run's prefix check per stage (golden): triage, build, review each byte-stable across items.

## Real run

After CI and the scenarios are green, run ONE real item through triage → build → review on the dev
box with the real runner: the main session files a small jarvis-loop item from Slack-style input
with no repo (e.g. "`/loop add` show the item's repo in `/loop show` and the thread header"), and
lets triage, build and review agents take it to `land`. Landing stays by hand until phase 6. The
PR touches loopd and Slack code → held for the user per jarvis-loop's CLAUDE.md; stop at held.
