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
bind mount on purpose: editing the file on the host hot-reloads in place.

## The config file mount

`OCFP_CONFIG` names the document path **inside the container**; the file it
points at must be mounted (read-only is fine — the proxy never writes it).
Secrets enter through `${VAR}` interpolation, resolved from the container's
environment at load time, so the mounted file itself carries no credentials —
forward each referenced variable through `compose.yaml`'s `environment` when
you add one:

```yaml
# compose.yaml (excerpt)
environment:
  OCFP_CONFIG: /etc/ofp/config.yaml
  # OCFP_PROXY_PASSWORD: ${OCFP_PROXY_PASSWORD} # forward vars your config.yaml references
volumes:
  - ./config.yaml:/etc/ofp/config.yaml:ro
```

The file is re-read every `OCFP_CONFIG_POLL_MS` (default 1 s); a bad rewrite
keeps the last good runtime — see
[configuration.md → Hot reload](configuration.md#hot-reload-semantics).

## Bootstrap environment

| Var                   | Default              | Meaning                                                             |
| --------------------- | -------------------- | ------------------------------------------------------------------- |
| `OCFP_PORT`           | `8090`               | Listen port (container listens on all interfaces)                   |
| `OCFP_CONFIG`         | _(empty = built-in)_ | Config document path; hot-reloaded. Empty = built-in direct runtime |
| `OCFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval (ms)                                       |
| `OCFP_SHUTDOWN_GRACE` | `55000`              | Drain window before force-close (ms)                                |

No other process env vars exist; every service setting (upstream base, UA
sync cadence, routing) lives in the config document.

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
   (`docker stop -t 60 …` to outlive the default 55 s).

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
  still parses but fails validation) keep the last good runtime — but
  prefer an atomic rewrite (write a temp file in the same directory, then
  `rename(2)` it into place) to avoid serving a torn read window.
- **Health gating is opt-in by config.** With no `OCFP_CONFIG` the process
  runs health OFF — a config-less deployment can never acquire a
  failure-threshold outage. A config file enables it (threshold 3, cooldown
  30s by default).
- **Observability.** Every request logs one completion line with
  `generation`, `route`, `egress`, `attempts`, `class`, `status`,
  `latency_ms`, `model`, `endpoint`, `fallback`. Responses carry
  `X-OFP-Egress: <id>`. Logs never contain credentials (proxy userinfo).
- **Egress sizing.** `max_concurrency` per egress bounds in-flight
  requests/streams; an egress at capacity is skipped (not failed) at dial
  time. Size it to what the upstream proxy vendor tolerates.
