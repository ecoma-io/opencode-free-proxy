# Deployment

## Docker

The image is a static Go binary on `scratch` — no shell, no curl. The
`HEALTHCHECK` runs the entrypoint binary itself against its own `/healthz`
(`opencode-free-proxy healthcheck`, 5 s timeout).

```sh
docker build -t opencode-free-proxy .
docker run -d -p 8090:8090 \
  -v "$PWD/config.yaml:/etc/ofp/config.yaml:ro" \
  -e OCFP_CONFIG=/etc/ofp/config.yaml \
  opencode-free-proxy
```

## Docker Compose

The repo ships a minimal compose file (`compose.yaml`) that builds the image
and mounts your local `./config.yaml` as the config document. That file is
operator-owned and gitignored — **create it before `docker compose up`**
(bind-mounting a missing path makes Docker materialize a directory, which the
proxy then fails to read at startup). For the anonymous free tier, a minimal
document is enough:

```yaml
# config.yaml — direct egress
egress:
  - id: direct

routes:
  - id: default
    egress: [direct]
```

```sh
docker compose up -d --build                  # anonymous free tier
HOST_PORT=9090 docker compose up -d --build   # different host port
```

To add egresses, routes or a custom upstream, edit the mounted document (or
point the mount at your own file) — see
[configuration.md](configuration.md) for the schema (the annotated shipped
example is `config.example.yaml` in the repo root). The mount is a
bind mount on purpose: editing the file on the host hot-reloads — but
because it binds a single file, the host edit must stay in place (see
[Editing the mounted file](#editing-the-mounted-file) below).

## The config file mount

`OCFP_CONFIG` names the document path **inside the container**; the file it
points at must be mounted (read-only is fine — the proxy never writes it).
Secrets enter through `${VAR}` interpolation, resolved from the container's
environment at load time, so the mounted file itself carries no credentials.
`compose.yaml` passes a present `.env` straight into the container
(`env_file`, optional) — copy `.env.example` from the repo root to `.env`
(gitignored) and set each variable your `config.yaml` references there; an
unset `${VAR}` is a load error:

```yaml
# compose.yaml (excerpt)
env_file:
  - path: .env
    required: false
environment:
  OCFP_CONFIG: /etc/ofp/config.yaml # pinned: environment overrides env_file
volumes:
  - ./config.yaml:/etc/ofp/config.yaml:ro
```

The file is re-read every `OCFP_CONFIG_POLL_MS` (default 1 s); a bad rewrite
keeps the last good runtime — see
[configuration.md → Hot reload](configuration.md#hot-reload-semantics).

### Editing the mounted file

Hot reload detects change by SHA-256 over the bytes the poller reads each
tick (`internal/config/store.go`), and every deployment path shipped here —
the `docker run -v` above and the compose `volumes:` entry — bind-mounts a
**single file**. A single-file bind mount stays pinned to the inode the
container started with: a host-side write to that inode is seen, a
host-side `rename(2)` (write a temp file, move it into place) is NOT — the
renamed file is a NEW inode, the container keeps reading the old one, the
hash never changes, and an unchanged file logs nothing, so the missed
reload is completely silent.

- **With a single-file mount, write in place** — shell redirection
  (`cat new.yaml > config.yaml`, `printf`/`echo >`), `tee`, `cp` from a
  staged file: all of these truncate-and-rewrite the SAME inode. Do NOT
  `sed -i`, `mv`, or any "atomic write" helper — those rename a temp file
  into place and are invisible to the container.
- **Watch your editor's write protocol.** Vim's default save renames a
  temp file over the original — the exact trap; `:set backupcopy=yes`
  makes `:w` overwrite in place. Anything else that recreates the file
  instead of rewriting it has the same failure.
- **To rename/replace freely, mount the directory instead** —
  `-v "$PWD:/etc/ofp:ro"` with `OCFP_CONFIG=/etc/ofp/config.yaml` (compose:
  `- ./:/etc/ofp:ro`). Inside a directory bind mount the poller opens the
  file by path on every tick, so a rename lands on the new inode and the
  atomic rewrite also removes the torn-read window (a partially-written
  file that still parses and validates would otherwise serve until the
  next tick).

## Bootstrap environment

| Var                   | Default              | Meaning                                                             |
| --------------------- | -------------------- | ------------------------------------------------------------------- |
| `OCFP_PORT`           | `8090`               | Listen port (container listens on all interfaces)                   |
| `OCFP_CONFIG`         | _(empty = built-in)_ | Config document path; hot-reloaded. Empty = built-in direct runtime |
| `OCFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval (ms)                                       |
| `OCFP_SHUTDOWN_GRACE` | `55000`              | Drain window before force-close (ms)                                |

No other process env vars exist; every service setting (upstream base, UA
sync cadence, routing) lives in the config document. `.env.example` (repo
root) is the template for the whole environment — copy it to `.env`
(gitignored); compose feeds that file both to its own `${...}` substitution
and, via `env_file`, into the container. Under compose the listen port
stays `8090` behind the `HOST_PORT` mapping — change `HOST_PORT`, not
`OCFP_PORT`.

## Health / readiness

- `GET /healthz` answers `200 ok` while the process serves — it is the
  Docker `HEALTHCHECK` target and a perfectly good k8s liveness probe.
- The model-facing `GET /v1/models` degrades independently: it falls back to
  a static free-tier registry when the upstream list is unreachable
  (fail-open), so a flaky upstream does not fail the probe surface.
- There is no separate readiness endpoint; a freshly started process begins
  admitting requests as soon as the listener is up (the UA cache warms in
  the background — first requests use the compiled-in default UA triple,
  fail-open).

## Graceful shutdown

On `SIGINT`/`SIGTERM` the server drains in two phases:

1. **Drain** — new requests get `503 Server is shutting down`; the config
   poller and UA sync stop; in-flight requests and streams run to completion
   (or to the upstream's own stall deadline) under `srv.Shutdown`.
2. **Force** — after `OCFP_SHUTDOWN_GRACE` (default 55 s) every still-tracked
   connection is force-closed, so a stuck stream cannot pin the process
   forever. Container stop commands should budget for the grace
   (`docker stop -t 60 …` to outlive the default 55 s). The shipped
   `compose.yaml` sets `stop_grace_period: 60s` for the same reason — a
   compose file without it SIGKILLs at compose's 10 s default, 45 s before
   the drain window ends.

## Production considerations

- **Inbound authentication is not this proxy's job.** It serves plain HTTP
  with no inbound auth by design — an internal sidecar. Keep it on a private
  network, or front it with your own ingress/reverse proxy if it must be
  reachable more widely.
- **TLS termination is not this proxy's job.** It serves plain HTTP by
  design; front it with your ingress/reverse proxy for TLS. The egress side
  supports `https` proxy types (TLS-to-the-proxy CONNECT hop) and `socks5`
  egresses natively.
- **Reloads are atomic per request.** Editing the mounted file is safe under
  traffic: requests in flight keep their generation; new requests pick up
  the swap. Invalid intermediate states (e.g. an editor's partial write that
  still parses but fails validation) keep the last good runtime. Mind the
  mount shape, though: with the shipped single-file bind mounts a host-side
  `rename(2)` is invisible to the container — a silent no-reload on a
  stale inode — so write in place, or mount the directory and rename
  atomically inside it to avoid serving a torn read window (see
  [Editing the mounted file](#editing-the-mounted-file)).
- **Health gating is opt-in by config.** With no `OCFP_CONFIG` the process
  runs health OFF — a config-less deployment can never acquire a
  failure-threshold outage. A config file enables it (threshold 3, cooldown
  30s by default).
- **Observability.** One JSON line per event on stdout, gated by the
  config's `log-level` (default `info`; `debug | info | warn | error`,
  hot-reloadable with the rest of the snapshot — see
  [configuration.md → Log level](configuration.md#log-level)). Every request
  logs one completion line at info with `generation`, `route`, `egress`,
  `attempts`, `class`, `status`, `latency_ms`, `model`, `endpoint`,
  `fallback`; set `log-level: debug` to also see opencode UA warm-up.
  The API is plain OpenAI-compatible: a served response adds only
  `X-OFP-Egress: <id>`, naming the configured egress it went out through, so a
  multi-egress deployment is diagnosable from the client side. Failure
  provenance — where a status came from, which step failed, whether a request
  byte provably left — is internal and is never published as a header; it
  reaches you through the completion line's `class`/`status`/`attempts` and
  through the `upstream_error` evidence events
  ([recovery-semantics.md → Injector ↔ OFP](recovery-semantics.md#injector--ofp)).
  Logs never contain credentials (proxy userinfo).
- **Egress sizing.** `max_concurrency` per egress bounds in-flight
  requests/streams; an egress at capacity is skipped (not failed) at dial
  time. Size it to what the upstream proxy vendor tolerates.
