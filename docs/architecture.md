# Architecture

A request flows through one pipeline, pinned to **one immutable runtime
generation** from arrival to last log line:

```text
HTTP request
  → Runtime snapshot (captured ONCE, generation N)
  → Route match     (routes of generation N)
  → Health policy   (policy pinned from generation N)
  → Scheduler       (round-robin / weighted head selection)
  → Egress          (per-egress transport of generation N)
  → Upstream        (upstream.base of generation N)
  → Fallback        (fallback policy of generation N; retry matrix per attempt)
  → Response        (+ completion log: generation=N route=… egress=…)
```

## Immutable runtime generations

The config document is loaded into an immutable `Runtime` snapshot
(`internal/config`). The store (`config.Store`) holds the current snapshot
behind an atomic pointer and hot-reloads by swapping the pointer:

- first file load = **generation 1**; each accepted reload = +1; the
  built-in no-config runtime = generation 0;
- **a request is bound to one Runtime generation for its lifetime.** The
  handler captures the snapshot exactly once, at arrival, and every later
  stage — route match, the health policy,
  egress resolution, `UpstreamBase()`, the UA configuration, the fallback
  policy, and the completion log — reads that same snapshot. Nothing after
  arrival re-reads the store (`internal/router/handler.go`);
- a reload therefore affects only requests that **start** after the swap. It
  can never split an in-flight request across two generations — not its
  upstream, not its fallback plan, not its logged generation;
- log lines carry `generation=N` naming the request's own snapshot — log
  facts never mix generations.

The snapshot is deeply copied at `Resolve` (egresses, routes): nothing
outside the loader can write a live snapshot, and a request
mutating what an accessor returned can never reach the store.

## Route vs eligibility vs health vs fallback

Four separate decisions over the one snapshot:

1. **Route match** — priority-descending, first `match` that holds serves
   (`Runtime.MatchRoute`).
2. **Hard eligibility** — the matched route's egress list filtered by
   enabled / streaming / model allow-list / body bound / health-eligible /
   concurrency headroom (`Server.routeHeads`).
3. **Scheduling** — orders only the INITIAL attempt among survivors
   (`routing.Scheduler.Plan`).
4. **Fallback** — the attempt loop walks the route's remaining egresses
   under the snapshot's `fallback` policy and the per-egress retry matrix
   (`internal/upstream`). Once a live upstream response exists the request
   is committed: no fallback after the first streamed byte.

Health (consecutive-failure cooldown) is the temporary-eligibility layer on
top: **policy** travels with the request's snapshot, **state** is
process-wide and keyed by egress id + transport signature.

## Process-wide state lifecycles

Three pieces of process-wide state meet every reload, each with its own
migration rule — never reset-everything. The once-per-generation maintenance
(`Server.onGeneration`) runs prune + reclaim + rotation-prune as ONE step
under the client mutex, guarded by a monotonic generation CAS so a request
holding a stale snapshot can never prune against its older keep-set.

- **Health state** (`internal/health`) — keyed by egress id + transport
  signature. A policy-only reload names the same identities, so failure
  history continues across the swap. A proxy swap starts a fresh identity;
  the abandoned one is reclaimed in the once-per-generation pass — but never
  while an in-flight request still pins it: every request pins its matched
  route's identities at routing time and releases them when the request
  ends. (Boundary case: a request bound to the older generation can reach
  its pin after a newer generation's reclaim already dropped an identity the
  new config removed; that request then plans against reset — healthy —
  state for it. Health is advisory; routing and fallback semantics are
  unaffected, and the observation re-registers the state.)
- **Scheduler rotation** (`internal/routing`) — per-route round-robin cursor
  and smooth-WRR current-weights are fingerprinted by route id + strategy +
  ordered egress membership + effective weights. A reload with the same
  fingerprint keeps rotation continuity; any fingerprint change resets that
  route's rotation deterministically; a removed route's state is pruned in
  the same pass.
- **Transports** (`internal/router`) — cached by transport signature.
  Cache membership is NOT request ownership: a request owns the `*Client` it
  resolved by reference, so eviction cannot fail or destabilize it. Eviction
  closes the old transport's idle connections immediately; the stale
  snapshot self-heals by rebuilding the client on its next dial. An active
  connection is never closed by the prune.

## The request pipeline

Mirrors 9router chatCore plus the chat.js pre-resolution stages
(`internal/router/handler.go`; line-for-line port of `open-sse/` — see
AGENTS.md for the porting discipline):

1. `[1m]` context-marker strip (Claude Code 1M beta annotation) →
   missing-model check → `x-test-connection` probe (fixed synthetic
   completion, no upstream call) → claude-cli bypass short-circuit (warmup /
   title extraction / count / title-prompt patterns answer without an
   upstream call, streaming or not).
2. Detect endpoint format → snapshot the client's thinking intent.
3. Unsupported-modality strip: `image_url` / `input_image` / `file` / audio
   blocks are replaced with text placeholders when the model's resolved
   modality capabilities say the model can't read them.
4. Prenorms: `normalizeThinkingConfig` → `ensureToolCallIds` →
   `fixMissingToolResponses`.
5. Format translation (`needsTranslation` when source ≠ target).
6. `applyThinking` (suffix/effort resolution) + `filterToOpenAIFormat` for
   chat-native targets, then claude-client tool dedupe (MCP/built-in
   duplicates).
7. Executor: session resolution (`x-opencode-*` headers, claude-code /
   antigravity extraction, assistant-text hashing), request transform
   (fingerprint tools, `max_output_tokens` clamp, `store=false`, input
   normalization), header forging (`Bearer public`, compound opencode
   User-Agent synced from GitHub — passed through verbatim when the
   downstream UA is a valid ≥ 1.17 opencode client and forged otherwise) —
   headers are rebuilt on every retry attempt.
8. Retry matrix: 429 → no retry; 502 ×3 @3s; 503 ×3 @2s; 504 ×2 @3s;
   network errors follow 502 — one shared attempt budget across all retryable
   statuses. 60 s response-header timeout, 360 s stream stall (reset per
   line).
9. Relay: format-matched passthrough (with usage estimation seam) or
   translation; non-streaming clients get the forced SSE→JSON aggregate
   with the same usage/thinking synthesis as the JS router (only when the
   upstream actually answered SSE — otherwise the stream path handles it,
   exactly like the JS fall-through).

## Endpoints

- `POST /v1/chat/completions` — OpenAI Chat Completions (SSE or JSON).
- `POST /v1/responses` — OpenAI Responses API (SSE or JSON).
- `GET /v1/models` — live free-tier model list from the upstream, with a
  static registry fallback (fail-open).
- `GET /healthz` — liveness.

Model naming: the `oc/` prefix is optional; thinking suffixes
`(high)/​(8192)/​(none)/​(auto)` are parsed, applied to the upstream body, and
stripped before dispatch; `muse-spark*` models speak the Responses API
upstream and are translated transparently for chat clients.

## Package layout

| Package              | Role                                                                                                                                                         |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `cmd/server`         | entrypoint; also serves `healthcheck` (Docker HEALTHCHECK on `scratch`)                                                                                      |
| `internal/config`    | every runtime constant + bootstrap env vars; multi-egress YAML model, interpolation, redaction, hot-reload store, the immutable `Runtime` snapshot           |
| `internal/routing`   | route planner: model/streaming/body gates, round-robin + smooth weighted rotation, snapshot-pinned attempt order                                             |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown; state survives config swaps, policy pinned per request                                  |
| `internal/router`    | endpoints + chatCore pipeline + bypass/test-connection/modality/tool-dedupe stages + routing/fallback orchestration + the per-request snapshot capture       |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                           |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms, modality strip                                                                         |
| `internal/upstream`  | HTTP client (retry matrix, failure taxonomy, SSE line scan), per-egress transports (direct, http/https CONNECT, socks5), executor transforms, header forging |
| `internal/cloak`     | thinking suffix parse/apply, model id/URL, fingerprint tools                                                                                                 |
| `internal/identity`  | session/request ids, opencode UA triple cache + GitHub sync loop (fail-open), session resolution chain                                                       |
| `internal/caps`      | per-model input-modality resolution (exact table → glob patterns → name heuristic)                                                                           |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                                      |
| `internal/jsonx`     | JS-semantics JSON accessors (`AsStr`/`AsArr`/`Truthy`/…)                                                                                                     |
| `e2e/`               | black-box e2e suite behind the `e2e` build tag (see `e2e/README.md`)                                                                                         |
