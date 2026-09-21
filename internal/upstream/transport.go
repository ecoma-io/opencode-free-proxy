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
//     URL userinfo) on the main transport.
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
// Redirects: both http.Clients below (and Client.HTTP from NewClient) leave
// CheckRedirect unset, so http.Client follows up to 10 redirects — faithful
// parity with the JS source, which passes no `redirect:` option in the
// executor path (base.js:144-149 → utils/proxyFetch.js:203-257 forwards the
// options to native fetch, default `redirect: "follow"`; the lone
// redirect:"manual" lives in translator/concerns/image.js:97-98, a different
// boundary). Accepted consequence: a 307/308 re-POSTs the body — including
// `Authorization: Bearer public` and the x-opencode-* headers — to the
// redirect target, exactly as the JS router would (pinned by
// TestRedirectFollowedLikeJSFetch).
func NewClientFor(p *config.Proxy) (*Client, error) {
	c := &Client{Sleep: time.Sleep, Now: time.Now}
	if p == nil {
		tr := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			ForceAttemptHTTP2:     true,
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
		// The http-origin transport: Go proxies it in absolute form.
		viaProxy := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			ForceAttemptHTTP2:     true,
			Proxy:                 http.ProxyURL(u),
		}
		// The https-origin transport: WE own the CONNECT (connect.go); the
		// transport sees a direct https dial. The proxy URL rides the
		// dialer closure, not the transport.
		tunneled := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			ForceAttemptHTTP2:     true,
			DialTLSContext:        newConnectDialer(u, func() *tls.Config { return c.TLSConfig }).DialTLSContext,
		}
		c.HTTP = &http.Client{Transport: viaProxy}
		c.tunneled = &http.Client{Transport: tunneled}
	case config.ProxySOCKS5:
		tr := &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			ForceAttemptHTTP2:     true,
			DialContext:           newSocks5Dialer(u).DialContext,
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
