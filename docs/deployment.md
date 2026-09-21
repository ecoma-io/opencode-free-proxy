# Deployment

## Docker

The image is a static Go binary on `scratch` — no shell, no curl. The
`HEALTHCHECK` runs the entrypoint binary itself against its own `/healthz`
(`opencode-free-proxy healthcheck`, 5 s timeout).

```sh
docker build -t opencode-free-proxy .
docker run -d -p 8090:8090 \
  -v "$PWD/config.yaml:/etc/ofp/config.yaml:ro" \
  -e OFP_CONFIG=/etc/ofp/config.yaml \
  opencode-free-proxy
```

## Docker Compose

The repo ships a minimal compose file (`compose.yaml`) that builds the image
and mounts `compose.config.yaml` (direct egress, auth off) as the config
document:

```sh
docker compose up -d --build                  # anonymous free tier
HOST_PORT=9090 docker compose up -d --build   # different host port
```

To require a Bearer key or a custom upstream, edit the mounted document (or
point the mount at your own file) — see
[configuration.md](configuration.md) for the schema and
[authentication.md](authentication.md) for the auth section. The mount is a
bind mount on purpose: editing the file on the host hot-reloads in place.

## The config file mount

`OFP_CONFIG` names the document path **inside the container**; the file it
points at must be mounted (read-only is fine — the proxy never writes it).
Secrets enter through `${VAR}` interpolation, resolved from the container's
environment at load time, so the mounted file itself carries no credentials:

```yaml
# compose.yaml (excerpt)
environment:
  OFP_CONFIG: /etc/ofp/config.yaml
  OFP_PRIMARY_API_KEY: ${OFP_PRIMARY_API_KEY} # forwarded from your shell/secret store
volumes:
  - ./config.yaml:/etc/ofp/config.yaml:ro
```

The file is re-read every `OFP_CONFIG_POLL_MS` (default 1 s); a bad rewrite
keeps the last good runtime — see
[configuration.md → Hot reload](configuration.md#hot-reload-semantics).

## Bootstrap environment

| Var                  | Default              | Meaning                                                                       |
| -------------------- | -------------------- | ----------------------------------------------------------------------------- |
| `PORT`               | `8090`               | Listen port (container listens on all interfaces)                             |
| `OFP_CONFIG`         | _(empty = built-in)_ | Config document path; hot-reloaded. Empty = built-in direct runtime, auth off |
| `OFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval (ms)                                                 |
| `OFP_SHUTDOWN_GRACE` | `30000`              | Drain window before force-close (ms)                                          |

No other process env vars exist; every service setting (upstream base, auth
keys, UA sync cadence, routing) lives in the config document.

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
2. **Force** — after `OFP_SHUTDOWN_GRACE` (default 30 s) every still-tracked
   connection is force-closed, so a stuck stream cannot pin the process
   forever. Container stop commands should budget for the grace
   (`docker stop -t 35 …` to outlive the default 30 s).

## Production considerations

- **Put auth on.** The built-in no-config runtime and `compose.config.yaml`
  run with auth off — fine for localhost, wrong for anything exposed. Add an
  `auth.keys` section and interpolate the secrets from the environment.
- **TLS termination is not this proxy's job.** It serves plain HTTP by
  design; front it with your ingress/reverse proxy for TLS. The egress side
  supports `https` proxy types (TLS-to-the-proxy CONNECT hop) and `socks5`
  egresses natively.
- **Reloads are atomic per request.** Editing the mounted file is safe under
  traffic: requests in flight keep their generation; new requests pick up
  the swap. Invalid intermediate states (e.g. an editor's partial write that
  still parses but fails validation) keep the last good runtime — but
  prefer atomic rewrites (`mv` over a `rename(2)`) to avoid serving a torn
  read window.
- **Health gating is opt-in by config.** With no `OFP_CONFIG` the process
  runs health OFF — a config-less deployment can never acquire a
  failure-threshold outage. A config file enables it (threshold 3, cooldown
  30s by default).
- **Observability.** Every request logs one completion line with
  `generation`, `route`, `egress`, `attempts`, `class`, `status`,
  `latency_ms`, `model`, `endpoint`, `fallback`, and `api_key_name` when auth
  is on. Responses carry `X-OFP-Egress: <id>`. Logs never contain
  credentials (key values, proxy userinfo) — see
  [authentication.md](authentication.md).
- **Egress sizing.** `max_concurrency` per egress bounds in-flight
  requests/streams; an egress at capacity is skipped (not failed) at dial
  time. Size it to what the upstream proxy vendor tolerates.
