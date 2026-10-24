# The status page signs in with HTTP basic auth

2026-10-14 · phase 2 (loopd core)

**Decision.** `GET /` and its form (`POST /ui/control`) take HTTP basic auth: any username, the
**user token as the password**. A runner token gets 403. The form post is wrapped in Go's
`http.CrossOriginProtection` (it checks `Sec-Fetch-Site` / `Origin`), and the page is served with
`Cache-Control: no-store`, `X-Frame-Options: DENY` and a CSP that allows no scripts.

**Why.** The spec offered a cookie set once by `GET /login?token=` or basic auth, "the simpler secure
option". Basic auth needs no session table, no cookie flags and no login route, and it keeps the
token out of URLs, where it would land in browser history, `Referer` headers and any proxy's access
log. Both options share one risk: the browser attaches the credentials to a cross-site form post.
`CrossOriginProtection` closes that for the one state-changing route.

**Consequences.** The browser remembers the credentials until it is closed; there is no "log out".
The JSON API stays bearer-only; basic auth is accepted only on the page's two routes. When the
Cloudflare Tunnel arrives (rollout 9), Cloudflare Access sits in front of this anyway.

**Changed 2026-10-23 (#550).** The tunnel is not being built: loopd moved to fly.io, where nothing
sits in front of it. So **basic auth over TLS is the whole guard** on `GET /` and `POST /ui/control`,
where before an attacker also had to be on the home LAN. Everything above is unchanged — the
constant-time comparison, `CrossOriginProtection` on the one state-changing route, `no-store`,
`X-Frame-Options: DENY`, the script-less CSP — and the credentials now always travel over TLS. If
that is not enough the next step is fly's own `[http_service]` restrictions or an authenticating
proxy in front, not a change here ([decision](2026-10-23-loopd-on-fly.md)).
