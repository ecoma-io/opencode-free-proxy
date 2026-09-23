package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"opencode-free-proxy/internal/config"
)

// maxConnectHeaderBytes bounds the CONNECT reply's header read (stdlib
// parity — net/http maxHeaderResponseSize, 10 MiB).
const maxConnectHeaderBytes = 10 << 20

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
// The returned conn is a handshaken *utls.UConn speaking the official
// client's origin hello (hello.go, issue #48); the transport speaks HTTP/1.1
// over it — ALPN parity with the official client, never h2. The whole
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
	// is itself https; nil means the tlsConfig seam below supplies the trust
	// anchors (see proxyTLSConfig), and system roots if that is nil too.
	// Tests inject skip-verify for self-signed fixtures.
	proxyTLS *tls.Config
}

func newConnectDialer(proxy *url.URL, tlsConfig func() *tls.Config) *connectDialer {
	return &connectDialer{proxy: proxy, dialer: net.Dialer{Timeout: config.ConnectTimeout}, tlsConfig: tlsConfig}
}

// dialProxy connects the TCP hop to the proxy endpoint and, when that endpoint
// is itself https, the TLS hop on top of it. It is the shared prologue of the
// two proxy transport paths and the reason both report the same phases for the
// same failures:
//
//   - the CONNECT tunnel (DialTLSContext), used for https origins;
//   - the absolute-form path (DialProxyTLSContext), used for http origins,
//     where net/http speaks the proxied request over the conn this returns.
//
// It arms one conn deadline that covers everything it and its callers do with
// the conn; each caller clears it once its own protocol work is done.
func (d *connectDialer) dialProxy(ctx context.Context) (net.Conn, error) {
	// Phase bookkeeping (provenance.go). This whole function runs BEFORE
	// net/http owns a connection, so every failure below is provably pre-
	// transmission; the trace only has to say WHICH step failed. The calls are
	// no-ops when the attempt carries no trace (a direct caller, a test).
	t := traceOf(ctx)
	t.enter(FailurePhaseProxyConnect)
	conn, err := d.dialer.DialContext(ctx, "tcp", proxyDialAddr(d.proxy))
	if err != nil {
		return nil, fmt.Errorf("proxy %s: dial: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	// One deadline across the rest of the dial: the response-header timeout
	// never starts — Client.Do has not returned — so without it a proxy that
	// accepts and stalls would strand the dial past any caller deadline. The
	// deadline is inherited from the dial context (net/http keeps Deadline
	// values across context.WithoutCancel, which is what the transport hands a
	// dialer) or falls back to config.ConnectTimeout.
	_ = conn.SetDeadline(deadlineFrom(ctx, config.ConnectTimeout))
	if d.proxy.Scheme != "https" {
		return conn, nil
	}
	// TLS to the proxy endpoint. The protocol spoken over this conn (CONNECT
	// for a tunnel, the request itself in absolute form) is HTTP/1.1 in both
	// cases, which is why proxyTLSConfig offers no ALPN — see its doc.
	t.enter(FailurePhaseProxyTLS)
	tlsConn := tls.Client(conn, d.proxyTLSConfig())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s: tls: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	return tlsConn, nil
}

// DialProxyTLSContext is the DialTLSContext of the ABSOLUTE-FORM proxy
// transport (an http origin behind an http(s) proxy). net/http calls it with
// the proxy's own address — this transport's connectMethod is the proxy — and
// then speaks the origin request over the returned conn. Reaching the proxy is
// all this dialer does: the CONNECT boundary (DialTLSContext) is for https
// origins, and an http origin must not be tunneled.
//
// Both arguments are deliberately ignored: the dialer owns the proxy endpoint
// (proxyDialAddr), exactly like the CONNECT boundary, so the address net/http
// computed cannot disagree with it.
//
// The returned conn is a handshaken *tls.Conn — never a uTLS wrapper — because
// net/http reads ConnectionState off it to decide whether the conn is usable
// (transport.go dialConn) and an opaque wrapper would silently lose that.
func (d *connectDialer) DialProxyTLSContext(ctx context.Context, _, _ string) (net.Conn, error) {
	conn, err := d.dialProxy(ctx)
	if err != nil {
		return nil, err
	}
	// The dial is over: from here the conn belongs to net/http, whose own
	// bounds (ResponseHeaderTimeout) govern the request and the response. A
	// dial deadline left armed would cut the request short at the dial budget.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// DialTLSContext establishes the proxy tunnel and returns the ORIGIN TLS
// conn. network is always "tcp"; addr is the origin "host:port" (the
// transport passes cm.targetAddr because this transport's Proxy is nil).
func (d *connectDialer) DialTLSContext(ctx context.Context, _, addr string) (net.Conn, error) {
	conn, err := d.dialProxy(ctx)
	if err != nil {
		return nil, err
	}
	// context.AfterFunc, not a select-watcher goroutine: a watcher that
	// selects between ctx.Done() and a handshake-done channel flips a coin
	// when BOTH become ready at once — it may Close() the just-returned live
	// tunnel. AfterFunc arms ONLY on ctx cancellation, and stop() disarms it
	// deterministically on the success path, so a completed handshake can
	// never race its own teardown. There is no leak either way: stop() runs
	// on every return below.
	//
	// It is a backstop, not the primary bound: the transport hands this dialer
	// a context detached from the request (getCtxForDial uses
	// context.WithoutCancel and keeps only Deadlines), so cancellation reaches
	// here via the derived deadline, and the conn deadline armed above is what
	// stops a stalled CONNECT or handshake.
	// Capture the conn AT SPAWN: conn is reassigned to the TLS wrapper below,
	// and a closure reading the variable would race that write (go memory
	// model). Closing the captured conn is correct on every path — every
	// later wrapper wraps exactly this conn.
	dialConn := conn
	stopWatcher := context.AfterFunc(ctx, func() { _ = dialConn.Close() })
	defer stopWatcher()

	if err := d.connect(ctx, conn, addr); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Origin TLS through the tunnel, in the official client's hello
	// (hello.go, issue #48): ServerName from the dial addr (the transport
	// cannot know it — from its view this is a direct connection), ALPN
	// http/1.1 only. handshakeOrigin closes the conn itself on failure, so
	// unlike the hops above there is no explicit Close on this error path.
	var base *tls.Config
	if d.tlsConfig != nil {
		base = d.tlsConfig()
	}
	tlsConn, err := handshakeOrigin(ctx, conn, originTLSConfig(base, addr))
	if err != nil {
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
	t := traceOf(ctx)
	t.enter(FailurePhaseConnectWrite)
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("proxy %s: connect write: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	// Safe to discard the buffered reader after the reply: a compliant proxy
	// sends nothing past the CONNECT response until the client speaks (the
	// same reasoning net/http cites for its own CONNECT reader). The read is
	// BOUNDED like stdlib's (net/http/transport.go maxHeaderResponseSize): a
	// hostile proxy streaming an unbounded header block must exhaust this
	// limit into an error, not grow memory until the conn deadline. Not an
	// open-sse constant — Go transport-boundary hygiene, mirrored from
	// stdlib, which is why it lives here and not in internal/config.
	t.enter(FailurePhaseConnectRead)
	br := bufio.NewReader(io.LimitReader(conn, maxConnectHeaderBytes))
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("proxy %s: connect read: %w", config.RedactProxyURL(d.proxy.String()), err)
	}
	// The CONNECT reply's body is deliberately NEVER closed or drained, on
	// the 200 path or the refusal paths — GOROOT net/http dialConn does the
	// same: it reads the reply with ReadResponse (transport.go:1908-1912,
	// "Okay to use and discard buffered reader here, because TLS server will
	// not speak until spoken to") and on a non-200 closes the CONN, never
	// the body (:1930-1937). Closing would be actively harmful: a
	// ReadResponse body's Close() fully DRAINS the declared body looking
	// for trailers (transfer.go body.Close default branch, io.Copy of the
	// remaining body), so a proxy answering "200 Content-Length: N>0" (or
	// chunked) and then tunneling would pin the dial in that drain until
	// the conn deadline — burning the whole ConnectTimeout budget and then
	// failing the origin TLS handshake against the already-expired
	// deadline. No leak results from skipping Close: the body owns no
	// goroutine or fd, and the conn's lifetime is the caller's (closed on
	// every error return in DialTLSContext, handed to TLS on success).
	if resp.StatusCode == http.StatusProxyAuthRequired {
		// The phase names the VERDICT (the proxy refused our credentials),
		// not the step: both this and the SOCKS5 RFC 1929 rejection report
		// proxy_auth so one failure mode reads identically on either proxy
		// type, while class/origin stay the deciding fields.
		t.enter(FailurePhaseProxyAuth)
		return &proxyAuthError{msg: fmt.Sprintf("proxy %s: CONNECT refused with 407 (authentication required)", config.RedactProxyURL(d.proxy.String()))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy %s: CONNECT refused with status %d", config.RedactProxyURL(d.proxy.String()), resp.StatusCode)
	}
	return nil
}

// tlsMinVersion is the package TLS floor, stated on every tls.Config this
// dialer builds rather than inherited from the stdlib default (code scanning
// go/missing-ssl-minversion, issue #18). Parity-neutral: Go 1.22+'s default
// minimum is already 1.2, and the ported open-sse runtime is Node, whose TLS
// floor has been 1.2 since Node 12.
const tlsMinVersion = tls.VersionTLS12

// floorTLS raises cfg's minimum to the package floor. It never lowers an
// explicit higher floor, and an unset MinVersion (0 — "stdlib default") is
// treated as needing the floor stated, not as an intentional downgrade.
func floorTLS(cfg *tls.Config) {
	if cfg.MinVersion < tlsMinVersion {
		cfg.MinVersion = tlsMinVersion
	}
}

// proxyTLSConfig builds the PROXY-hop TLS settings for an https proxy
// endpoint: ServerName from the proxy host unless the injected config names
// one; the TLS floor applies to the literal and to every injected clone.
//
// Trust anchors come from the injected config — the dialer's own proxyTLS
// first, then the client's TLSConfig (the same seam every ORIGIN handshake
// takes its roots from, so one injected pool can cover a fixture's proxy and
// its origin) — and from the system roots when both are nil. Production leaves
// both nil: nothing in internal/config reaches this seam.
//
// Deliberately NO NextProtos, on either hop that ends at the proxy endpoint:
//
//   - the CONNECT tunnel speaks HTTP/1.1 CONNECT literally (connect.go), so an
//     h2-negotiated conn would carry bytes the proxy never parses as CONNECT;
//   - the absolute-form hop's proxy credentials ride stdlib's h1-only hook —
//     dialConn installs `pconn.mutateHeaderFunc` to add the URL userinfo's
//     Proxy-Authorization (net/http/transport.go:1850-1861) and only the
//     HTTP/1.1 write path applies it (:2829). The h2 transport never does, so
//     an h2 conn to the proxy would silently DROP the credentials of exactly
//     the egresses configured with them.
//
// An empty ALPN list offers no protocol, so the proxy answers HTTP/1.1 in both
// cases — the one protocol this file implements against a proxy.
func (d *connectDialer) proxyTLSConfig() *tls.Config {
	base := d.proxyTLS
	if base == nil && d.tlsConfig != nil {
		base = d.tlsConfig()
	}
	cfg := &tls.Config{ServerName: d.proxy.Hostname()}
	if base != nil {
		cfg = base.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = d.proxy.Hostname()
		}
	}
	cfg.NextProtos = nil // never negotiable — see the doc above
	floorTLS(cfg)
	return cfg
}

// deadlineFrom arms a conn deadline for a dial bounded by def: now+def when
// the context carries no deadline, the context's own deadline when it is
// earlier (the caller's bound wins), or def when the context deadline is
// later than def. def <= 0 never clears anything — with a deadline-less
// context it would arm now+def, an already-expired deadline; no caller does
// that today (the only caller passes config.ConnectTimeout). Clearing after
// the handshake is the caller's own explicit SetDeadline(time.Time{}).
func deadlineFrom(ctx context.Context, def time.Duration) time.Time {
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(def)
	}
	if def <= 0 || time.Until(dl) <= def {
		return dl
	}
	return time.Now().Add(def)
}
