# Recon: the free-tier gate, 2026-10-13

> **SUPERSEDED IN PART (2026-10-15).** OpenCode moved Zen to a new
> **inference** backend and re-wrote the gate. Three of the findings below are
> now history, and the whole request shape changed with them. Read
> [§ 2026-10-15](#2026-10-15-the-inference-backend) first if you are debugging
> a 403 today; the 2026-10-13 material is kept because two of its findings —
> the session-id counter rule and the UA version floor — still hold verbatim on
> the new endpoint, and because its _method_ (bisect against the live upstream
> one variable at a time) is what produced the replacement table.

**Question.** Every free model answered `403 FreeTierError` ("OpenCode's free
tier can only be used from within OpenCode. Download it at
https://opencode.ai and keep building.") through this proxy on 2026-10-13,
while the official CLI answered normally from the same machine and IP. Which
part of the forged identity stopped being accepted?

**Date:** 2026-10-13 · upstream `https://opencode.ai/zen/v1`, models
`big-pickle`, `space-bunny-free`, `step-5-preview-free`,
`muse-spark-1.2/1.3-contributor-free` · official CLI `v2.0.27` (local binary)
and release tag `v1.19.0` (2026-10-11).

## Answer

The session id. Not the UA, not the TLS fingerprint, not the headers we
considered decoration.

The gate decodes the 12 hex chars of `x-opencode-session` back into the
counter field of `~(millis*0x1000 + counter)` and **rejects any session whose
counter is ≥ 64**. A sha256-derived prefix carries random bits there, so ~63/64
of the ids produced by `identity.TranslateSessionID` were refused on every
model, forever.

`identity.GenerateSessionID` was never affected — its counter starts at 1 per
millisecond. Only the translate path broke, which is exactly the path that
serves chat clients that send no usable session.

## Method

Bisection against the live upstream, one variable at a time, via direct HTTPS
(no proxy in the loop) so nothing about this repo's transports could confound
the result:

1. Replay the proxy's full identity with plain `curl` → 403 reproduced. So the
   uTLS ClientHello forge was not the cause (issue #48 could be ruled out
   without touching it).
2. Capture what the real CLI sends by pointing its `opencode` provider
   `options.baseURL` at a local capture server (project-level
   `opencode.json`, so the user's global config was never modified) and diffing
   that against `BuildHeaders`.
3. Walk every candidate — UA form, `x-opencode-client`, `x-opencode-project`,
   `x-opencode-request`, fingerprint quartet, `tool_choice`, `Accept` — one at
   a time. All were neutral.
4. The session id survived as the sole discriminator; then characterise it.

## What the real client sends (capture, 2026-10-13)

`POST /zen/v1/chat/completions`, CLI `v2.0.27`:

```
Authorization: Bearer public
User-Agent: opencode/latest/2.0.27/cli
x-opencode-client: cli
x-opencode-project: 014efc3b52f2cea44fb26c6a86ea5347ae0ab1f6
x-opencode-session: ses_ecc8505aeffeH85KipO5CODTdG
x-opencode-session-id: ses_ecc8505aeffeH85KipO5CODTdG
x-session-affinity: ses_ecc8505aeffeH85KipO5CODTdG
x-session-id: ses_ecc8505aeffeH85KipO5CODTdG
b3: f171736b5347242598e2d7c5314f9131-…
traceparent: 00-f171736b5347242598e2d7c5314f9131-…-01
```

Body carries `stream: true` and `stream_options: {include_usage: true}`, and
for the first (title-generation) request, no tools at all.

### The UA is no longer compound

Tag `v1.19.0` (`packages/opencode/src/session/llm/request.ts`) builds
`const USER_AGENT = \`opencode/${InstallationVersion}\``and attaches it
directly — the`ai-sdk/provider-utils/… runtime/bun/…`suffix documented in
[recon-opencode-ua.md](recon-opencode-ua.md) §1 is **absent** in the 2.x-era
client, which sends`opencode/latest/2.0.27/cli`. `InstallationVersion`is`OPENCODE_VERSION`, a build-time global (`packages/core/src/installation/
version.ts`), not the compound triple we replicate.

The compound form is **still accepted** (see below), so this is a fidelity
note, not the bug. §5 of the UA recon should be read with it in mind.

## The rule, measured

Given everything else held at a legal value, varying only the 12-bit counter
embedded in the session prefix (`counter = (^hex12) & 0xfff`):

| counter | status |     | counter | status  |
| ------- | ------ | --- | ------- | ------- |
| 0       | 200    |     | 61      | 200     |
| 1       | 200    |     | 62      | 200     |
| 3       | 200    |     | 63      | **200** |
| 15      | 200    |     | 64      | **403** |
| 40      | 200    |     | 65      | 403     |
| 60      | 200    |     | 100     | 403     |

Deterministic across repeated rounds, and identical on `/zen/v1/chat/
completions` and `/zen/v1/responses`. Both `/zen/v1/*` endpoints obey it.

### The timestamp field is irrelevant

The counter's upper neighbours in the same encoding — the millisecond
timestamp — are **not** checked:

| offset from now                                 | status |
| ----------------------------------------------- | ------ |
| −365 d, −180 d, −90 d, −30 d, −7 d              | 200    |
| −1 h, −5 min, now, +5 min, +1 h, +24 h          | 200    |
| proxy's hash-derived id (decodes to 2027-10-11) | 403    |

So the gate is not a recency window. It reads one field, and reads it as a
_validity_ check rather than a freshness check — consistent with "did this id
come out of `generateSessionId`?", since a real client increments the counter
per millisecond and never approaches 64 in normal operation.

### Ordering of the other checks

- **426 before 403**: a UA below `1.18.0` returns `426 UpgradeRequired`
  ("OpenCode 1.18.0 or newer is required to use the free tier") _even with an
  otherwise perfect identity_. A bad session with a `1.17` UA returns 403, so
  the version gate is evaluated first.
- **The fingerprint quartet is still required**: dropping `tools` from an
  otherwise legal request returns 403. `tool_choice` is not the lever.
- **`x-opencode-project` remains unvalidated**: `global` (our forged default),
  a real 40-hex project id, `not-a-project`, and 200-char junk all pass.

### Version floor moved

The free-tier minimum is now **`1.18.0`**, not `1.17`:

| UA                                                                                                  | status |
| --------------------------------------------------------------------------------------------------- | ------ |
| `opencode/1.16.0`, `1.17.0`, `1.17.5`, `1.17.6`, `1.17.7`, `1.17.8`, `1.17.9`, `1.17.10`, `1.17.99` | 426    |
| `opencode/1.18.0`, `1.18.30`, `1.18.31`, `1.19.0`, `2.0.0`, `2.0.27`, `3.0.0`, `99.0.0`             | 200    |
| `opencode/0.99.0`                                                                                   | 426    |
| `opencode/latest` (no numeric version)                                                              | 403    |
| `opencode/1.17.0 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14` (compound)                        | 426    |
| `opencode/latest/2.0.27/cli`                                                                        | 200    |

The compound suffix is irrelevant to admission; only the leading version is
read. `opencode/latest` gets **403, not 426** — a different rejection path from
a merely-too-old version, worth knowing when reading logs.

## Consequences for this proxy

`internal/config.ClientMinMinor` is `17`. A downstream client sending
`User-Agent: opencode/1.17.x` passes `HasValidVersion`, so `BuildHeaders`
forwards it verbatim, and upstream answers **426 relayed as-is**. Verified
end to end through the running proxy:

```
client UA opencode/1.17.0    => 426   (relayed)
client UA opencode/1.18.0    => 200
client UA whatever/9.9       => 200   (forged to the synced UA)
```

Not fatal — the client can retry without a UA and get the forged identity —
but it is a self-inflicted 426 for any 1.17-era opencode client. Raising
`ClientMinMinor` to 18 makes those clients forge instead. **Left as-is, not
changed by this investigation**: it widens the forge-vs-forward boundary, and
that belongs to a deliberate decision with its own test update, not to a recon
note.

## Not a porting error

`open-sse/executors/opencode.js:97` (9router, the source of record) has the
identical shape:

```js
const timeHex = digest.subarray(0, 6).toString("hex");
return `ses_${timeHex}${randomPart}`;
```

So the JS router is broken against the current upstream in the same way, and
this is an **upstream behaviour change**, not a divergence this port
introduced. The fix in `internal/identity/session.go` is therefore a
deliberate, documented divergence from the JS — see the comment at the site
and `AGENTS.md` rule 4.

## The fix

Keep the 36 high bits hash-derived (they carry conversation stickiness) and
force the 12 low bits to the legal `~1` encoding, so a translated id is
byte-identical to what `generateSessionId` emits for its first call in a
millisecond:

```go
var w [8]byte
copy(w[2:], sum[:6])
x := binary.BigEndian.Uint64(w[:])
x = (x &^ 0xfff) | (^uint64(1) & 0xfff)
timeHex := fmt.Sprintf("%012x", x)
```

Pinned by `TestTranslateSessionIDCounterUnderUpstreamGateLimit`
(`internal/identity/session_test.go`), which decodes the counter of a
translated id and fails above 63.

## Model list drift (same day, separate cause)

`TestLiveChatCompletion` also failed with `400 "Upstream request failed: Model
is unavailable."` — `qwen3-coder-free`, the value of `testedModel` in the
e2e suite, has been dropped from the upstream free tier. The live list at the
time of writing:

```
big-pickle, jev-1.13-free, ling-3.1-flash-free, longcat-2.5-preview-free,
mimo-v2.6-flash-free, muse-spark-1.2-contributor-free,
muse-spark-1.3-contributor-free, nemotron-3.5-lightning-free,
space-bunny-free, step-5-preview-free
```

`muse-spark-1.2-contributor-free` remains; `1.3` was added. `testedModel` is
now `space-bunny-free` and `museModel` is `muse-spark-1.3-contributor-free`.
This is the third distinct failure mode in the same session: a **stale test
constant**, a **model removal**, and the **gate change** — worth separating
when triaging a red live suite, since only the last one is a code defect.

`config.DeadFreeModels` lists only `deepseek-v4-flash-free`. `qwen3-coder-free`
is not listed because the live filter already excludes it (absent from the
upstream list), but if the upstream ever reintroduces the id it would pass the
filter while remaining unusable — the same class of drift as
`deepseek-v4-flash-free`.

## Re-verification

```sh
# decode the counter of any session id (the field the gate reads)
python3 -c 'p="ecc8505aeffe"; print((~int(p,16))&0xfff)'

# live identity probe (expect 200; swap in counter>=64 for 403)
curl -s -o /dev/null -w '%{http_code}\n' https://opencode.ai/zen/v1/chat/completions \
  -H 'Authorization: Bearer public' -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' -H 'User-Agent: opencode/2.0.27' \
  -H 'x-opencode-client: desktop' -H 'x-opencode-project: global' \
  -H 'x-opencode-session: ses_ecc8505aeffeAAAAAAAAAAAAAAAAAAAAAAA' \
  -d @body.json
```

Live suites: `E2E_LIVE=1 go test -tags e2e ./e2e/ -run TestLive` (all three
pass after the fix).

## 2026-10-15: the inference backend

**Date:** 2026-10-15 · upstream `https://opencode.ai` · official CLI v2.0.27
(local binary, control: answered normally from the same machine and IP while
every forged request was refused) · upstream source read at tag `v2.0.29`.

**Question.** Two days after the session-counter fix, every free model through
this proxy answered `403 FreeTierError` again, while the official CLI on the
same machine and IP kept working. The v1 fingerprint had stopped working.

**Answer: four independent things, of which only ONE is fatal on its own.** The
belt-and-braces request this proxy forges now satisfies four conditions; the
previous shape satisfied three of them, and the fourth is what the gate
actually reads.

### 1. The endpoint moved — `/zen/*` is dead (fatal)

`/zen/v1/chat/completions` answers `403 FreeTierError` **for every identity**,
including the official client's own header set byte for byte. Zen is now a thin
proxy onto a separate inference service
(`packages/console/app/src/lib/inference-proxy.ts` at v2.0.29: _"New inference
owns authentication, model availability, free-tier access, and billing"_, with
`paths["POST /zen/v1/chat/completions"] = "/openai/v1/chat/completions"`). The
admitted path is **`/inference/v1/...`**, which is also what the client itself
uses (`packages/ai/src/providers/opencode.ts`: `baseURL =
"https://opencode.ai/inference/v1"`).

| endpoint                              | status                    |
| ------------------------------------- | ------------------------- |
| `POST /inference/v1/chat/completions` | **200**                   |
| `POST /inference/v1/responses`        | **200** (flat tool shape) |
| `GET /inference/v1/models`            | 200                       |
| `POST /zen/v1/chat/completions`       | 403, any identity         |

### 2. `x-opencode-request` is refused outright (fatal)

This is the change no source-reading would have predicted, and the one that
made the diagnosis expensive: **any** value under the v1-era
`x-opencode-request` header is a 403, and its absence is what admits the
request. Measured against an otherwise-admitted request:

| header sent                    | status  |
| ------------------------------ | ------- |
| _(neither)_                    | **200** |
| `x-opencode-request-id: msg_…` | **200** |
| `x-opencode-request-id: req_…` | 200     |
| `x-opencode-request: msg_…`    | **403** |
| `x-opencode-request: req_…`    | 403     |
| `x-opencode-request: <24 hex>` | 403     |
| `x-opencode-request: global`   | 403     |

The v2 client sends **neither** (`packages/core/src/session/model-request.ts`
at v2.0.29 builds exactly `x-opencode-session-id`, `x-opencode-parent-session-id`,
`x-session-affinity`, `X-Session-Id`, `x-parent-session-id`, `User-Agent`,
`x-opencode-project`, `x-opencode-session`, `x-opencode-client` — no request
header at all), and the Console's proxy reads
`x-opencode-request-id ?? x-opencode-request`. So the v2 name is the one both
the gate and the Console understand, and the v1 name is a live rejection rather
than a no-op: `BuildHeaders` now forges `x-opencode-request-id` and never emits
the legacy header, **including when a downstream client sent one** (forwarding
it would 403 a request this proxy could have served).

### 3. The session id must travel under BOTH opencode names

`x-opencode-session` alone is 403; `x-opencode-session-id` alone is 403; both
with the same value is 200. The two non-opencode mirrors (`x-session-affinity`,
`x-session-id`) are **not** read by the gate, but the official client sends
them, so all four ride along. (This corrects the 9router community note, which
lists all three mirrors as required — two of the three are not.)

**The counter rule below is unchanged and still enforced:** counter ≤ 63 is
admitted, ≥ 64 is 403, on the new endpoint exactly as on the old one.

### 4. The gate reads the tool schemas

`body.tools` must carry the quartet **with real parameter schemas**:

| tools sent                                                 | status  |
| ---------------------------------------------------------- | ------- |
| the four real v2 declarations                              | **200** |
| stub quartet `bash/glob/grep/read` with properties         | **200** |
| real declarations, `shell` renamed `bash`                  | 200     |
| real declarations, every **description** blanked           | 200     |
| real declarations plus an extra client tool                | 200     |
| real declarations, **order reversed**                      | 200     |
| real declarations, **`shell` removed**                     | **403** |
| real declarations, **all `parameters.properties` emptied** | **403** |
| no tools at all                                            | **403** |

So the gate reads _names_ (a command tool plus `glob`, `grep`, `read`) and it
reads that each declaration carries a **non-empty** parameter schema; it does
not read descriptions, order, or the exact property set. This proxy therefore
forges the **real** v2 declarations anyway (`config.FingerprintToolSchemas`,
read from `packages/core/src/tool/plugin/{shell,glob,grep,read}.ts` at
v2.0.29) rather than name-only stubs: the current gate tolerates stubs, a
stricter one need not have to be told twice, and a proxy whose entire purpose
is client fidelity should not send a shape the real client would never send.

### What did NOT move

- **`stream: true` is still mandatory** on the wire, exactly as before. The
  proxy's existing "always stream upstream, aggregate for non-streaming
  clients" rule is load-bearing and unchanged.
- **The UA version floor is still 1.18.0.** `opencode/1.17.x` is 426,
  `opencode/1.18.0` and up are admitted, `opencode/latest` with no numeric
  version is 403 (a different path from 426), and a non-opencode UA is 403.
  `opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14` — the
  compound form this proxy forges — is **still admitted**, so the UA is not
  part of the regression. `ClientMinMinor` moved 17 → 18 only so a 1.17-era
  client forges the current identity instead of having its too-old UA
  forwarded and the 426 relayed (the deliberate call the 2026-10-13 note left
  open).
- **`x-opencode-project` remains unvalidated** (`global`, a real 40-hex id and
  200 bytes of junk all pass) — do not generalise any of this into "the
  upstream validates our identity properly".
- **TLS is not involved.** The uTLS ClientHello forge (issue #48) was the first
  suspect and it is exonerated: with the forged hello the request is refused,
  with stdlib Go TLS it is refused identically, and the _same bytes_ replayed
  by curl are admitted. `accept-encoding` was cleared the same way (gzip,
  identity and absent all admit).

### Method note

The decisive step was not any single probe but replaying the proxy's **own
captured request**: point `upstream.base` at a local plain-HTTP capture server,
send one request through the real binary, and replay those exact bytes with
curl. curl 200 / proxy 403 on an identical payload isolates the difference to
the header set, which is what pointed at `x-opencode-request`. A per-IP
cooldown is a real confound — an early bisect run returned a uniform 403 for
every case, including a known-good baseline, because it had omitted the
`x-opencode-session-id` mirror — so every row in every table above was
confirmed over repeated runs with a known-good baseline interleaved.

## Caveats

- The rule is characterised from **observed behaviour**, not from upstream
  source; the 426 body is the only thing that states a rule in prose
  ("1.18.0 or newer"), and the counter limit has no published explanation.
  Treat 64 as a measurement, not a specification.
- The threshold may be a heuristic that changes without notice. A future gate
  change would again present as a blanket 403 with no local signal. **This is
  no longer hypothetical: the rules changed completely within two days, twice
  inside a single day according to the 9router community report.** The
  fingerprint quartet, the endpoint and the request header are all upstream
  policy with no published contract; treat every table in this document as a
  measurement of one moment, not a specification.
- Per [recon-session-continuity.md](recon-session-continuity.md) §Project id,
  the _project_ header is still unvalidated — do not generalise this finding
  into "the upstream validates our identity properly".
