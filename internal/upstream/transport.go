package upstream

import (
	"crypto/tls"
	"fmt"
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
// Hostnames resolve LOCALLY for socks5 (config-level contract). The proxy
// URL itself is never logged; errors carry the redacted form.
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
		return nil, fmt.Errorf("egress proxy %s: %v", config.RedactProxyURL(p.URL), err)
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
