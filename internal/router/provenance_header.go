package router

import (
	"net/http"
	"strings"

	"opencode-free-proxy/internal/upstream"
)

// provenance_header.go — the wire half of the recovery contract (issue #55,
// docs/recovery-semantics.md "Cross-service contracts").
//
// A caller that receives an error from this proxy cannot act correctly on the
// status alone: a 502 the PROVIDER sent and a 502 this process synthesized
// because the egress path failed demand opposite responses. The first is a
// provider verdict that must travel upward with its budget untouched; the
// second is a transport failure whose only consequence may be a new egress —
// and only when the request provably never went out. The status cannot tell
// them apart, which is exactly why OFP records a Failure (provenance.go in
// internal/upstream) and why the classification is READ OFF THAT RECORD here,
// never inferred from the status that is about to be written.
//
// The header names are namespaced and internal:
//
//	X-OFP-Failure-Origin: upstream | ambiguous | gateway
//	X-OFP-Failure-Phase:  <dial/transport phase>   (gateway only, absent if unattributable)
//	X-OFP-Request-State:  not_sent | unknown | response_started   (anything but upstream)
//	X-OFP-Egress:         <configured egress id>   (existing; success only)
//
// Absence means "this response is not an upstream-interaction outcome" — a
// local 400 for a bad body, an unknown model, a draining server, a synthetic
// bypass/test-connection completion. It never means "upstream".
//
// The three origins are three different instructions to the caller:
//
//	upstream   the provider's own answer. Relay the status and body as they
//	           are and leave the provider-level budget alone.
//	ambiguous  an answer from a hop an HTTP intermediary carried, so the
//	           PROVIDER may never have seen the request (a forward proxy's own
//	           407/403/502/503, a captive portal's 200). Relay it — it is the
//	           only answer there is — but never treat it as a provider verdict,
//	           and never re-send: the request byte left this process and no
//	           layer here can prove what became of it.
//	gateway    this process's own failure, before or after the request byte.
//	           With not_sent the caller's retry may re-send; with unknown or
//	           response_started it may not.
//
// Nothing here carries identity: the values are fixed enums, never a proxy
// URL, host, port, address, egress id or pool fact. X-OFP-Egress stays the
// egress id it always was.
const (
	headerFailureOrigin = "X-OFP-Failure-Origin"
	headerFailurePhase  = "X-OFP-Failure-Phase"
	headerRequestState  = "X-OFP-Request-State"
	// headerEgress names the configured egress that served the request (id
	// only — never its URL, address or credentials), on a success response.
	headerEgress = "X-OFP-Egress"
	// internalHeaderPrefix is the entire namespace this proxy reserves. Every
	// inbound header carrying it is dropped before any stage of a request
	// runs (relay and HandleModels — the endpoints that read a request).
	internalHeaderPrefix = "x-ofp-"

	originUpstream  = "upstream"
	originAmbiguous = "ambiguous"
	originGateway   = "gateway"
)

// setFailureProvenance labels a response that came out of the upstream
// attempt path with where its status came from.
//
//   - OriginUpstream: the provider answered (any status, 2xx and every
//     4xx/5xx including 429), so the status and body are its own, relayed
//     verbatim. No phase or state is emitted: those describe a REQUEST's
//     fate, and a request that was answered has no open question about it.
//   - OriginAmbiguous: a response exists on a path an HTTP-level forward proxy
//     carried, where the proxy is an answering peer and the wire cannot say
//     whether it or the origin wrote the reply (issue #63). The origin is
//     `ambiguous` and the request state `unknown` — the request byte provably
//     reached the proxy, so not_sent would be a fabrication in the dangerous
//     direction, and response_started would claim a provider answer nobody
//     proved. No phase: the phase vocabulary names the step of the EGRESS PATH
//     that failed, and on this response nothing failed — the response is
//     relayed exactly like a provider's, because a status cannot be
//     re-litigated. What the caller must not do is treat it as the provider's
//     verdict (see the header table above).
//   - OriginTransport: the egress path failed and no provider response exists
//     FOR THE FAILING HOP; OFP produced this status because of it. The phase
//     and the proven request state ride along so the caller can decide whether
//     its own retry may re-send without inferring anything from a status code.
//     Only not_sent authorises a re-send: it says the whole logical call
//     provably never put a request byte on the wire. unknown says the call
//     cannot be proven either way, and response_started says a response HAD
//     been received on this call before the hop that failed — a followed
//     redirect on an earlier hop (issue #60). The last two both forbid a
//     re-send, which is the only decision this header is read for.
//   - OriginClient: the caller went away mid-attempt. No provenance is
//     written: no upstream interaction concluded and neither side can be
//     blamed, so the honest answer is the absent header, not a label.
//   - OriginNone: nothing to attribute (a skip, or a status this process
//     synthesized with no attempt behind it).
//
// Note that a SERVED response goes through here too, not just a failed one: the
// authorship of a 200 is the same question as the authorship of a 502 (a
// captive portal answers 200), and the failure record is where the executor
// reports it in both cases. Ownership is read off that record — never off the
// status that is about to be written, which is the whole point of the header.
func setFailureProvenance(h http.Header, f upstream.Failure) {
	switch f.Origin {
	case upstream.OriginUpstream:
		h.Set(headerFailureOrigin, originUpstream)
	case upstream.OriginAmbiguous:
		h.Set(headerFailureOrigin, originAmbiguous)
		h.Set(headerRequestState, f.RequestState.String())
	case upstream.OriginTransport:
		h.Set(headerFailureOrigin, originGateway)
		if phase := f.Phase.String(); phase != "" {
			h.Set(headerFailurePhase, phase)
		}
		h.Set(headerRequestState, f.RequestState.String())
	}
}

// setGatewayNotSent labels a gateway-generated error produced WITHOUT an
// upstream attempt: no eligible egress for the matched route. Nothing was
// dialed, so not_sent is not an inference — it is the only thing that could
// have happened. The 502 this accompanies is the exact status a provider may
// also send, which is why the label matters here.
func setGatewayNotSent(h http.Header) {
	h.Set(headerFailureOrigin, originGateway)
	h.Set(headerRequestState, upstream.RequestStateNotSent.String())
}

// setGatewayResponseStarted labels a gateway-generated error produced AFTER
// an upstream response had already been received on this logical call (issue
// #72). The 502 is this process's own, and the state header records the fact
// that forbids a re-send: a response existed before this process failed.
// Called where the forced SSE→JSON path synthesizes a 502 over a live 2xx
// stream — the label the success-path header (upstream/ambiguous) would
// otherwise have left on the wire.
func setGatewayResponseStarted(h http.Header) {
	h.Set(headerFailureOrigin, originGateway)
	h.Set(headerRequestState, upstream.RequestStateResponseStarted.String())
}

// stripInternalHeaders removes every inbound X-OFP-* header.
//
// Defence in depth today, a hard requirement the moment one of these headers
// is read rather than written: a public client must never be able to forge
// provenance, impersonate the trusted boundary, or reach a decision through
// the internal namespace. Stripping is unconditional — it does not depend on
// which names are currently read, so adding a reader later cannot quietly
// turn a client-supplied value into a trusted one.
//
// The match is case-insensitive on lowercased keys: net/http canonicalises
// inbound names, but a hand-built request in a test (or a future caller) need
// not have, and a prefix that only catches the canonical spelling would be a
// silent bypass.
func stripInternalHeaders(r *http.Request) {
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), internalHeaderPrefix) {
			delete(r.Header, k)
		}
	}
}
