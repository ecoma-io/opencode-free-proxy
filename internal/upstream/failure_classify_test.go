package upstream

import (
	"context"
	"errors"

	"testing"

	"opencode-free-proxy/internal/config"
)

// netTimeoutErr is a net.Error whose Timeout() is true — what the transport
// produces when a dial exceeds its deadline.
type netTimeoutErr struct{ msg string }

func (e *netTimeoutErr) Error() string   { return e.msg }
func (e *netTimeoutErr) Timeout() bool   { return true }
func (e *netTimeoutErr) Temporary() bool { return false }

// TestClassifyNetErrForRows: the context-aware error classifier. The rows
// pin the P1 contract: a canceled context always wins; the SOCKS5 RFC 1929
// typed marker wins unconditionally; the "Proxy Authentication Required"
// phrase is the primary HTTP 407 marker (stdlib strips the code — transport.go
// errors.New(text)), bare "407" is a secondary probe for non-stdlib shapes;
// and both text probes fire ONLY when a proxy was configured — an IPv6
// dial-error octet must not classify as proxy-auth.
func TestClassifyNetErrForRows(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	timeoutErr := &netTimeoutErr{msg: "dial tcp 10.0.0.1:443: i/o timeout"}

	cases := []struct {
		name     string
		ctx      context.Context
		err      error
		hasProxy bool
		want     Class
	}{
		{
			name: "canceled context beats proxy-auth marker",
			ctx:  canceled, err: &proxyAuthError{msg: "407 Proxy Authentication Required"},
			hasProxy: true, want: ClassContextCanceled,
		},
		{
			name: "typed SOCKS5 auth failure is always proxy-auth",
			ctx:  context.Background(), err: &proxyAuthError{msg: "socks5: authentication failed"},
			hasProxy: false, want: ClassProxyAuthError,
		},
		{
			name: "CONNECT-407 phrase with proxy is proxy-auth",
			ctx:  context.Background(), err: errors.New("Proxy Authentication Required"),
			hasProxy: true, want: ClassProxyAuthError,
		},
		{
			name: "bare 407 with proxy is proxy-auth",
			ctx:  context.Background(), err: errors.New("407"),
			hasProxy: true, want: ClassProxyAuthError,
		},
		{
			name: "CONNECT-407 phrase without proxy is a connection error",
			ctx:  context.Background(), err: errors.New("Proxy Authentication Required"),
			hasProxy: false, want: ClassConnectionError,
		},
		{
			name: "bare 407 without proxy is a connection error (IPv6 octet guard)",
			ctx:  context.Background(), err: errors.New("dial tcp [2001:407::9]:443: connect: connection refused"),
			hasProxy: false, want: ClassConnectionError,
		},
		{
			name: "net.Error timeout is a timeout",
			ctx:  context.Background(), err: timeoutErr,
			hasProxy: true, want: ClassTimeout,
		},
		{
			name: "plain dial refusal is a connection error",
			ctx:  context.Background(), err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
			hasProxy: true, want: ClassConnectionError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNetErrFor(tc.ctx, tc.err, tc.hasProxy); got != tc.want {
				t.Fatalf("classifyNetErrFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyStatusFor407Matrix: the response-status classifier. 429 and
// 5xx map regardless of proxy; a 407's owner decides: no proxy → the origin
// answered (ClientError); https target → CONNECT-tunneled, the 407 can only
// come from the origin (ClientError); http target with credentials we sent →
// the proxy gated us (ProxyAuth); http without credentials → ambiguous,
// conservative (ClientError); socks5 → always the origin's verdict (RFC 1929
// auth happens once at tunnel setup, so no wire 407 is ever the proxy's).
func TestClassifyStatusFor407Matrix(t *testing.T) {
	proxy := func(pType config.ProxyType, url string) *config.Proxy {
		return &config.Proxy{Type: pType, URL: url}
	}
	withCreds := proxy(config.ProxyHTTP, "http://user:pass@127.0.0.1:8080")
	noCreds := proxy(config.ProxyHTTP, "http://127.0.0.1:8080")
	socksWithCreds := proxy(config.ProxySOCKS5, "socks5://user:pass@127.0.0.1:1080")
	socksNoCreds := proxy(config.ProxySOCKS5, "socks5://127.0.0.1:1080")

	cases := []struct {
		name   string
		status int
		prx    *config.Proxy
		target string
		want   Class
	}{
		{name: "429 bypasses proxy logic", status: 429, prx: noCreds, target: "https://x", want: ClassUpstream429},
		{name: "500 is upstream 5xx", status: 500, prx: noCreds, target: "https://x", want: ClassUpstream5xx},
		{name: "407 without proxy is client error", status: 407, prx: nil, target: "https://x", want: ClassClientError},
		{name: "407 through proxy with https target is client error (origin answered)", status: 407, prx: noCreds, target: "https://x", want: ClassClientError},
		{name: "407 through proxy with http target and creds is proxy-auth", status: 407, prx: withCreds, target: "http://x", want: ClassProxyAuthError},
		{name: "407 through proxy with http target and no creds is client error", status: 407, prx: noCreds, target: "http://x", want: ClassClientError},
		{name: "404 is client error", status: 404, prx: nil, target: "https://x", want: ClassClientError},
		{name: "407 through socks5 with creds and http target is client error (tunnel origin answered)", status: 407, prx: socksWithCreds, target: "http://x", want: ClassClientError},
		{name: "407 through socks5 without creds is client error", status: 407, prx: socksNoCreds, target: "http://x", want: ClassClientError},
		{name: "407 through socks5 with https target is client error", status: 407, prx: socksWithCreds, target: "https://x", want: ClassClientError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStatusFor(tc.status, tc.prx, tc.target); got != tc.want {
				t.Fatalf("classifyStatusFor(%d) = %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}
