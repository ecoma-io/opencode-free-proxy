# Recon: the official opencode CLI User-Agent

How the official CLI builds the compound User-Agent the free-tier gate sees,
how each version segment is sourced, and how this proxy replicates and syncs
it. Sources pinned to tag **v1.18.31** (commit `014614d`, `release: v1.18.31`)
of `anomalyco/opencode`; live capture 2026-09-20.

## 1. The wire form

```
User-Agent: opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14
```

Three segments, single-space separated. (An earlier working note recorded
`ai-sdk/provider-utils/4.0.40` as "observed live" — that observation did not
come from an official v1.18.31 build; the capture in §3 supersedes it. 4.0.40
is the provider-utils copy resolved under `@ai-sdk/openai`, not
`@ai-sdk/openai-compatible` — see §4.)

## 2. Source trace (v1.18.31)

- `packages/opencode/src/session/llm/request.ts:18` —
  `const USER_AGENT = \`opencode/${InstallationVersion}\``.
- `request.ts:187-204` — for any model whose `providerID` starts with
  `"opencode"` (zen), the request headers carry that UA plus
  `x-opencode-session` / `x-opencode-request` / `x-opencode-client`
  (`cli` for the CLI) and `x-opencode-project`. Foreign providers get the
  same UA with `x-session-affinity` / `X-Session-Id` instead.
- `InstallationVersion` (`@opencode-ai/core/installation/version`) is the
  release version — `1.18.31`.
- The zen provider comes from the models.dev registry (`id: "opencode"`,
  `npm: "@ai-sdk/openai-compatible"`) and is instantiated through the generic
  SDK loader (`packages/opencode/src/provider/provider.ts:122-123`,
  `createOpenAICompatible`). The AI SDK fetch layer
  (`@ai-sdk/provider-utils` `postJsonToApi` → `withUserAgentSuffix`) APPENDS
  its own identity to the caller's UA:
  `ai-sdk/provider-utils/${VERSION} runtime/bun/<runtime version>`
  (template verified in the npm dist of `@ai-sdk/provider-utils`, both
  4.0.23 and 4.0.40: `ai-sdk/provider-utils/${VERSION}`; the runtime segment
  comes from its runtime detection — `runtime/bun/<Bun version>` under Bun).
- Note on transports: v1.18.31 also has a native LLM runtime
  (`packages/opencode/src/session/llm/native-runtime.ts` routing
  `openai|anthropic|opencode*` through `@opencode-ai/llm`, which has no
  ai-sdk dependency and passes headers through). Regardless of which
  transport fired the captured request, the wire UA matched the
  openai-compatible chain's provider-utils copy exactly (§3/§4) — the string
  below is what the gate sees and what we replicate.

## 3. Live capture (ground truth, 2026-09-20)

Procedure (reproducible):

1. Download the official release asset
   `opencode-linux-x64.tar.gz` from
   `github.com/anomalyco/opencode/releases/tag/v1.18.31` (binary is a
   ~185MB Bun-compiled executable; static `strings` grep alone is
   inconclusive because ALL provider-utils copies are embedded).
2. Run it against a local header-capture server with a sandboxed config:
   - `$XDG_DATA_HOME/opencode/auth.json`:
     `{"opencode":{"type":"api","key":"public"}}` (fixture value — the free
     tier's literal bearer).
   - `$XDG_CONFIG_HOME/opencode/opencode.json`: provider `"opencode"` with
     `options.baseURL` pointed at the capture server.
   - `opencode run -m opencode/big-pickle "say hi in one word"`.
3. Captured request to `POST /v1/chat/completions`:

```
User-Agent: opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14
x-opencode-client: cli
```

## 4. Where each version comes from (the sync chains)

| Segment                     | Source at tag `vX.Y.Z`                                                                            | v1.18.31 value | Verified by                                                                                                                                               |
| --------------------------- | ------------------------------------------------------------------------------------------------- | -------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `opencode/<v>`              | GitHub releases API, latest `tag_name`                                                            | `1.18.31`      | API query 2026-09-20 (published 2026-09-14)                                                                                                               |
| `runtime/bun/<b>`           | root `package.json` → `"packageManager": "bun@<b>"`                                               | `1.3.14`       | raw file at the tag; matches the captured UA                                                                                                              |
| `ai-sdk/provider-utils/<p>` | `bun.lock` at the tag, resolution key `opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils` | `4.0.23`       | captured UA == this resolution; cross-checked `registry.npmjs.org/@ai-sdk/openai-compatible/2.0.41` → `dependencies["@ai-sdk/provider-utils"] = "4.0.23"` |

Why the bun.lock key: the lockfile installs several provider-utils copies
(4.0.21 / 23 / 27 / 32 / 33 / 35 / 38 / **40** / 45 / 46 / 50 / 51 at this
tag). The one that stamps the zen UA is the copy nested under the
`@ai-sdk/openai-compatible` chain that packages/opencode uses for zen —
`opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils`. The 4.0.40 copy
sits under the `@ai-sdk/openai` chain (`opencode/@ai-sdk/openai/...`), which
is what lured the earlier (wrong) pin.

`bun.lock` is JSONC-flavored, so the sync extracts the version by regex on
the raw text instead of a JSON parse: exact key first, then any
`*/@ai-sdk/openai-compatible/@ai-sdk/provider-utils` key as a rename-tolerant
fallback.

## 5. How the proxy replicates it

- **Default triple** — `internal/config`: `ClientFallbackVersion` (releases
  probe fallback) + `ClientFallbackProviderUtils` + `ClientFallbackBun`
  compose the compiled-in UA `opencode/1.18.31 ai-sdk/provider-utils/4.0.23
runtime/bun/1.3.14` — byte-identical to the capture. This is the "chosen
  tag" default; sync keeps it current without rebuilds.
- **Sync** — `internal/identity.UserAgentCache.Warm` (all-or-nothing, so the
  cache never holds segments from two releases):
  1. releases API → opencode version (`opencodeClientVersion.js` parity);
  2. `raw.githubusercontent.com/anomalyco/opencode/v<version>/package.json`
     → bun segment;
  3. `.../v<version>/bun.lock` → provider-utils segment.
- **Cadence** — `StartSync` ticker, default **1h**
  (`user_agent.sync_interval`, integer seconds in the OCFP_CONFIG document;
  `0` disables), plus one forced warm at startup.
- **Hot path** — `Get()` is a pure cache read. This deliberately diverges
  from `open-sse/utils/opencodeClientVersion.js` (9router warms lazily per
  request behind a 12h TTL, `executors/opencode.js:376`): with the ticker the
  cache is at most one interval stale, and requests never block on GitHub.
  Fail-open semantics are preserved: any sync failure keeps the previous
  triple (or the compiled-in default while cold), TTL/single-flight retained.
- **Forging** — `internal/upstream.BuildHeaders`: a downstream UA that is a
  valid opencode ≥ 1.17 client passes through verbatim (including a full
  compound UA); otherwise the synced UA is forged. The free-tier gate itself
  checks `opencode/≥1.17` + `stream:true` + the fingerprint quartet — the
  ai-sdk/bun segments are parity fidelity, not admission criteria (requests
  with either 4.0.23 or 4.0.40 pass upstream).

## 6. Re-verification commands

```sh
# current release tag
curl -s https://api.github.com/repos/anomalyco/opencode/releases/latest | grep tag_name
# bun segment at that tag
curl -s https://raw.githubusercontent.com/anomalyco/opencode/v<TAG>/package.json | grep packageManager
# provider-utils segment at that tag
curl -s https://raw.githubusercontent.com/anomalyco/opencode/v<TAG>/bun.lock \
  | grep -o '"opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils": \["@ai-sdk/provider-utils@[0-9.]*"'
```

Unit coverage: `internal/identity/useragent_test.go` (chain URLs, extraction
regexes, all-or-nothing, TTL/force, single-flight, ticker).
