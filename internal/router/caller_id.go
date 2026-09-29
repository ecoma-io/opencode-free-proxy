package router

import (
	"net/http"
	"regexp"

	"opencode-free-proxy/internal/config"
)

// Caller correlation id (issue #83). This process mints its own `request_id`
// (16 hex chars) and logs it everywhere; that id is a good handle INSIDE the
// process and stops at the process boundary. In the deployment this process
// actually runs in — the Injector sits in front, mints its own id, and both
// services' logs are read side by side during an incident — a completion line
// therefore cannot be tied to the caller's request from anything emitted here.
//
// callerRequestID reads that caller-supplied value from ONE fixed header
// (config.CallerRequestIDHeader) so it can ride along as a SEPARATE log field.
//
// Three properties are load-bearing, and each is pinned by a test in
// caller_id_test.go:
//
//  1. LOG-ONLY. The value never reaches routing, health, fallback, session
//     resolution, the executor, or the response. It is recorded, never
//     derived, never accepted from the wire into any decision — the general
//     form of the intent rule (AGENTS.md rule 12). `X-Request-Id` is also
//     absent from captureDownstream's forwarding allow-list, so it is never
//     sent upstream either.
//  2. NEVER PROMOTED. The local `request_id` stays authoritative and
//     `attempt_id` stays `request_id/N` (evidence_log.go). A caller may name a
//     request; it may not name the attempt sequence.
//  3. ACCEPT-OR-IGNORE, NEVER SANITIZE. An out-of-charset value is treated as
//     ABSENT — the field is simply not emitted. It is never folded, collapsed
//     or truncated, because a rewritten correlation key is the worst outcome
//     available: the join fails silently with no field pointing at the reason,
//     and an operator ends up grepping for a value that was never sent.
//     Rejection fails loudly instead: this process's own request_id is still
//     there, so the join degrades to "same service, same time" rather than to
//     "wrong id". A malformed value is never an error the caller sees — it is
//     a value this process declines to use, and the request proceeds exactly as
//     it would have.
//
// The pattern (config.CallerRequestIDPattern) is compiled here with its
// consumer, mirroring ResponsesURLModelsPattern -> cloak.go:27: the STRING
// lives in internal/config per porting rule 3, the compiled regexp here.
var callerRequestIDRe = regexp.MustCompile(config.CallerRequestIDPattern)

// callerRequestID returns the validated caller-supplied correlation id, or ""
// when the header is absent, empty, over-long, or carries any byte outside the
// allowlist. "" means "this request has no caller id" — callers of this
// function must treat it as an ABSENT FIELD and leave the log field off, not
// as a value to render.
//
// That guard is not optional. zerolog's Event.Str appends unconditionally —
// unlike the issue's original assumption, an empty value IS emitted, as
// `"caller_request_id":""`. A field that is always present but usually empty is
// exactly the "looks meaningful, says nothing" shape this change exists to
// avoid, so every emit site guards on != "".
func callerRequestID(h http.Header) string {
	v := h.Get(config.CallerRequestIDHeader)
	if !callerRequestIDRe.MatchString(v) {
		return ""
	}
	return v
}
