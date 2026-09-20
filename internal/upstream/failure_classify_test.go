package upstream

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// netTimeoutErr is a net.Error whose Timeout() is true — what the transport
// produces when a dial exceeds its deadline.
type netTimeoutErr struct{ msg string }

func (e *netTimeoutErr) Error() string   { return e.msg }
func (e *netTimeoutErr) Timeout() bool   { return true }
func (e *netTimeoutErr) Temporary() bool { return false }

// TestClassifyNetErrForRows: the context-aware error classifier. The rows pin
// the issue-#6 contract: a canceled context always wins; ONLY a typed
// *proxyAuthError — raised by the code that spoke the proxy protocol
// (connect.go's CONNECT reply, socks5.go's RFC 1929 verdict) — classifies as
// proxy-auth; timeouts beat connection errors; everything else is a
// connection error. There is no error-text probing in either direction.
func TestClassifyNetErrForRows(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	timeoutErr := &netTimeoutErr{msg: "dial tcp 10.0.0.1:443: i/o timeout"}

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want Class
	}{
		{
			name: "canceled context beats a typed proxy-auth marker",
			ctx:  canceled, err: &proxyAuthError{msg: "proxy http://prx:8080: CONNECT refused with 407"},
			want: ClassContextCanceled,
		},
		{
			name: "typed CONNECT 407 is proxy-auth",
			ctx:  context.Background(), err: &proxyAuthError{msg: "proxy http://prx:8080: CONNECT refused with 407 (authentication required)"},
			want: ClassProxyAuthError,
		},
		{
			name: "typed SOCKS5 RFC 1929 rejection is proxy-auth",
			ctx:  context.Background(), err: &proxyAuthError{msg: "socks5 proxy: proxy authentication failed"},
			want: ClassProxyAuthError,
		},
		{
			name: "typed marker is found through wrapping",
			ctx:  context.Background(), err: fmt.Errorf("proxyconnect: %w", &proxyAuthError{msg: "407"}),
			want: ClassProxyAuthError,
		},
		{
			name: "the reason phrase alone is NEVER proxy-auth",
			ctx:  context.Background(), err: errors.New("Proxy Authentication Required"),
			want: ClassConnectionError,
		},
		{
			name: "a 407-bearing IPv6 octet is NEVER proxy-auth",
			ctx:  context.Background(), err: errors.New("dial tcp [2001:407::9]:443: connect: connection refused"),
			want: ClassConnectionError,
		},
		{
			name: "bare 407 text is NEVER proxy-auth",
			ctx:  context.Background(), err: errors.New("407"),
			want: ClassConnectionError,
		},
		{
			name: "net.Error timeout is a timeout",
			ctx:  context.Background(), err: timeoutErr,
			want: ClassTimeout,
		},
		{
			name: "plain dial refusal is a connection error",
			ctx:  context.Background(), err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
			want: ClassConnectionError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNetErrFor(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("classifyNetErrFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// Test407HeuristicCannotFalsePositive: the §16 regression row. Ownership of a
// 407 is decided ONLY by the typed transport-boundary marker; no error string
// — the stripped reason phrase, an IPv6 octet, a port, a query parameter —
// may promote an error to proxy-auth. Every decoy below must land elsewhere.
func Test407HeuristicCannotFalsePositive(t *testing.T) {
	decoys := []string{
		"Proxy Authentication Required",
		"407",
		"407 Proxy Authentication Required",
		"dial tcp 10.0.0.7:407: connect: connection refused",
		"proxy http://[::1]:407: CONNECT tunnel closed after error 4079",
		"Get \"https://host/path?e=407\": Proxy Authentication Required",
	}
	for _, msg := range decoys {
		if got := classifyNetErrFor(context.Background(), errors.New(msg)); got == ClassProxyAuthError {
			t.Fatalf("error text %q must never classify as proxy-auth without a typed marker", msg)
		}
	}
}

// TestClassifyStatusForRows: the response-status classifier. A status is a
// well-formed HTTP response — it arrives only AFTER the transport boundary
// has done its job (proxy credentials already accepted, or no proxy at all),
// so it can never be promoted to proxy-auth (issue #6): 429 → Upstream429
// (fallback allowed, no health mark); 5xx → Upstream5xx; every other status —
// including EVERY 407 that surfaces as a status, however it traveled — is the
// conservative ClientError (a verdict about the REQUEST).
func TestClassifyStatusForRows(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   Class
	}{
		{name: "429 is upstream_429", status: 429, want: ClassUpstream429},
		{name: "500 is upstream_5xx", status: 500, want: ClassUpstream5xx},
		{name: "502 is upstream_5xx", status: 502, want: ClassUpstream5xx},
		{name: "503 is upstream_5xx", status: 503, want: ClassUpstream5xx},
		{name: "404 is client_error", status: 404, want: ClassClientError},
		{name: "direct 407 is client_error", status: 407, want: ClassClientError},
		{name: "407 through a CONNECT tunnel (origin answered) is client_error", status: 407, want: ClassClientError},
		{name: "407 relayed by a forward proxy (ambiguous owner) is client_error", status: 407, want: ClassClientError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStatusFor(tc.status); got != tc.want {
				t.Fatalf("classifyStatusFor(%d) = %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}
