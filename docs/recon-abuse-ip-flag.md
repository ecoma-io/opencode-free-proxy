# Recon: the per-IP abuse flag ("detected abusive traffic"), 2026-10-20

> **Follow-up same day (see § Round 2):** the "fingerprint" hypothesis was
> pursued to an interleaved A/B against the live gate and is **dead**: the
> ClientHello (forge vs Go stdlib), UA triple generation, `x-opencode-client`,
> `prompt_cache_key`, header order — all admitted once the session id is
> shape-valid. The residual 403 axes are (a) session-id tail length ≠ 14
> (identity gate, caught by a faulty probe, not by production) and (b) the
> per-IP abuse flag. Every "clean" IP used in Round 2 was burned by its own
> probe's repeated identity-failed 403s — which in itself confirms that the
> gate treats clusters of rejected requests as additional IP-abuse signal.
>
> **Question.** ~33% of chat requests through the production stack answered
> `403 FreeTierError` with a NEW message — _"OpenCode's free models aren't
> available from your current network because we've detected abusive traffic
> from this IP address … If you're on a VPN or proxy, turn it off …"_ — while
> RPGW kept rotating egress IPs. Hypothesis raised: our forged identity
> (fingerprint) is stale and the gate flags it. Which is it — fingerprint or
> IP reputation?

**Date:** 2026-10-20 · upstream `https://opencode.ai/inference/v1` ·
upstream source read at HEAD (`133b1eb`, synced to v2.1.1 that day) ·
production stack: 4 × opencode-free-proxy v0.12.0 behind RPGW v0.6.0
(8 upstreams), traefik ingress.

## Answer: it is the IP, not the fingerprint

A controlled experiment with the **real production binary and its real
forgery**, pinning one egress IP at a time (temporary containers per
upstream, identical minimal `glm-5.3-flash-free` body each):

| egress                                   | exit IP (cloudflare trace) | 10 sequential requests          |
| ---------------------------------------- | -------------------------- | ------------------------------- |
| **direct** (server's own IP, no proxy)   | —                          | **200 ×10**                     |
| home2 (`180.93.2.169:3129`)              | `171.246.30.238`           | **200 ×10**                     |
| mkvn5 (`socks5-01.proxy.mkvn.net:44814`) | `65.111.7.29`              | 200 ×7 (2×429, 1×502 transient) |
| home1 (`180.93.2.171:3130`)              | `42.113.0.1`               | **403 ×10**                     |
| home3 (`180.93.2.169:3129`)              | `42.118.100.88`            | **403 ×10**                     |
| mkvn1 (`socks5-01.proxy.mkvn.net:41751`) | `160.250.167.64`           | **429 ×10**                     |
| mkvn2 (`socks5-01.proxy.mkvn.net:43780`) | `160.191.241.21`           | **403 ×10**                     |
| mkvn3 (`socks5-01.proxy.mkvn.net:43672`) | `160.191.240.111`          | **403 ×10**                     |
| mkvn4 (`socks5-01.proxy.mkvn.net:41084`) | `104.207.44.212`           | **403 ×10**                     |

**6 of the 8 pool upstreams are hard-flagged at the moment measured** (every
request 403), one is rate-limited (429), only 2 stay clean. The flag is
**deterministic per IP** (10/10 within a single egress), not probabilistic —
which is why production sees ~33% failures: with OFP's provider-status
contract (`client_error` → `fallback_decision=stop`, no health mark), each
request is served by whatever pool entry it draws, and bad IPs keep drawing
traffic.

Notably the SAME instant produced 200 ×10 via the server's own IP: if the
fingerprint were flagged, EVERY row above would 403. The fingerprint is
admitted; the per-IP abuse flag is what varies.

### The intra-test IP churn is real but too slow

RPGW `/status` at measurement time: `rotations: 64` over `70h41m` and
`ipRevisits: 11`, against `requests: 66706` (~16/min average, all 8
upstreams). Warm pool (`created: 221987`) churns TCP connections, not exit
IPs. The home-proxy upstreams DID change exit IPs between probes a few
minutes apart (`14.250.78.225` → `42.113.0.1` for home1; both flagged), so
"rotation" exists at the IP level too — but the whole pool volume concentrates
on a handful of consumer-ISP/proxy addresses at rates of thousands of requests
per hour, which is exactly the pattern an abuse detector scores. 403 rate
trend from the ingress accesslog (error-status filter records the huge jump):
≤ ~50/h on 10-18/19 nights, exploding to **~200-230/h from 2026-10-20T09h**
(a ~4-5× step), consistent with the flag set being populated same-day.

## What the v2.1.1 source says (nothing static to fix)

The client-side material our forgery reproduces is **unchanged** between
v2.0.29 (the last gate recon) and v2.1.1:

- Session-id generation: `packages/schema/src/identifier.ts` — same
  `~(millis*0x1000 + counter)` encoding, 12 hex + 22 base62; counter still must
  decode < 64. (Caveat: 9router's `opencode.js` regex expects only **14**
  random chars; the real client now makes **22** — the JS router is stale
  against upstream again; our generator already emits the real shape.)
- The request headers (`packages/core/src/session/model-request.ts`) —
  same exact set as the 2026-10-15 capture: both `x-opencode-session` +
  `x-opencode-session-id` mirrors, `x-session-affinity`/`X-Session-Id`,
  `x-opencode-project`, `x-opencode-client`, and a UA now built as
  `opencode/<channel>/<version>/<name>` (`App.useragent`, `app.ts`) — the
  same compound triple our sync loop replicates (floor stays 1.18.0).
  No `x-opencode-request` of either spelling (we correctly suppress the v1
  name). New optional `x-opencode-parent-session-id` only for child sessions.
- Tool gate: `packages/core/src/tool/plugin/{shell,glob,grep,read}.ts` —
  unchanged section names; the quartet requirement persists.
- The console passthrough (`packages/console/app/src/lib/inference-proxy.ts`)
  forwards `x-real-ip` from `cf-connecting-ip`, forwards
  `x-opencode-request-id ?? x-opencode-request`, and strips `authorization`
  for `public`. **The string/"abusive traffic" appears NOWHERE in the OSS
  repo** — the scorer lives in the closed "new inference" service (plus
  Cloudflare at the edge). Its verdict is per request to the _client IP edge
  sees_ — i.e. the egress IP — not to our forged header set.

So there is no new client-side fingerprint to chase; the detection moved to
IP reputation / edge scoring on the server side.

## What is NOT yet settled

- Longevity of a clean IP: `direct` (a hosting-range IP) answered 200 ×10
  at probe time. Hosting IPs are a known listing target; the server's own IP
  is a better egress than the flagged pool but is not guaranteed permanent,
  and it was already served daily traffic through other paths without being
  flagged for 7 days.
- The trigger side of the scorer: per-IP volume thresholds, ASN reputation,
  or a shared residual score from prior occupants of these proxy IPs. Not
  separable from here; treat both 403 (ban) and 429 (rate) as per-IP states
  that come and go.
- `mkvn5`'s 502→200 tail early in its run: transient cooldown from a dial
  that had just been borrowed from the warm pool — rechecked below as 200.

## Consequences for the stack

1. **Prune/replace flagged upstreams.** The single highest-leverage change is
   dropping the 6 IPs the gate flags and widening the pool beyond
   2 hosts / 3 ASNs. Volume-per-IP, not volume-total, is the signal the gate
   grades — more upstreams each carrying less traffic is the shape the free
   tier tolerates (the official client's own behavior from ordinary networks).
2. **Add a `direct` egress.** The server's own IP measured clean (200 ×10).
   Worth a route member until it stops being so.
3. **Do not "fix" the forge.** Every table in recon-free-tier-gate.md §2026-10-15
   still reproduced the same result; no change to identity/cloak/headers is
   indicated by this regression. Also: because provider-status 403 is
   deliberately NOT replay-safe and does NOT mark health (issue #62, the
   recovery-semantics contract), a flagged IP keeps being scheduled — that is
   why the failure looks "permanent" at the pool level. If an automated
   quarantine for the `FreeTierError` body is ever wanted, it is a **design
   change** that must be argued against that contract, not a bug fix here.
4. **Re-measure, do not conclude, on pool changes**: flag state on these
   IPs has been observed to coincide with volume bursts; re-run the probe
   script after any pool edit (the per-IP determinism makes 8-10 sequential
   requests per IP diagnostic enough).

## Round 2, same day: every fingerprint hypothesis, bisected against the live gate

Driven by the pool owner's remaining suspicion that a _fingerprint_ — not the
IP — is what the gate punishes, the same wildfire bisect method as the
2026-10-13 recon was applied one variable at a time, over two hours, using
(a) the production binary pinned per egress, (b) a temporary Go probe
importing `internal/upstream`/`internal/identity` unchanged, and (c) curl,
all against `/inference/v1/chat/completions` with the quartet tool bodies.

### The probe error that looked like a fingerprint finding

The first Round 2 swings returned `403 FreeTierError` (the OLD identity-gate
wording — "only be used from within OpenCode") for **every** raw-probe shape
while the production container kept 200/429'ing through the same identity at
the same moment. The apparent suspects — ClientHello (curl OpenSSL, Go
stdlib, python ssl), UA triple, `Accept-Encoding`, `x-opencode-client` — were
all eventually exonerated, because the raw probes were generating session ids
with a **22-char random tail** (30-char total) while upstream's
`generateSessionId` (`v2.1.1`, `packages/schema/src/identifier.ts`,
`length = 26`) and this repo's `unstableRandom` both emit **14** base62 chars
(26-char total). The current gate rejects a 30-char id with the same
`FreeTierError` body the identity gate has always used.

With corrected sessions the same probe lattice scored, on the same server
direct egress, in one process, alternating arms per request:

| arm                                         | ClientHello | result     |
| ------------------------------------------- | ----------- | ---------- |
| stdlib (Go default JA3)                     | yes         | **200 ×4** |
| forge (`internal/upstream` Bun-shaped utls) | yes         | **200 ×4** |

So: **the ClientHello is not read** by the edge or the gate — the uTLS forge
(issue #48) matters for CF-edge hygiene only at Cloudflare's own scoring, and
the burned-pool affair stands as a pure IP problem. Corollary controls from
the same day: `x-opencode-client: cli` vs `desktop`, UA
`opencode/latest/2.1.1/cli` vs the compound `1.19.3` triple, body-level
`prompt_cache_key`, and the exact capture-replay of a full production request
(header order, `Accept-Encoding: gzip`, `tool_choice:"none"`, real tool
descriptions) — none changed the verdict once sessions were valid. Python's
plain `ssl` ClientHello WAS 403'd with a raw `cloudflare` HTML page (a
different layer), curl's OpenSSL hello was not — so the CF edge does read
_something_ in the hello band, but the production forge is already past it.

### Rate-state instability is real and probe traffic itself is abusive

Half-way through Round 2 the previously-clean IPs (`home2`, `mkvn5`, and the
server's own direct IP) independently crossed into the flagged/rate-capped
set after my repeated rejected probes, while the container pipeline kept
passing on `direct` (200 x4 at a moment the raw probes 403'd). Two
constants:

1. The gate treats **clusters of rejected requests** as independent — a
   deluge of 403-inducing probes flags an IP fast; a "clean" IP in this
   regime is fragile, so debugging a live IP at this stage must be small-N.
2. Rate caps (`429 free-tier concurrency`-shape) arrive much earlier than
   the abuse 403 on freshly-used IPs: direct was 429-capped after ~3 QUEUED
   requests per second-range while other IPs 403'd hard.

### Final Round 2 conclusions

- The "fingerprint" door is closed: header-level identity is fully
  admitted (Round 1's 200-through-clean-IPs plus this Round 2 bisect matrix,
  done with corrected sessions).
- The gate reads two verdicts into the same `403 FreeTierError` body:
  (a) per-IP abuse ban (Round 1's finding),
  (b) request-identity shape (session-id tail length/counter — the axis
  this repo's `internal/identity` is already pinned to).
- Any future "403 is back" triage should FIRST re-measure per upstream IP
  with 8-10 requests (Round 1 style), because after Round 2 the flag set
  churns within minutes-to-hours: the flagged set is dynamic, not a list.
- CF edge layer: python-plain-ssl ClientHello scored as bot; anything Go/curl
  gets through; production's uTLS hello mixed in. The full forge is still
  what you want on the wire — zero drift indicated.
