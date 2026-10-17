# The Qwen builder: OpenCode for items marked builder=qwen, claude after the second strike

2026-10-16 · item #32, following the trial in item #31
([2026-10-16-qwen-builder-plans.md](2026-10-16-qwen-builder-plans.md)). The user's decision: build
runs of items whose plan piece says `builder: qwen` run OpenCode against the prod jarvisd's
background slot (Qwen3.8-27B, 128k context, one request at a time) instead of `claude -p`.

## What runs

- **Config** (`runner.json`, `qwen` block, `internal/runner/qwen.go`): `base_url` (jarvisd's
  OpenAI-compatible route up to `/v1`, from jarvis-server item #30), `credentials_file` (an env
  file with `PROD_APP_ID` and `PROD_APP_KEY`, mode 0600, checked at start), `model` (default
  `background`), `max_minutes` (default 45; loopd's `max_run_minutes` still applies) and
  `opencode` (the binary; prefer the real one over mise's shim). No `qwen` block: qwen items
  build with claude on that runner (a `builder_fallback` event with `this_run_only`; loopd's
  builder stays qwen).
- **The agent never sees the credentials.** The runner reads the file per run and starts a proxy
  of its own on `127.0.0.1:<random>` (`qwenproxy.go`) for that run only: it accepts only the run's
  random bearer token and only `POST /v1/chat/completions` and `GET /v1/models`, drops any
  `Authorization`, cookie, `X-Api-Key` or `X-Jarvis-*` header the agent's side sends, adds
  `X-Jarvis-App-Id`/`X-Jarvis-App-Key`, and streams (server-sent events pass as they come). The
  credentials file is on the fence's mask list.
- **OpenCode runs in the verify fence** (`opencode.go`, `fence.go`): bwrap, HOME read-only, the
  SSH keys, `gh`, loop and harness config, Claude's login and the user's own OpenCode config,
  data, state and cache masked, the environment scrubbed. Its config (`opencode.json`: one
  OpenAI-compatible provider at the proxy, `permission` edit and bash `allow`, webfetch `deny`)
  is written by the runner in the run dir, read-only to it; its data, state and cache (`XDG_*`)
  are in `<work_root>/opencode/<item>/`, kept across the item's runs; `--pure` and no project
  config. The brief, `.loop/result.json`, the evidence and their validation are the build's,
  unchanged.
- **Sessions**: loopd stores the OpenCode session id a qwen build reports (`items.build_session`,
  migration 0012); the item's next build resumes it (`opencode run -s <id>`) when its data dir is
  still on that runner, and starts a new one otherwise.

## The strike rule

The ask says both "one strike: bounced or failed verify once → claude" and, in its scenario list,
"qwen build → review bounce → resumed session → approve; a second bounce → claude". They disagree;
a resumed qwen session after a bounce only makes sense if the first bounce keeps qwen. **Built as
the scenarios say**: the first bounce (review or the runner's verify, counted together as for any
item) resumes the qwen session; the second flips the item's builder to `claude` for good and
clears its session (event `builder_fallback`, `{from: qwen, to: claude, bounce_count}`). The
second bounce sends any item to the user, so the claude build starts after the user's answer.
`pipeline.QwenStrikes` is the one number to change for the stricter reading (1).

## Measuring it

Each build run reports `builder`, `run_seconds` and, for qwen, `qwen_requests` and OpenCode's
token counts; Claude's usage and cost stay on claude runs only. The digest has a line comparing
the window's build runs per builder (runs, average wall time, Claude's cost) and counting
fallbacks. The board card shows the builder and follows the flip.

## Gaps, said plainly

- **Network**: the fence shares the host's network namespace (`--unshare-net` would also cut the
  proxy on the host's loopback; bridging it over a unix socket would also take the module proxy
  away from builds and tests). So OpenCode can reach the LAN, jarvisd included, and the internet,
  but without the app key; webfetch is denied in its config, not by the kernel.
- **One request at a time** on jarvisd's background slot is not enforced by the runner: a runner
  with `slots` > 1 could start two qwen builds. Keep qwen on a runner with one slot, or let
  jarvisd queue.
- The real OpenCode against the real jarvisd is not exercised by the tests: they use the fake
  opencode (the fake claude called as `run`) and a fake upstream.
