# opencode-free-proxy

A minimal OpenAI-compatible router that exposes **only** the OpenCode free
provider — a line-for-line port of the hardened OpenCode handling in the
9router sources (`open-sse/`). Chat Completions and Responses APIs,
streaming and non-streaming.

## What it does

- **Two endpoints, one upstream**: `POST /v1/chat/completions`,
  `POST /v1/responses` (plus `GET /v1/models` and `GET /healthz`), proxied
  to the OpenCode Zen free tier with the full client-identity hardening —
  fingerprint tools, compound opencode User-Agent (synced from GitHub),
  session resolution, thinking-suffix handling.
- **Multi-egress routing**: per-egress http/https/socks5 proxies (or direct),
  routes with priorities and match conditions, round-robin and smooth
  weighted rotation.
- **Fallback + health**: distinct-egress fallback with a per-egress retry
  matrix, consecutive-failure cooldowns, typed 407 handling, streaming
  commitment (no fallback after the first byte).
- **One config file, hot-reloaded**: every service setting lives in a single
  YAML document — upstream base, UA-sync cadence,
  egresses, routes, fallback, health. Edits hot-reload in place; requests in
  flight keep the generation they started on.
- **Secret hygiene**: credentials are `${VAR}` env references resolved at
  load — the file carries no literals, logs and errors never echo them.

## Quick start

```sh
go run ./cmd/server          # listens on :8090, upstream https://opencode.ai
docker compose up -d --build # build + serve via compose (HOST_PORT, default 30258);
                             # create ./config.yaml first — see docs/deployment.md
```

With no config the built-in runtime serves the default upstream directly.
Point `OCFP_CONFIG` at a document to enable egress routing or a
different upstream:

```yaml
upstream:
  base: https://opencode.ai
user_agent:
  sync_interval: 3600
egress:
  - id: primary
    proxy:
      type: http
      url: "http://${PROXY_USER}:${PROXY_PASS}@proxy-a.example:8080"
  - id: direct # host's own network
routes:
  - id: default
    egress: [primary, direct] # fallback order after the scheduled head
```

Bootstrap env (the process itself — everything else lives in the file):

| Var                   | Default              | Meaning                                                      |
| --------------------- | -------------------- | ------------------------------------------------------------ |
| `OCFP_PORT`           | `8090`               | Listen port                                                  |
| `OCFP_CONFIG`         | _(empty = built-in)_ | Config document (YAML); hot-reloaded                         |
| `OCFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval (ms)                                |
| `OCFP_SHUTDOWN_GRACE` | `55000` (55s)        | Drain window: active streams finish before forced close (ms) |

## Architecture in one paragraph

Every request captures ONE immutable runtime generation at arrival and is
served entirely under it — route match, health policy, egress,
upstream base, fallback, logging — so a hot reload affects only requests
that start after the swap. Routing picks where to start, fallback picks what
to try after a retryable failure, health decides what may be tried at all;
streaming responses are a commitment once started. Details:
[docs/architecture.md](docs/architecture.md).

## Commands

```sh
go build ./...                       # compile
go test ./...                        # offline unit suite (httptest only, no egress)
go test -tags e2e ./e2e/             # black-box e2e: server subprocess + fake upstream
go vet ./... && go vet -tags e2e ./e2e/ && gofmt -l .   # must all be clean
golangci-lint run ./...              # CI's Lint step
pnpm format                          # prettier over docs/workflows/configs
```

## Documentation

| Doc                                            | What it covers                                                                                    |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| [docs/configuration.md](docs/configuration.md) | The full config document: schema, defaults, `${VAR}` interpolation, hot reload, validation errors |
| [docs/deployment.md](docs/deployment.md)       | Docker, Compose, config mounts, bootstrap env, graceful shutdown, production notes                |
| [docs/architecture.md](docs/architecture.md)   | Request pipeline, immutable generations, process-wide state lifecycles                            |
| [docs/recon-*.md](docs/README.md)              | Investigation records: UA chain, session continuity, mid-stream IP switches                       |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) — setup, commands, hooks, commit
conventions, and the release flow. Security matters go through
[SECURITY.md](SECURITY.md), never a public issue. Licensed
[Apache-2.0](LICENSE).
