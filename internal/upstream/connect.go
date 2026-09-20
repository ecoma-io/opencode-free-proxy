package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"opencode-free-proxy/internal/config"
)

// connectDialer owns the https-target CONNECT boundary for http/https proxy
// egresses (issue #6). Where Go's built-in proxy support answers a refused
// CONNECT with an error carrying only the stripped reason phrase —
// transport.go `strings.Cut(resp.Status, " ")` → `errors.New(text)` — this
// dialer speaks the proxy protocol itself and turns a refusal into a TYPED
// *proxyAuthError. That makes proxy-auth classification evidence-based: the
// 407 came from the proxy because THIS code read the proxy's status line.
//
// It is wired as http.Transport.DialTLSContext on a transport whose Proxy is
// nil, so (net/http/transport.go dialConn) the transport dials nothing
// itself and hands the ORIGIN "host:port" to DialTLSContext, then speaks the
// request over whatever conn comes back. The dialer:
//
//	TCP → proxy (net.Dialer, ConnectTimeout)
//	[proxy scheme https: TLS to the proxy, ServerName = proxy host]
//	CONNECT origin:port HTTP/1.1 [Proxy-Authorization from userinfo]
//	407            → *proxyAuthError            (ClassProxyAuthError)
//	other non-200  → plain error                (ClassConnectionError)
//	200            → TLS to the ORIGIN over the tunnel
//
// The returned conn is a handshaken *tls.Conn so the transport reads
// ConnectionState for HTTP/2 negotiation (ForceAttemptHTTP2). The whole
// dial+CONNECT+TLS sequence is bounded by one conn deadline — mirroring
// socks5.go, whose handshake has exactly the same stranding hazard.
//
// Credentials appear nowhere in any error message; errors name the proxy in
// its redacted form only.
type connectDialer struct {
	proxy  *url.URL
	dialer net.Dialer
	// tlsConfig returns the ORIGIN TLS settings (tests inject a root pool);
	// read per dial so a test can set Client.TLSConfig after NewClientFor.
	tlsConfig func() *tls.Config
	// proxyTLS overrides the PROXY-hop TLS settings when the proxy endpoint
	// is itself https; nil (production) means system roots with ServerName
	// from the proxy host. Tests inject skip-verify for self-signed fixtures.
	proxyTLS *tls.Config
}

func newConnectDialer(proxy *url.URL, tlsConfig func() *tls.Config) *connectDialer {
	return &connectDialer{proxy: proxy, dialer: net.Dialer{Timeout: config.ConnectTimeout}, tlsConfig: tlsConfig}
}

// DialTLSContext establishes the proxy tunnel and returns the ORIGIN TLS
// conn. network is always "tcp"; addr is the origin "host:port" (the
// transport passes cm.targetAddr because this transport's Proxy is nil).
func (d *connectDialer) DialTLSContext(ctx context.Context, _, addr string) (net.Conn, error) {
	proxyAddr := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		proxyAddr = net.JoinHostPort(proxyAddr, "1080")
	}
	conn, err := d.dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: dial: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	// One deadline across CONNECT + origin TLS: the HTTPS transport's
	// ResponseHeaderTimeout never starts — Client.Do has not returned — so
	// without it a proxy that accepts and stalls would strand the dial past
	// any caller deadline. The watcher aborts on request-ctx cancellation
	// the same way socks5.go's does (the transport detaches dial context
	// cancellation from the request, but a canceled CLIENT ctx must still
	// tear the tunnel down; Deadline values survive WithoutCancel).
	_ = conn.SetDeadline(deadlineFrom(ctx, config.ConnectTimeout))
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()

	if d.proxy.Scheme == "https" {
		// TLS to the proxy first (an https proxy endpoint); the CONNECT then
		// travels inside that TLS layer. No h2 on this hop — the proxy speaks
		// HTTP/1.1 CONNECT regardless of what the tunnel carries.
		cfg := &tls.Config{ServerName: d.proxy.Hostname()}
		if d.proxyTLS != nil {
			cfg = d.proxyTLS.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName = d.proxy.Hostname()
			}
		}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy %s: tls: %w", config.RedactProxyURL(d.proxy.String()), err)
		}
		conn = tlsConn
	}

	if err := d.connect(ctx, conn, addr); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Origin TLS through the tunnel. ServerName comes from the dial addr
	// (the transport cannot know it — from its view this is a direct
	// connection); h2 is offered so the negotiated protocol flows through
	// ConnectionState like a direct https dial.
	cfg := d.originTLSConfig(addr)
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy tunnel: origin tls %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{}) // caller owns the conn from here
	return tlsConn, nil
}

// connect sends CONNECT for addr and validates the proxy's reply. Only the
// proxy's OWN status line is interpreted here — the origin is unreachable
// until this returns, so a 407 read off this response is proxy-owned by
// construction (the wire proof behind ClassProxyAuthError).
func (d *connectDialer) connect(ctx context.Context, conn net.Conn, addr string) error {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if d.proxy.User != nil {
		pass, _ := d.proxy.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(d.proxy.User.Username() + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("proxy %s: connect write: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	// Safe to discard the buffered reader after the reply: a compliant proxy
	// sends nothing past the CONNECT response until the client speaks (the
	// same reasoning net/http cites for its own CONNECT reader).
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("proxy %s: connect read: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return &proxyAuthError{msg: fmt.Sprintf("proxy %s: CONNECT refused with 407 (authentication required)", config.RedactProxyURL(d.proxy.String()))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy %s: CONNECT refused with status %d", config.RedactProxyURL(d.proxy.String()), resp.StatusCode)
	}
	return nil
}

// originTLSConfig clones the client TLS settings for the origin hop,
// pinning ServerName to the dialed host and offering h2 + http/1.1.
func (d *connectDialer) originTLSConfig(addr string) *tls.Config {
	cfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	if d.tlsConfig != nil {
		if base := d.tlsConfig(); base != nil {
			cfg = base.Clone()
			if cfg.ServerName == "" {
				if host, _, err := net.SplitHostPort(addr); err == nil {
					cfg.ServerName = host
				}
			}
			if len(cfg.NextProtos) == 0 {
				cfg.NextProtos = []string{"h2", "http/1.1"}
			}
		}
	} else if host, _, err := net.SplitHostPort(addr); err == nil {
		cfg.ServerName = host
	}
	return cfg
}

// deadlineFrom returns now+def when the context carries no deadline, else
// the earlier of the two; def <= 0 means "deadline already passed / none"
// and clears (zero Time).
func deadlineFrom(ctx context.Context, def time.Duration) time.Time {
	dl, ok := ctx.Deadline()
	if !ok || (def > 0 && time.Until(dl) > def) {
		return time.Now().Add(def)
	}
	if !ok {
		return time.Time{} // def <= 0 and no ctx deadline: clear
	}
	return dl
}
