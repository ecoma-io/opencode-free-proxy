package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
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
	// ClassProxyAuthError: the proxy refused credentials (SOCKS5 REP 0x02,
	// RFC 1929 status != 0, HTTP 407-style proxy rejection).
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
// RFC 1929 / REP 0x02) so classifyNetErr can single them out.
type proxyAuthError struct{ msg string }

func (e *proxyAuthError) Error() string { return e.msg }

// classifyNetErr maps a transport error to its class. A canceled context
// wins over everything (the request is gone); proxy-auth markers beat
// generic timeouts; timeouts beat connection errors (net.Error.Timeout). The
// 407 check carries BOTH the numeric code and the canonical reason phrase:
// Go stdlib proxy errors embed the status line ("407 Proxy Authentication
// Required" — net/http transport), but the phrase wording is not a stable
// API contract, so the numeric marker is the durable half of the match.
func classifyNetErr(ctx context.Context, err error) Class {
	if ctx.Err() != nil {
		return ClassContextCanceled
	}
	var pae *proxyAuthError
	if errors.As(err, &pae) {
		return ClassProxyAuthError
	}
	if strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), "Proxy Authentication Required") {
		return ClassProxyAuthError
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTimeout
	}
	return ClassConnectionError
}

// classifyStatus maps a terminal (post-retry-matrix) response status to a
// class. Only >= 400 reach it (below that Do returns the response as a
// success); 429 is its own class so fallback can treat it specially.
func classifyStatus(status int) Class {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassUpstream429
	case status >= 500:
		return ClassUpstream5xx
	default:
		return ClassClientError
	}
}
