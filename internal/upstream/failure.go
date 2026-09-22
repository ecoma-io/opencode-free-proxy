package upstream

import (
	"context"
	"errors"
	"net"
)

// Class is the executor's failure taxonomy. It exists to keep three concerns
// separate inside upstream: the retry matrix (unchanged, INSIDE one egress
// attempt), health observation (which classes poison an egress), and
// fallback (which classes may move to the next attempt).
type Class int

const (
	ClassSuccess Class = iota
	// ClassConnectionError: transport failure (dial, DNS for the proxy or
	// target, reset before response). 502-mapped in the retry matrix.
	ClassConnectionError
	// ClassProxyAuthError: the proxy refused credentials, PROVEN at the
	// transport boundary by a typed *proxyAuthError — SOCKS5 RFC 1929 status
	// != 0 (socks5.go) or an HTTP(S) CONNECT answered 407 by the proxy
	// (connect.go). Nothing is ever promoted to this class by error text:
	// ownership comes from the code that spoke the proxy protocol. SOCKS5
	// REP 0x02 is deliberately NOT one (RFC 1928 §6 calls it "connection not
	// allowed by ruleset", a plain connection refusal — fallback + health
	// mark either way, only the label changes).
	ClassProxyAuthError
	// ClassTimeout: response headers took longer than ConnectTimeout.
	ClassTimeout
	// ClassUpstream429: 429 from the upstream. Fallback allowed, health is
	// NEVER marked — rate limiting says nothing about the egress's ability
	// to serve, and poisoning it on 429 would suppress healthy egresses.
	ClassUpstream429
	// ClassUpstream5xx: 502/503/504 after the retry matrix is exhausted.
	ClassUpstream5xx
	// ClassClientError: other 4xx — including EVERY 407 that arrives as a
	// response status (see classifyStatusFor). Never falls back (the request
	// itself was rejected; another egress would repeat the 400) and never
	// marks health.
	ClassClientError
	// ClassResponseStarted: the mid-stream commitment boundary — upstream
	// died after a live response was already delivered downstream; no
	// fallback is possible and the relay synthesizes the abort. It is a
	// LOGGING classification only, produced exclusively by the router's
	// evidence rows (stream.go / forced.go phase failures): the executor
	// never sees it, so it neither falls back (the commitment stands) nor
	// marks health (MarksHealth/FallbackAllowed are false below, pinned by
	// test) — a stream that dies after a 200 start leaves the egress's health
	// exactly as the header-time success observation left it.
	ClassResponseStarted
	// ClassContextCanceled: downstream disconnected. No fallback (nothing
	// to deliver to) and no health mark (the egress did nothing wrong).
	ClassContextCanceled
)

func (c Class) String() string {
	switch c {
	case ClassSuccess:
		return "success"
	case ClassConnectionError:
		return "connection_error"
	case ClassProxyAuthError:
		return "proxy_auth_error"
	case ClassTimeout:
		return "timeout"
	case ClassUpstream429:
		return "upstream_429"
	case ClassUpstream5xx:
		return "upstream_5xx"
	case ClassClientError:
		return "client_error"
	case ClassResponseStarted:
		return "response_started"
	case ClassContextCanceled:
		return "context_canceled"
	default:
		return "unknown"
	}
}

// MarksHealth reports whether this class counts toward the consecutive-
// failure threshold. Deliberately excludes 429 and 4xx: both are verdicts
// about a request, not about the egress.
func (c Class) MarksHealth() bool {
	switch c {
	case ClassConnectionError, ClassProxyAuthError, ClassTimeout, ClassUpstream5xx:
		return true
	}
	return false
}

// FallbackAllowed reports whether the executor may try the next egress.
//
// Deliberate divergence from base.js, cited per porting discipline: the JS
// router moves to the next URL only on 429 (base.js:83-85 shouldRetry) and
// network errors (base.js:175-178) — an exhausted 502/503/504 is returned
// as-is from the same URL. This executor ALSO falls back on 5xx/timeouts:
// with several egresses, a proven-bad upstream answer is a reason to try
// another PATH, and the health registry (not the retry matrix) is what
// absorbs the poisoning concern.
func (c Class) FallbackAllowed() bool {
	switch c {
	case ClassConnectionError, ClassProxyAuthError, ClassTimeout, ClassUpstream429, ClassUpstream5xx:
		return true
	}
	return false
}

// proxyAuthError is produced ONLY by the transport boundary speaking the
// proxy's own protocol (connect.go: the proxy answered CONNECT with 407;
// socks5.go: RFC 1929 credential rejection, or a demand for auth we never
// sent). It is the single source of ClassProxyAuthError — there is no
// string-based promotion anywhere (issue #6: a dial error that happens to
// contain "407" must never classify as proxy-auth).
type proxyAuthError struct{ msg string }

func (e *proxyAuthError) Error() string { return e.msg }

// classifyNetErrFor maps a transport error to its class. A canceled context
// wins over everything (the request is gone); typed proxy-auth markers beat
// generic timeouts; timeouts beat connection errors (net.Error.Timeout).
//
// There is deliberately NO error-text probing — not even the canonical
// "Proxy Authentication Required" reason phrase. (Go's own proxy CONNECT
// path strips the numeric code of a refused CONNECT and keeps only the
// phrase — an unclassifiable error. This package keeps stdlib OFF that
// path: attempt routes every https hop through the tunneled DialTLSContext
// boundary in connect.go and every http hop through absolute-form, so no
// transport here both carries a Proxy AND can see an https URL — the
// manual redirect walk re-picks the transport per hop, so even an
// http→https redirect hop rides the typed boundary instead of stdlib's
// CONNECT. See the Client doc in client.go.) Beyond that, any transport
// error may carry arbitrary text (an IPv6 literal with a :407 octet, a
// port, a hostname) that a substring probe would misclassify. Ownership of
// a 407 is decided where the proxy protocol is spoken — the typed boundary
// in connect.go/socks5.go — never by inspecting the message (issue #6).
func classifyNetErrFor(ctx context.Context, err error) Class {
	if ctx.Err() != nil {
		return ClassContextCanceled
	}
	var pae *proxyAuthError
	if errors.As(err, &pae) {
		return ClassProxyAuthError
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	return ClassConnectionError
}

// classifyStatusFor maps a response STATUS to its class. A status arrives as
// a well-formed HTTP response — and after the transport boundary has already
// accepted our proxy credentials, a 407 on that response belongs to one of
// two owners the wire cannot distinguish:
//
//   - https target through an HTTP(S) proxy: the CONNECT tunnel is up, so
//     the proxy has already authenticated us; the response came through the
//     tunnel from the ORIGIN.
//   - plain http target through a forward proxy: the proxy relays the
//     request and the reply byte-for-byte; a 407 may be the proxy gating the
//     request OR the origin's own answer, and no header is reliably
//     proxy-authored (issue #6 rejects content sniffing).
//
// Both are 4xx verdicts about the REQUEST, so both classify conservatively
// as ClientError: no fallback (the next egress repeats the rejection), no
// health mark (a credential problem is not an outage). The provable
// proxy-auth cases never reach this function — a proxy refusing CONNECT, or
// a SOCKS5 RFC 1929 rejection, is a transport ERROR typed proxyAuthError by
// the boundary (classifyNetErrFor). SOCKS5 tunnels carry no Proxy-
// Authorization at all (RFC 1929 authenticates once at setup), so a 407
// surfacing after the tunnel is the origin's by construction.
func classifyStatusFor(status int) Class {
	if status == 429 {
		return ClassUpstream429
	}
	if status >= 500 {
		return ClassUpstream5xx
	}
	// Parity with the pre-hardening fallthrough: any other status (only ever
	// called for >= 400) is a client-error verdict.
	return ClassClientError
}
