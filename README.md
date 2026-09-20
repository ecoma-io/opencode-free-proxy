# opencode-free-proxy

A minimal OpenAI-compatible router that exposes **only** the OpenCode free
provider, ported line-for-line from the hardened OpenCode handling in
`9router-src` (`open-sse/`). Chat Completions and Responses APIs, streaming
and non-streaming.

## Run

```sh
go run ./cmd/server          # listens on :8090, upstream https://opencode.ai
docker compose up -d --build # or: build + serve via compose (HOST_PORT, default 30258)
```

Environment:

| Var                    | Default               | Meaning                                                       |
| ---------------------- | --------------------- | ------------------------------------------------------------- |
| `PORT`                 | `8090`                | Listen port                                                   |
| `OFP_UPSTREAM_BASE`    | `https://opencode.ai` | Zen upstream base (all routes incl. `/v1/models`)             |
| `OFP_API_KEY`          | _(empty = auth off)_  | Bearer key required from clients                              |
| `OFP_UA_SYNC_INTERVAL` | `3600000` (1h)        | UA identity sync cadence (ms) — see docs/recon-opencode-ua.md |
| `OFP_CONFIG`           | _(empty = built-in)_  | Multi-egress routing config file (YAML) — see below           |
| `OFP_CONFIG_POLL_MS`   | `1000`                | Hot-reload poll interval for `OFP_CONFIG` (ms)                |
| `OFP_SHUTDOWN_GRACE`   | `30000` (30s)         | Drain window: active streams finish before forced close (ms)  |

## Multi-egress routing (`OFP_CONFIG`)

`OFP_CONFIG` points at a YAML file describing egresses (per-egress proxy:
http/https/socks5 or direct), routes that pick from them, and the global
fallback/health policy. The file is re-read on the `OFP_CONFIG_POLL_MS`
interval — route/egress/health edits apply without a restart; invalid files
keep the last good runtime and are logged.

```yaml
egress:
  - id: primary
    proxy: { type: http, url: "https://creds@proxy-a.example:8080" }
    # max_concurrency: 4   # in-flight cap; 0 = unlimited (default)
    # models: ["*-free"]   # glob allow-list; empty = every model
    # streaming: true      # accepts streaming; default true
    # max_body_bytes: 0    # inbound body bound; 0 = unlimited
    # weight: 5            # share under weighted_round_robin
  - id: backup
    proxy: { type: socks5, url: "socks5://user:secret@proxy-b.example:1080" }
  - id: direct
    enabled: true # false = configured but never scheduled
routes:
  - id: default
    egress: [primary, backup, direct]
    strategy: round_robin # or weighted_round_robin
    match: # AND of the set conditions; empty = catch-all
      # models: ["*-free", "big-pickle"]
      # streaming: true
      # min_body_bytes: 0
      # max_body_bytes: 10485760
fallback:
  enabled: true
  max_attempts: 3 # distinct egresses incl. the first; 0 = default 3
health:
  enabled: true # default true with a config file
  failure_threshold: 3 # consecutive failures before cooldown; 0 = never
  cooldown: 1m # Go duration string ("30s", "1m")
```

Head selection is round-robin; a per-egress `weight` switches a route to
smooth weighted rotation (`strategy: weighted_round_robin`). A route
references egresses in fallback order — the planner rotates the head, the
executor walks the rest. 429s fall back but never mark an egress unhealthy;
5xx/network/connection errors count toward `failure_threshold` consecutive
failures and cool the egress for `cooldown`; 4xx (other than 429) never fall
back (the request is the problem, not the egress). Concurrency caps
(`max_concurrency`) skip an egress at capacity — a skip is not a failure. On
total failure the client gets a 502 envelope.

Defaults with a config file: health enabled (`failure_threshold` 3, `cooldown`
30s), fallback `max_attempts` 3 — set `health.enabled: false` to disable
gating. Unset `OFP_CONFIG` = the historical single direct egress,
byte-for-byte the old behavior: health gating is off there, so a config-less
deployment cannot acquire a failure-threshold outage.

## Endpoints

- `POST /v1/chat/completions` — OpenAI Chat Completions (SSE or JSON).
- `POST /v1/responses` — OpenAI Responses API (SSE or JSON).
- `GET /v1/models` — live free-tier model list from the upstream
  (ids ending `-free` plus `big-pickle`, minus known-dead ids), with a static
  registry fallback.
- `GET /healthz`.

## Model naming

`oc/` prefix is optional. Thinking suffixes work on any model:
`muse-spark-1.2-contributor-free(high)`, `(8192)`, `(none)`, `(auto)` —
parsed, applied to the upstream body (`reasoning.effort` / `reasoning_effort`)
and stripped before dispatch. `muse-spark*` models speak the Responses API
upstream and are translated transparently for chat clients.

## Pipeline (mirrors 9router chatCore + the chat.js pre-resolution stages)

1. `[1m]` context-marker strip (Claude Code 1M beta annotation).
2. Auth → missing-model check → `x-test-connection` probe (fixed synthetic
   completion, no upstream call) → claude-cli bypass short-circuit (warmup /
   title extraction / count / title-prompt patterns answer without an upstream
   call, streaming or not).
3. Detect endpoint format → snapshot the client's thinking intent.
4. Unsupported-modality strip: `image_url` / `input_image` / `file` / audio
   blocks are replaced with text placeholders when the model's resolved
   modality capabilities (exact table → glob patterns → name heuristic) say
   the model can't read them.
5. Prenorms: `normalizeThinkingConfig` → `ensureToolCallIds` →
   `fixMissingToolResponses`.
6. Format translation (`needsTranslation` when source ≠ target).
7. `applyThinking` (suffix/effort resolution) + `filterToOpenAIFormat` for
   chat-native targets, then claude-client tool dedupe (MCP/built-in
   duplicates).
8. Executor: session resolution (`x-opencode-*` headers, claude-code /
   antigravity extraction, assistant-text hashing), request transform
   (fingerprint tools, `max_output_tokens` clamp, `store=false`, input
   normalization), header forging (`Bearer public`, compound opencode
   User-Agent — the official CLI shape
   `opencode/<v> ai-sdk/provider-utils/<v> runtime/bun/<v>`, all three
   versions synced from GitHub (startup + hourly ticker, hot path is a pure
   cache read; [recon](docs/recon-opencode-ua.md)) — passed through
   verbatim when the downstream UA is a valid ≥ 1.17 opencode client and
   forged otherwise, `x-opencode-client/request/project/session`) —
   headers are rebuilt on every retry attempt.
9. Retry matrix: 429 → no retry; 502 ×3 @3s; 503 ×3 @2s; 504 ×2 @3s;
   network errors follow 502 — one shared attempt budget across all retryable
   statuses. 60 s response-header timeout, 360 s stream stall (reset per line).
10. Relay: format-matched passthrough (with usage estimation seam) or
    translation; non-streaming clients get the forced SSE→JSON aggregate with
    the same usage/thinking synthesis as the JS router (only when the upstream
    actually answered SSE — otherwise the stream path handles it, exactly like
    the JS fall-through).

## Layout

| Package              | Ports                                                                                                                                                |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/config`    | runtime constants/env, multi-egress YAML model, hot-reload store, interpolation, redaction                                                           |
| `internal/routing`   | route planning: model/streaming/body gates, round-robin + smooth weighted rotation, attempt order                                                    |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown, survives config swaps                                                           |
| `internal/identity`  | session/request-id generation, UA triple cache + GitHub sync, session resolution chain                                                               |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms                                                                                 |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                   |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                              |
| `internal/upstream`  | HTTP client (retry, SSE line scan), per-egress transports (http/socks5/direct), executor transforms, headers, fallback executor, concurrency limiter |
| `internal/caps`      | per-model input-modality resolution (vision/pdf/audio/video)                                                                                         |
| `internal/router`    | endpoints, chatCore pipeline, routing + fallback orchestration, forced-SSE-to-JSON, bypass/test-connection/modality/tool-dedupe stages               |
| `e2e/`               | black-box e2e suite (`-tags e2e`): compiled server subprocess + fake upstream; opt-in live suite                                                     |

## Tests

```sh
go test ./...                # offline unit suite
go test -tags e2e ./e2e/     # black-box e2e: compiles the server, runs it as a
                             # subprocess against a fake zen upstream
E2E_LIVE=1 go test -tags e2e ./e2e/ -run TestLive   # optional: real proxy + real upstream
```

See `e2e/README.md`. Golden unit vectors are ported from
`9router-src/tests/unit/opencode-*.test.js`
(session ids, client version gate, tool-choice forcing, max output tokens,
muse-spark thinking) plus end-to-end httptest coverage of every relay branch.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) — setup, commands, hooks, commit
conventions, and the release flow. Security matters go through
[SECURITY.md](SECURITY.md), never a public issue. Licensed
[Apache-2.0](LICENSE).
