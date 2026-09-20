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
	"sync/atomic"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// noSleepClient stubs the retry sleep so hardening loops run instantly; every
// failure row below exercises the 502 retry budget (1 initial + 3 retries).
func noSleepClient(c *Client, err error) *Client {
	if err != nil {
		panic(err)
	}
	c.Sleep = func(time.Duration) {}
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
	resp, uerr, class := c.DoClassified(context.Background(), "https://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	resp, uerr, class := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	c.Sleep = func(time.Duration) {}
	resp, uerr, class := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	resp, uerr, class := c.DoClassified(context.Background(), "http://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	a.Sleep = func(time.Duration) {}
	// b is DIRECT, so its origin trust rides the transport's TLSClientConfig
	// (Client.TLSConfig only feeds the tunneled CONNECT boundary).
	b := NewClient()
	b.Sleep = func(time.Duration) {}
	b.HTTP.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
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
}

func newSocks5Fake(t *testing.T, method, authRep, rep byte) *socks5Fake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &socks5Fake{ln: ln, url: "socks5://" + ln.Addr().String(), method: method, authRep: authRep, rep: rep}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
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

	// 0.0.0.0:0 bind address
	if _, err := conn.Write([]byte{0x05, f.rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	if f.rep != 0x00 {
		return nil
	}

	target, err := net.Dial("tcp", net.JoinHostPort(net.IP(addr).String(), strconv.Itoa(int(port[0])<<8|int(port[1]))))
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
	resp, uerr, class := c.DoClassified(context.Background(), "http://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	resp, uerr, class := c.DoClassified(context.Background(), "http://127.0.0.1:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
	resp, uerr, class := c.DoClassified(context.Background(), upstream.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want success through the tunnel", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("data: {}")) {
		t.Fatalf("tunneled body = %q, want the upstream SSE chunk", body)
	}
}
