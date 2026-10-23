# loopd's image for fly.io (docs/decisions/2026-10-23-loopd-on-fly.md,
# docs/runbooks/loopd-on-fly.md). CGO off (SQLite is modernc.org/sqlite, pure Go), so the final
# stage is FROM scratch: two static binaries and the CA roots, nothing else to keep patched.
# America/New_York works without zoneinfo because internal/slack/bot.go imports time/tzdata.
FROM golang:1.27 AS builder

WORKDIR /src

# Dependencies first, so a change to the code does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The vcs.revision stamp is what loopd serves at GET /version, and the runner reads it to tell
# which qa items are deployed (internal/runner/deploy.go). It needs .git in the build context (see
# .dockerignore) and git in this image; -buildvcs=true makes the build fail rather than silently
# omit it. git refuses a tree it does not own, which COPY leaves it.
RUN git config --global --add safe.directory /src

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -buildvcs=true -trimpath -ldflags=-s -o /out/loopd ./cmd/loopd \
 && go build -buildvcs=true -trimpath -ldflags=-s -o /out/loop-entrypoint ./cmd/loop-entrypoint

# Fail here, not three weeks later with items stuck in qa: the binary must carry a revision and
# must not have been built from a dirty checkout (internal/build.Revision calls that no revision).
RUN set -eu; \
    info="$(go version -m /out/loopd)"; \
    rev="$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*build[[:space:]]*vcs\.revision=//p')"; \
    mod="$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*build[[:space:]]*vcs\.modified=//p')"; \
    if [ -z "$rev" ]; then \
      echo "loopd has no vcs.revision stamp: build with .git in the context (see .dockerignore)" >&2; exit 1; \
    fi; \
    if [ "$mod" = "true" ]; then \
      echo "loopd was built from a checkout with uncommitted changes, so it contains no commit for certain:" >&2; \
      echo "commit them, or deploy from a clean checkout of main" >&2; exit 1; \
    fi; \
    echo "loopd revision $rev"

FROM scratch

# Slack's Web API and Socket Mode are HTTPS, so the roots have to come along.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/loopd /loopd
COPY --from=builder /out/loop-entrypoint /loop-entrypoint

# /data is the volume ([mounts] in fly.toml): loop.db, evidence/ and regressions/.
# /run/loop is on the machine's own filesystem, where the entrypoint writes the secrets at 0600.
VOLUME /data
EXPOSE 7780

# The entrypoint writes the secret files from the environment and execs /loopd with these flags
# plus --tokens (and --slack-env when Slack's secret is set). --lan is required: checkAddr refuses
# a non-loopback address without it (cmd/loopd/main.go).
#
# --repos/--repos-file and --slack-workspace are not here: they are the prod unit's own values,
# which this repo does not hold. Step 2 of docs/runbooks/loopd-on-fly.md has the user copy them
# from the unit's ExecStart into this line before the first deploy. Without them loopd still runs,
# but triage cannot give a repo-less item a repo and the board has no links to item threads.
ENTRYPOINT ["/loop-entrypoint"]
CMD ["--addr", "0.0.0.0:7780", "--lan", "--data", "/data"]
