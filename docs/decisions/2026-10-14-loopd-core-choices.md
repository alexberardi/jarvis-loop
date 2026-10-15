# Choices made where the phase 2 spec left room

2026-10-14 · phase 2 (loopd core). Smaller calls; each is enforced by a test.

**Rules (internal/pipeline)**

- **The 2nd bounce returns to build.** Bounce → needs_you sets the return stage to `build`, so the
  user's answer sends the item back to be built again. The bounce count is **not** reset by the
  answer: every later bounce also goes to the user. Reading of DESIGN.md's "after 2 bounces →
  needs-you".
- **Hold risk is computed by loopd, and a hold item merges only at the approved head.** Superseded
  and extended after review: see `2026-10-14-hold-rules.md`.
- **Review independence is enforced twice:** the claim skips the builder's own review (spec), and
  an `approved` advance from the builder agent is refused (`same_agent`) in case a runner reuses an
  agent id.
- **Resources:** `reproduced` must carry the `resources` field, but it may be empty (or `none`): an
  item that needs nothing special runs anywhere. Lists are stored sorted and deduplicated.
- **Limits** live in one map in control: resources (`gpu`, `voice`, `emulator`, `macos`) and stages
  (`triage`, `review`, `land`). `build` is not a valid key: build is bounded only by the
  active-agents number (spec). A resource or stage without a key is unlimited. `PUT /control`
  merges limits key by key.
- **Refusals are history.** Every refused action on an existing item (wrong stage, missing field,
  wrong sha, no lease, someone else's lease, too large) writes a `refused` event with the code.
  Auth failures (401/403 on the token kind) are not item events; they are in the request log.
- **No `POST /items/{id}/ask`.** DESIGN.md lists it; the phase 2 spec puts questions in the
  `question` outcome of `advance`, which is what agents will use. It can be added with Slack (phase 3)
  if the user wants to park an item themselves.
- **Answering an item whose return stage is unknown** (cannot happen today) sends it to triage.

**State (internal/store)**

- One writer connection (`MaxOpenConns(1)`, `BEGIN IMMEDIATE`) and a separate `query_only` reader
  pool, WAL. Every rule runs inside a write transaction, which is how concurrent claims stay
  correct; a unique partial index (`leases(item_id) WHERE ended_at IS NULL`) backs it up.
- Times are Unix nanoseconds (UTC) from the injected clock. `events` refuses UPDATE and DELETE by
  trigger. Migrations are embedded `NNNN_name.sql` files recorded in `schema_migrations`.
- Lease expiry runs on every authenticated request (a cheap read first; the write lock only when a
  lease is due) and on a 1-minute ticker.

**Security (cmd/loopd, internal/api)**

- `loopd` refuses a non-loopback `--addr` unless `--lan` is passed.
- The tokens file is refused if group or others have any permission bit; tokens must be at least 16
  characters and unique. Lookup hashes the presented token and compares against every entry in
  constant time. Tokens are never logged (the request log has method, path without query, status,
  actor).
- The data directory is made 0700 and the database 0600.
- Evidence: the content type is sniffed by the server, not taken from the client. Only PNG, JPEG,
  GIF, WebP and plain text are served inline; everything else is `application/octet-stream` as an
  attachment, and every download carries `X-Content-Type-Options: nosniff` and
  `Content-Security-Policy: sandbox`, so an uploaded page can never run in loopd's origin. Names are
  reduced to `[A-Za-z0-9._-]` with directories dropped. The upload is streamed (hashed and capped at
  20 MB on the way) to a temp file and renamed into place in the transaction that records it;
  in the multipart body the optional `label` field must come before `file`, as `loop` sends it.
- JSON bodies are capped at 1 MB and decoded strictly (unknown fields are a 400).
