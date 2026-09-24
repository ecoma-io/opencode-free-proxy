package router

// response_headers.go — the only response header this proxy adds beyond the
// OpenAI-compatible envelope, the CORS trio (handler.go corsHeaders),
// Content-Type and the SSE framing headers (Cache-Control, Connection).
//
// # OFP is a plain OpenAI-compatible API
//
// A caller treats this process like any other OpenAI-compatible upstream: it
// POSTs to /v1/chat/completions or /v1/responses, it reads the status, the
// body and the content type, and it decides its own retry/fallback policy from
// those. No OFP-specific failure header is part of that surface, and a caller
// never has to know that OFP exists to be correct against it.
//
// # Failure provenance is internal
//
// Where a status came from, which step of the egress path failed, and whether
// a request byte provably left this process are all REAL, and all recorded —
// but internally, and only for this process's own decisions:
//
//   - the egress move — `Failure.ReplaySafe()`, which gates whether the
//     attempt may be re-sent on another egress (internal/upstream/fallback.go);
//   - the health mark — `Failure.MarksEgressHealth()`, a deliberately separate
//     and narrower predicate for whether the failure implicates the egress
//     itself (issues #62/#68);
//   - the evidence/forensics rows — the origin, phase, request state, health
//     and fallback decisions of every failed interaction
//     (internal/upstream/evidence.go, internal/router/evidence_log.go);
//   - the completion log line, which is this process's own record of an
//     outcome.
//
// None of that is serialized to a response. It used to be, as a namespaced
// three-header recovery contract (X-OFP-Failure-Origin / X-OFP-Failure-Phase /
// X-OFP-Request-State) that treated the caller as a trusted peer of OFP's
// internals. It was removed: provenance is an implementation detail, it is
// free to change shape without notice (it has, several times), and making a
// caller depend on it is the opposite of exposing an OpenAI-compatible API.
// The internal model is unchanged by that removal — the labels simply stopped
// leaving the process. See docs/recovery-semantics.md ("Cross-service
// contracts") for the ownership split: the caller owns provider-level retry
// and fallback, OFP owns the relay, the transformation and the transport
// classification that drives its own internal egress recovery, and RPGW owns
// the shared egress infrastructure.
//
// # X-OFP-Egress is a diagnostic, not provenance
//
// headerEgress is deliberately NOT part of the removed contract and must not
// be grouped with it: it names the configured egress an attempt was made
// through, and it carries no classification, no phase and no request state. It
// answers "which configured egress did this request go out through", which is
// what makes a multi-egress deployment diagnosable from the client side and
// what the end-to-end suite asserts rotation and failover with. It is an egress
// id and nothing else — never a proxy URL, host, port or credential.
//
// It is written once, on the post-executor path, before the response is
// relayed — so it appears on any outcome that DIALED something, including the
// forced-conversion 502 this process synthesizes over a live stream (the
// attempt did go out through that egress, which is the question the header
// answers). It is absent from every path that never dialed, notably a
// transport-failure 502 and the pre-plan "no eligible egress" 502. That is
// unchanged from before the contract removal — the header never claimed to
// separate "the provider answered" from "a gateway error followed a dial".
//
// It is response-only: nothing reads it from a request, and a client cannot
// steer or forge egress selection with it (a client-supplied value is a
// different header map entirely; the selected id is written by this process).
const headerEgress = "X-OFP-Egress"
