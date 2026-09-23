package upstream

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"opencode-free-proxy/internal/config"
)

// NewClientFor builds the per-egress Client. A nil proxy is the direct path
// (the historical single-upstream client: bytes over the host's own
// network). Per proxy type:
//
//   - http/https targets through a SOCKS5 proxy: one transport whose
//     DialContext tunnels (socks5.go) — scheme-agnostic, RFC 1929 auth
//     failures surface typed.
//   - HTTP origins through an HTTP(S) proxy: Go's own proxy support
//     (absolute-form request line; Proxy-Authorization derived from the
//     URL userinfo) on the main transport, with the PROXY-side dial owned by
//     this package whenever that proxy endpoint is https (DialProxyTLSContext,
//     connect.go) so a proxy-hop TLS failure is attributable.
//   - HTTPS origins through an HTTP(S) proxy: a SECOND transport whose
//     DialTLSContext is the hand-rolled CONNECT boundary (connect.go), so a
//     proxy refusal is a typed *proxyAuthError instead of stdlib's
//     reason-phrase-only error. Client.attempt picks it by URL scheme.
//
// Hostnames resolve LOCALLY for a socks5:// proxy; a socks5h:// proxy sends
// the hostname in the CONNECT instead (RFC 1928 ATYP=3) and resolves at the
// proxy — the pairing is validated at config load (issue #8). The proxy URL
// itself is never logged; errors carry the redacted form.
//
// Redirects: the returned client sets followRedirects — attempt walks the
// chain itself and re-picks the scheme-appropriate transport per hop, so a
// cross-scheme redirect can never leave the egress (https→http would
// otherwise dial DIRECT off the tunneled transport, http→https would put
// STDLIB on the CONNECT with an untyped 407). See the Client doc in
// client.go for the full rationale and the undici citations; the JS router
// stays on its ProxyAgent for every hop the same way.
func NewClientFor(p *config.Proxy) (*Client, error) {
	c := &Client{Now: time.Now, followRedirects: true}
	if p == nil {
		// Issue #48: the origin handshake speaks the official client's
		// ClientHello (hello.go) via DialTLSContext. Deliberately NO
		// TLSHandshakeTimeout — stdlib never handshakes here;
		// originTLSDialer bounds the handshake with the same 60s budget
		// config.TLSHandshakeTimeout carries — and NO ForceAttemptHTTP2:
		// the official client offers http/1.1 only, and stdlib never
		// upgrades a non-*tls.Conn anyway (dead config lies).
		// Every dialer this file installs is wrapped in recordingDialer
		// (provenance.go): the wrapper marks the dial started/finished and
		// wraps the conn so the first write the transport makes through it is
		// recorded — the fact that decides whether a request is provably
		// unsent. Two separate closures per path, because each dial function
		// must be wrapped exactly ONCE and at its OUTERMOST layer: wrapping
		// the raw dial under the TLS seam would count handshake records as
		// request bytes.
		dialer := &net.Dialer{Timeout: config.DialTimeout}
		tr := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			IdleConnTimeout:       config.IdleConnTimeout,
			DialContext:           recordingDialer(FailurePhaseTargetConnect, dialer.DialContext),
			DialTLSContext:        recordingDialer(FailurePhaseTargetConnect, originTLSDialer(dialer.DialContext, func() *tls.Config { return c.TLSConfig })),
		}
		c.HTTP = &http.Client{Transport: tr}
		return c, nil
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		// Same redaction rule as config.Validate: *url.Error's text embeds the
		// raw url with credentials; only the reason may surface. (Unreachable
		// via OCFP_CONFIG — Validate parses the identical string first — but
		// this constructor must not become a leak for future callers.)
		reason := err
		if inner := errors.Unwrap(err); inner != nil {
			reason = inner
		}
		return nil, fmt.Errorf("egress proxy %s: %v", config.RedactProxyURL(p.URL), reason)
	}
	switch p.Type {
	case config.ProxyHTTP, config.ProxyHTTPS:
		// The http-origin transport: Go proxies it in absolute form. Both
		// dials this transport performs — the plain TCP dial for an http://
		// proxy endpoint, the TCP+TLS dial for an https:// one — are TO THE
		// PROXY, so a failure on either is a proxy-path failure, not a target
		// one: proxy_connect and proxy_tls respectively (issue #61). Owning
		// the TLS dial is also what makes that phase attributable at all:
		// stdlib's own handshake reports a certificate or handshake failure as
		// an opaque error from a dial this package never sees.
		//
		// Deliberately NO TLSHandshakeTimeout and NO ForceAttemptHTTP2 here:
		// with DialTLSContext set stdlib never handshakes this hop (dialConn
		// takes the custom-TLS branch), so a handshake timeout is dead config —
		// dialProxy bounds the handshake with the same config.ConnectTimeout
		// on the conn deadline, and it additionally honours the caller's
		// context deadline, which stdlib's timer ignores. ForceAttemptHTTP2
		// goes with it: proxyTLSConfig offers no ALPN, so no proxy can
		// negotiate h2 and the bundled h2 transport could never engage on this
		// hop (and must not — see proxyTLSConfig on the credentials h2 would
		// drop).
		viaProxy := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			IdleConnTimeout:       config.IdleConnTimeout,
			// The dial this transport performs is TO THE PROXY (stdlib's
			// absolute-form path), so a failure here is a proxy_connect
			// failure, not a target one.
			DialContext:    recordingDialer(FailurePhaseProxyConnect, (&net.Dialer{Timeout: config.DialTimeout}).DialContext),
			DialTLSContext: recordingDialer(FailurePhaseProxyConnect, newConnectDialer(u, func() *tls.Config { return c.TLSConfig }).DialProxyTLSContext),
			Proxy:          http.ProxyURL(u),
		}
		// The https-origin transport: WE own the CONNECT (connect.go); the
		// transport sees a direct https dial. The proxy URL rides the
		// dialer closure, not the transport. Deliberately NO DialContext
		// and NO TLSHandshakeTimeout here: this transport never dials or
		// handshakes itself — DialTLSContext does both (dial to proxy +
		// CONNECT + origin TLS in the official client's hello, hello.go)
		// under the single conn deadline connect.go arms, and an extra
		// dialer on this transport could never fire. No ForceAttemptHTTP2
		// either: the dialer returns a non-*tls.Conn that never negotiates
		// h2 (ALPN parity with the official client — issue #48), and
		// stdlib's h2 dispatch never fires off a non-*tls.Conn regardless.
		tunneled := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			IdleConnTimeout:       config.IdleConnTimeout,
			DialTLSContext:        recordingDialer(FailurePhaseProxyConnect, newConnectDialer(u, func() *tls.Config { return c.TLSConfig }).DialTLSContext),
		}
		c.HTTP = &http.Client{Transport: viaProxy}
		c.tunneled = &http.Client{Transport: tunneled}
	case config.ProxySOCKS5:
		// The socks5 dialer bounds its own TCP dial + SOCKS handshake under
		// one conn deadline (socks5.go). Both schemes ride the SAME dialer:
		// http origins through DialContext, https origins through
		// DialTLSContext, which adds the origin TLS handshake in the
		// official client's hello (hello.go, issue #48) — bounded by
		// originTLSDialer's own conn deadline (the pre-parity gap this
		// branch closed, an https origin whose tunnel came up but whose TLS
		// handshake blackholed, stays closed). No TLSHandshakeTimeout /
		// ForceAttemptHTTP2: stdlib never handshakes or upgrades here.
		socks := newSocks5Dialer(u)
		tr := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			IdleConnTimeout:       config.IdleConnTimeout,
			DialContext:           recordingDialer(FailurePhaseProxyConnect, socks.DialContext),
			DialTLSContext:        recordingDialer(FailurePhaseProxyConnect, originTLSDialer(socks.DialContext, func() *tls.Config { return c.TLSConfig })),
		}
		c.HTTP = &http.Client{Transport: tr}
	default:
		return nil, fmt.Errorf("egress proxy %s: unknown type %q", config.RedactProxyURL(p.URL), p.Type)
	}
	return c, nil
}

// proxyDialAddr is the TCP address the proxy-protocol dialers (connect.go,
// socks5.go) connect to. A URL without an explicit port takes the SCHEME's
// default — http→80 and https→443 like stdlib's own proxy dialing
// (net/http/transport.go schemePort), socks5/socks5h→1080 per RFC 1928 — so
// a portless proxy dials the same endpoint whichever side (http-origin
// absolute-form vs CONNECT tunnel) it serves. An IPv6 literal is bracketed
// exactly once: url.Host keeps the brackets, Hostname() strips them,
// JoinHostPort re-adds them.
func proxyDialAddr(u *url.URL) string {
	if _, _, err := net.SplitHostPort(u.Host); err == nil {
		return u.Host
	}
	port := "1080"
	switch u.Scheme {
	case "http":
		port = "80"
	case "https":
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}
