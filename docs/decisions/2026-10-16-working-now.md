# The board's "Working now": live activity on heartbeats (2026-10-16)

Item #68. The user wants to see, on the board, what each running agent is doing right now; a
lease's note said only "agent running: 37 tool calls".

## Decided

- **The runner builds the activity** from the stream it already parses (`runner/activity.go`) and
  sends it with every heartbeat (`pipeline.Activity`): phase (`<stage> agent`, `plan agent` with
  persona and plan id, `verify` with the step and its time, `land` with what it waits for),
  elapsed, tool calls, the to-do list from the latest TodoWrite (text and
  pending/in_progress/completed), the last five tool calls and the latest assistant text cut to
  300 characters.
- **What a tool call shows:** `Bash: <its description>` (just `Bash` without one), `Agent`/`Task`
  with their description, Read/Edit/Write/MultiEdit/NotebookEdit with the path relative to the
  worktree (a path outside it: only its base name), every other tool by name. Never a command's
  text, a pattern, or a file's contents.
- **loopd** takes `activity` on `POST /leases/{id}/heartbeat` (runner token, lease holder, as the
  note) and refuses one past its limits with 400: 20 to-dos of 200 characters, 5 calls of 200,
  a 300-character message, 64 for phase/persona/step, known statuses only, 8 KB encoded, unknown
  fields refused. It is stored on the live lease (`leases.activity`, `plan_leases.activity`,
  migration 0012), kept when a heartbeat omits it, and cleared when the lease ends (advance,
  expiry, stop, drop). The runner retries a heartbeat refused with 400 without the activity, so a
  runner newer than loopd keeps its lease.
- **The board**: a "Working now" section at the top of `/board`, one card per live lease and plan
  lease (#id or P<id> + title linking to the item or plan page, stage, runner/agent, elapsed,
  phase, to-dos with ✓/▶/○, the last calls, the latest message); the item page shows the same card
  while the item is worked. Everything goes through html/template; no `template.HTML`.
- **JavaScript, once.** The 2026-10-15 board was script-free; refreshing one section every 5 s
  without reloading the page needs a little. `GET /board/live.js` (embedded, same origin) fetches
  `GET /board/working` (the section's server-rendered, escaped inside; `?item=N` for one item) and
  swaps it in, skipping hidden tabs. Only the board and a worked item's page allow it:
  `script-src 'self'; connect-src 'self'`, still nothing inline; every other board page keeps the
  old policy. The rest of the board keeps its 30 s meta refresh. A phone gets one column of cards
  (`grid` with `minmax(min(100%, 340px), 1fr)`).
- **Nothing new goes to Slack.**

## Not done

- Setup commands send no heartbeat today (each has `SetupTimeout`), so there is no `setup` phase
  on the board yet; the card shows the claim until the agent's first heartbeat.
