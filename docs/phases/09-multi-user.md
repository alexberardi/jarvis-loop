# Phase 9: two people, loopd on fly.io

Goal: a second person works the same board from their own machine, with their own Claude
subscription and their own GPU, without holding any of this loop's secrets and without being in
the Slack workspace. `loopd` moves off the prod box to fly.io so both runners reach one shared
state. **Proves:** the pipeline is a shared service, not one person's box — a second builder's
items go from filed to merged under the same three gates, and nothing about who may approve, merge
or spend changes hands.

Status: **designed** (2026-10-23; the user's answers are in "Decisions" at the end). Not built.
Line references were read against `491c4ba`; the claims behind them (one global `ActiveAgents`, the
three token kinds, no hold rules for jarvis-node-setup, `credentials_file` required, no fly or
sign-in code) were each re-checked against `origin/main` on 2026-10-23.
Every piece here touches a held path (`api-auth`, `store`, `loopd-wiring`, `approval`, `tokens`),
so each one lands only with the user's approval in its thread — as it should: this phase changes
who can reach the loop.

## What changes

```
                    fly.io: loopd (API + SQLite on a volume + board + Slack socket)
                      ▲  TLS. Browser: GitHub sign-in. Runners: bearer tokens.
                      │
        ┌─────────────┴──────────────┐
   alex's box                   collaborator's box
   loop-runner                  loop-runner
     gpu, voice, emulator          his own gpu
     stages: all, land             stages: triage, build, review  (no land)
     his Claude subscription       his Claude subscription
     his GitHub credentials        his GitHub credentials
```

What does **not** change: runners hold every credential that can push or merge, and `loopd` holds
none; the guard, the sandbox and the verify fence are local to each machine; the three merge gates;
the hold rules; who may approve.

| | Today | After |
|---|---|---|
| Where state lives | prod box, LAN only | fly.io, public TLS |
| Who reaches the API | one user token, one token per runner, from a 0600 file | the same file for bootstrap, plus tokens minted per person from a GitHub sign-in |
| Who reaches the board | basic auth, the user token as the password | GitHub sign-in, collaborators of the meta repo |
| The throttle | one active-agents number for the whole loop | one per person; stage limits stay shared |
| `gpu`, `voice`, `emulator` limits | global | per runner (they are each machine's hardware) |
| The second builder | `qwen` block in `runner.json`, jarvisd only | any OpenAI-compatible endpoint inside the admin's host policy, configured per runner on a page |
| Answering a question | the user, in Slack | the user in Slack; a member answers the questions his own runs raise, from the board |
| Slack | the user's whole interface | unchanged, and still only the user's |

## 1. loopd on fly.io

One machine, one volume, no horizontal scale: the store is SQLite with a single writer
(`internal/store/store.go:178`, WAL, `_txlock=immediate`) and the evidence is files on disk, so
`internal/store` and `internal/api` need no change for the move.

| Setting | Value | Why |
|---|---|---|
| `[mounts]` | `loop_data` → `/data` | `loop.db`, the WAL and `evidence/` |
| `auto_stop_machines` | `'off'`, `min_machines_running = 1` | the Slack socket, lease expiry and the digest timers need a live process |
| machine count | exactly 1 | one SQLite writer; a volume belongs to one machine |
| deploy strategy | `immediate` | a rolling deploy needs a second machine, which cannot have the volume. A few seconds of downtime per deploy; runners retry their poll |
| `[http_service]` | `internal_port = 7780`, `force_https = true` | fly's proxy terminates TLS |
| health check | `GET /healthz` — **new**, unauthenticated, prints nothing but `ok` | fly needs one; it must leak no state |
| image | `FROM scratch` + `ca-certificates`, `CGO_ENABLED=0` | `time/tzdata` is already embedded (`internal/slack/bot.go:16`), so `America/New_York` works with no zoneinfo in the image |
| `--addr` | `0.0.0.0:7780 --lan` | `checkAddr` refuses a non-loopback address without `--lan` (`cmd/loopd/main.go:321`) |
| `--board-addr`, `--runner-addr` | **not set** | both existed to put the board and remote runners on LAN listeners. On fly everything arrives on one TLS port, and the token rules on each route (`internal/api/api.go:70`) are what separates them |

**Secrets** are `fly secrets`, which arrive as env: the Slack bot and app tokens (they move off
`~/.config/jarvis-harness/slack.env`), the GitHub client id and secret, and `LOOP_TOKENS` — the
tokens file's content, which the entrypoint writes to `/run/loop/tokens` at 0600 so
`api.LoadTokens`'s mode check still passes.

**Backups:** fly volume snapshots (daily, 5 days) are not enough on their own for a database —
add a nightly `sqlite3 .backup`-equivalent copy of `loop.db` plus `evidence/` pushed off the
machine, as the prod box does today.

**Moving the data:** stop the prod loopd, copy `loop.db*` and `evidence/` to the volume, start,
check `SchemaVersion` and the board, then point the dev-box runner at the fly URL and keep the
prod box's loopd stopped (not deleted) for a week.

**One thing the move breaks:** `qa_after_deploy` assumes "loopd and the runner are deployed
together, so the runner's build stands for the loop's"
(`internal/runner/deploy.go:11`). On fly they are deployed separately. loopd must report **its
own** build revision and a loop item's qa check must wait on that, not on a runner's.

**Region and org** (the user, 2026-10-23: he is in New Jersey, the collaborator in Australia).
A volume is pinned to its region and there is only one machine, so this is chosen once: **`ewr`**
(Secaucus, NJ), with `iad` as the roomier alternative. Distance costs almost nothing here — the
runners poll every 30 s and heartbeat every minute, the Slack socket dials out from fly either way,
and every GitHub call is made by a runner, not by loopd. The one thing that crosses the Pacific is
his runner's evidence upload, a few MB an item. Put it near the approver, who reads the board and
answers the gates. **The collaborator never touches fly.io**: no org membership, no deploy rights, no fly CLI, no
account. He signs in to the board (which is loopd, not fly), holds a runner token, and runs his
runner on his own machine. loopd is deployed from the user's **personal fly org** by the user alone,
and that is the whole of fly's involvement with him. Side effect of one clock: the digest's day and the
orchestrator's (`store.OrchDayZone`, `America/New_York`) stay New York, so his spend lands either
side of his own midnight.

**Cost,** roughly: a shared-cpu-1x/512MB machine always on plus a 3 GB volume is a few dollars a
month; two runners polling every 30 s is noise. Worth checking against fly's current prices before
piece 1.

## 2. Who you are: GitHub sign-in

Every repo is public (the user, 2026-10-23), which changes the scope arithmetic: the check needs no
blanket grant over private code. Note first that a public repo's **collaborator list is still not
public** — the endpoints that answer "may this person push?" need push access themselves — so this
is a real access check, not a public lookup.

Three ways to make it. **Build (3), and let the piece try (1) first as an optimisation** — the
reasoning is under the list.

1. `GET /repos/{slug}` with the person's own token, read `permissions.push`. On a public repo the
   authenticated user's `permissions` object comes back, and a token with **no scopes at all**
   (plain sign-in, `read:user`) can read it. One request, nothing granted. *To confirm: that
   `permissions` is present for an unscoped token — one `curl` with a fresh token settles it.*
2. A plain OAuth app with **`public_repo`**, and `GET /repos/{slug}/collaborators/{login}`
   (204/404). `public_repo` is write access to all his public repos — more than we need, though the
   token is used once and discarded.
3. A **GitHub App in user-to-server mode**, installed on the meta repo only: the user token is
   scoped to that one installation, so the same check needs no account-wide scope. More moving
   parts (an App, an installation), and it is what a private meta repo would have forced.

In all three: the token is used for that one request and **discarded**. Nothing GitHub-issued is
stored, and loopd holds no credential that can push.

**Why (3) and not (1), decided 2026-10-23.** (1) would be ideal — an OAuth app that asks for
nothing — but whether a scopeless token gets the `permissions` object back is unconfirmed, and three
attempts to test it by hand were void (a pasted block feeds its own next line to `read`, so the
token never reached GitHub; an unauthenticated call returns exactly the same
`permissions: null`, which is what made the failures look like answers). The fallback (2) is the
one to avoid: `public_repo` is write access to all of *his* public repos, and asking a collaborator
for that merely to sign in is worse than the setup cost of an App. (3) needs no account-wide scope
and no guesswork. So the piece installs the App, and tries the no-scope read first: if
`permissions.push` comes back for a token with no scopes, it uses that and asks for nothing; if not,
the App's installation-scoped token answers. Either way nothing broader than (3) is ever
requested.

**Which repo the check reads.** The collaborator (`opticon454`) gets push on **jarvis-node-setup,
jarvis-node-mobile and jarvis-server** — not on jarvis-loop, and not necessarily on a meta repo. So
the flag should name a repo whose push access he actually has, and the natural one is
**`alexberardi/jarvis-server`**: "may push to the main repo" is the same right his runner needs to
do any work, which makes it a better gate than membership of a doc repo. (If you would rather it be
the meta repo, add him to it first, or the check refuses him at sign-in.) The flag is
`--access-repo`, and the admins list stays separate.

1. `GET /login` → GitHub → callback.
2. The check. Pass → a session row (`sessions`: id hash, github id, login, role, created, expires)
   and a cookie: `HttpOnly`, `Secure`, `SameSite=Lax`, 12 h. Fail → refused, and the attempt is
   recorded as an event, as a non-approver's Slack press is today.
3. The check runs again at sign-in and when a session is refreshed, so losing push access closes
   the door within a session's lifetime.

**Role** is not GitHub's: `--admins alexberardi` (a flag, like `slack.Approver` is a constant — who
may approve is a code or config change that goes through review).

| | admin | member |
|---|---|---|
| see the board, items, evidence, plans | yes | yes |
| file items, edit, link, set priority | yes | yes |
| its own tokens, runners, builders | yes | yes |
| its own active-agents number | yes | yes |
| **answer needs-you, send back to build** | yes | **own runs only** (§8) |
| approve held merges, trust, drop, qa, verified, reopen | yes | no |
| `PUT /control` (stage limits, max run), unfreeze | yes | no |
| Slack | the approver | not in the workspace |

The POST forms on the board's control panel and every member-forbidden route answer `403` with the
same recorded refusal the API gives a wrong token kind today.

**What else being public changes:** the screenshots rule generalises — DESIGN.md's "jarvis-server is
public, so PR screenshots come only from sandboxes with seeded demo data" now applies to every repo,
and the briefs should say so without naming one repo. It also caps what a wrong builder endpoint can
leak (§6). And if **jarvis-loop itself** is public now, DESIGN.md's "this private repo" (Decisions,
2026-10-14) is stale: the approver's member ID, the hold rules, the guard and the land script become
public code — fine in itself, they hold no secrets, but it changes what may ever be committed here.
Worth confirming.

## 3. Tokens, per person

`api.Tokens` is a file of `token<TAB>kind<TAB>name` lines read at boot
(`internal/api/auth.go:66`). That stays, for bootstrap and for the orchestrator. Added next to it:
a `tokens` table — sha256 of the token, kind, name, **owner**, created, last used, expires,
revoked — looked up the same constant-time way, so a token can be minted and revoked without a
redeploy.

Minted from a signed-in session, on the board:

- **a personal CLI token** (`kind user`, or `member` — see below), for `LOOP_TOKEN` on their laptop;
- **a runner token** (`kind runner`, owner = them), shown once, for `runner.json`.

`kind user` today means "may do everything the user may do". Rather than widen it, the member's
token is a **new kind** (`member`): the reads a user token has, plus `POST /items`, `PATCH /items`,
`/edit`, `/link`, `/unlink`, `/resources`, its own `/control` row, and — under the `member+own`
rule of §8 — `/answer` and `/rebuild` on its own runs. Nothing else.

Keeping this a *kind* is what lets the route table stay readable in one screen
(`internal/api/api.go:70`), which is what the `api-auth` hold rule exists to protect. §8's two
routes are the single exception, and they carry one named ownership function rather than a second
permission system.

## 4. Owners in the data

| Table | Column | Meaning |
|---|---|---|
| items | `filed_by` | the login that filed it; `''` for the loop's own (findings, nightly, scout) |
| runners | `owner` | the login whose token enrolled it |
| tokens | `owner` | who it belongs to |
| builders | `owner`, `runner` | §6 |
| control | `owner` | one row per person for `active_agents` and `stopped` (§5) |

Events already carry an actor (`kind:name`, `internal/api/auth.go:37`); with per-person tokens that
becomes "who" for free. The board shows it on the card; the digest groups by it.

Both people see everything. Nothing is hidden per person: this is one board, and the review gate
depends on it.

## 5. Usage and limits, per person

Today `Control` is one global row: one `active_agents` (0–10 on main), one `stopped`, one `limits` map
(`internal/pipeline/claim.go:12`, migration `0001_init.sql:90`). With two subscriptions that is
wrong in three ways, and each has a different fix.

| Limit | Today | After | Why |
|---|---|---|---|
| active agents | global 0–10 | **per owner**, its own 0–10 | it is a throttle on one person's plan usage. Claims count only that owner's live leases against that owner's number |
| `stopped` | global | per owner, plus an admin-only global stop | a member pausing himself must not pause the loop |
| `gpu`, `voice`, `emulator`, `macos` | global | **per runner** | they are each machine's hardware. `gpu: 2` was always about one jarvisd's slot; his GPU is his own |
| `max_run_minutes` | global (75 on main) | **per owner**, admin-set | a 125B at 12–38 tok/s needs longer than a 27B; the cap is about a machine's speed, not the board |
| `triage: 1`, `review: 2`, `land: 1` | global | unchanged, admin-only | contention over the board, not over a machine |
| usage-limit auto-zero | sets the global number to 0 | sets **that owner's** to 0, and tells the owner | one person hitting a weekly limit must not stop the other |
| digest spend | one estimate | per person, in the user's digest only | the loop still sees only its own runs, not interactive sessions |

Review independence (`claim.go:289`) already compares agent names and keeps working. Worth adding
while here: when both owners have a live runner, prefer a reviewer from the **other** owner. Cheap,
and it makes the second gate mean more than it does with one person's agents on both sides.

## 6. The builder page

Generalise the `qwen` block (`internal/runner/qwen.go:21`) into named **builders**, configured on a
page instead of in `runner.json`, so a collaborator can point his items at his own GPU without
editing a file he does not have.

A builder row (table `builders`, owner + runner + name): `tier` (which blueprint builder it
serves, below), `kind` (`opencode`), `base_url` (OpenAI-compatible, up to `/v1`), `model`,
`max_minutes`, `enabled`. Nothing secret is ever stored
on fly. `credentials_file` becomes **optional** — it is required today (`qwen.go:43`), which a local
GPU with no auth cannot satisfy — and when a builder does need credentials, the page names a **local
key** (`credentials: "prod-jarvisd"`), which the runner resolves in its own `runner.json`'s
`credentials` map. The server never learns a path, never a secret.

### Two checks, with different jobs

An allowlist in `runner.json` lives on the machine its owner controls, so it is **no defence against
that owner** — he can edit it. Policy therefore has to be enforced where he cannot reach it, in
loopd:

- **On save.** The row's owner must own the runner it configures — nobody configures anybody else's
  machine. The URL must parse as http(s) with no query, fragment or userinfo (the rules
  `normalize` already applies, `qwen.go:37`). And the host must match an **admin-set policy**
  (`builder_policy`, admin-only: hosts and CIDRs, e.g. `127.0.0.1`, `10.0.0.0/8`). A member picks
  within the policy and cannot widen it; an attempt is refused and recorded.
- **On hand-off.** The row is validated again when a claim hands it to a runner, so a policy
  tightened afterwards takes effect with nobody re-saving, and a row written straight into the
  database is refused.

The runner-side allowlist (`builders_allow`) **stays**, with its job stated honestly: it defends
against a **compromised loopd**, which is a real threat now the API is on the internet — without it,
one bad row in the fly database redirects every agent's prompts to an attacker. It is defence in
depth against the server, not a check on the person.

And the limit neither check removes: **nothing loopd does can constrain what someone runs on their
own machine.** He could patch his own runner. What actually bounds a collaborator is that his work
arrives as a pull request — the review gate, the hold rules, CI, your digest, and GitHub push
access. These checks are here to keep misconfiguration and a compromised server out. With every
repo public, what a wrong endpoint could leak is public code and the item's brief.

Unchanged: the per-run proxy that keeps credentials from the agent (`qwenproxy.go`), the verify
fence OpenCode runs in, the `base_url` reachability check, the fallback-to-claude event, and the
two-strike rule (`pipeline.QwenStrikes`).

### A tier in the blueprint, an endpoint on the page

`builder: qwen|claude` was one machine's model. With two people it should name a **tier**, not an
endpoint: `claude`, or `local` (with `qwen` kept as an alias so existing items and plans still
parse). A blueprint says "this piece is mechanical enough for a local model"; each runner's page
says which endpoint serves `local` on that machine. Nobody writes another person's hardware into a
plan, and an item at tier `local` claimed by a runner with no local builder builds with claude and
says so, as it does now.

### His box: Strata

He mentioned Qwen3.8-Flash-Next 125B through **Strata** — an open-source local inference engine
(MIT, Niko1221, `github.com/Niko1221/Strata`) that runs that 125B MoE model on a consumer GPU by
keeping the experts in system RAM and paging the hot ones into VRAM. It matters here that it is
**local and OpenAI-shaped**: a web server on `127.0.0.1:8080` with `/v1/chat/completions` and
`/v1/models`. So his builder row is the existing OpenCode path with nothing new underneath it:

| | |
|---|---|
| `base_url` | `http://127.0.0.1:8080/v1` — inside the default host policy, no public host to allow |
| `credentials` | none. This is the case that makes `credentials_file` optional (`qwen.go:43`) |
| `model` | whichever Qwen3.8-Flash-Next quant he loads |
| data | stays on his machine: nothing is sent to a third party, so none of CLAUDE.md's third-party rules come into play |
| hardware | 12 GB VRAM + 64 GB RAM minimum at 12–18 tok/s; 16–24 GB + 64–128 GB for 25–38. Not a gate: if his box is too slow he disables the builder row and his items build with claude |

At 12–38 tok/s an agentic build is slow — which is why `max_run_minutes` goes per owner in §5. Time
one real item before trusting the tier with anything.

### The spike, run 2026-10-23: Strata's Anthropic endpoint is real

The documented hole in the OpenCode builder is that **OpenCode runs no hooks, so `loop-guard` never
runs** (`docs/decisions/2026-10-16-qwen-builder.md`), and the fence has to stand in for each of the
guard's rules. If Strata speaks Anthropic's Messages API, the real `claude` CLI can run against it
with `ANTHROPIC_BASE_URL` and his local builds get the hooks, the guard, the sandbox and the
generated settings for free. Read first-hand in the source (`github.com/Niko1221/Strata`, cloned
2026-10-23):

- **It is a real implementation, not a shim.** `POST /v1/messages` goes through
  `anthropic_to_messages` (`serve/server.py:7294`): tools, `tool_choice` (`none`, `any`, a forced
  tool), thinking budget, `stop_sequences`, sampling validation. Streaming emits the whole event
  set — `message_start`, `content_block_start/delta/stop`, **`input_json_delta`** (streaming tool
  arguments, the part that is easy to get wrong), `message_delta`, `message_stop`, `ping` — with
  Anthropic-shaped `error` events for `api_error` and `rate_limit_error`. Non-streaming goes through
  `anthropic_collect`, and `POST /v1/messages/count_tokens` exists.
- **Claude Code has already been run against it**, which is better evidence than the claim: the code
  carries fixes named for it. `serve/server.py:6630` strips the query because "Claude Code posts
  `/v1/messages?beta=true`"; `V1_V1_ALIASES` (line 272) catches an Anthropic client given a base URL
  that ends in `/v1`; the module docstring notes "Claude Code asks for 32000" max tokens and
  shortens it to the room left instead of refusing; there is a `serve/test_anthropic_base_url.py`.
- **Its own check command** is the right thing to run on his box: `strata connect claude-code
  --check` "sends one prompt and one tool call the way that app does and says whether each worked".

Four facts that land in the design:

| Fact | Consequence |
|---|---|
| `ANTHROPIC_BASE_URL` takes the host with **no** `/v1` (`docs/AI_SETUP.md:185`), while an OpenAI `base_url` ends in `/v1` | the page's one URL field means two different things per `kind`; store it canonically and let the runner derive each form, or the `/v1/v1/messages` alias is doing the work silently |
| `ANTHROPIC_MODEL` must be a Claude name the CLI knows; Strata ignores it | for this kind the runner's `--model` is a Claude name while the real model is whatever Strata loaded; the builder row keeps the true name for the digest |
| A prompt that alone does not fit the context is a hard 400 | the runner should read that as a **tier failure → fall back to claude**, not as a bounce against the item |
| One request at a time by default (`--parallel N` to raise it); default `--access local`, loopback, no key | his runner keeps one slot for the tier, exactly like your jarvisd background slot, and the endpoint stays inside the host policy with no credentials key |

**Two things the spike could not settle here**, both of which his box settles in one sitting:

1. **Do hooks still run under `ANTHROPIC_BASE_URL`?** This is a Claude Code question, not a Strata
   one, and it is the whole point: no hooks, no guard. Nothing documented says a custom base URL
   changes hook handling — `--bare` is the only flag that skips them
   (`docs/decisions/2026-10-15-claude-headless-facts.md`) — but this repo does not build on
   "nothing says otherwise". The test is a stub Anthropic endpoint, a `PreToolUse` hook that writes a
   marker, one `claude -p`, and a look at whether the marker exists. It was written and then refused
   by this machine's permission classifier ("create unsafe agents"), so it is **unverified**: run it
   on his box, or allow it here.
2. **Does a 125B Qwen hold up under Claude Code's tool protocol?** Needs his hardware. And note the
   trade: Claude Code's system prompt and tool definitions are much larger than OpenCode's, and
   prefill on a local engine is not free. Guard coverage may cost real wall time. Measure both on one
   mechanical item before choosing.

**So piece 7 builds `kind: opencode` first** — today's fenced path, already written and known to
work — and adds `kind: claude-compatible` as a second kind once those two checks pass on his box.
That ordering ships something that works this week and keeps the better answer available, rather
than betting the piece on an endpoint nobody here has run.

## 7. The collaborator's machine

He installs the runner, the guard, `loop`, bubblewrap and Claude Code, signs in to his own Claude
subscription, and gets a runner token from the board. His `runner.json`:

- `stages: ["triage", "build", "review"]` — **no `land`**. The config field already exists
  (`internal/runner/config.go:40`); landing stays on the user's runner.
- `repos`: **jarvis-server only, to start** (below). He needs write to push branches; the merge is
  still the user's runner's job. He has no push on jarvis-loop, so items about the loop itself never
  reach his runner — nothing routes them there, and a push would fail if one did. That is the right
  default: this phase's own pieces are the user's to land.

### The minimum grant, and the order to widen it

Every repo is public, so he needs **no grant at all to read or clone**. Push is needed only where
his runner pushes branches, which makes the minimum **one repo**, doubling as `--access-repo`:

| Repo | Grant | Why |
|---|---|---|
| **jarvis-server** | **now** | The repo the loop has worked longest, with real hold rules (`serverRules`, 8 of them), verify steps that run on a Linux box, and the harness. It is also the right `--access-repo`: "may push to the main repo" is the same right his runner needs |
| jarvis-node-mobile | when his box can run mobile verify | It has rules and verify, but verify wants jarvis-server's `scripts/sandbox` and `scripts/mobile-shot`, an emulator, and the one-verify-at-a-time lock (`docs/phases/06b-all-repos.md`). Without those his builds arrive unverified. Branch pushes are safe — it is the **merge** to main that ships to TestFlight and Play internal, and he cannot merge |
| jarvis-node-setup | only after the loop knows it | **It is not configured in the loop at all**: no `NodeSetupRepo` in `internal/pipeline/holdrules.go`, no row in 06b's table, no group, setup or verify steps. Every default hold rule names a repo (`DefaultHoldRules`, `holdrules.go:43`), so a PR there would face **no hold rules whatever** — a change to its auth, pairing, CI or a deleted test would merge on the three gates alone. Adding it is a 06b-style piece (group, aliases, setup, verify, hold rules) and should come before the grant, not after |

So: grant **jarvis-server** today and leave the other two until each is ready. Nothing in the phase
needs more, and each addition is one line in his `repos` map plus the GitHub grant.


- `capabilities: ["gpu"]`, his own `slots`, and his own `builders_allow` — which guards his machine
  against a bad row arriving from the server, not against himself; the policy that binds him is
  loopd's (§6).
- His own GitHub credentials, his own `gh` login. loopd never sees them.

A runbook goes in `docs/runbooks/collaborator.md`, next to `mac-runner.md`.

## 8. Needs-you, answered from the board

Needs-you is the busiest gate in the loop: phase 8's day of real use counted ~15 runner and
environment errors, 6 harness refusals and 6 re-asked questions in one day, almost none of them
decisions (`docs/phases/08-orchestrator.md`). A member who cannot answer would stall on nearly all
of it, and he is not in Slack. So he gets a text box.

**Who may answer what.** A member may answer, or send back to build, an item that is **his own
run**: one he filed, or one whose question came from a run on a runner he owns. His machines'
questions are his to unstick. Everything else — held merges, trust OKs, qa, drops, the throttle,
unfreeze — stays the user's, with no ownership exception.

**The one place the route table is not kind-only.** `POST /items/{id}/answer` and
`POST /items/{id}/rebuild` take a `member+own` rule: one small `ownsRun(item, principal)` function,
named in the route table beside the kinds, tested both ways (his own item answered; another
person's refused and recorded). Every other gate route keeps its plain `user` kind, so the
`api-auth` hold rule still covers a table that reads in one screen.

**The UI.** On the item page, when it is in needs-you and `ownsRun` holds: the question, the history
it already has, a text box with **Answer**, and a **Send back to build** button with a note field.
No approval words and no classifier — a box is an answer, a button is a rebuild note. The Slack
reply classifier (`internal/slack/approve*.go`, the `slack-approver` hold path) is neither touched
nor reused, so nothing here can become a way to approve a merge.

**Mirrored into Slack.** Every answer given on the board posts into the item's thread — "Answered on
the board by `<login>`: …" — so that thread stays the item's whole history and the user sees what
was decided on items he never saw asked. The event records the login, the words and the stage, as a
Slack answer does.

**How he knows to look.** The board's needs-you column puts his own runs first; `loop watch` prints
his items waiting and returns when one changes; and his runner logs it when one of its items goes to
needs-you. No Slack, no email.

If the two of you later want him in Slack after all, nothing here is wasted: the board box and the
Slack reply end in the same `/answer` route and the same event.

## 9. What is new on the attack surface

| New | Mitigation |
|---|---|
| The API is on the public internet | TLS; every route still behind a token or a session; the runners' LAN listener is gone, and its "runner tokens only" rule is replaced by the per-route rules that were always underneath |
| loopd sees a GitHub token at sign-in | used for one request, never stored, no scope over private code (§2). loopd still cannot push or merge anywhere |
| Slack tokens live on fly | `fly secrets`, never in the image, never in argv (the rule in CLAUDE.md), never logged |
| Evidence is reachable off the LAN | only behind a session or a token; Slack still gets summaries and links, never logs or transcripts |
| A member points a builder somewhere bad | refused on save and on hand-off against the admin's host policy (§6); only his own runners; a wrong endpoint reaches public code and a brief |
| A compromised loopd redirects agents' prompts | the runner's local `builders_allow`, which the server cannot edit (§6) |
| A member answers a question that was really a product call | it is his own run only, recorded with his login, mirrored into the item's Slack thread, and the work still faces review, the hold rules, CI and the digest |
| A second person's agents open PRs | the same three gates, the same hold rules; his runner cannot merge; the user's digest shows every item with who filed it |
| A revoked collaborator | the check reruns at sign-in and refresh; his tokens are revoked by hand on the board. Removing push access on GitHub stops his runner regardless |

## 10. Subscription auth only, enforced (the user, 2026-10-23)

Every Claude run in the loop is paid for by a person's **Claude subscription**, through the CLI's own
login — never an API key. That is how it works today by accident of omission: the runner sets no
auth, so `claude` falls back to the OAuth credentials in `$HOME` (`agentEnv()`,
`internal/runner/runner.go:805`, scrubs only loopd's and Slack's secrets), and `--bare` is rejected
precisely because it would drop that login
(`docs/decisions/2026-10-15-claude-headless-facts.md`). The user's instruction makes it a rule, so
it stops being an accident:

- **Nothing in this phase introduces an API key.** loopd on fly holds no Claude credential of any
  kind; each person's subscription stays on their own machine, which is also why runners stay local
  (§1).
- **No `ANTHROPIC_*` variable reaches an agent run.** `agentEnv()` scrubs the lot — the runner
  itself needs none, so the rule is absolute and one test covers it. Today a stray
  `ANTHROPIC_API_KEY` in the runner's environment would silently take over billing for every build.
- **The run proves it.** The runner already reads `system/init` and kills a run whose
  `permissionMode` is not what it asked for (`mode_mismatch`). The same event carries
  `apiKeySource`; a run whose source is not the subscription is killed the same way
  (`auth_mismatch`) instead of quietly spending from somewhere else. The exact value for
  subscription auth is still unconfirmed in the headless-facts note, so piece 9 records it from the
  first real run and then asserts it.

**What this costs:** the `claude-compatible` builder kind of §6 is **out**, not postponed. Pointing
the CLI at Strata needs `ANTHROPIC_BASE_URL` plus some token in `ANTHROPIC_AUTH_TOKEN` — a
subscription login cannot authenticate to a third-party endpoint, so that kind cannot exist under
this rule. (The token would be a dummy string for a loopback server, costing nothing and reaching
no Anthropic account — but it is still an `ANTHROPIC_*` auth variable in an agent's environment,
which is the thing being ruled out.) So **the local tier stays `kind: opencode`**, which needs no
Anthropic variable at all, and the guard gap there stays covered by the fence as it is today. If you
ever want to revisit it, the spike's findings in §6 are what you would be deciding on, and the
decision is "allow one dummy loopback token", nothing more.

## Pieces (filed as loop items, in order, held for the user)

1. **loopd on fly** — Dockerfile, `fly.toml`, `/healthz`, tokens written from a secret at boot,
   `/data`, the deploy and backup runbook, the data move, and loopd reporting its own build
   revision for `qa_after_deploy`. *(repo: jarvis-loop, size M, builder claude)*
2. **GitHub sign-in and sessions** — the check of §2 (confirm (1) first), `/login` and the callback
   with the token discarded, `sessions`, `--access-repo` and `--admins`, admin/member on every board
   route, refusals recorded. *(size L, claude)*
3. **Tokens in the store** — the `tokens` table beside the file, the `member` kind and its route
   list, minting and revoking from the board, `last_used`. *(size M, claude)*
4. **Owners** — `filed_by`, `runners.owner`, both on the board and in the digest. *(size S, qwen)*
5. **Needs-you from the board** — `ownsRun` on `/answer` and `/rebuild`, the question block with its
   text box and Send-back-to-build note, the Slack mirror, `loop watch`, the runner's log line.
   *(size M, claude)*
6. **Per-person throttle** — `control` by owner, hardware limits per runner, usage-limit zero scoped
   to its owner, spend per person in the digest, cross-owner review preference. *(size M, claude)*
7. **The builder page** — the `builders` and `builder_policy` tables, the page, validation on save
   and on hand-off, the runner's fetch, `builders_allow`, optional credentials by local key, and
   `builder:` as a tier (`local`, `qwen` an alias). One kind only, `opencode`: `claude-compatible` is
   ruled out by §10. *(size L, claude)*
8. **Onboarding** — `docs/runbooks/collaborator.md`, his `runner.json` template, the repo
   push-access checklist, the screenshots rule generalised, and this doc's diagram into DESIGN.md.
   *(size S, qwen)*
9. **Subscription auth, enforced** (§10) — `agentEnv()` scrubs every `ANTHROPIC_*`; `apiKeySource`
   from `system/init` recorded, then asserted, with an `auth_mismatch` kill beside `mode_mismatch`.
   Independent of the rest of the phase and worth landing first: it protects today's runs.
   *(size S, claude)*

## Harness

New scenarios, test-first, against the fake GitHub as well as the fake Slack:

- a member token claims build and review, and is refused on `land`, `approve`, `trust`, `drop`,
  `qa`;
- a member's sign-in without push access is refused and recorded;
- a member answers a needs-you raised by a run on his own runner: the answer reaches the next run
  and appears in the item's Slack thread; the same answer on another person's item is refused and
  recorded;
- two owners each at their own active-agents number run side by side; one hitting a usage limit
  zeroes only his;
- both runners declare `gpu`; each gets its own limit and they do not contend;
- a builder row for someone else's runner is refused on save; one outside the admin's host policy is
  refused on save, and an already-saved row is refused on hand-off after the policy tightens;
- a builder with no credentials key runs (the local-GPU case); a base URL outside the runner's own
  `builders_allow` is refused with an event and the item builds with claude;
- an item filed by the member goes build → review → land on the user's runner, and its held merge
  waits for the user, not for him.

## Open

Settled 2026-10-23: the collaborator is **`opticon454`**, with push on jarvis-node-setup,
jarvis-node-mobile and jarvis-server; `--access-repo` is **`alexberardi/jarvis-server`**; the region
is **`ewr`** in the user's personal org; sign-in is the **GitHub App** (§2); the local tier is
**Strata, `kind: opencode`** (§6, §10). What is left is small, and none of it blocks piece 1:

1. **His hardware is not a gate.** Ship the tier, let him turn it on, and if it is too slow he sets
   the builder row `enabled: false` (or deletes it) and his items build with claude like anyone
   else's. No specs needed up front, no measurement step to clear: the fallback is the same one that
   already handles a missing binary, an unreachable endpoint and two bounces. The digest's
   per-builder numbers are there if either of you wants to know how it did.

2. **The meta repo's slug**, for the docs only — the access check no longer needs it (§2).
3. **Is jarvis-loop itself public now?** DESIGN.md still says "this private repo" (§2).

## Decisions (user, 2026-10-23)

1. **Topology:** loopd whole — API, SQLite, board and the Slack socket — on fly.io; runners stay
   local on each person's machine, each with its own Claude subscription and GPU.
2. **Store:** SQLite on a fly volume, one machine. No Postgres, no object storage: `internal/store`
   is not rewritten for this.
3. **Identity:** GitHub, checked at sign-in, the person's token used once and discarded. loopd
   stores no GitHub credential.
4. **What the collaborator may do:** file items, and his runner takes triage, build and review. Not
   land, not approve. The user stays the only approver (`slack.Approver` is unchanged).
5. **Builder config is checked server-side** — ownership plus an admin-set host policy, on save and
   on hand-off. The runner's local allowlist stays, as defence against a compromised loopd rather
   than against its owner.
6. **Every repo is public**, so the sign-in check needs no scope over private code, and a wrong
   builder endpoint can leak only public code and a brief.
7. **The member answers the needs-you questions his own runs raise**, from a text box on the board,
   mirrored into the item's Slack thread; he is not in Slack. Every other gate stays the user's.
8. **Where it runs:** `ewr` (confirmed), one machine, one volume, the user's personal org — near the
   approver; the runners' polling and his evidence uploads tolerate the distance (§1).
9. **His tier is local:** Strata on `127.0.0.1:8080`, no credentials, nothing leaving his machine.
   A blueprint names the tier (`local`), the page names the endpoint (§6).
10. **Subscription only, never an API key:** no `ANTHROPIC_*` variable reaches an agent run, the run
    proves its auth source from `system/init`, and the `claude-compatible` builder kind is ruled out
    rather than postponed (§10).
11. **Deploying loopd is the user's alone**, from their personal fly org. The collaborator has no
    fly account, no org membership and no deploy rights; nothing in the phase needs him to (§1).
