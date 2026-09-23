# Configuration

Everything the proxy does at runtime is configured through **one YAML
document** (`OCFP_CONFIG`) plus a handful of **bootstrap environment
variables** that shape the process itself. Service settings — the upstream
base and the UA-sync cadence — plus egresses, routes, fallback and
health all live in the config document, not in env vars. (The pre-0.4
`OFP_UPSTREAM_BASE` / `OFP_UA_SYNC_INTERVAL` env vars moved into the config
document in v0.4.0; inbound auth survived one more release as config
document `auth.keys` and was removed entirely in v0.5.0 — the removed
`OFP_API_KEY` env var from before v0.4 has no successor, see
[deployment.md → Production considerations](deployment.md#production-considerations).)

`config.example.yaml` in the repo root is the complete annotated schema, and
`internal/config/example_test.go` loads it through the real loader, so the
example and the parser cannot drift.

## Bootstrap (process) environment

| Var                   | Default              | Meaning                                                                             |
| --------------------- | -------------------- | ----------------------------------------------------------------------------------- |
| `OCFP_PORT`           | `8090`               | Listen port (`0` valid in tests)                                                    |
| `OCFP_CONFIG`         | _(empty = built-in)_ | Path to the config document; hot-reloaded. Empty = the built-in direct runtime.     |
| `OCFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval (ms)                                                       |
| `OCFP_SHUTDOWN_GRACE` | `55000`              | Drain window: in-flight streams finish before forced close (ms) — see deployment.md |

These are read once at startup and are **not** hot-reloaded. Everything below
is.

## The config document

```yaml
# process-wide JSON log threshold; hot-reloadable
log-level: info # debug | info | warn | error
upstream:
  base: https://opencode.ai
user_agent:
  sync_interval: 3600
egress:
  - id: primary
    proxy:
      type: http # http | https | socks5 — socks5h:// url = resolve at the proxy
      url: "http://${PROXY_USER}:${PROXY_PASS}@proxy-a.example:8080"
    max_concurrency: 4 # in-flight cap; 0 = unlimited (default)
    models: ["*-free"] # glob allow-list (OR); empty = every model
    streaming: true # streaming requests allowed; default true
    max_body_bytes: 0 # eligibility gate; 0 = unlimited
    weight: 5 # weighted_round_robin share only
  - id: direct # no proxy key = host's own network
routes:
  - id: default
    priority: 0 # higher wins; file order breaks ties; first match serves
    egress: [primary, direct] # fallback order after the head
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

## Log level

| Field       | Type                                   | Default | Meaning                                                                                                                                                                                                                           |
| ----------- | -------------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `log-level` | `debug` \| `info` \| `warn` \| `error` | `info`  | Process-wide JSON log threshold. The config-reload goroutine is the ONLY runtime writer of the global level; a live config edit takes effect on the next poll without touching in-flight requests. Anything else is a load error. |

The mapping is exact; `trace` and `fatal` are deliberately not config values.
Events are leveled:

- **info** — lifecycle (listening, config loaded, shutdown) and one
  per-request completion line (the black-box suite asserts its facts at this
  level, so it cannot be demoted);
- **debug** — opencode UA warm-up; per-request completion carries extra
  structured fields for correlation;
- **warn** — fault signals a request survived (reload read/rejection,
  transport-setup failure, forced SSE→JSON abort, shutdown grace forced
  close);
- **error** — fatal startup/config/healthcheck failures.

## Service sections

### `upstream`

| Field  | Type   | Default               | Meaning                                                                                                                                                     |
| ------ | ------ | --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `base` | string | `https://opencode.ai` | Zen upstream base, all routes; must be an absolute http/https URL with a host (userinfo, query and fragment rejected); trailing `/` normalized at `Resolve` |

Every endpoint (`/zen/v1/chat/completions`, `/zen/v1/responses`,
`/zen/v1/models`) is derived from `base` — no other upstream URL appears in
config or code. A non-default base is how tests and self-hosted gateways
redirect the whole proxy.

### `user_agent`

The opencode free-tier gate reads a compound User-Agent; a background loop
keeps that triple fresh from GitHub (startup warm + periodic sync; the
request hot path is a pure cache read — see
[the UA recon record](recon-opencode-ua.md)).

| Field           | Type    | Default | Meaning                                                                       |
| --------------- | ------- | ------- | ----------------------------------------------------------------------------- |
| `sync_interval` | int sec | `3600`  | UA identity sync cadence in SECONDS; `0` = disabled; negative is a load error |

### `egress[]`

| Field             | Type                          | Default               | Meaning                                                                                                                                                                                        |
| ----------------- | ----------------------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`              | string                        | required, unique      | referenced by routes; names the egress in logs and `X-OFP-Egress`                                                                                                                              |
| `proxy`           | map                           | _(omitted = direct)_  | `{ type, url }`; the transport an egress dials through                                                                                                                                         |
| `proxy.type`      | `http` \| `https` \| `socks5` | required with `proxy` | a `socks5` egress picks its DNS side by url scheme — `socks5h://` sends the hostname in the CONNECT (ATYP 3, remote resolve); the type name `socks5h` is rejected, use the scheme              |
| `proxy.url`       | string                        | required with `proxy` | scheme must equal `type` (`socks5` also accepts `socks5h://` = remote resolve); host required; optional `user:password@` userinfo (env-interpolated at load, redacted in every log/error path) |
| `enabled`         | bool                          | `true`                | `false` = configured but never scheduled                                                                                                                                                       |
| `weight`          | int ≥ 0                       | `1`                   | feeds `weighted_round_robin` only (ignored under `round_robin`); shapes which egress STARTS, never eligibility                                                                                 |
| `max_concurrency` | int ≥ 0                       | `0` = unlimited       | in-flight requests/streams; an egress at capacity is skipped at dial time (a skip is not a failure)                                                                                            |
| `models`          | []glob                        | empty = every model   | allow-list — ANY pattern matching admits the model (`path.Match` syntax against the suffix-stripped id)                                                                                        |
| `streaming`       | bool                          | `true`                | `false` = streaming requests never pick this egress                                                                                                                                            |
| `max_body_bytes`  | int ≥ 0                       | `0` = unlimited       | eligibility gate: larger requests never pick this egress (not a request cap — see [Body limits](#body-limits))                                                                                 |

### `routes[]`

| Field                  | Type                                    | Default             | Meaning                                                                    |
| ---------------------- | --------------------------------------- | ------------------- | -------------------------------------------------------------------------- |
| `id`                   | string                                  | required, unique    | names the route in logs                                                    |
| `priority`             | int                                     | `0`                 | HIGHER wins; equal priorities keep file order; the first match serves      |
| `match.streaming`      | bool                                    | _(unset = any)_     | the client asked for `stream: true`                                        |
| `match.models`         | []glob                                  | _(unset = any)_     | AND — every listed pattern must match (disjoint globs would match nothing) |
| `match.min_body_bytes` | int                                     | `0` = unset         | raw inbound body must be ≥                                                 |
| `match.max_body_bytes` | int                                     | `0` = unset         | raw inbound body must be ≤                                                 |
| `egress`               | []id                                    | required, non-empty | ids must exist; order is the FALLBACK order after the scheduler's head     |
| `strategy`             | `round_robin` \| `weighted_round_robin` | `round_robin`       | how the head is picked per route (state is per route id, process-wide)     |

### `fallback`

| Field          | Type | Default | Meaning                                                                                                                                                                 |
| -------------- | ---- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enabled`      | bool | `true`  | `false` pins every request to its single scheduled head                                                                                                                 |
| `max_attempts` | int  | `3`     | DISTINCT egresses one request may try, first included; `0` = default 3; negative rejected. One attempt = one logical upstream call, so this bounds egress movement only |

### `health`

| Field               | Type            | Default                   | Meaning                                                                                                    |
| ------------------- | --------------- | ------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `enabled`           | bool            | `true` with a config file | the built-in no-config runtime runs with health OFF (no outage can be acquired by config-less deployments) |
| `failure_threshold` | int ≥ 0         | `3`                       | consecutive failures that arm a cooldown; explicit `0` = never cool down                                   |
| `cooldown`          | duration string | `30s`                     | exclusion window once the threshold fires; `0s` = no window; bare numbers are a load error                 |

## Environment interpolation (`${VAR}`)

The shipped documents keep every secret as a dollar-brace env reference,
never a literal — a convention `internal/config/example_test.go` pins for
`config.example.yaml`, not a rule the loader enforces: an inlined literal
loads fine, so never commit one. Interpolation runs over the **raw file
bytes before YAML parsing — comments included** — and an UNSET variable is
a load error naming the variable. Two consequences:

- never write the placeholder syntax inside a comment (an unset reference in
  a comment still fails the load);
- a document that references only `${VAR}`s carries no literal credential
  anywhere, so it can be committed, mounted read-only, and shared without
  redaction.

Proxy credentials ride the proxy url userinfo (`user:password@host:port`);
they are stripped by `config.RedactProxyURL` everywhere a proxy URL can reach
a log or an error.

## Hot reload semantics

With `OCFP_CONFIG` set, the file is re-read every `OCFP_CONFIG_POLL_MS`. Each
poll reads the file **once**, hashes those bytes (SHA-256), and re-parses
only on change; repeated writes between ticks coalesce into one parse of the
final content.

- A file that fails parse or validation **keeps the last good runtime** and
  logs the rejection — a valid config is never replaced by an invalid one.
- Each accepted parse stamps a new immutable snapshot (generation 1 at first
  load, +1 per swap; the built-in no-config runtime is generation 0).
- An in-flight request keeps the snapshot it started on — a reload never
  touches a request that has already begun. See
  [architecture.md](architecture.md) for the generation model.
- Process-wide state migrates deliberately across swaps (health history,
  scheduler rotation, transport cache) — see
  [architecture.md](architecture.md#process-wide-state-lifecycles).
- `log-level` swaps with the rest of the snapshot: the reload goroutine writes
  `zerolog.SetGlobalLevel` for it, so the very next event after a swap obeys
  the new threshold.

### Log volume per level

Upstream-failure forensics (`upstream_error` events — see
[architecture.md](architecture.md#upstream-error-evidence-forensics)) are
shaped so the default level is already investigative:

- **info (default)** — one completion line per request, plus one warn
  `upstream_error` event per failed upstream interaction (dial, stream
  death, forced-conversion failure) with rate limits, classification,
  health and failover decisions. Successful requests emit nothing extra.
- **debug** — adds `egress_skipped` diagnostics (slot-full / transport
  build / unknown-egress pass-overs), which can be frequent under load.
- **warn / error** — silences the completion lines; failure evidence
  stays visible at warn.

## Validation errors

`File.Validate` (`internal/config/file.go`) applies the structural +
semantic checks in a fixed order and fails with the first violation, with an
actionable path (`egress proxy-b: …`, `route streaming: …`). The rules most
often hit:

- at least one `egress` and one `route`; every route `egress` id must exist
  and be listed at most once per route;
- egress/route ids must be unique and free of control characters;
- `weight`, `max_concurrency`, `max_body_bytes`, `min/max_body_bytes`,
  `failure_threshold`, `cooldown`, `max_attempts`, `sync_interval` must be
  ≥ 0 (explicit 0 keeps its documented meaning — never a default);
- `strategy` is `round_robin` or `weighted_round_robin` (the latter needs at
  least one egress with weight > 0 in the route);
- `upstream.base` must be an absolute http/https URL with a host and without
  userinfo, query, or fragment;
- durations (`health.cooldown`) are Go duration strings (`"30s"`) — a bare
  number is a load error;
- an unset `${VAR}` fails the whole load, naming the variable.
- `log-level` is one of `debug`, `info`, `warn`, `error` (anything else is
  rejected with `log-level must be one of debug, info, warn, error, got %q`).

Input values echoed into errors are bounded (`boundedEcho`) — a hostile
config line cannot bloat a log or smuggle unbounded bytes into an error
surface.

## Routing: match, then eligibility, then scheduling

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

## Body limits

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

## Attempt budget (there is no retry budget)

This proxy holds **no provider-level budget**. One attempt is one logical
upstream call: a provider response — 2xx, 429, any 4xx or 5xx — is relayed
verbatim and ends the request on the egress that produced it. There is
nothing to retry because there is nothing this proxy could learn from
asking the same provider a second time about a request the provider has
already answered (`docs/recovery-semantics.md`).

`fallback.max_attempts` therefore bounds **egress movement only**: at most
that many DISTINCT egresses per request (default 3, including the first;
`enabled: false` or a budget < 1 pins the request to one attempt). An
attempt may move on for exactly one reason — the failure provably happened
before a request byte existed (`Failure.ReplaySafe()`, i.e. `origin =
transport ∧ request_state = not_sent`). Everything else stops: a provider
verdict, a response-header timeout after transmission, a post-transmission
reset, a failure on a pooled connection, a client cancellation.

The worst case is one POST per egress, and it is only reachable when every
egress fails before a request exists: a three-egress route whose proxies
all refuse CONNECT costs 3 dials and **0** upstream calls. A 5xx storm
costs exactly 1, however many egresses the route lists — the first
provider answer is the answer. No inter-attempt sleep — the cooldown is
health-based, per egress, and applies to FUTURE requests only.

The client sees the LAST REAL failure: when the plan runs out of egresses
before the budget, the final dialed egress's own failure class and message
are returned — a deliberate divergence from base.js, which synthesizes
`All N URLs failed with status 429` when the plan outlives its URLs
(base.js:183; see the divergence note in `internal/upstream/fallback.go`).
Under the issue #53 contract that case can no longer arise — a 429 never
moves egress at all — but the divergence stands as the reason this proxy
keeps the last real verdict instead of a synthesized one. The synthetic
`502 none of the eligible egresses could serve the request` appears only when
NOTHING was dialed — every plan entry was skipped (slot-full, unknown
egress, transport build failed) or the head set was empty (`attempts=0` in
that request's log line).

## Health

Policy and state are split (`internal/health/health.go`):

- **Policy** (`enabled`, `failure_threshold`, `cooldown`) is captured from
  the request's config snapshot and travels with it — a hot reload affects
  only requests that have not started.
- **State** (failure streak + cooldown deadline) is process-wide, keyed by
  egress id + transport signature (`type:url`). A policy-only reload keeps
  the key, so history continues across generations; swapping an egress's
  proxy URL changes the key, so a fresh transport never inherits the old
  transport's streak or cooldown.

Health measures **egress-path health only** — whether the egress can carry a
request at all — so the mark uses the same predicate as the failover
decision (`Failure.ReplaySafe()`). Marked: a proxy TCP/TLS connect failure, a
typed proxy-auth refusal, a SOCKS5 negotiation or CONNECT refusal, a target
TCP connect failure, an origin TLS handshake failure. Never marked: **any**
provider HTTP status (429, 4xx and 5xx alike — those are verdicts about the
REQUEST or about the provider, not about this path), a request-write failure,
a response-header timeout after transmission, a pooled-connection failure, a
mid-stream death, a client cancellation. Health is observed when response
HEADERS arrive: a stream that dies or stalls after a 200 start is the
streaming commitment's abort, not a health observation. Poisoning an egress
on a verdict would suppress a healthy path and invert the meaning of the
table. Failures during an active cooldown never extend it;
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

## Proxy-authentication (407) classification

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

`proxy_auth_error` is a typed dial-phase failure, so it is replay-safe: the
attempt moves to the next egress and this one is marked unhealthy. Exactly
one dial — the failure is proof the request was never sent, so there is
nothing to repeat. One honest exception: a 407 that loses the race against
the connect deadline classifies as a timeout (a dial-phase failure too, so
likewise marked and moved on) — the typed proof never arrived, and
text-probing to recover it is banned. SOCKS5
detail: REP `0x02` ("connection not allowed by ruleset") is a plain
connection error, not proxy-auth; only RFC 1929 rejections are. A `socks5`
egress picks its DNS side by url scheme: `socks5://` resolves the target
locally and CONNECTs the IP literal;
`socks5h://` CONNECTs the hostname (RFC 1928 ATYP 3) and the proxy resolves
it — the opt-in for vendors that refuse IP-literal CONNECT targets. Either
way the proxy host itself is dialed locally, and a failed remote resolve is
the same visible connection error as any other CONNECT refusal.

## Streaming commitment

Once the executor returns a live upstream response, the request is committed:
the relay owns it and NO fallback ever happens — downstream writes happen
only after `Execute` returns, so the guarantee is structural
(`internal/upstream/fallback.go`). A mid-stream death — transport reset, or a
stall past the 360s SSE stall timeout — aborts the downstream response; a
Responses passthrough client still receives a parseable `response.failed`
terminal plus `[DONE]` (`internal/router/stream.go`).
