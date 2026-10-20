# More builds to the Qwen builder: routed at any pace, one at a time, back to claude on a bounce

2026-10-19 · item #305. The user: expand Qwen builds a little to save Claude usage. Follows
[2026-10-16-qwen-builder.md](2026-10-16-qwen-builder.md) (OpenCode against prod's background
label) and [2026-10-18-usage-awareness.md](2026-10-18-usage-awareness.md) (routing only under a
claude pace).

## The record so far (the user's numbers)

- 6 qwen-built items, every PR approved on its first review (#38 #39 #44 #45 #48 #249).
- About 23 min per qwen PR against about 12 for Claude. The only failures were runner setup.
- Last 7 days of Claude cost (USD-equivalent): build 902, review 356, triage 145. P3–P5 builds of
  size S or M were about 82 of the 902, and P2 size S builds about 124.

## What routes to qwen

A build with **no builder set** goes to qwen for its run when the claiming runner has a qwen
block and the qwen limit has room (`pipeline.autoRoutable`, `newPacing`), **at any pace** (before,
only under a claude pace):

- P3–P5 of size S or M;
- P2 of size S that triage says changes only docs or only tests. Triage's result has a new
  optional field, `change_kind` (`code`, `docs`, `test`; left out: code), stored on the item
  (`items.change_kind`, migration 0034).

The route is for that run only: the item's builder stays unset, so the next claim decides again.
The claim event says `routed: qwen` (and the pace only when claude is paced); the thread says
"Claimed for build, with qwen this run."

A builder that is set wins: a plan's piece (`builder: qwen|claude`), triage's new optional
`builder` field (only when no piece named one), or the user's **`loop builder <id> qwen|claude|auto`**
(`POST /items/{id}/builder`, and a form on the board's item page for the user's token). `auto`
clears it. It can change only before a build starts: in triage or build, or on a question that
returns to one, with no run holding the item (`pipeline.BuilderSettable`). Setting one starts the
item's count of failed qwen builds over; a builder other than qwen drops the qwen session. Event
`builder_set` (`from`, `to`, the actor), one line in the thread.

## One qwen build at a time

`qwen` is a control limit like `gpu` (`pipeline.LimitQwen`, default 1; migration 0034 adds it to
an existing control row). It counts the live leases whose builder is qwen, across all runners.

- An item whose builder is qwen waits while qwen is busy (the flow panel says `limit qwen:1`).
- A build qwen would take with no builder set **builds with claude** when claude's pace is normal.
  When claude is paced, a size-S one still builds with claude (claude may take size S then), and a
  size-M one waits (it is then one of the items claude may not take). Decided this
  way so a single slow qwen build does not hold back a queue of small builds. The saving is still
  most of what qwen can do: at about 23 minutes a build, one at a time, qwen does at most about 2.5
  builds an hour.

**Do prod's live and background labels share one engine?** No, according to jarvis-server's own
docs at origin/main `a911238` (read from the runner's mirror, not from prod):
`docs/llm/QUESTIONS.md` LQ1 says "Prod has two RTX 3090s and runs two engines: live on GPU 1,
background on GPU 0", and the cutover notes in `docs/STATUS.md` (2026-10-09/10) give the same
layout (live on device 1 with whisper, background on device 0 with 131072 context). So a qwen
build does not take the engine that voice turns run on. This is **not checked on prod itself**
(no access from the build). The limit stays at 1 anyway: the background slot serves one request
at a time, and the household's background jobs (memory, errands, phone summaries) share it, so qwen
must not crowd them.

## Back to claude

Per item, loopd counts its failed qwen builds (`items.qwen_failures`) and records who built the
PR's head (`items.built_by`, from the build run's reported builder). The item's builder becomes
claude for the rest, its qwen session dropped, when:

- review bounces a PR whose head qwen built, **once** (reason `review_bounce`); or
- a qwen build fails **twice** (reason `failed_twice`, `pipeline.QwenFailLimit`). A failed qwen
  build is a failed agent run that reported builder qwen, a failed verify of one, red CI on a head
  qwen built, or a qwen lease past the time cap.

Not counted: the runner's own failures before an agent started (no builder reported), a runner
that fell back to claude for one run (no qwen block, opencode missing, jarvisd unreachable, as
before: those were the only failures in the record), verify's refusal of a trusted-file change,
and a lease that expired (the runner died). Both routed items and items whose builder is qwen fall
back. A first failed qwen build keeps the session, so the next build resumes it.

Event `builder_fallback` (`from: qwen`, `to: claude`, `reason`, `outcome`, `qwen_failures`,
`bounce_count`), as a note in the stage the advance left the item in; one line in the thread
naming the reason. This **supersedes the 2026-10-16 strike rule** (a first review bounce resumed
the qwen session, the second turned the item to claude). The bounce limit is unchanged: an item's
second bounce of any kind still goes to the user.

## Measuring it

- `loop usage` and the board's Usage panel get one line for qwen's last 7 days
  (`pipeline.QwenWeek`): its build runs; the Claude spend they replaced, **an estimate**: per size,
  the mean cost of the week's claude build runs of that size times qwen's build runs of it (a size
  claude did not build that week is counted apart, unpriced); qwen's first-review approval rate
  (the items whose first review verdict came this week and judged a head that qwen built, by the
  builder the last `pr_opened` before it reported, so the record before this change counts too);
  and loopd's fallbacks.
- Every review verdict records `built_by` on its event. The digest's builds line adds how many of
  qwen's PRs review bounced in the window ("qwen's PRs bounced at review 1 of 4").

## Unchanged

Review: every qwen PR still gets a claude review (`UsesClaude` for review is untouched). The
runner, the fence, the proxy and the result contract of a qwen build are as in 2026-10-16.

## Gaps

- The estimate assumes a qwen-built item costs what a same-size claude build costs; qwen gets the
  lower priorities and the docs/test P2s, which may be cheaper than the mean.
- The sandbox's runner has a qwen block only in scenarios that ask for it (`"qwen": true`):
  everywhere else its default P3 size-S items would now build with qwen.
- How the rate of first-review approvals holds up on the larger set of items routed now is what
  the digest and `loop usage` will show; nothing acts on it on its own.
