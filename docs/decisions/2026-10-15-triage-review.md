# Triage and review agents (phase 5)

2026-10-15 · phase 5 (`docs/phases/05-triage-review.md`). Where the spec left room, and what was
chosen. Each is enforced by a test unless it says otherwise. **Not yet run with a real agent**: the
spec's "Real run" is the main session's, after merge.

## How loopd learns the configured repos: a `--repos` flag

`loopd --repos alexberardi/jarvis-loop,alexberardi/jarvis-server` (owner/name slugs, comma-separated,
checked and deduplicated at start-up; `TestParseRepos`). The store holds the list
(`Store.SetRepos`) and passes it to the pipeline rule, so `reproduced` accepts `repo` only when the
item has none **and** the value is one of these slugs (`TestReproducedRepo`, table-tested in
`internal/pipeline`). The Slack bot gets the same list for the add words.

Why the flag and not "the runner sends them":

- **loopd is the check, so it needs its own list.** The value being checked comes (through the
  runner) from an agent. If the runner also supplied the allowlist, the check would only be as good
  as the runner's word, and any runner token could widen it. A flag is set by whoever runs loopd,
  nothing at runtime can change it, and no new API surface is needed.
- **The Slack add words need the list where no runner is involved.** `/loop add … in jarvis-loop`
  is parsed by loopd's bot.
- Cost: two lists to keep in step (`runner.json`'s `repos` and loopd's `--repos`). A slug loopd
  accepts but no runner has fails visibly at build ("runner … has no repo"), into needs-you.
- Without `--repos`, triage cannot set a repo: `reproduced` with one is refused with "loopd has no
  configured repos (--repos)". **The live loopd needs the flag added to its unit before the real
  run** (not done here: the live service and its config were not touched).

The runner checks the agent's `repo` first against its own config (by key or slug; it reports the
slug), and requires one when the item has none; loopd checks it again. An item's repo is stored as
the slug, as `pr_opened` already did.

## Runner

- **Stages:** config `stages` (default `triage`, `build`, `review`; validated). Claims pass them to
  `POST /claim`. Land stays a script's (phase 6).
- **Per-stage working directories:** `<work>/<id>-<stage>`; run dirs `runs/<id>-<stage>-<time>`.
  - *Build:* unchanged (the item's branch, resumed from the mirror or remote).
  - *Review:* a detached worktree at the head loopd recorded at `pr_opened` (`item.head_sha`), not
    whatever the branch is now. Before posting, the runner checks the PR on GitHub is open at that
    head (`gh pr view`); if it moved, the review is not posted and the item goes to the user.
  - *Triage:* a plain directory with each repo in question checked out detached at its base under
    `repos/<key>/`: the item's repo, or every configured repo when it has none (the agent picks).
    The repo's `prepare` commands run only when there is one repo. **Known limit, not verified:**
    these are linked worktrees of the mirror, whose admin dirs (index) are outside the working
    directory; the OS sandbox may refuse index writes there (`git checkout -- f`, `git stash`).
    Reading, diffing, building and testing write only inside the working directory. Revisit after
    the real run if triage needs them.
- **Context files** (`.loop/context/`, listed in the brief's item part, never in the static part):
  triage: `repos.md` (key, slug, base, the new per-repo `description`), `open-items.md`
  (`GET /items?open=1`: id, title, stage, repo, PR; the item itself marked), `merged-prs-<key>.md`
  (last 30, `gh pr list --state merged --json number,title,mergedAt,files`, 40 paths each), and
  `evidence/`. Review: `pr.md` (`gh pr view`), `diff.patch` (`git diff origin/<base>...<head>` from
  the mirror, cut at 4 MB with a note), `evidence/` (triage notes, build evidence), and
  `previous-reviews.md` after a bounce. A context file that cannot be fetched is named in the brief
  ("Not available this run"), not fatal.
- **After a bounce, build gets `.loop/review/`:** `notes.md` (every `bounce` event's notes from
  loopd) and `pr-comments.md` (the PR's conversation comments, non-empty reviews and inline review
  comments, oldest first: `gh pr view --json comments,reviews` and `gh api --method GET
  repos/<slug>/pulls/<n>/comments?per_page=100`, read only). The runner fetches them because agents
  hold no GitHub credential. The re-run continues the branch and the runner updates the open PR
  (`gh pr edit`); `pr_opened` now carries `pr_action` `opened`/`updated`, and the thread says
  "updated" for the latter.
- **Results, per outcome:** the result file has one allowlist of fields per outcome (a build result
  carrying `repo` is refused, and so on). Triage: `reproduced` (`area` one line, `size` S/M/L,
  `risk` normal/hold, `resources` from the known four, `summary`, `notes`, `repo` as above),
  `not_reproducible` (`summary`, `notes`), `question`. Review: `approved` (`summary`, `review`) or
  `bounce` (`notes`; `review` defaults to the notes); `head_sha`, if given, must be the reviewed
  commit, and the runner reports loopd's head either way. Review has no `question` (loopd has
  none for review); an agent that cannot review fails into needs-you.
- **Triage notes are evidence:** the runner writes `notes` to `triage-notes.md` in the run dir and
  uploads it first (label "triage notes"), then `.loop/evidence/*`. That also meets loopd's "at
  least one evidence file" for `reproduced` and `not_reproducible`; build and review then find it
  in `.loop/triage/` and `.loop/context/evidence/`.
- **The review on the PR:** posted with `gh pr comment --body-file` (never `gh pr review`, never
  `--approve`), before the outcome is reported; a failed post fails the run. The comment is the
  verdict heading, the agent's review, for a bounce the notes ("What to change"), and a footer: the
  reviewed commit, "not the one that built it", "a comment, not a GitHub approval", the evidence
  files and the run's numbers.

## Read-only triage and review

- `loop-guard` config `read_only` (set for triage and review): `git commit`, `commit-tree`, `merge`,
  `rebase`, `cherry-pick`, `revert`, `am`, `update-ref`, `filter-branch`, `replace`, `notes`, and
  `tag` other than listing are refused as `git_commit`, in every spelling the parser already
  handles (`-C`, `-c`, `bash -c`, `$(…)`, absolute paths). Checking out files, stashing and diffing
  stay allowed: a reviewer reverts the fix to see the new test fail.
- The generated settings for those stages add deny rules for the same subcommands. Settings are per
  repo and stage (`settings/<repo>-<stage>.json`; `settings/triage.json` when triage has no repo),
  so each stage keeps one byte-identical file. **The build settings did not change** (the existing
  golden files are byte-identical); the build brief did (the skill's bounce paragraph and a
  `- Stage:` line), so the first build run after this misses the prompt cache once.
- Nothing a review run does reaches GitHub: the runner never pushes from triage or review.

## Prompt prefixes

`StaticBrief(stage)` is the stage skill plus its result contract; `Brief` appends the item part
after `## The item`. Golden files `internal/runner/testdata/brief_{triage,build,review}.golden.md`
pin each stage's static part; `TestStagePrefixesAreStable` prepares two items per stage and checks
their briefs start with exactly that, with identical settings bytes and argv. The runner writes
each run's argv to `argv.json` in its run dir, and the harness's `same_prefix` expectation checks
the same across real runner runs (scenario `stage-prefixes`).

## Slack

- **Repo words** (`ParseRepo`, table-tested next to the priority words): a configured repo named by
  its short name or full slug, case-insensitive, as `in <name>` anywhere, `<name>:` at the start, or
  `[<name>]` anywhere. The first marker wins and only its words leave the title; a name inside a
  longer word (`jarvis-loopd`), a possessive or a path (`jarvis-loop's`, `jarvis-loop/runner`), or
  an unconfigured repo is not a marker. A short name two configured repos share counts only as a
  full slug. Repo words are read before priority words (`jarvis-loop: low priority x`).
- Wording: `/loop add` answers "Added #N (P3, owner/name): …" when a repo was set; triage's
  reproduced line reads "Reproduced in owner/name: …" when triage set the repo; a rebuilt PR reads
  "… updated, head …". The help text names the repo words.

## Harness

- The sandbox has a second scratch repo (`sandbox/jarvis-loop`, key `jarvis-loop`) and loopd's
  `--repos` equivalent; the runner claims all three stages by default, as a real one.
- Scenario format additions: scenario `stages` (the runner's stages; `priority` pins `["build"]`,
  since a runner that reviews takes the P1's review before the P3's build); `script` step `for`
  (triage, build, review) and `picks` (the repo triage picks); `add` `via: slack` takes `text` (the
  words as typed); expectations `repo` (alias → the item's repo), `same_prefix` (stages), and PR
  `repo`, `edits`, `comments`, `comment`. The fake claude plays `reproduce`, `duplicate`,
  `triage_question`, `triage_bad_repo`, `approve`, `bounce`, `review_commit`, and its build play
  addresses `.loop/review/` when present.
- New scenarios: `triage-picks-repo`, `triage-duplicate`, `review-bounce`, `review-no-commit`,
  `second-bounce`, `stage-prefixes`.

## Not verified

- Any of this with a real agent (the spec's real run).
- `gh pr list --json files`, `gh pr view --json comments,reviews`, `gh api …/pulls/<n>/comments` and
  `gh pr comment` against real GitHub: only against a fake `gh` (`TestGHReads`) and the harness's
  fake GitHub. The field names were checked against the installed gh 2.102.0's `--json` list.
- The triage checkouts' index writes under the OS sandbox (above).
