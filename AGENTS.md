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
commitlint's `scope-enum` mirrors the package table below. CI (`.github/
workflows/`) runs the same gates plus CodeQL, Semgrep, Gitleaks, and the
black-box e2e suite; release-please owns `CHANGELOG.md` and tags (do not
hand-edit either).

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

`example.config.yaml` (repo root) is the annotated schema, and
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
   identity. Only connection errors, typed proxy-auth, timeouts, and 5xx
   mark health; 429 and other 4xx never; success resets; a threshold
   decrease never arms retroactively.
6. Proxy-auth (407) is typed at the transport boundary only
   (`internal/upstream/connect.go`, `socks5.go`). A 407 that arrives as a
   response status is conservatively `client_error` — no fallback, no health
   mark. Never classify by error text. Typed proxy-auth bypasses the
   per-egress retry matrix: exactly one dial, then immediate fallback.
7. Streaming commitment: once a live upstream response exists there is no
   fallback, ever; a mid-stream death aborts the downstream response
   (`internal/router/stream.go`).

## Layout

| Package              | Role                                                                                                                                                                                                        |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/server`         | entrypoint; also serves `healthcheck` (Docker HEALTHCHECK on `scratch`)                                                                                                                                     |
| `internal/config`    | every runtime constant + the `PORT`/`OFP_CONFIG` env vars; multi-egress YAML model (routes, egresses, `upstream.base`, `auth.keys`, `user_agent.sync_interval`), interpolation, redaction, hot-reload store |
| `internal/routing`   | route planner: model/streaming/body gates, round-robin + smooth weighted rotation, snapshot-pinned attempt order                                                                                            |
| `internal/health`    | per-egress health registry: consecutive-failure threshold, cooldown; state survives config swaps, policy pinned per request                                                                                 |
| `internal/router`    | endpoints + chatCore pipeline + bypass/test-connection/modality/tool-dedupe stages + routing/fallback orchestration                                                                                         |
| `internal/relay`     | passthrough/translate SSE relays, SSE→JSON aggregation, usage seam                                                                                                                                          |
| `internal/translate` | request translators (chat ↔ responses), SSE state machines, prenorms, modality strip                                                                                                                        |
| `internal/upstream`  | HTTP client (retry matrix, failure taxonomy, SSE line scan), per-egress transports (direct, http/https CONNECT, socks5), executor transforms, header forging                                                |
| `internal/cloak`     | thinking suffix parse/apply, model id/URL, fingerprint tools                                                                                                                                                |
| `internal/identity`  | session/request ids, opencode UA triple cache + GitHub sync loop (fail-open), session resolution chain                                                                                                      |
| `internal/caps`      | per-model input-modality resolution (exact table → glob patterns → name heuristic)                                                                                                                          |
| `internal/usage`     | usage normalization/merge/estimation/thinking synthesis                                                                                                                                                     |
| `internal/jsonx`     | JS-semantics JSON accessors (`AsStr`/`AsArr`/`Truthy`/…)                                                                                                                                                    |
| `e2e/`               | black-box e2e suite behind the `e2e` build tag (see `e2e/README.md`)                                                                                                                                        |

## Porting discipline (the rules that keep parity)

1. **Cite the JS source.** Every ported behavior carries its source location
   in the Go comment (`bypassHandler.js:34-39`, `chatCore.js:169-180`, …).
   A comment without a citation is a parity risk.
2. **JS semantics, not Go reflexes.** Falsy/truthiness chains (`!x`,
   `x?.error`) go through `jsonx` helpers — never Go zero-value checks. `[]`
   and `{}` are truthy in JS; `"0"` is truthy; `null`/`false`/`0`/`""` are not.
3. **Constants live only in `internal/config`**, mirroring `open-sse/config`
   (URLs, retry matrix, timeouts, fingerprints, error envelopes). Nothing
   upstream-shaped may be hardcoded elsewhere.
4. **Deliberate divergences are documented**, with the reason, at the site:
   e.g. the JS `customToolNames?.has()` array crash is not replicated, the
   ccFilterNaming bypass pattern (P5) is dropped for lack of the settings
   flag, `formatDataLine`'s key re-serialization order differs (Go sorts map
   keys — semantics unchanged). If you can't articulate why Go differs, Go
   is wrong.
5. **Behavioral changes need JS evidence.** Before "fixing" relay/pipeline
   behavior, read the matching 9router code and cite it — the weirdness is
   usually load-bearing (double `[DONE]` on chat→chat, `data: null` drops,
   non-iterable `choices` chunk drops, shared retry budget across statuses).
6. **Fail-open vs fail-closed is part of the contract** (UA cache warm probe,
   models fallback to the static registry, 429 never retried). Keep it.
7. **Process-wide state has a lifecycle, not just a shape** (README "Process
   wide state lifecycles"): health state is reclaimed once per generation and
   never under a live request's pin (`health.Registry.Pin` at snapshot pin
   time); scheduler rotation migrates by fingerprint (preserve on equivalent
   reload, deterministic reset on change, prune on removal); the transport
   cache is a lookup, not an ownership registry — eviction never revokes a
   request's held `*Client` nor closes its active connection. When adding a
   fourth piece of process-wide state, give it the same three answers:
   what survives a reload, what resets, who still needs it.

## Environment

| `PORT` | `8090` | Listen port (`0` valid in tests) |
| `OFP_CONFIG` | _(empty = built-in)_ | Multi-egress routing config file (YAML); hot-reloaded, invalid keeps last good; also carries `upstream.base`, `auth.keys`, `user_agent.sync_interval` |
| `OFP_CONFIG_POLL_MS` | `1000` | Hot-reload poll interval for `OFP_CONFIG` (ms) |
| `OFP_SHUTDOWN_GRACE` | `30000` | Drain window: in-flight streams finish before forced close (ms) |

## Security

Never commit credentials, live API keys, or real session ids — test fixtures
use clearly fake values (`e2e-secret`, `public`). The 9router workspace may
contain live secrets; never copy anything from it into this repo.
