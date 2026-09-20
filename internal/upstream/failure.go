package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"opencode-free-proxy/internal/config"
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
	// ClassProxyAuthError: the proxy refused credentials (SOCKS5 RFC 1929
	// status != 0, HTTP(S) CONNECT 407). SOCKS5 REP 0x02 is NOT auth — RFC
	// 1928 §6 calls it "connection not allowed by ruleset", a plain
	// connection refusal (fallback + health mark either way, only the label
	// changes).
	ClassProxyAuthError
	// ClassTimeout: response headers took longer than ConnectTimeout.
	ClassTimeout
	// ClassUpstream429: 429 from the upstream. Fallback allowed, health is
	// NEVER marked — rate limiting says nothing about the egress's ability
	// to serve, and poisoning it on 429 would suppress healthy egresses.
	ClassUpstream429
	// ClassUpstream5xx: 502/503/504 after the retry matrix is exhausted.
	ClassUpstream5xx
	// ClassClientError: other 4xx. Never falls back (the request itself was
	// rejected; another egress would repeat the 400) and never marks health.
	ClassClientError
	// ClassResponseStarted: reserved for the relay's mid-stream abort hook —
	// upstream died after the first downstream write; no fallback is
	// possible (the commitment boundary), the relay synthesizes the abort.
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
func (c Class) FallbackAllowed() bool {
	switch c {
	case ClassConnectionError, ClassProxyAuthError, ClassTimeout, ClassUpstream429, ClassUpstream5xx:
		return true
	}
	return false
}

// proxyAuthError marks dialer-level proxy authentication failures (SOCKS5
// RFC 1929 status != 0, or a proxy that demanded auth we never sent) so
// classifyNetErrFor can single them out. SOCKS5 REP 0x02 is deliberately NOT
// one (RFC 1928 §6: "connection not allowed by ruleset" — plain refusal).
type proxyAuthError struct{ msg string }

func (e *proxyAuthError) Error() string { return e.msg }

// classifyNetErrFor maps a transport error to its class. A canceled context
// wins over everything (the request is gone); proxy-auth markers beat
// generic timeouts; timeouts beat connection errors (net.Error.Timeout). The
// canonical reason phrase "Proxy Authentication Required" is the PRIMARY
// 407 marker: Go's stdlib strips the numeric code from a failed CONNECT
// (transport.go: strings.Cut(resp.Status, " ") → errors.New(text)), so a
// real CONNECT-407 error's text is the phrase only. The bare "407" probe is
// secondary, for non-stdlib error shapes — and it fires ONLY when a proxy
// was configured: without one the text is a dial error (an IPv6 octet, say)
// and would misfire.
func classifyNetErrFor(ctx context.Context, err error, hasProxy bool) Class {
	if ctx.Err() != nil {
		return ClassContextCanceled
	}
	var pae *proxyAuthError
	if errors.As(err, &pae) {
		return ClassProxyAuthError
	}
	if hasProxy && (strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), "Proxy Authentication Required")) {
		return ClassProxyAuthError
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	return ClassConnectionError
}

// classifyStatusFor is classifyStatus plus proxy knowledge for the 407
// ambiguity — the one status whose owner determines the class. The decision
// matrix (transport/proxy boundary):
//
//	429                   → Upstream429 (fallback, no health mark)
//	>= 500                → Upstream5xx
//	407, no proxy         → ClientError (an origin's 407 is a plain 4xx)
//	407, proxy, https     → ClientError: an https target is CONNECT-tunneled,
//	                         so the proxy could never inject a 407 response —
//	                         only the origin behind the tunnel could; its 407
//	                         is a request verdict, not a proxy-credential one.
//	407, proxy, http, creds → ProxyAuth: we sent Proxy-Authorization and got
//	                         407 back — the proxy gated the request, the
//	                         origin never saw it. Definitive proxy-auth.
//	407, proxy, http, no creds → ClientError (ambiguous, conservative: the
//	                         proxy may want credentials we never sent, or the
//	                         origin may have answered; both are the operator's
//	                         4xx-style verdict, and falling back would repeat
//	                         the rejection on the next proxy).
//	407, socks5 proxy       → ClientError always: a SOCKS5 tunnel authenticates
//	                         once via RFC 1929 at connect time — nothing on the
//	                         wire carries Proxy-Authorization — so any 407 the
//	                         client sees is the origin's verdict.
func classifyStatusFor(status int, proxy *config.Proxy, targetURL string) Class {
	if status == http.StatusTooManyRequests {
		return ClassUpstream429
	}
	if status >= 500 {
		return ClassUpstream5xx
	}
	if status == http.StatusProxyAuthRequired && proxy != nil {
		// Only HTTP(S) forward proxies speak Proxy-Authorization and can
		// gate a request with a 407: an http target goes absolute-form, an
		// https target CONNECTs — either way the origin's requests carry the
		// header Go derives from the URL userinfo. A SOCKS5 proxy has NO
		// HTTP layer: credentials are exchanged once at tunnel setup (RFC
		// 1929), so a 407 that surfaces after the tunnel is up can only be
		// the origin's verdict — same as the https row below.
		if proxy.Type != config.ProxySOCKS5 {
			if u, err := url.Parse(targetURL); err == nil && u.Scheme == "http" && proxyHasCredentials(proxy) {
				return ClassProxyAuthError
			}
		}
	}
	return ClassClientError
}

// proxyHasCredentials reports whether the proxy URL carries userinfo (http
// proxies: Go derives Proxy-Authorization from it; socks5: it drives
// RFC 1929 auth). Credentials never leave transport.go.
func proxyHasCredentials(p *config.Proxy) bool {
	u, err := url.Parse(p.URL)
	return err == nil && u.User != nil
}
