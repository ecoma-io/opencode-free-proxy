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
  → Upstream        (upstream.base of generation N; ONE logical call)
  → Failover        (fallback policy of generation N; pre-request failures ONLY)
  → Response        (+ completion log: generation=N route=… egress=…)
```

## Logging

JSON lines on stdout (one logger, `internal/logging`), gated by a
**package-global zerolog level** that mirrors `log-level` from the config
snapshot. The global level, not per-module loggers, is the design: every
event checks it at emit time, so a config reload that flips the level applies
it atomically to the next event — no handler state is touched.

- the **reload goroutine is the only runtime writer** of
  `zerolog.SetGlobalLevel` (`config.Store.tick`); `cmd/server` bootstraps the
  first level after the initial load;
- the level travels with the snapshot like any other field — an in-flight
  request still logs under the threshold of the generation it pinned;
- fields are the JSON-envelope constants `time` / `level` / `msg`,
  initialized once in `internal/logging`.
- every event carries `generation=N` naming its request's snapshot; events
  from the store reload path carry the NEW generation after a swap.

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
4. **Failover** — the executor makes exactly ONE logical upstream call and
   walks the route's remaining egresses only when that call failed at a
   phase that provably preceded transmission (`Failure.ReplaySafe()`:
   origin `transport` + state `not_sent`), bounded by the snapshot's
   `fallback` policy (`internal/upstream`). A provider response of any
   status is terminal, and once a live upstream response exists the
   request is committed: no failover after the first streamed byte. See
   `docs/recovery-semantics.md` for the contract this implements.

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
   headers are rebuilt per egress attempt.
8. One logical upstream call, then a failover decision. There is no
   per-egress retry matrix: a provider response (2xx, 429, any 4xx/5xx) is
   relayed verbatim and ends the call, and the route's next egress is tried
   only when the attempt failed before the request existed — a dial that
   never put a byte on the wire. 60 s response-header timeout, 360 s stream
   stall (reset per line).
9. Relay: format-matched passthrough (with usage estimation seam) or
   translation; non-streaming clients get the forced SSE→JSON aggregate
   with the same usage/thinking synthesis as the JS router (only when the
   upstream actually answered SSE — otherwise the stream path handles it,
   exactly like the JS fall-through).

## Upstream error evidence (forensics)

Every failed upstream interaction leaves one structured log event, so a
request's log alone reconstructs **request → attempts → egress → phase →
provenance → verdict → classification → health → failover decision →
outcome**. The layer is strictly observational: nothing it records
influences routing, failover, health or streaming behavior, and the
successful attempt that serves a request produces no event at all (the
completion line owns success telemetry — happy-path log volume is
unchanged).

- **Capture happens where facts become known, before classification
  reduces them** — ordering is response → capture → classify → health →
  failover decision → emit. An HTTP verdict row is extracted from the same
  capped body slice `parseUpstreamError` reads (never a second read), so
  rate-limit headers and structured `error.type`/`error.code` are on the
  row as the provider sent them.
- **One warn `upstream_error` event per row, emitted exactly once**
  (`internal/router/evidence_log.go` is the only emit boundary — a
  post-header abort renders only its own new row, never re-rendering what
  the post-Execute pass already logged); skipped plan entries (`slot_full`,
  `transport_build`, `unknown_egress`) render at debug as
  `egress_skipped` — scheduling diagnostics stay out of the info stream.
- **Correlation**: every event carries `request_id`; a failure row carries
  `attempt_id` = `request_id/N`, where N is the cross-egress attempt — one
  logical upstream call, so there is no dial suffix. Stream-phase rows and
  skips have no attempt id (the attempt already committed and returned, or
  never dialed at all).
- **Phases, not re-classification**: `response` (an HTTP verdict ≥ 400),
  `transport` (no HTTP response exists), `stream`/`forced` (a live
  response died or failed conversion after headers). Stream/forced rows
  carry class `response_started` — the reserved logging-only
  classification: the delivered status stays the delivered status, no
  health observation, no failover (streaming commitment untouched). The
  `status` field records what the UPSTREAM did — the status the response
  started with — never the synthesized client 502 a forced-conversion
  failure writes downstream (that is the error-write path's fact, visible
  in the completion line).
- **The row states the decision, not just the outcome**:
  `health_decision` is `marked` | `neutral` and `fallback_decision` is
  `fallback` | `stop`, both fixed at the moment the executor decided — so
  one row answers "was this failure attributed to the egress, and did the
  request move on?". A verdict row (429, any 4xx/5xx) is always
  `neutral` + `stop`: the provider answered, so nothing about the egress
  failed and there is nothing to move past. Only a replay-safe transport
  failure can be `marked` + `fallback`.
- **Provenance rides the row**: `origin` (`upstream` | `transport` |
  `client`), `failure_phase` and `request_state`. An HTTP verdict is
  `upstream` / `response_headers` / `response_started`; a pre-request
  transport failure is `transport` / its dial phase / `not_sent` — the
  same evidence the failover decision was made from, rendered for the
  operator.
- **What a row may never contain**: request bodies, tool arguments,
  authorization/cookie headers, proxy URLs (transport rows carry the
  proxy _type_ only), raw session ids. The session travels as
  `session_fp` = sha256(session id)[:16] — a stable **pseudonym for
  correlation, not anonymization** (an operator with known ids could
  brute-force it); the request body as `request_body_sha256`[:16],
  computed lazily only when evidence is emitted.
- **Fingerprints** (`error_fingerprint`, FNV-1a over
  status|type|code|normalized message) group the same logical error
  across attempts/egresses/requests: digit runs fold ("retry in 17s" ≡
  "retry in 31s"), and address-shaped tokens strip (host:port — the shape
  that varies across egresses — plus dotted numeric hosts), while dotted
  identifiers ("config.yaml" vs "secrets.env") keep distinguishing.
  Grouping is deliberately coarse where shapes coincide ("file.go:42"
  folds like host:port; version numbers fold with any digits) — the
  `message` field disambiguates within a group. A fingerprint is an
  equality key for humans — never an input to behavior.
- **Bounds**: 16 rows per request (past that, a `dropped` counter rides
  the last event), 512 B messages, 256 B body peeks, 8 rate-limit
  entries, 64 B header values. A hostile upstream cannot grow memory or
  log volume through this layer. SSE is never buffered for evidence —
  stream rows record labels and timing only.
- **All text is sanitized** (control bytes folded, whitespace collapsed,
  rune-safe clamps) and `%q`-quoted in the rendered line (CWE-117).
- **Present/absent semantics**: absent information is an absent field —
  missing rate-limit headers, an uncapped egress's in-flight, a
  failure phase that could not be attributed. Nothing fakes a default.

Logs are the analysis surface for upstream failures by design: the
proxy has no metrics subsystem, so nothing here can introduce
high-cardinality metric labels.

## Transport failure provenance

`internal/upstream/provenance.go` answers the one question a relay must be
able to answer before it re-sends anything: **did this request reach the
provider?** Every failure carries a `Failure{Class, Origin, Phase,
RequestState}` — where it happened, at which protocol step, and what the
transport can _prove_ about transmission. The contract those fields serve is
`docs/recovery-semantics.md`; this section is how they are produced.

- **`RequestState` is `not_sent` | `unknown` | `response_started`**, and
  `unknown` is deliberately the zero value: an unset state must never read as
  proof of non-transmission. An HTTP verdict of any status is
  `response_started` (a response existing proves the request arrived); a
  context cancellation is `client` and proves nothing.
- **Proof only at the boundaries this package owns.** The dialers record
  their phase as they climb it — proxy TCP connect, proxy TLS, proxy
  credential exchange, SOCKS5 greeting/auth/CONNECT, CONNECT request/read,
  target TCP connect, origin TLS. A failure at a recorded dial phase is
  `not_sent`; a failure at any phase _after_ the dial succeeded is
  `not_sent` only if the traced connection never carried a request byte.
- **Everything past the hand-off is `unknown`.** `net/http` owns request
  write, header wait and body read; this package cannot prove what left the
  socket, so a request-write failure, a response-header timeout and a reset
  after transmission all report `unknown` — and `ReplaySafe()` is false for
  them. A response-header timeout is the canonical case: the provider may be
  executing the request right now.
- **No dial means no proof.** The trace rides the attempt's request context
  (viable because `net/http` builds its dial context with
  `context.WithoutCancel`, which retains values), and `net/http` skips the
  dial entirely on a pooled connection — so a failure on a reused connection
  has no phase and degrades to `unknown`, never `not_sent`.
- **The write flag is set on write _entry_.** The final connection handed to
  `net/http` is wrapped (`recordingConn`), and it marks the attempt as having
  transmitted on entry to the write, not on success — a partial write may have
  put a prefix on the wire, and an _attempted_ write must close the `not_sent`
  door permanently. The wrapper is applied at the outermost layer of each
  transport path exactly once, so TLS handshake records are never counted as
  request bytes. It is transparent when no trace is in the context.
- **Nothing is inferred from error text.** Phases come from the boundary that
  spoke the protocol and from Go's error typing (`net.Error.Timeout()`,
  `*net.OpError.Op`); classifying by `strings.Contains(err.Error(), …)` is
  banned (issue #6). The one post-hand-off inference is that a `net.http`
  write op means the write ran — typed, not textual.
- **Observational, like the rest of the forensics layer.** These fields are
  recorded on evidence rows and rendered as `origin` / `failure_phase` /
  `request_state`; they are fixed vocabulary and carry no egress identity
  (no proxy URL, host, port or credential can reach them). Where the phase
  cannot be attributed the field is absent, which is an honest gap rather
  than a guess. `ReplaySafe()` is the single predicate the recovery policy
  reads — `origin = transport ∧ request_state = not_sent` — and it gates
  both halves of recovery: whether the attempt may move to another egress
  and whether the egress is marked unhealthy.
- **The same record labels the response on the wire** (issue #55,
  `internal/router/provenance_header.go`). The executor returns the terminal
  `Failure`, not just its `Class`, because a caller cannot act on a status
  alone: a 502 the provider sent and a 502 this process synthesized because
  the egress path failed demand opposite responses. `X-OFP-Failure-Origin:
upstream | gateway` is written from the recorded `Origin` — never from the
  status — with `X-OFP-Failure-Phase` and `X-OFP-Request-State` riding along
  on `gateway` only. `OriginClient` writes nothing: no interaction concluded,
  so there is nothing to attribute. Absence means "not an upstream-interaction
  outcome" (a local rejection, a draining server, a synthetic completion),
  never "upstream". Every inbound `X-OFP-*` header is stripped at the top of
  the pipeline, so a public client can neither forge provenance nor reach a
  decision through the namespace.

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

| Package              | Role                                                                                                                                                                                                            |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/server`         | entrypoint; also serves `healthcheck` (Docker HEALTHCHECK on `scratch`)                                                                                                                                         |
| `internal/logging`   | zerolog construction + the compatibility adapter at constructor edges; centralized `time`/`level`/`msg` field names                                                                                             |
| `internal/config`    | every runtime constant + bootstrap env vars; multi-egress YAML model, interpolation, redaction, hot-reload store, the immutable `Runtime` snapshot                                                              |
| `internal/routing`   | route planner: model/streaming/body gates, round-robin + smooth weighted rotation, snapshot-pinned attempt order                                                                                                |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown; state survives config swaps, policy pinned per request                                                                                     |
| `internal/router`    | endpoints + chatCore pipeline + bypass/test-connection/modality/tool-dedupe stages + routing/failover orchestration (one logical upstream call) + the per-request snapshot capture + the evidence emit boundary |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                                                                              |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms, modality strip                                                                                                                            |
| `internal/upstream`  | HTTP client (single-call execute, failure taxonomy + provenance, SSE line scan), per-egress transports (direct, http/https CONNECT, socks5), executor transforms, header forging, the error-evidence recorder   |
| `internal/cloak`     | thinking suffix parse/apply, model id/URL, fingerprint tools                                                                                                                                                    |
| `internal/identity`  | session/request ids, opencode UA triple cache + GitHub sync loop (fail-open), session resolution chain                                                                                                          |
| `internal/caps`      | per-model input-modality resolution (exact table → glob patterns → name heuristic)                                                                                                                              |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                                                                                         |
| `internal/jsonx`     | JS-semantics JSON accessors (`AsStr`/`AsArr`/`Truthy`/…)                                                                                                                                                        |
| `e2e/`               | black-box e2e suite behind the `e2e` build tag (see `e2e/README.md`)                                                                                                                                            |
