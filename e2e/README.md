# E2E tests

Black-box tests: the real server binary is compiled, launched as a subprocess
(`PORT` / `OFP_API_KEY` / `OFP_UPSTREAM_BASE`) against a fake OpenCode Zen
upstream, and exercised over HTTP like any external client.

They are behind the `e2e` build tag — the default `go test ./...` unit suite
(fully offline, `httptest`-only) never builds them.

## Local suite (fake upstream, hermetic)

```sh
go test -tags e2e ./e2e/
```

`TestMain` builds `./cmd/server`, starts a fake `/zen/v1` upstream (SSE chat +
responses + models, request recording, a loose replica of the free-tier gate:
mandatory `stream:true`, `opencode/*` UA) and polls `/healthz` before the tests
run.

Covered, end to end through the wire:

- `/healthz`; auth on/off (401 envelope + CORS, no upstream call)
- chat non-streaming: forced upstream SSE → JSON aggregate (content, usage
  5/2/7, finish reason); upstream sees `stream:true`, `Bearer public`,
  `opencode/*` UA, the fingerprint tool quartet
- chat streaming: deltas + usage chunk + `[DONE]` forwarded
- `/v1/responses` non-streaming → aggregated `response` object (id, status
  `completed`, usage) against the muse-spark upstream path
- `/v1/responses` streaming → event-framed SSE, no synthesized
  `response.failed` when a terminal event exists, `[DONE]` terminator
- upstream 403 → `[403]: …` error envelope (`permission_error` /
  `insufficient_quota`)
- `x-test-connection` probe → fixed `Hello!` completion, zero upstream calls
- claude-cli `Warmup` bypass → synthetic completion, zero upstream calls
- `big-pickle[1m]` → marker stripped before dispatch
- `image_url` on a non-vision model → stripped, placeholder text upstream
- `/v1/models` → free filter (`-free` + `big-pickle`, dead ids dropped, sorted)

### Config snapshot, fallback & shutdown (`reload_test.go`, `shutdown_test.go`)

Each test spawns its OWN server subprocess with bespoke env
(`OFP_CONFIG`, `OFP_CONFIG_POLL_MS`, `OFP_SHUTDOWN_GRACE`) and real HTTP
forward proxies per egress — per-egress behavior is observable on the wire
even though `OFP_UPSTREAM_BASE` is a single value:

- hot reload mid-request: an in-flight request keeps its generation-1 plan,
  falls back to the OLD route's egress, never dials the new route, and logs
  `generation=1 fallback=true`; the next request pins generation 2 and dials
  the new egress (`X-OFP-Egress` headers prove both)
- 429 falls back but never marks the egress unhealthy (a later round-robin
  request is served by it again); a 5xx DOES poison it with
  `health.failure_threshold: 1` — `TestEgress429FallsBackButStaysHealthy`
  proves the exclusion with a final round that would round-robin onto the
  500-egress if it were still eligible, and asserts its call count stayed 3
  (the two 429s + the one 500 it actually served).
- streaming commitment: after the first byte is written, a mid-stream
  upstream death never triggers fallback
- SIGTERM: new requests answer 503 (drain gate) while the in-flight stream
  finishes under grace; a stream that stalls past `OFP_SHUTDOWN_GRACE` is
  force-closed

### Tool pipeline & client personas (`tools_test.go`)

Cross-interface checks — clients with different tool shapes must all reach
the upstream correctly:

- Claude Code persona (`claude-code/*` UA or `x-app: cli`): Exa MCP presence
  strips the duplicated `WebSearch`/`WebFetch` built-ins while every other
  client tool survives verbatim; the lowercase fingerprint quartet is
  injected alongside the client's capitalized `Bash` (casing never satisfies
  the gate)
- Non-claude clients: identical tool list passes through untouched (dedupe is
  claude-gated), quartet still merged
- Tools-less caller: quartet + `tool_choice: "none"` (model can't call the
  injected no-ops)
- Chat client → muse-spark: translated to `/zen/v1/responses` upstream —
  input items, flattened tools, `tool_choice` demoted to auto (muse is
  auto-only), `store:false`
- Native Responses client → muse: explicit non-auto `tool_choice` demoted
- Upstream tool_call round trip: split SSE fragments (id + partial name +
  arguments across chunks) reassembled for non-streaming chat clients
  (`finish_reason: tool_calls`, `content: null`), forwarded raw for
  streaming clients, and converted to a `function_call` output item for
  Responses clients on a chat-native model
- Agent-loop follow-up: assistant `tool_calls` + `role:"tool"` result reach
  upstream with id linkage verbatim; a missing tool_call id is repaired to
  the deterministic `call_msg1_tc0_read_file`
- Session/header personas: native `ses_…` passes through verbatim, a Claude
  Code session id is translated to the opencode shape, client-declared
  `x-opencode-client` honored over the `desktop` default

## Live suite (real proxy + real upstream)

Against an already-running proxy (default `http://127.0.0.1:8090`). Skipped
unless `E2E_LIVE=1`; spends free-tier quota.

```sh
E2E_LIVE=1 go test -tags e2e ./e2e/ -run TestLive
# optional:
#   E2E_BASE_URL=http://127.0.0.1:9000   target proxy
#   E2E_API_KEY=...                      value of the proxy's OFP_API_KEY
```

Covers the live model list (free filter holds), one tiny chat completion, and
one muse-spark streaming responses request.
