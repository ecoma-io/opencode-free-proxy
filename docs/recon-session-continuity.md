# Recon: session / project continuity across requests

Empirical check, 2026-09-20, against the **real** free upstream
(`https://opencode.ai/zen/v1`, model `big-pickle`) through the local proxy.
Question: request 1 sends session S1, request 2 continues the conversation
with session S2 — does upstream error, and what happens to caching?
Extended the same day with the `x-opencode-project` id (§ Project id).

## Method

Proxy on `:8091` (real upstream), `POST /v1/chat/completions`, non-streaming.
Native session ids (matching `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`) pass
through the proxy verbatim (`upstream.ResolveSession`, `executor.go:54-60`),
so the upstream saw exactly the header values below.

**A — server-side memory probe**: A1 stores a secret with S1; A2 asks for it
with S2; A3 asks again with S1. Upstream is stateless, so neither can recall
it — this confirms it (and that a session switch never trips an error).

**B — prompt-cache probe**: byte-identical body sent 4× varying ONLY the
session header; read `usage.prompt_tokens_details.cached_tokens`.

## Results

| #   | Session                                 | Status | prompt | cached_tokens | Note                           |
| --- | --------------------------------------- | ------ | ------ | ------------- | ------------------------------ |
| A1  | S1                                      | 200    | 463    | 256           | stored secret, answered `ok`   |
| A2  | **S2** (different)                      | 200    | 457    | 256           | **no error**                   |
| A3  | S1 (same)                               | 200    | 457    | 256           | **no error**, same shape as A2 |
| B1  | S1                                      | 200    | 651    | 256           | first hit of the shared prefix |
| B2  | S1 (repeat)                             | 200    | 651    | **512**       | cache grew                     |
| B3  | **S2** (different)                      | 200    | 651    | **512**       | cache NOT lost                 |
| B4  | _(none — proxy forged a fresh session)_ | 200    | 651    | **512**       | cache NOT lost                 |

## Conclusions

1. **No error.** A different session id on a continuation request is accepted
   like any other request — the upstream API is stateless; conversation
   continuity lives entirely in the message history the client resends.
2. **No server-side conversation memory.** A3 (same session) could recall the
   secret no better than A2 — the session header carries no conversation
   state; it is only an identity/routing label (and a cache-stability hint).
3. **Prompt cache is content-keyed, not session-keyed.** `cached_tokens`
   tracks the shared prompt prefix and is unaffected by switching sessions —
   even a fresh forged session keeps the full 512. A client that changes its
   session id on every turn loses nothing.
4. **Where stability still matters (our side)**: for clients that send NO
   usable session, `identity.ResolveSessionID` synthesizes a conversation-
   stable id (assistant-text hash → TTL store, sessionresolver.go) precisely
   so the upstream sees a stable `x-opencode-session`; with content-keyed
   caching that stability is belt-and-suspenders, not a correctness
   requirement.

## Project id (`x-opencode-project`)

Same rig, header `x-opencode-project` varied alongside/instead of the
session (`P1`/`P2` native-looking values, the official CLI sends
`project.id` here — request.ts:190; the proxy forges `global` by default).

| #   | Session    | Project                    | Status | cached_tokens | Note                                                                           |
| --- | ---------- | -------------------------- | ------ | ------------- | ------------------------------------------------------------------------------ |
| C1  | S1         | P1                         | 200    | 256           | stored secret, answered `ok`                                                   |
| C2  | **S2**     | **P2**                     | 200    | 256           | both different — **no error**                                                  |
| C3  | S1         | P1                         | 200    | 256           | no recall (stateless, same as §A)                                              |
| D1  | S1         | P1                         | 200    | **512**       | shared filler prefix already cached from the earlier session run (~30 min old) |
| D2  | S1         | P1                         | 200    | 512           |                                                                                |
| D3  | S1         | **P2**                     | 200    | **512**       | project switch — cache intact                                                  |
| D4  | **S2**     | **P2**                     | 200    | **512**       | both switched — cache intact                                                   |
| D5  | _(forged)_ | _(proxy default `global`)_ | 200    | **512**       |                                                                                |
| D6  | S1         | _200-char junk_            | 200    | **512**       | no validation, no error                                                        |
| D7  | S1         | `not-a-project`            | 200    | **512**       | no validation, no error                                                        |

Findings:

1. **The project id is not validated** upstream — arbitrary, overlong and
   junk values are accepted exactly like real ones (the proxy's `global`
   default was already evidence of this).
2. **The prompt cache does not partition by project** either — `cached_tokens`
   followed the shared content prefix across project switches, across
   session+project switches, and across a completely forged identity. In D1
   it even hit a cache entry warmed ~30 minutes earlier by a _different_
   body sharing only the prefix filler — pure content prefix caching,
   ~256-token block granularity.
3. Practical rule: neither `x-opencode-session` nor `x-opencode-project`
   changes admission, routing correctness, or cacheability. They are
   labels; only the request content matters upstream.

## Side observation (upstream model quirk, not session-related)

A2/A3 answers came back as raw DeepSeek tool-markup text
(`<｜DSML｜tool_calls>… <｜DSML｜invoke name="bash">`) in `content` — the model
tried to "call" the injected fingerprint `bash` tool even though the proxy
sends `tool_choice: "none"` for tools-less callers. Both sessions produced
the same shape, so it is model variance, not session behavior. Ported
behavior relays it verbatim like the JS router would; noted here in case it
ever surfaces as a client complaint.
