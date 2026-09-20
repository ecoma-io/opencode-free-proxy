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
http/https/socks5, or no proxy at all = the host's own network), routes that
pick from them, and the global fallback/health policy. The file is re-read on
the `OFP_CONFIG_POLL_MS` interval — each poll hashes the bytes (SHA-256) and
re-parses only on change; a file that fails parse or validation keeps the
last good runtime and logs the rejection. `example.config.yaml` in the repo
root is the complete annotated schema, and
`internal/config/example_test.go` loads it through the real loader so the
example and the parser cannot drift.

Credentials in the file are `${VAR}` env references, never literals — an
unset variable is a load error naming it. (Interpolation runs over the raw
bytes, comments included, so keep placeholder syntax out of comments.)

```yaml
egress:
  - id: primary
    proxy:
      type: http # http | https | socks5 — socks5h is rejected at load
      url: "http://${PROXY_USER}:${PROXY_PASS}@proxy-a.example:8080"
    max_concurrency: 4 # in-flight cap; 0 = unlimited (default)
    models: ["*-free"] # glob allow-list (OR); empty = every model
    streaming: true # streaming requests allowed; default true
    max_body_bytes: 0 # eligibility gate; 0 = unlimited
    weight: 5 # weighted_round_robin share only
  - id: backup
    proxy: { type: socks5, url: "socks5://${PROXY_USER}:${PROXY_PASS}@proxy-b.example:1080" }
  - id: direct # no proxy key = host's own network
routes:
  - id: default
    priority: 0 # higher wins; file order breaks ties; first match serves
    egress: [primary, backup, direct] # fallback order after the head
    strategy: round_robin # or weighted_round_robin
    match: # AND of the set conditions; empty = catch-all
      # models: ["*-free"]       # AND here — every pattern must match
      # streaming: true
      # min_body_bytes: 0        # raw inbound body bytes
      # max_body_bytes: 10485760
fallback:
  enabled: true
  max_attempts: 3 # distinct egresses incl. the first; 0 = default 3
health:
  enabled: true # default true with a config file
  failure_threshold: 3 # consecutive failures before cooldown; 0 = never
  cooldown: 30s # Go duration string; bare numbers are a load error
```

### Schema

Types and defaults (applied at `Resolve`, `internal/config/file.go`, from the
constants in `internal/config/config.go` — weight 1, max_attempts 3, health
threshold 3, cooldown 30s, strategy round-robin):

| `egress[]`        | Type                          | Default               | Meaning                                                                                                                                  |
| ----------------- | ----------------------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `id`              | string                        | required, unique      | referenced by routes; names the egress in logs and `X-OFP-Egress`                                                                        |
| `proxy`           | map                           | _(omitted = direct)_  | `{ type, url }`; the transport an egress dials through                                                                                   |
| `proxy.type`      | `http` \| `https` \| `socks5` | required with `proxy` | `socks5h` is rejected by name — hostnames resolve locally, remote-DNS is out of scope by design                                          |
| `proxy.url`       | string                        | required with `proxy` | scheme must equal `type`; host required; optional `user:password@` userinfo (env-interpolated at load, redacted in every log/error path) |
| `enabled`         | bool                          | `true`                | `false` = configured but never scheduled                                                                                                 |
| `weight`          | int ≥ 0                       | `1`                   | feeds `weighted_round_robin` only (ignored under `round_robin`); shapes which egress STARTS, never eligibility                           |
| `max_concurrency` | int ≥ 0                       | `0` = unlimited       | in-flight requests/streams; an egress at capacity is skipped at dial time (a skip is not a failure)                                      |
| `models`          | []glob                        | empty = every model   | allow-list — ANY pattern matching admits the model (`path.Match` syntax against the suffix-stripped id)                                  |
| `streaming`       | bool                          | `true`                | `false` = streaming requests never pick this egress                                                                                      |
| `max_body_bytes`  | int ≥ 0                       | `0` = unlimited       | eligibility gate: larger requests never pick this egress (not a request cap — see [Body limits](#body-limits))                           |

| `routes[]`             | Type                                    | Default             | Meaning                                                                    |
| ---------------------- | --------------------------------------- | ------------------- | -------------------------------------------------------------------------- |
| `id`                   | string                                  | required, unique    | names the route in logs                                                    |
| `priority`             | int                                     | `0`                 | HIGHER wins; equal priorities keep file order; the first match serves      |
| `match.streaming`      | bool                                    | _(unset = any)_     | the client asked for `stream: true`                                        |
| `match.models`         | []glob                                  | _(unset = any)_     | AND — every listed pattern must match (disjoint globs would match nothing) |
| `match.min_body_bytes` | int                                     | `0` = unset         | raw inbound body must be ≥                                                 |
| `match.max_body_bytes` | int                                     | `0` = unset         | raw inbound body must be ≤                                                 |
| `egress`               | []id                                    | required, non-empty | ids must exist; order is the FALLBACK order after the scheduler's head     |
| `strategy`             | `round_robin` \| `weighted_round_robin` | `round_robin`       | how the head is picked per route (state is per route id, process-wide)     |

| `fallback`     | Type | Default | Meaning                                                                                   |
| -------------- | ---- | ------- | ----------------------------------------------------------------------------------------- |
| `enabled`      | bool | `true`  | `false` pins every request to its single scheduled head                                   |
| `max_attempts` | int  | `3`     | DISTINCT egresses one request may try, first included; `0` = default 3; negative rejected |

| `health`            | Type            | Default                   | Meaning                                                                                                    |
| ------------------- | --------------- | ------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `enabled`           | bool            | `true` with a config file | the built-in no-config runtime runs with health OFF (no outage can be acquired by config-less deployments) |
| `failure_threshold` | int ≥ 0         | `3`                       | consecutive failures that arm a cooldown; explicit `0` = never cool down                                   |
| `cooldown`          | duration string | `30s`                     | exclusion window once the threshold fires; `0s` = no window; bare numbers are a load error                 |

### Routing: match, then eligibility, then scheduling

Three separate decisions, in this order:

1. **Route matching** (`Runtime.MatchRoute`, `internal/config/file.go`):
   routes are sorted priority-descending (stable — file order breaks ties)
   and the FIRST route whose `match` holds serves the request. No match →
   400 `No route matched this request`.
2. **Hard eligibility** (`Server.routeHeads`, `internal/router/server.go`):
   the matched route's egress list is filtered — egress enabled, streaming
   gate, model allow-list, `max_body_bytes`, health-eligible, and under its
   concurrency cap (a soft check; re-checked at dial time). Empty result →
   502 `No eligible egress for route …`.
3. **Scheduling** (`routing.Scheduler.Plan`, `internal/routing/routing.go`):
   orders the INITIAL attempt among the survivors only — `round_robin`
   rotates a per-route cursor; `weighted_round_robin` is nginx's smooth
   weighted algorithm. The rest of the plan stays in the route's listed
   order for fallback.

`weight` never decides eligibility: every eligible egress stays in the
attempt list whatever its weight. A `weight: 0` egress contributes nothing to
the smooth-WRR totals and never heads the plan while any positive-weight
sibling is eligible; only when every eligible head is weight 0 does the first
listed one serve (deterministically). Under `round_robin`, weight is ignored
entirely.

### Body limits

Three different layers — do not conflate them:

1. **Server-level request cap: 8 MiB** (`internal/router/handler.go`) — any
   larger (or unreadable) body is rejected with 400 before routing. This is
   the only true request limit.
2. **`match.min_body_bytes` / `match.max_body_bytes`** — gate ROUTE
   SELECTION against the raw inbound body size; an unmatched size just picks
   a different route (or none → 400).
3. **`max_body_bytes` on an egress** — eligibility only: an over-limit
   egress is filtered out of the head set (all egresses filtered → 502), and
   `0` = unlimited. It never rejects a request by itself.

### Retry × fallback budgets

The upstream retry matrix sits INSIDE each fallback attempt:
429 → 0 retries (fail fast by contract), 502 → 3 retries @3s (4 POSTs),
503 → 3 @2s, 504 → 2 @3s; unlisted statuses never retry. Network errors draw
the 502 rule. The counter is ONE per URL shared by every retryable status and
network errors alike, and the cap is the firing rule's (base.js parity) —
alternating 502/503 gives up after 3 combined attempts, not 3 of each. A
typed proxy-auth failure is the exception: exactly ONE dial, no budget
consumed, straight to fallback (see
[Proxy-authentication (407)](#proxy-authentication-407-classification)).

The fallback loop then adds at most `fallback.max_attempts` DISTINCT egresses
per request (default 3, including the first; `enabled: false` or a budget < 1
pins the request to one attempt). Worst case for a 502 storm: 4 POSTs × 3
egresses = 12 upstream calls; a credential-refusing proxy storm is 1 POST per
egress. No inter-attempt sleep — the cooldown is health-based, per egress,
and applies to FUTURE requests only.

The client sees the LAST REAL verdict: when the plan runs out of egresses
before the budget, the final dialed egress's own status and message are
returned (base.js never synthesizes a failure — a 429 that outlives the plan
stays a 429, preserving the client's backoff semantics). The synthetic
`502 none of the eligible egresses could serve the request` appears only when
NOTHING was dialed — every plan entry was skipped (slot-full, unknown
egress, transport build failed) or the head set was empty (`attempts=0` in
that request's log line).

### Health

Policy and state are split (`internal/health/health.go`):

- **Policy** (`enabled`, `failure_threshold`, `cooldown`) is captured from
  the request's config snapshot and travels with it — a hot reload affects
  only requests that have not started.
- **State** (failure streak + cooldown deadline) is process-wide, keyed by
  egress id + transport signature (`type:url`). A policy-only reload keeps
  the key, so history continues across generations; swapping an egress's
  proxy URL changes the key, so a fresh transport never inherits the old
  transport's streak or cooldown.

Only real egress faults mark health: connection errors, typed proxy-auth,
response-header timeouts (60s), and 5xx after the retry matrix. 429 and every
other 4xx are verdicts about the REQUEST — they fall back (429) or not (4xx)
but never mark. Health is observed when response HEADERS arrive: a stream
that dies or stalls after a 200 start is the streaming commitment's abort,
not a health observation. Failures during an active cooldown never extend it;
a success resets the streak and clears the deadline — but a cooling egress is
filtered from the head set, so in practice only an in-flight request that was
planned before the arm can deliver that clearing success; the usual ways an
armed window ends are expiry, a process restart, or a proxy-URL swap (new
identity). With health disabled — or an explicit `failure_threshold: 0` —
observations record NOTHING: no streak accrual, and no clearing either, so
`0` means "never arm" (an already-armed window still runs to expiry; a
shortened `cooldown` in a new generation applies only on a later re-arm).
A threshold DECREASE never arms retroactively — only a new failing
observation crossing the observing request's threshold arms a cooldown.

### Proxy-authentication (407) classification

A 407 is classified `proxy_auth_error` ONLY when this proxy itself read the
proxy's status line — the proof is a typed `*proxyAuthError` produced at the
transport boundary (`internal/upstream/connect.go` for an HTTP(S) CONNECT
answered 407; `internal/upstream/socks5.go` for an RFC 1929 credential
rejection, or a demand for credentials that were never sent). Nothing is ever
promoted by error text — a dial error that happens to contain "407" classifies
as a connection error, never proxy-auth (`internal/upstream/failure.go`).

Any 407 arriving as a RESPONSE status is conservatively `client_error`: the
wire cannot tell whether it came from the origin through an established
CONNECT tunnel, or was relayed byte-for-byte by a plain-HTTP forward proxy,
so ownership is not claimed. Consequences: surfaced to the client, NO
fallback, no health mark.

`proxy_auth_error` itself falls back to the next egress, marks health, and —
uniquely — bypasses the per-egress retry matrix: one dial, then immediate
fallback, because the proxy will refuse the same credentials identically on
every retry. One honest exception: a 407 that loses the race against the
connect deadline classifies as a timeout (health-marked, retried) — the
typed proof never arrived, and text-probing to recover it is banned. SOCKS5
detail: REP `0x02` ("connection not allowed by ruleset") is a plain
connection error, not proxy-auth; only RFC 1929 rejections are. `socks5h` is
rejected at config load by name — remote-DNS semantics would silently change
which resolver sees upstream hostnames.

### Streaming commitment

Once the executor returns a live upstream response, the request is committed:
the relay owns it and NO fallback ever happens — downstream writes happen
only after `Execute` returns, so the guarantee is structural
(`internal/upstream/fallback.go`). A mid-stream death — transport reset, or a
stall past the 360s SSE stall timeout — aborts the downstream response; a
Responses passthrough client still receives a parseable `response.failed`
terminal plus `[DONE]` (`internal/router/stream.go`).

### Snapshot semantics (one request = one config generation)

Every request captures ONE immutable `Runtime` from the store (first file
load = generation 1, each hot-reload swap +1, no-config default = 0) and uses
it for its whole lifetime — route match, egress resolution, the health
policy, and the pinned `*config.Egress` list the executor dials. A reload
mid-request cannot change which egresses that request may fall back to: the
swap stamps only the NEW snapshot. Log lines carry `generation=N`; the
success log ends with `fallback=true` when an attempt actually fell back.
Responses carry `X-OFP-Egress: <id>` naming the egress that served them
(observability + the e2e suite's wire-level evidence).

Transports are cached per egress by transport signature, so a reload that
keeps a proxy URL reuses the same immutable client while a URL swap simply
adds a new entry. Health STATE also survives swaps — it is keyed by
id + signature — while the health POLICY each request applies is pinned to
that request's snapshot.

### Process-wide state lifecycles (health, rotation, transports)

Three pieces of process-wide state meet every reload, each with its own
migration rule — never reset-everything:

- **Health state** (`internal/health`) — keyed by egress id + transport
  signature. A policy-only reload names the same identities, so failure
  history continues across the swap. A proxy swap starts a fresh identity;
  the abandoned one is reclaimed by the once-per-generation maintenance
  (`onGeneration`), but never while an in-flight request still pins it:
  every request pins its matched route's identities at snapshot time and
  releases them when the request ends, so state a request is mid-way through
  adjudicating can never vanish under it. Reclaim is deterministic (one pass
  per generation) — never a timer.
- **Scheduler rotation** (`internal/routing`) — per-route round-robin cursor
  and smooth-WRR current-weights are fingerprinted by route id + strategy +
  ordered egress membership + effective weights (weights only matter under
  `weighted_round_robin`). A reload with the same fingerprint keeps rotation
  continuity; any fingerprint change resets that route's rotation
  deterministically (this is what keeps the weight-0-never-heads invariant
  true across reloads); a removed route's state is pruned in the same
  once-per-generation pass.
- **Transports** (`internal/router`) — cached by signature. Cache membership
  is NOT request ownership: a request owns the `*Client` it resolved by
  reference, so eviction cannot fail or destabilize it. Eviction closes the
  old transport's idle connections immediately (deterministic, observable)
  and the stale snapshot self-heals by rebuilding the client on its next
  dial. An active connection is never closed by the prune.

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

| Package              | Ports                                                                                                                                                                                         |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/config`    | runtime constants/env, multi-egress YAML model, hot-reload store, interpolation, redaction                                                                                                    |
| `internal/routing`   | route planning: model/streaming/body gates, round-robin + smooth weighted rotation, attempt order                                                                                             |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown, survives config swaps                                                                                                    |
| `internal/identity`  | session/request-id generation, UA triple cache + GitHub sync, session resolution chain                                                                                                        |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms                                                                                                                          |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                                                            |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                                                                       |
| `internal/upstream`  | HTTP client (retry matrix, SSE line scan), per-egress transports (direct, http/https CONNECT, socks5), failure taxonomy, executor transforms, headers, fallback executor, concurrency limiter |
| `internal/caps`      | per-model input-modality resolution (vision/pdf/audio/video)                                                                                                                                  |
| `internal/router`    | endpoints, chatCore pipeline, routing + fallback orchestration, forced-SSE-to-JSON, bypass/test-connection/modality/tool-dedupe stages                                                        |
| `e2e/`               | black-box e2e suite (`-tags e2e`): compiled server subprocess + fake upstream; opt-in live suite                                                                                              |

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
