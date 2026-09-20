# Recon: mid-conversation egress IP switch

**Question.** One Claude Code session is served by this proxy. Request 1
egresses via the machine's own IP; the conversation then continues with the
egress switched to a different IP — and the fresh turn carries a completely
different identity (new `x-opencode-session`, `-request`, `-project`). Does
the opencode free tier block, error, or degrade the conversation?

**Date:** 2026-09-20 · upstream `big-pickle`, non-streaming (forced SSE→JSON
aggregate), UA triple `opencode/1.18.31 ai-sdk/provider-utils/4.0.23
runtime/bun/1.3.14` (live sync).

## Method

Real proxy binary in the loop, egress chosen per request:

```
Claude Code (driver) ──HTTP──► opencode-free-proxy :8090
                                  │  upstream.base=http://127.0.0.1:8091
                                  ▼
                    egress-switching forwarder :8091  (mode file per request)
                      ├─ mode=direct  → https://opencode.ai   (machine IP)
                      └─ mode=proxy   → external IPv4 HTTP CONNECT proxy →
                                        https://opencode.ai   (different IP)
```

- The driver replays one conversation for 4 turns and rotates to a COMPLETELY
  fresh identity on every turn (native `x-opencode-*` passthrough — the
  proxy forwards them verbatim; UA not sent by the driver, so the proxy
  forges the synced opencode UA exactly as it does for Claude Code).
- Continuity is tested behaviorally: turn 1 plants a secret code word, later
  turns re-send the full history and ask for it. Recall ⇒ the upstream
  accepted the continued conversation.
- Egress A: machine IP `123.16.157.22` (Viettel AS7552, IPv4). Egress B, two
  different external HTTP proxies for two runs — run 1: `14.165.219.244`
  (Viettel AS7552, IPv4); run 2: `2401:3660:0:43ca:82ea:d807:dedc:45c8`
  (Megacore AS140810, IPv6) — spanning a different ASN _and_ address family.
  All egresses verified live via `https://www.cloudflare.com/cdn-cgi/trace`
  (`ip=` line); run 2's ASN via ipinfo.
- Harness lives in `/tmp` (ephemeral, deliberately not committed; proxy
  credentials are never written into the repo). An earlier SOCKS5 candidate
  for egress B answered `0x07` (command not supported) to hand-rolled SOCKS5
  clients while curl succeeded against the same endpoint — abandoned as a
  harness-side quirk, unrelated to the upstream.

## Results

Run 1 — egress B = Viettel IPv4 proxy:

| Turn     | Egress                 | session / project / request id | Status | Recall                | `cached_tokens` |
| -------- | ---------------------- | ------------------------------ | ------ | --------------------- | --------------- |
| R1 plant | A `123.16.157.22`      | fresh                          | 200    | — (plant ack "OK.")   | 256             |
| R2 ask   | **B `14.165.219.244`** | fresh                          | 200    | **YES — "MANGO-42."** | 256             |
| R3 ask   | A again                | fresh                          | 200    | YES                   | 256             |
| R4 ask   | B again                | fresh                          | 200    | YES                   | 256             |

Run 2 — egress B = Megacore IPv6 proxy (different ASN + family):

| Turn     | Egress                   | session / project / request id | Status | Recall               | `cached_tokens` |
| -------- | ------------------------ | ------------------------------ | ------ | -------------------- | --------------- |
| R1 plant | A `123.16.157.22`        | fresh                          | 200    | — (plant ack "OK")   | 256             |
| R2 ask   | **B `2401:3660:…:45c8`** | fresh                          | 200    | **YES — "MANGO-42"** | 256             |
| R3 ask   | A again                  | fresh                          | 200    | YES                  | 256             |
| R4 ask   | B again                  | fresh                          | 200    | YES                  | 256             |

Non-200: none in either run (8/8 turns). First attempt succeeded on every
turn — no retry budget was consumed by the switches.

## Conclusions

1. **No block on mid-conversation IP change.** The upstream answered 200 on
   every turn regardless of which egress it came from, including the first
   turn after the switch (R2) and after switching back (R3/R4). No 403, no
   429, no silent degradation, no retry consumption visible (first attempt
   succeeded).
2. **Conversation continuity is entirely client-side.** "Continuing" the
   conversation across an IP + identity switch works because the full history
   is resent; the upstream keeps no session state (consistent with
   [recon-session-continuity.md](recon-session-continuity.md)).
3. **The prompt cache ignores both IP and identity.** `cached_tokens=256`
   on every turn — including the first turn from a brand-new IP with a
   brand-new session id — confirms the cache is keyed on content prefix
   (~256-token blocks), not on connection, IP, or `x-opencode-*` values.
4. **Nothing for the proxy to handle.** Egress IP is invisible to the free
   tier's contract (UA + fingerprint tools + `Bearer public`). No sticky-IP
   routing, no IP pinning, no special reconnect logic is needed when a
   client's network changes mid-conversation (laptop roaming, NAT pool rotation,
   VPN reconnect — all fine).

## Limitations

- Minutes-long windows, 8 turns total: this rules out _observed_ blocking,
  not a statistical rate-limit policy. The three egresses span two ASNs
  (Viettel AS7552, Megacore AS140810) and both address families, but each
  pair was exercised only briefly.
- The free tier is anonymous (`Bearer public`) — there is no account for an
  IP change to invalidate; these results match that model.
