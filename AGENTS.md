# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

`opencode-free-proxy` is a minimal OpenAI-compatible router exposing **only**
the OpenCode **free** provider (opencode zen free tier). It is a
line-for-line port of the hardened OpenCode handling in the 9router sources
(`~/9router/9router-src/open-sse/`), which are **not** part of this repo —
they are the external source of truth for behavior.

Scope is fixed: free tier only (`Bearer public`, fingerprint-tool gate,
`-free` + `big-pickle` models). No paid-tier, multi-provider, or API-key
upstream support belongs here.

## Commands

```sh
go build ./...                       # compile
go test ./...                        # offline unit suite (httptest only, no egress)
go test -tags e2e ./e2e/             # black-box e2e: builds cmd/server, runs it as a
                                     #   subprocess against a fake zen upstream
E2E_LIVE=1 go test -tags e2e ./e2e/ -run TestLive   # optional: real proxy + real upstream
go vet ./... && go vet -tags e2e ./e2e/ && gofmt -l .   # must all be clean
golangci-lint run ./...              # v2 standard baseline — what CI's Lint step runs
pnpm format                          # prettier over docs/workflows/configs
```

All gates (vet both tag sets, gofmt, golangci-lint, full test suites) must
pass before every commit. The repo toolchain (commitlint, lefthook,
prettier) lives in `package.json` — `pnpm install` wires the git hooks;
commitlint's `scope-enum` (`commitlint.config.mjs`) holds one scope per
package in the layout table below plus the non-package scopes `docs`,
`deps`, `ci`, `workspace`, and `release`. CI (`.github/workflows/`) runs
the same gates plus a `-trimpath` build, a commitlint gate on the
pull-request title (squash merges make it the commit subject), and the
black-box e2e suite; the Analysis workflow (CodeQL, Semgrep, Gitleaks)
also runs weekly on a cron (`.github/workflows/analysis.yml`), and Renovate
(`.github/renovate.json5`) drafts dependency PRs under the `deps` scope.
release-please owns `CHANGELOG.md` and tags (do not hand-edit either).

## The Semgrep directory has two non-obvious constraints

`semgrep --config .github/semgrep` loads every `.yml`/`.yaml` file in that
directory as a candidate rule config, regardless of naming:

1. A file without a top-level `rules:` key aborts the whole run with exit 7
   — which is why the `workflows.test.yaml` fixture declares `rules: []`.
2. A `.yml`/`.yaml`-suffixed file needs the `.test.` infix to be recognised
   as a test target rather than only as a config candidate.

Both were confirmed against semgrep 1.172.0 by running it, and both apply
only to `languages: [yaml]` fixtures.

## The multi-egress config: canonical example + parity test

`config.example.yaml` (repo root) is the annotated schema, and
`internal/config/example_test.go` loads it through the real loader
(`config.LoadFile` — interpolate → parse → validate → resolve), so the
example and the parser are locked together: change them together or CI fails.
The semantics agents most often get wrong:

1. The env interpolator scans the RAW file bytes — comments included — and
   an unset `${VAR}` is a load error. The example therefore keeps placeholder
   syntax out of comments, and carries no literal credential anywhere.
2. Decisions run: route matching FIRST (`MatchRoute` — priority descending,
   stable sort, file order breaks ties, first match serves), then hard
   eligibility filters the matched route's egress list, then the scheduler
   orders the INITIAL attempt only. `weight` feeds `weighted_round_robin`
   head selection only — never eligibility; explicit `weight: 0` never heads
   a weighted route while a positive-weight sibling is eligible and is
   ignored under `round_robin`.
3. Route `match.models` is AND (every pattern must match — disjoint globs
   match nothing); egress `models` is OR (an allow-list).
4. Body limits are three layers: the 8 MiB server cap in
   `internal/router/handler.go` (rejects the request), route `match` body
   bytes (route selection), egress `max_body_bytes` (eligibility filter).
5. Health splits POLICY from STATE: policy (enabled/threshold/cooldown) is
   pinned per request from its snapshot; state (streak + cooldown) is
   process-wide keyed by egress id + transport signature (`type:url`). A
   policy-only reload keeps history; a proxy URL swap starts a fresh
   identity. Health is **egress-path health only**, marked by the same
   predicate that permits the egress move — `Failure.ReplaySafe()` (issue
   #53, docs/recovery-semantics.md). Provider statuses mark nothing, 4xx and
   5xx alike; success resets; a threshold decrease never arms retroactively.
6. Proxy-auth (407) is typed at the transport boundary only
   (`internal/upstream/connect.go`, `socks5.go`). A 407 that arrives as a
   response status is conservatively `client_error` — no fallback, no health
   mark. Never classify by error text. Typed proxy-auth is a dial-phase
   failure, so it marks health and moves to the next egress after exactly
   one dial — there is no retry budget for it to bypass.
7. **One logical upstream call per attempt.** There is no provider-level
   retry: a response of ANY status (2xx, 429, 4xx, 5xx) is relayed verbatim
   and ends the request on the egress that produced it; `fallback.max_attempts`
   bounds DISTINCT egresses, not provider requests. Only a failure that
   provably preceded the request byte may move the request on
   (`Failure.ReplaySafe()`). The delivery record is the LOGICAL CALL's and is
   monotonic across its hops (issue #60): once any hop has handed a request
   byte to a connection, no later hop's dial failure may claim `not_sent`,
   and a hop that fails after an answered redirect reports `response_started`
   — the request is already on the wire and the provider already answered it.
   Request bytes are observed with a per-request `httptrace.WroteRequest`
   hook (`internal/upstream/provenance.go`), never by wrapping connections —
   a wrap sees only the conns this call dialed, so a write over a pooled
   connection would be invisible, and it counts proxy TLS bytes as request
   bytes. This is a deliberate behaviour change — do not
   reintroduce status-keyed retry, shared retry budgets, or retry-delay
   tables. Pinned by `internal/upstream/terminal_test.go` (every status in
   the table → exactly one upstream request), `logical_call_test.go`
   (pooled hop 1 → 307 → hop 2 dial refusal → one attempt, no duplicate POST),
   the router's
   `TestNoFallbackAfterProviderStatus` / `TestUpstreamErrorEvent429Terminal`,
   and the black-box `TestEgress429IsTerminalAndMarksNoHealth` /
   `TestEvidence429TerminalReconstructsFromLogs`.
8. Streaming commitment: once a live upstream response exists there is no
   fallback, ever; a mid-stream death aborts the downstream response
   (`internal/router/stream.go`).
9. `log-level` (debug|info|warn|error, default info) is a process-global
   zerolog threshold. The config-reload goroutine is the ONLY runtime writer
   of `zerolog.SetGlobalLevel`; the swap message must keep the literal
   `config reload: swapped to new config (generation %d` — the e2e suite
   greps it. Completion lines log at info and are the e2e suite's only
   request-outcome evidence (Debug would hide them).
10. Upstream-error forensics (issue #45) are strictly observational. The
    recorder (`internal/upstream/evidence.go`) only COLLECTS rows;
    `internal/router/evidence_log.go` is the only emit boundary. The
    completion line's `Msgf` text is frozen — evidence events are additive
    only (warn `upstream_error` per failed interaction, debug
    `egress_skipped` per pass-over; success emits nothing). Rows are
    captured BEFORE classification reduces the verdict, but never before a
    decision exists (`fallback_decision` only after the executor decided).
    Upstream- or attacker-derived text reaches a log only sanitized and
    inside JSON fields — the rendered `Msgf` quotes it with `%q`. Never
    route request bodies, credentials, proxy URLs or session ids into a row;
    the session travels only as `session_fp` (sha256[:16] pseudonym).
    Stream/forced deaths are `response_started` PHASE rows — the delivered
    status is never rewritten into an HTTP verdict. See docs/architecture.md
    "Upstream error evidence".
11. Responses are **attributed, not inferred** (issues #55, #63,
    `internal/router/provenance_header.go`). Anything that came out of the
    upstream attempt path carries `X-OFP-Failure-Origin` (`upstream` |
    `ambiguous` | `gateway`), with `X-OFP-Failure-Phase` on gateway-origin
    responses only and `X-OFP-Request-State` on everything but `upstream`
    (`not_sent` | `unknown` | `response_started` — the last means the logical
    call had been answered before the hop that failed, e.g. by a followed
    redirect, issue #60; all three are read off the `Failure`, never off the
    status). Authorship is a property of the PATH, never of the status code:
    on an intermediated hop (a plain-http target carried by an http/https
    forward proxy, `internal/upstream` `hopPathOf`) the proxy is an HTTP peer
    that answers for itself, so the label is `ambiguous` with state `unknown`
    — relayed verbatim like any response, never replay-safe, never marking an
    egress, and never claiming the provider wrote it. The label is read off the returned
    `Failure` — NEVER off the status that is about to be written, because a
    provider 502 and a synthesized 502 are the same number. A local error
    (bad body, unknown model, draining) and a client cancellation carry no
    provenance header; absence means "not an upstream-interaction outcome",
    never "upstream". Every inbound `X-OFP-*` header is stripped at the top
    of the pipeline, so no client can forge one. Pinned by
    `internal/router/provenance_header_test.go` and the black-box
    `e2e/provenance_test.go`.
12. Egress selection is a **logical intent, not an index walk** (issues #56,
    #64, `internal/routing/intent.go`). The attempt loop asks
    `routing.Selector` for the next egress under `normal` (no history) or
    `new-egress` (a replay-safe failure invalidated the previous one), and
    the currency across that seam is an **opaque `Selection` handle**, never
    an egress identity: `Next(intent Intent, prev Selection) (Selection,
bool)`, with `Selection.Resolve()` telling the caller only what to dial.
    A caller cannot request a named egress, exclude one by name, or read an
    identity out of a selection — `prev` means "the egress that served the
    previous attempt" without the caller spelling it, which is what lets a
    pool-backed selector define its own handles. `new-egress` is not a
    preference the caller can satisfy by naming: the selector decodes `prev`
    and must not deliberately reuse it. The in-process `PlanSelector` walks
    the request's pinned plan, and a pool-backed (RPGW) selector is a
    recorded cross-repo dependency, never invented here. The intent is
    recorded on every evidence row (`egress_intent`), never derived from a
    provider status (a 429 attempt stays `normal`), and never accepted from
    the wire — inbound `X-OFP-*` is stripped before any stage.
    `fallback.max_attempts` bounds DISTINCT egresses; one route member is
    tried at most once per request. Pinned by
    `internal/routing/intent_test.go`, `internal/upstream/intent_test.go`,
    and `internal/router/intent_router_test.go`.

## Layout

| Package              | Role                                                                                                                                                                                                                                       |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `cmd/server`         | entrypoint; also serves `healthcheck` (Docker HEALTHCHECK on `scratch`)                                                                                                                                                                    |
| `internal/logging`   | zerolog construction (`time`/`level`/`msg` field names), the constructor-edge compatibility adapter for legacy test callbacks                                                                                                              |
| `internal/config`    | every runtime constant + the `OCFP_`-prefixed bootstrap env vars; multi-egress YAML model (routes, egresses, `upstream.base`, `user_agent.sync_interval`, `log-level`), interpolation, redaction, hot-reload store                         |
| `internal/routing`   | route planner: model/streaming/body gates, round-robin + smooth weighted rotation, snapshot-pinned attempt order                                                                                                                           |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown; state survives config swaps, policy pinned per request                                                                                                                |
| `internal/router`    | endpoints + chatCore pipeline + bypass/test-connection/modality/tool-dedupe stages + routing/failover orchestration (one logical upstream call per attempt) + evidence emit boundary (`evidence_log.go`)                                   |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                                                                                                         |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms, modality strip                                                                                                                                                       |
| `internal/upstream`  | HTTP client (single-call execute, failure taxonomy + provenance, SSE line scan), per-egress transports (direct, http/https CONNECT, socks5), executor transforms, header forging, strictly-observational evidence recorder (`evidence.go`) |
| `internal/cloak`     | thinking suffix parse/apply, model id/URL, fingerprint tools                                                                                                                                                                               |
| `internal/identity`  | session/request ids, opencode UA triple cache + GitHub sync loop (fail-open), session resolution chain                                                                                                                                     |
| `internal/caps`      | per-model input-modality resolution (exact table → glob patterns → name heuristic)                                                                                                                                                         |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                                                                                                                    |
| `internal/jsonx`     | JS-semantics JSON accessors (`AsStr`/`AsArr`/`Truthy`/…)                                                                                                                                                                                   |
| `e2e/`               | black-box e2e suite behind the `e2e` build tag (see `e2e/README.md`)                                                                                                                                                                       |

## Porting discipline (the rules that keep parity)

1. **Cite the JS source.** Every ported behavior carries its source location
   in the Go comment (`bypassHandler.js:34-39`, `chatCore.js:169-180`, …).
   A comment without a citation is a parity risk.
2. **JS semantics, not Go reflexes.** Falsy/truthiness chains (`!x`,
   `x?.error`) go through `jsonx` helpers — never Go zero-value checks. `[]`
   and `{}` are truthy in JS; `"0"` is truthy; `null`/`false`/`0`/`""` are not.
3. **Constants live only in `internal/config`**, mirroring `open-sse/config`
   (URLs, timeouts, fingerprints, error envelopes, evidence bounds). Nothing
   upstream-shaped may be hardcoded elsewhere. (The JS retry matrix's
   constants are deliberately absent: issue #53 removed the table, and
   re-adding its numbers anywhere re-adds its semantics.)
4. **Deliberate divergences are documented**, with the reason, at the site:
   e.g. the JS `customToolNames?.has()` array crash is not replicated, the
   ccFilterNaming bypass pattern (P5) is dropped for lack of the settings
   flag, `formatDataLine`'s key re-serialization order differs (Go sorts map
   keys — semantics unchanged). If you can't articulate why Go differs, Go
   is wrong.
5. **Behavioral changes need JS evidence.** Before "fixing" relay/pipeline
   behavior, read the matching 9router code and cite it — the weirdness is
   usually load-bearing (double `[DONE]` on chat→chat, `data: null` drops,
   non-iterable `choices` chunk drops). The recovery rules are the standing
   exception: the JS retry matrix and its status-keyed budgets were removed
   by issue #53 as a deliberate divergence, documented in
   docs/recovery-semantics.md — do not restore them for parity's sake.
6. **Fail-open vs fail-closed is part of the contract** (UA cache warm probe,
   models fallback to the static registry). Keep it.
7. **Process-wide state has a lifecycle, not just a shape**
   (docs/architecture.md "Process-wide state lifecycles"): health state is
   reclaimed once per generation and
   never under a live request's pin (`health.Registry.Pin` at snapshot pin
   time); scheduler rotation migrates by fingerprint (preserve on equivalent
   reload, deterministic reset on change, prune on removal); the transport
   cache is a lookup, not an ownership registry — eviction never revokes a
   request's held `*Client` nor closes its active connection. When adding a
   fourth piece of process-wide state, give it the same three answers:
   what survives a reload, what resets, who still needs it.

## Environment

| Variable              | Default              | Meaning                                                                                                                                  |
| --------------------- | -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `OCFP_PORT`           | `8090`               | Listen port (`0` valid in tests)                                                                                                         |
| `OCFP_CONFIG`         | _(empty = built-in)_ | Multi-egress routing config file (YAML); hot-reloaded, invalid keeps last good; also carries `upstream.base`, `user_agent.sync_interval` |
| `OCFP_CONFIG_POLL_MS` | `1000`               | Hot-reload poll interval for `OCFP_CONFIG` (ms)                                                                                          |
| `OCFP_SHUTDOWN_GRACE` | `55000`              | Drain window: in-flight streams finish before forced close (ms)                                                                          |

## Security

Never commit credentials, live API keys, or real session ids — test fixtures
use clearly fake values (`e2e-secret`, `public`). The 9router workspace may
contain live secrets; never copy anything from it into this repo.
