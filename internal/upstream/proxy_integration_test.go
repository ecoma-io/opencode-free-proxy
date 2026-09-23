package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// noSleepClient unpacks NewClientFor's two-value result inline, panicking on
// a transport-build error (a test fixture that cannot build its transport has
// nothing to assert). It used to stub the retry sleep so the retry matrix ran
// instantly; one Client.Do is now one logical call with nothing to wait for,
// so there is no seam left to stub and the helper is just the compact wrapper
// the proxy tests below are written against.
func noSleepClient(c *Client, err error) *Client {
	if err != nil {
		panic(err)
	}
	return c
}

// originRootPool builds a root pool trusting the httptest fixture origin, so
// the tunneled/direct origin TLS verifies against it (the cert carries the
// 127.0.0.1 SAN the server listens on).
func originRootPool(t *testing.T, origin *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	return pool
}

// connect407Proxy is a fake HTTP CONNECT proxy that counts CONNECTs and
// always answers 407.
func connect407Proxy(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var connects atomic.Int64
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects.Add(1)
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-proxy"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(pxy.Close)
	return pxy, &connects
}

// tunnelingProxy is a fake CONNECT proxy that establishes real tunnels to
// r.Host — the wire the origin-407-behind-proxy case travels.
func tunnelingProxy(t *testing.T) *httptest.Server {
	t.Helper()
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(up, client) }()
		go func() { _, _ = io.Copy(client, up) }()
	}))
	t.Cleanup(pxy.Close)
	return pxy
}

// TestHTTPConnect407IsProxyAuth: an HTTP CONNECT proxy answers the CONNECT
// with 407 — the ONE case where a 407 is provably proxy-owned. Our
// hand-rolled CONNECT boundary (connect.go) reads the proxy's status line
// itself and raises the typed *proxyAuthError, so the class is proxy-auth by
// wire evidence, not text inference. The client-facing envelope stays 502
// (network errors map to the 502 rule in the JS error matrix); the CLASS is
// what drives retry/fallback.
func TestHTTPConnect407IsProxyAuth(t *testing.T) {
	pxy, connects := connect407Proxy(t)

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL}))
	resp, uerr, failure := c.DoClassified(context.Background(), "https://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want the 502 envelope for a transport-level refusal", uerr)
	}
	if !strings.Contains(uerr.Message, "CONNECT refused with 407") {
		t.Fatalf("error text = %q, want the typed boundary message", uerr.Message)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECTs = %d, want exactly 1 (proxy-auth never rides the 502 retry matrix)", got)
	}
}

// TestHTTPSProxyConnect407IsProxyAuth: the same refusal through an HTTPS
// proxy endpoint (TLS to the proxy BEFORE the CONNECT). The typed marker must
// survive the extra hop — this exercises connectDialer's https branch.
func TestHTTPSProxyConnect407IsProxyAuth(t *testing.T) {
	var connects atomic.Int64
	pxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer pxy.Close()

	u, err := url.Parse(pxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := newConnectDialer(u, func() *tls.Config { return nil })
	d.proxyTLS = &tls.Config{InsecureSkipVerify: true} // fixture: self-signed proxy cert
	_, derr := d.DialTLSContext(context.Background(), "tcp", "origin.invalid:443")
	var pae *proxyAuthError
	if !errors.As(derr, &pae) {
		t.Fatalf("err = %v (%T), want *proxyAuthError", derr, derr)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECTs = %d, want 1", got)
	}
}

// TestHTTPSOrigin407IsClientError: with an https target the proxy
// CONNECT-tunnels; a 407 AFTER the tunnel can only come from the ORIGIN — the
// proxy already accepted our credentials at CONNECT time. It must NOT
// classify as proxy-auth; it surfaces to the client as the 407 it is.
func TestHTTPSOrigin407IsClientError(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"origin auth"}}`)
	}))
	defer origin.Close()

	pxy := tunnelingProxy(t)

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL}))
	c.TLSConfig = &tls.Config{RootCAs: originRootPool(t, origin)}
	resp, uerr, failure := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassClientError {
		t.Fatalf("class = %s, want ClassClientError (origin answered inside the tunnel)", class)
	}
	if uerr == nil || uerr.Status != http.StatusProxyAuthRequired {
		t.Fatalf("uerr = %+v, want status 407 surfaced to the client", uerr)
	}
}

// TestDirectOrigin407IsClientError: no proxy in the path at all — a 407 is
// the origin's own verdict about the request. Plain client_error.
func TestDirectOrigin407IsClientError(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer origin.Close()

	c := NewClient()
	resp, uerr, failure := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassClientError {
		t.Fatalf("class = %s, want ClassClientError", class)
	}
	if uerr == nil || uerr.Status != http.StatusProxyAuthRequired {
		t.Fatalf("uerr = %+v, want status 407", uerr)
	}
}

// TestHTTPOrigin407ThroughForwardProxyIsNotBlindlyProxyAuth: a plain-http
// target through a forward proxy is the AMBIGUOUS case — the proxy relays
// requests and replies byte-for-byte, so a 407 may be the proxy gating the
// request OR the origin's own answer, and no header is reliably proxy-authored
// (issue #6 rejects content sniffing). Even though we SENT Proxy-Authorization
// (asserted below), the conservative class is client_error: no fallback (the
// next egress would repeat the rejection), no health mark (a credential
// problem is not an outage). Only the transport boundary — a CONNECT refusal,
// an RFC 1929 rejection — may claim proxy_auth_error.
func TestHTTPOrigin407ThroughForwardProxyIsNotBlindlyProxyAuth(t *testing.T) {
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Errorf("forward proxy expected an absolute-URI request, got %s", r.URL)
		}
		if got := r.Header.Get("Proxy-Authorization"); got == "" {
			t.Error("expected Proxy-Authorization to be derived from the proxy URL userinfo")
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer pxy.Close()

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://user:pass@" + strings.TrimPrefix(pxy.URL, "http://")}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassClientError {
		t.Fatalf("class = %s, want ClassClientError (ambiguous 407 must not be promoted by credentials-sent)",
			class)
	}
	if uerr == nil || uerr.Status != http.StatusProxyAuthRequired {
		t.Fatalf("uerr = %+v, want status 407", uerr)
	}
}

// TestProxyAuthFailureDoesNotUseGeneric502Retry: egress a sits behind a
// CONNECT-407 proxy, egress b is a healthy https origin. The proxy-auth
// refusal must cost a EXACTLY ONE dial — the generic 502 matrix (1 initial +
// 3 retries = 4 POSTs) must NOT run, because the proxy refuses the same
// credentials identically every time (issue #6 §5: A=1 call, never A=4).
func TestProxyAuthFailureDoesNotUseGeneric502Retry(t *testing.T) {
	f := newProxyAuthFixture(t)

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.origin.URL+"/zen/v1/chat/completions",
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan, policy(true, 3))
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if uerr != nil || resp == nil {
		t.Fatalf("uerr=%v resp=%v, want fallback to b serving", uerr, resp)
	}
	if id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s, want b/2/success", id, attempts, class)
	}
	if got := f.connects.Load(); got != 1 {
		t.Fatalf("CONNECTs on a = %d, want 1 (never the 502 matrix's 4)", got)
	}
	// The refusal is a true egress failure class: it must have marked health.
	if f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a's proxy-auth refusal must mark it unhealthy (threshold 1)")
	}
}

// TestProxyAuthFailureFallsBackImmediately: the executor half of the §5
// contract — the typed refusal is FallbackAllowed, so the very next attempt
// serves from b; no further dials against a, no budget consumed.
func TestProxyAuthFailureFallsBackImmediately(t *testing.T) {
	f := newProxyAuthFixture(t)

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.origin.URL+"/zen/v1/chat/completions",
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan, policy(true, 3))
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if uerr != nil || resp == nil || id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s uerr=%v, want immediate fallback to b", id, attempts, class, uerr)
	}
	if got := f.connects.Load(); got != 1 {
		t.Fatalf("CONNECTs on a = %d, want 1 (the refusal ends a's turn)", got)
	}
	if !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("b's success must keep it healthy")
	}
}

// proxyAuthFixture: egress a dials through a CONNECT-407 fake proxy, egress b
// is a healthy https origin reached directly.
type proxyAuthFixture struct {
	exec     *Executor
	health   *health.Registry
	origin   *httptest.Server
	connects *atomic.Int64
	plan     routing.RoutePlan
}

func newProxyAuthFixture(t *testing.T) *proxyAuthFixture {
	t.Helper()
	pxy, connects := connect407Proxy(t)
	origin := httpsOKOrigin(t)
	pool := originRootPool(t, origin)

	a, err := NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	// b is DIRECT; since issue #48 the Client.TLSConfig seam feeds EVERY
	// origin handshake (direct, socks5, tunneled CONNECT alike), so the
	// root pool rides the same seam the router uses in production.
	b := NewClient()
	b.TLSConfig = &tls.Config{RootCAs: pool}
	clients := map[string]*Client{"a": a, "b": b}

	rt := fixtureRuntime(map[string]int{"a": 200, "b": 200})
	healthReg := health.New()
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		c, ok := clients[e.ID]
		return c, ok
	}, healthReg, NewLimiter())

	egA, _ := rt.Egress("a")
	egB, _ := rt.Egress("b")
	plan := routing.RoutePlan{RouteID: "r", Strategy: config.StrategyRoundRobin, Attempts: []string{"a", "b"}}
	plan.Egresses = []*config.Egress{egA, egB}

	return &proxyAuthFixture{exec: exec, health: healthReg, origin: origin, connects: connects, plan: plan}
}

// httpsOKOrigin is a 200-speaking https fixture origin.
func httpsOKOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	return origin
}

// ---- SOCKS5 (RFC 1928 + RFC 1929) ----

// socks5Fake is a minimal in-test SOCKS5 server whose greet/connect replies
// and auth verdict are scripted.
type socks5Fake struct {
	ln      net.Listener
	url     string
	method  byte // greet reply: 0x00 no-auth, 0x02 auth, 0xff none
	authRep byte // RFC 1929 status reply when auth was negotiated
	rep     byte // CONNECT reply (0x00 success, 0x02 not-allowed, ...)
	// gotConnect records the CONNECT request exactly as it hit the wire
	// (atyp + address bytes + port) — the local-DNS/atyp assertions read it.
	gotConnect atomic.Pointer[socks5ConnectRecord]
	// mu guards the resolve map below: the test goroutine writes it before
	// dialing, handler goroutines read it.
	mu sync.Mutex
	// resolve maps ATYP=3 hostnames to dialable "host:port" — the fake's
	// stand-in for proxy-side DNS (under socks5h the NAME resolves at the
	// proxy, not in the test process). A miss dials the name itself.
	resolve map[string]string
}

// socks5ConnectRecord is the recorded CONNECT wire shape.
type socks5ConnectRecord struct {
	atyp  byte
	addrs []byte
	port  int
}

func newSocks5Fake(t *testing.T, method, authRep, rep byte) *socks5Fake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &socks5Fake{ln: ln, url: "socks5://" + ln.Addr().String(), method: method, authRep: authRep, rep: rep, resolve: map[string]string{}}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

// resolveAtProxy teaches the fake where an ATYP=3 hostname should dial —
// its stand-in for proxy-side resolution. Call before the first dial.
func (f *socks5Fake) resolveAtProxy(name, addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolve[name] = addr
}

func (f *socks5Fake) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func() { _ = f.handle(conn) }()
	}
}

func (f *socks5Fake) handle(conn net.Conn) error {
	defer func() { _ = conn.Close() }()

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return errVersion
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{0x05, f.method}); err != nil {
		return err
	}
	if f.method == 0x02 {
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil {
			return err
		}
		user := make([]byte, auth[1])
		if _, err := io.ReadFull(conn, user); err != nil {
			return err
		}
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		pass := make([]byte, l[0])
		if _, err := io.ReadFull(conn, pass); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{0x01, f.authRep}); err != nil {
			return err
		}
		if f.authRep != 0x00 {
			return nil // the client aborts on a failed auth
		}
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return err
	}
	if req[0] != 0x05 || req[1] != 0x01 {
		return errBadCommand
	}
	var addrLen int
	switch req[3] {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		d := make([]byte, 1)
		if _, err := io.ReadFull(conn, d); err != nil {
			return err
		}
		addrLen = int(d[0])
	default:
		return errBadAddr
	}
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(conn, addr); err != nil {
		return err
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return err
	}
	f.gotConnect.CompareAndSwap(nil, &socks5ConnectRecord{
		atyp:  req[3],
		addrs: append([]byte(nil), addr...),
		port:  int(port[0])<<8 | int(port[1]),
	})

	// 0.0.0.0:0 bind address
	if _, err := conn.Write([]byte{0x05, f.rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	if f.rep != 0x00 {
		return nil
	}

	// Resolve the CONNECT address the way a proxy would: IP literals dial
	// at the frame's port; an ATYP=3 hostname goes through the resolve map
	// (proxy-side DNS — a map hit is a full resolved "host:port" endpoint),
	// falling back to dialing the name itself at the frame's port.
	var dialAddr string
	framePort := strconv.Itoa(int(port[0])<<8 | int(port[1]))
	switch req[3] {
	case 0x03:
		name := string(addr)
		f.mu.Lock()
		dialAddr = f.resolve[name]
		f.mu.Unlock()
		if dialAddr == "" {
			dialAddr = net.JoinHostPort(name, framePort)
		}
	default:
		dialAddr = net.JoinHostPort(net.IP(addr).String(), framePort)
	}
	target, err := net.Dial("tcp", dialAddr)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	go func() { _, _ = io.Copy(target, conn) }()
	_, err = io.Copy(conn, target)
	return err
}

var (
	errVersion    = errSocks("bad version")
	errBadCommand = errSocks("bad command")
	errBadAddr    = errSocks("bad addr type")
)

type errSocks string

func (e errSocks) Error() string { return string(e) }

// TestSocks5AuthFailureIsProxyAuth: the proxy demanded a username/password
// (RFC 1929) and rejected our credentials — a typed proxyAuthError from the
// boundary code, so the class is proxy-auth regardless of any error text.
func TestSocks5AuthFailureIsProxyAuth(t *testing.T) {
	f := newSocks5Fake(t, 0x02, 0x01, 0x00) // auth required, reject creds
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5://user:bad@" + strings.TrimPrefix(f.url, "socks5://")}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "proxy authentication failed") {
		t.Fatalf("uerr.Message = %q, want the auth-failure text", uerr.Message)
	}
}

// TestSocks5Rep02NotProxyAuth: REP 0x02 is RFC 1928 §6 "connection not
// allowed by ruleset" — a policy refusal, explicitly NOT a credential
// failure. Labels matter: fallback/health behave identically either way, but
// the class must not lie about the cause.
func TestSocks5Rep02NotProxyAuth(t *testing.T) {
	f := newSocks5Fake(t, 0x00, 0x00, 0x02) // no-auth greet, REP 0x02 CONNECT
	// 127.0.0.1 resolves locally without DNS — the point of the test is the
	// proxy's REP 0x02, reached only after the resolution step succeeds.
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://127.0.0.1:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class == ClassProxyAuthError {
		t.Fatal("REP 0x02 must NOT classify as proxy-auth (RFC 1928 ruleset refusal)")
	}
	if class != ClassConnectionError {
		t.Fatalf("class = %s, want ClassConnectionError", class)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "connection not allowed by ruleset") {
		t.Fatalf("uerr.Message = %q, want the ruleset-refusal text", uerr.Message)
	}
}

// TestSocks5TunnelServesUpstream: the full loop — the hand-rolled dialer
// negotiates, CONNECTs, and pipes a real HTTP round-trip to the target. This
// is the proof the SOCKS5 row is not just error paths.
func TestSocks5TunnelServesUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer upstream.Close()

	f := newSocks5Fake(t, 0x00, 0x00, 0x00) // no-auth, tunnel everything
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, failure := c.DoClassified(context.Background(), upstream.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want success through the tunnel", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("data: {}")) {
		t.Fatalf("tunneled body = %q, want the upstream SSE chunk", body)
	}
}

// TestSocks5hConnectCarriesHostname (issue #8): a socks5h:// egress sends
// the CONNECT target as ATYP=3 — 1 length byte followed by the EXACT
// hostname bytes — and the proxy does the resolution; the greeting, the
// auth method choice, and the reply handling are the same wire flow as
// socks5. The fake "resolves" the name proxy-side, proving the full
// round-trip works with the hostname never appearing in any local lookup.
func TestSocks5hConnectCarriesHostname(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer upstream.Close()

	f := newSocks5Fake(t, 0x00, 0x00, 0x00) // no-auth, tunnel everything
	f.resolveAtProxy("target.example", strings.TrimPrefix(upstream.URL, "http://"))
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5h://" + strings.TrimPrefix(f.url, "socks5://")}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://target.example:80/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want success through the hostname-form CONNECT", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("data: {}")) {
		t.Fatalf("tunneled body = %q, want the upstream SSE chunk", body)
	}
	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp != 0x03 {
		t.Fatalf("CONNECT ATYP = %#x, want 0x03 (domain form)", got.atyp)
	}
	if string(got.addrs) != "target.example" {
		t.Fatalf("CONNECT address = %q, want the exact hostname bytes", got.addrs)
	}
	if got.port != 80 {
		t.Fatalf("CONNECT port = %d, want 80", got.port)
	}
}

// TestSocks5ConnectCarriesResolvedIP: the socks5:// form is byte-for-byte
// the historical behavior (issue #8 non-goal: zero change) — the hostname
// resolves LOCALLY and the CONNECT carries ATYP=1/4 with the resolved
// address. Membership, not order: the frame must carry one of the
// addresses the local resolver produces for the name. Only the wire form is
// asserted (tunnel success is TestSocks5TunnelServesUpstream's proof); if
// the fake cannot reach addrs[0] the dial error is irrelevant here.
func TestSocks5ConnectCarriesResolvedIP(t *testing.T) {
	f := newSocks5Fake(t, 0x00, 0x00, 0x00)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	_, _, _ = c.DoClassified(context.Background(), "http://localhost:8080/zen/v1/chat/completions", staticHeaders(), []byte("{}"))

	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("test precondition: resolve localhost: %v", err)
	}
	want := map[string]bool{}
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			want[string(v4)] = true
		} else {
			want[string(a.IP.To16())] = true
		}
	}
	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp != 0x01 && got.atyp != 0x04 {
		t.Fatalf("CONNECT ATYP = %#x, want 0x01/0x04 (resolved IP literal)", got.atyp)
	}
	if !want[string(got.addrs)] {
		t.Fatalf("CONNECT address %v not among the locally resolved IPs", net.IP(got.addrs))
	}
	if got.port != 8080 {
		t.Fatalf("CONNECT port = %d, want 8080", got.port)
	}
}

// TestSocks5hIPLiteralStaysLiteral: an IP-literal TARGET never becomes an
// ATYP=3 "domain" — RFC 1928 defines the domain form for FQDNs, and a
// literal needs no DNS on either side. It keeps its ATYP=1/4 form under
// socks5h too (documented choice, curl-shaped). Only the wire form is
// asserted; nothing listens on port 1 and the dial error is irrelevant.
func TestSocks5hIPLiteralStaysLiteral(t *testing.T) {
	f := newSocks5Fake(t, 0x00, 0x00, 0x00)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5h://" + strings.TrimPrefix(f.url, "socks5://")}))
	_, _, _ = c.DoClassified(context.Background(), "http://127.0.0.1:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))

	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp != 0x01 {
		t.Fatalf("CONNECT ATYP = %#x, want 0x01 (literal stays literal)", got.atyp)
	}
	if !net.IP(got.addrs).Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("CONNECT address = %v, want 127.0.0.1", net.IP(got.addrs))
	}
	if got.port != 1 {
		t.Fatalf("CONNECT port = %d, want 1", got.port)
	}
}

// TestSocks5hRep05StaysConnectionError (issue #8 failure mode): a refused
// remote-resolve CONNECT fails EXACTLY like any other CONNECT refusal — the
// vendor's instant REP 0x05 surfaces as the same connection-error class and
// the 502 envelope. No silent fallback to local resolution, no retry, and
// the frame that was refused is still the hostname form.
func TestSocks5hRep05StaysConnectionError(t *testing.T) {
	f := newSocks5Fake(t, 0x00, 0x00, 0x05) // no-auth greet, REP 0x05 refused
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5h://" + strings.TrimPrefix(f.url, "socks5://")}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://target.example:443/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassConnectionError {
		t.Fatalf("class = %s, want ClassConnectionError", class)
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want the 502 envelope", uerr)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "connection refused") {
		t.Fatalf("uerr.Message = %q, want the REP 0x05 text (replyErr maps 0x05 to it)", uerr.Message)
	}
	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp != 0x03 || string(got.addrs) != "target.example" || got.port != 443 {
		t.Fatalf("refused frame = ATYP %#x %q:%d, want the hostname form", got.atyp, got.addrs, got.port)
	}
}

// TestSocks5hAuthFlowUnchanged: the RFC 1929 credential flow is scheme-
// independent — a socks5h:// egress offers and answers username/password
// auth exactly like socks5://, and a rejection is still the typed
// proxyAuthError (one dial, immediate fallback).
func TestSocks5hAuthFlowUnchanged(t *testing.T) {
	f := newSocks5Fake(t, 0x02, 0x01, 0x00) // auth required, reject creds
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5h://user:bad@" + strings.TrimPrefix(f.url, "socks5://")}))
	resp, uerr, failure := c.DoClassified(context.Background(), "http://target.example/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "proxy authentication failed") {
		t.Fatalf("uerr.Message = %q, want the auth-failure text", uerr.Message)
	}
}

// TestProxyDialAddr: a portless proxy URL dials the SCHEME's default port —
// http→80 / https→443 like stdlib's own proxy dialing, socks5→1080 per RFC
// 1928 — and an IPv6 literal is bracketed exactly once. The old fallback
// defaulted everything to 1080 and double-bracketed IPv6 ([[::1]]:1080),
// producing unusable egresses with misleading dial errors.
func TestProxyDialAddr(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"http://p.example:8080", "p.example:8080"},
		{"http://p.example", "p.example:80"},
		{"https://p.example", "p.example:443"},
		{"socks5://p.example", "p.example:1080"},
		{"socks5h://p.example", "p.example:1080"}, // same default port; only the CONNECT form differs
		{"http://[::1]", "[::1]:80"},
		{"http://[::1]:8080", "[::1]:8080"},
		{"socks5://[2001:db8::1]", "[2001:db8::1]:1080"},
		// Host carries no userinfo — credentials never reach a dial address.
		{"http://user:pw@p.example:3128", "p.example:3128"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if got := proxyDialAddr(u); got != tc.want {
			t.Fatalf("proxyDialAddr(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// egressProxy is the fake HTTP(S) proxy for the redirect-path tests: a REAL
// tunneling proxy for CONNECT (issue #6's wire) that ALSO forwards
// absolute-form plain-http requests the way a forward proxy must — plus
// per-request counters, so tests can prove which side of the boundary each
// hop arrived on.
type egressProxy struct {
	srv      *httptest.Server
	connects atomic.Int64
	forwards atomic.Int64
}

// newEgressProxy builds the dual-protocol fake. connect407 makes the CONNECT
// arm answer 407 instead of tunneling (the typed-refusal fixture).
func newEgressProxy(t *testing.T, connect407 bool) *egressProxy {
	t.Helper()
	e := &egressProxy{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			e.connects.Add(1)
			if connect407 {
				w.Header().Set("Proxy-Authenticate", `Basic realm="egress-proxy"`)
				w.WriteHeader(http.StatusProxyAuthRequired)
				return
			}
			up, err := net.Dial("tcp", r.Host)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			client, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				_ = up.Close()
				return
			}
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { _, _ = io.Copy(up, client) }()
			go func() { _, _ = io.Copy(client, up) }()
			return
		}
		// Absolute-form plain-http request: forward it as a real forward
		// proxy would, and count the arrival — the proof an http hop rode
		// the egress instead of a direct dial.
		if !r.URL.IsAbs() {
			t.Errorf("forward proxy expected an absolute-URI request, got %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e.forwards.Add(1)
		fwd := &http.Request{
			Method: r.Method,
			URL:    r.URL,
			Header: r.Header.Clone(),
			Host:   r.URL.Host,
		}
		resp, err := http.DefaultTransport.RoundTrip(fwd)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// TestCrossSchemeRedirectStaysOnEgress (the https→http direction): an https
// origin behind an HTTP proxy egress answers 302 pointing at a plain-http
// target. The manual redirect walk must re-pick the transport for the NEW
// scheme — the http hop rides the absolute-form proxy path — instead of
// letting stdlib follow inside the TUNNELED client, where a nil DialContext
// on a Proxy-less transport dials the origin DIRECT and leaks the host IP
// past the egress (plus the re-POSTed body, Authorization, and x-opencode-*
// headers). The proxy's counters are the wire proof: 1 CONNECT (the https
// hop) and 1 absolute-form forward (the http hop), with the target actually
// served — and served ONLY through the proxy.
func TestCrossSchemeRedirectStaysOnEgress(t *testing.T) {
	var mu sync.Mutex
	targetHits := 0
	var targetBody string
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		targetHits++
		targetBody = string(b)
		targetAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: hop2\n\n")
	}))
	defer target.Close()

	// The https origin: one POST, then a 302 to the PLAIN-HTTP target.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/zen/v1/chat/completions")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	pxy := newEgressProxy(t, false)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.srv.URL}))
	c.TLSConfig = &tls.Config{RootCAs: originRootPool(t, origin)}
	headers := func() map[string]string {
		return map[string]string{
			"Authorization":      "Bearer " + config.PublicBearer,
			"x-opencode-session": "e2e-session",
		}
	}

	resp, uerr, failure := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", headers, []byte(`{"model":"big-pickle"}`))

	class := failure.Class
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want the followed chain served through the egress", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: hop2") {
		t.Fatalf("body = %q, want the http target's SSE through the proxy", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if targetHits != 1 {
		t.Fatalf("target hits = %d, want 1 (the http hop arrived)", targetHits)
	}
	if got := pxy.connects.Load(); got != 1 {
		t.Fatalf("proxy CONNECTs = %d, want 1 (the https hop tunneled)", got)
	}
	// THE assertion: the plain-http hop ARRIVED ON THE PROXY in absolute
	// form. Pre-fix behavior dials the target directly from the host —
	// the forward counter would read 0 while the target still got hit.
	if got := pxy.forwards.Load(); got != 1 {
		t.Fatalf("proxy absolute-form forwards = %d, want 1 — the http redirect hop bypassed the egress", got)
	}
	// 302 rewrites the hop to a body-less GET; the kept headers (same-host
	// 127.0.0.1 chain — stdlib's cross-DOMAIN rule does not strip) still
	// arrive via the proxy: the accepted consequence, now egress-safe.
	if targetBody != "" {
		t.Fatalf("302 target saw body %q, want none (POST→GET drops it)", targetBody)
	}
	if targetAuth != "Bearer "+config.PublicBearer {
		t.Fatalf("302 target saw Authorization %q, want the public bearer forwarded through the egress", targetAuth)
	}
}

// TestCrossSchemeRedirect407IsTyped (the http→https direction): a plain-http
// origin behind an HTTP proxy egress answers 302 to an https target, and the
// proxy refuses the CONNECT with 407. The https hop must ride the TYPED
// connect.go boundary — pre-fix, stdlib spoke the CONNECT itself on the
// absolute-form transport, whose 407 surfaces as a reason-phrase-only plain
// error (ClassConnectionError → the full 502 retry matrix = 4 CONNECT
// dials). The issue #6 contract instead: exactly ONE CONNECT, typed
// proxy-auth, immediate fallback eligibility.
func TestCrossSchemeRedirect407IsTyped(t *testing.T) {
	// The http origin: one POST, then a 302 to an unreachable https target —
	// unreachable is fine, the proxy refuses the CONNECT before any origin
	// dial.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://origin2.invalid/zen/v1/chat/completions")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	pxy := newEgressProxy(t, true) // CONNECT arm answers 407
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.srv.URL}))

	resp, uerr, failure := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))

	class := failure.Class
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError (the redirected https hop must ride the typed CONNECT boundary)", class)
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want the 502 envelope", uerr)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "CONNECT refused with 407") {
		t.Fatalf("error text = %q, want the typed boundary message", uerr.Message)
	}
	if got := pxy.connects.Load(); got != 1 {
		t.Fatalf("CONNECTs = %d, want exactly 1 (typed proxy-auth never rides the 502 retry matrix — stdlib's CONNECT would have made it 4)", got)
	}
	if got := pxy.forwards.Load(); got != 1 {
		t.Fatalf("proxy forwards = %d, want 1 (the http hop rode the egress)", got)
	}
}

// TestRedirectCapIsUndiciTwenty: the manual walk follows 20 redirects and
// gives up on the 21st with a network-class error — undici's contract
// (`if (request.redirectCount === 20) return … makeNetworkError('redirect
// count exceeded')`, fetch/index.js:1250-1255), NOT net/http's default cap
// of 10. attempt is called DIRECTLY so the retry matrix does not multiply
// the hit count; DoClassified routes this error through the normal 502
// network-error rule exactly like any other fetch exception.
func TestRedirectCapIsUndiciTwenty(t *testing.T) {
	var hits atomic.Int64
	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", r.URL.Path) // same-URL redirect: keep walking
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer chain.Close()

	c := noSleepClient(NewClientFor(nil)) // direct egress client: manual walk
	resp, err := c.attempt(context.Background(), chain.URL+"/zen/v1/chat/completions", nil, []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if got := hits.Load(); got != int64(config.MaxRedirects)+1 {
		t.Fatalf("upstream hits = %d, want %d (initial + all 20 followed redirects)", got, config.MaxRedirects+1)
	}
	if err == nil || !strings.Contains(err.Error(), "redirect count exceeded") {
		t.Fatalf("err = %v, want the redirect-count-exceeded network error", err)
	}
	var pae *proxyAuthError
	if errors.As(err, &pae) {
		t.Fatalf("the cap error must stay a plain network error, never proxy-auth: %v", err)
	}
}

// TestConnectReplyHeadersAreBounded: a hostile proxy flooding the CONNECT
// reply with an unbounded header block must hit the reader's limit (stdlib
// parity — net/http maxHeaderResponseSize) and fail the dial as a connection
// error — never grow memory to the conn deadline and never promote to
// proxy-auth.
func TestConnectReplyHeadersAreBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)                                                  // the CONNECT request line (contents unused)
				junk := bytes.Repeat([]byte("X-Junk: 0123456789abcdef\r\n"), 1<<19) // ~13 MiB
				_, _ = c.Write(junk)
			}(conn)
		}
	}()

	u, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	d := newConnectDialer(u, func() *tls.Config { return nil })
	done := make(chan error, 1)
	go func() {
		_, err := d.DialTLSContext(context.Background(), "tcp", "origin.invalid:443")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the header flood must fail the dial")
		}
		var pae *proxyAuthError
		if errors.As(err, &pae) {
			t.Fatalf("a header flood is a connection error, never proxy-auth: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded read did not fail the dial in time — the limit did not engage")
	}
}
