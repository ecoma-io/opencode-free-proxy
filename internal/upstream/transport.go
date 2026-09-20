package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"opencode-free-proxy/internal/config"
)

// NewClientFor builds the per-egress Client. A nil proxy is the direct path
// (the historical single-upstream client: bytes over the host's own
// network). http/https route through Go's proxy transport — net/http derives
// Proxy-Authorization from the proxy URL userinfo, so credentials never
// leave the transport. socks5 uses the hand-rolled dialer (socks5.go) so
// hostnames resolve LOCALLY, the config-level contract. The proxy URL itself
// is never logged; errors carry the redacted form.
func NewClientFor(p *config.Proxy) (*Client, error) {
	tr := &http.Transport{
		ResponseHeaderTimeout: config.ConnectTimeout,
		ForceAttemptHTTP2:     true,
	}
	if p != nil {
		u, err := url.Parse(p.URL)
		if err != nil {
			return nil, fmt.Errorf("egress proxy %s: %v", config.RedactProxyURL(p.URL), err)
		}
		switch p.Type {
		case config.ProxyHTTP, config.ProxyHTTPS:
			// http.ProxyURL derives Proxy-Authorization from the userinfo.
			tr.Proxy = http.ProxyURL(u)
		case config.ProxySOCKS5:
			tr.DialContext = newSocks5Dialer(u).DialContext
		}
		return &Client{
			HTTP:  &http.Client{Transport: tr},
			Sleep: time.Sleep,
			Now:   time.Now,
			Proxy: p,
		}, nil
	}
	// The direct transport: no proxy, no credentials.
	return &Client{
		HTTP:  &http.Client{Transport: tr},
		Sleep: time.Sleep,
		Now:   time.Now,
	}, nil
}
