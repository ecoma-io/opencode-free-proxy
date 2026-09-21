package upstream

// Transport-boundary hardening tests: SOCKS5 wire-shape proofs (method
// negotiation, the local-DNS invariant, IPv6 addressing), handshake bounding
// under hostile proxies (stall + ctx cancel), credential hygiene across every
// error-surfacing path, the JS-parity redirect behavior, and the bounded
// terminal-error-body read.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// ---- SOCKS5 wire-shape proofs ----

// TestSocks5NoAcceptableMethodsIsProxyAuth: the proxy rejects EVERY method we
// offered (RFC 1928 §3 NO ACCEPTABLE METHODS, 0xff). Having offered only
// 0x00, this is the proxy demanding an auth mechanism we never sent — typed
// proxyAuthError by the boundary code (socks5.go), so it costs exactly one
// dial and falls back immediately at the executor.
func TestSocks5NoAcceptableMethodsIsProxyAuth(t *testing.T) {
	f := newSocks5Fake(t, 0xff, 0x00, 0x00)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, class := c.DoClassified(context.Background(), "http://127.0.0.1:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "no acceptable authentication method") {
		t.Fatalf("uerr.Message = %q, want the no-acceptable-method text", uerr.Message)
	}
}

// TestSocks5AuthDemandedWithoutCredentials: the proxy selects RFC 1929
// username/password although the egress carried NO userinfo (the socks5.go
// negotiate 0x02/!hasAuth branch). A credential demand we cannot answer is a
// typed proxyAuthError — the proxy will refuse us identically on every
// retry, so the class ends the egress's turn after one dial.
func TestSocks5AuthDemandedWithoutCredentials(t *testing.T) {
	f := newSocks5Fake(t, 0x02, 0x00, 0x00) // demands auth; no credentials will be sent
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, class := c.DoClassified(context.Background(), "http://127.0.0.1:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "demanded auth but none configured") {
		t.Fatalf("uerr.Message = %q, want the demanded-auth text", uerr.Message)
	}
}

// TestSocks5HostnameArrivesAsIP is the socks5:// local-resolve contract made
// real on the wire: the origin is given as a HOSTNAME and must be resolved
// LOCALLY — the CONNECT request carries atyp 0x01/0x04 with raw address
// bytes, never 0x03 + domain (socks5.go's LOCAL resolution; remote resolve
// is the socks5h:// scheme's opt-in, never the default — this is the proof).
func TestSocks5HostnameArrivesAsIP(t *testing.T) {
	// no-auth greet; REP 0x01 ends the dial deterministically right after the
	// CONNECT has been recorded — the target is never dialed (port 1).
	f := newSocks5Fake(t, 0x00, 0x00, 0x01)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, class := c.DoClassified(context.Background(), "http://localhost:1/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassConnectionError || uerr == nil || !strings.Contains(uerr.Message, "general failure") {
		t.Fatalf("class=%s uerr=%v, want the REP 0x01 terminal (this test only reads the CONNECT bytes)", class, uerr)
	}
	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp == 0x03 {
		t.Fatalf("the proxy received a DOMAIN (atyp 0x03, %q) — remote-DNS semantics leaked into the socks5 path", got.addrs)
	}
	if got.atyp != 0x01 && got.atyp != 0x04 {
		t.Fatalf("atyp = %#x, want an IP address type", got.atyp)
	}
	ips, err := net.LookupIP("localhost")
	if err != nil {
		t.Fatalf("fixture flaw: cannot resolve localhost: %v", err)
	}
	for _, ip := range ips {
		if net.IP(got.addrs).Equal(ip) {
			if got.port != 1 {
				t.Fatalf("CONNECT port = %d, want 1", got.port)
			}
			return
		}
	}
	t.Fatalf("CONNECT address %v matches none of the locally resolved IPs %v — the wire carried a foreign address", net.IP(got.addrs), ips)
}

// TestSocks5IPv6OriginArrivesAsAtyp4: an IPv6 origin target travels as atyp
// 0x04 with the full 16-byte address — and the tunnel still serves a real
// round-trip through it.
func TestSocks5IPv6OriginArrivesAsAtyp4(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback in this environment: %v", err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: v6\n\n")
	}))
	origin.Listener = ln
	origin.Start()
	defer origin.Close()

	f := newSocks5Fake(t, 0x00, 0x00, 0x00) // no-auth, tunnel everything
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	resp, uerr, class := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want a served round-trip through the v6 tunnel", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: v6") {
		t.Fatalf("tunneled body = %q, want the upstream SSE chunk", body)
	}
	got := f.gotConnect.Load()
	if got == nil {
		t.Fatal("the proxy never received a CONNECT request")
	}
	if got.atyp != 0x04 {
		t.Fatalf("atyp = %#x, want 0x04 for an IPv6 origin", got.atyp)
	}
	if len(got.addrs) != 16 {
		t.Fatalf("address = %d bytes, want the full 16-byte IPv6 form", len(got.addrs))
	}
	if !net.IP(got.addrs).Equal(net.ParseIP("::1")) {
		t.Fatalf("address = %v, want ::1", net.IP(got.addrs))
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("fixture flaw: bad origin address %q: %v", ln.Addr(), err)
	}
	wantPort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("fixture flaw: bad origin port: %v", err)
	}
	if got.port != wantPort {
		t.Fatalf("CONNECT port = %d, want %d", got.port, wantPort)
	}
}

// ---- Handshake bounding under hostile proxies ----

// stallProxy accepts TCP, lets a per-test callback consume the protocol
// preamble, then keeps the conn open but SILENT. Its single blocked Read
// unblocks only when the client tears the conn down — `dropped` is the
// closure proof for the ctx-abort contract.
type stallProxy struct {
	addr    string
	greeted chan struct{}
	dropped chan struct{}
}

func newStallProxy(t *testing.T, hello func(net.Conn) error) *stallProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stallProxy{addr: ln.Addr().String(), greeted: make(chan struct{}, 1), dropped: make(chan struct{}, 1)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if err := hello(c); err != nil {
					return
				}
				s.greeted <- struct{}{}
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						s.dropped <- struct{}{}
						return
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// socks5GreetHello consumes the RFC 1928 method-selection greeting.
func socks5GreetHello(c net.Conn) error {
	hdr := make([]byte, 2)
	_, err := io.ReadFull(c, hdr)
	return err
}

// connectRequestHello consumes the CONNECT request (through its blank line).
func connectRequestHello(c net.Conn) error {
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			return nil
		}
	}
}

// TestSocks5HandshakeStallBoundedByContext: a proxy that accepts TCP and then
// never answers the greeting must not strand the dial — a short ctx deadline
// aborts it promptly AND closes the tunnel conn (the fixture's silent hold
// only breaks on client-side teardown).
func TestSocks5HandshakeStallBoundedByContext(t *testing.T) {
	s := newStallProxy(t, socks5GreetHello)
	d := newSocks5Dialer(&url.URL{Scheme: "socks5", Host: s.addr})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := d.DialContext(ctx, "tcp", "127.0.0.1:1")
		errCh <- err
	}()
	select {
	case <-s.greeted:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture flaw: the proxy never received the greeting")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a stalled handshake must fail once the ctx deadline fires")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled dial outlived its 250ms deadline by seconds — the handshake is unbounded")
	}
	select {
	case <-s.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("the ctx deadline did not CLOSE the stalled tunnel conn")
	}
}

// TestSocks5HandshakeStallAbortsOnContextCancel: the same stall, an explicit
// cancel parked mid-handshake — the dial returns and the conn is torn down
// (the watcher/AfterFunc contract, deterministic since the select race fix).
func TestSocks5HandshakeStallAbortsOnContextCancel(t *testing.T) {
	s := newStallProxy(t, socks5GreetHello)
	d := newSocks5Dialer(&url.URL{Scheme: "socks5", Host: s.addr})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := d.DialContext(ctx, "tcp", "127.0.0.1:1")
		errCh <- err
	}()
	select {
	case <-s.greeted:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture flaw: the proxy never received the greeting")
	}
	time.Sleep(50 * time.Millisecond) // park the handshake inside the blocked read
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a canceled handshake must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx cancel did not abort the mid-handshake dial — the conn stayed open")
	}
	select {
	case <-s.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not CLOSE the tunnel conn")
	}
}

// TestConnectStallBoundedByContext: the CONNECT boundary (connect.go) under
// the same hostile proxy — a proxy that reads the CONNECT request and never
// replies must not strand the dial past the ctx deadline, and the abort must
// close the conn.
func TestConnectStallBoundedByContext(t *testing.T) {
	s := newStallProxy(t, connectRequestHello)
	u, err := url.Parse("http://" + s.addr)
	if err != nil {
		t.Fatal(err)
	}
	d := newConnectDialer(u, func() *tls.Config { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := d.DialTLSContext(ctx, "tcp", "origin.invalid:443")
		errCh <- err
	}()
	select {
	case <-s.greeted:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture flaw: the proxy never received the CONNECT request")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a stalled CONNECT must fail once the ctx deadline fires")
		}
		var pae *proxyAuthError
		if errors.As(err, &pae) {
			t.Fatalf("a stall is a connection error, never proxy-auth: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled dial outlived its 250ms deadline by seconds — the CONNECT read is unbounded")
	}
	select {
	case <-s.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("the ctx deadline did not CLOSE the stalled tunnel conn")
	}
}

// TestConnect200WithDeclaredBodyStillTunnels: a proxy that answers the
// CONNECT with 200 plus a body it DECLARED but never sends
// (Content-Length: 100) and then tunnels normally. The successful CONNECT
// reply's body must never be closed or drained: body.Close() io.Copy-drains
// the declared length (net/http transfer.go body.Close default branch),
// which would pin the dial until the conn deadline AND swallow the origin's
// first TLS bytes as "body" — GOROOT dialConn keeps the reply body unclosed
// for exactly this reason (transport.go:1908-1912). The dial must succeed
// promptly and the tunnel must carry a real round-trip.
func TestConnect200WithDeclaredBodyStillTunnels(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: tunneled\n\n")
	}))
	defer origin.Close()

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
				if err := connectRequestHello(c); err != nil {
					return
				}
				// 200 + a declared body that never arrives, then a tunnel.
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\nContent-Length: 100\r\n\r\n")); err != nil {
					return
				}
				up, err := net.Dial("tcp", origin.Listener.Addr().String())
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(conn)
		}
	}()

	u, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	d := newConnectDialer(u, func() *tls.Config { return &tls.Config{RootCAs: pool} })

	type dialResult struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialResult, 1)
	go func() {
		conn, err := d.DialTLSContext(context.Background(), "tcp", origin.Listener.Addr().String())
		done <- dialResult{conn: conn, err: err}
	}()
	var res dialResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the dial blocked on the CONNECT reply — the declared body was drained instead of ignored")
	}
	if res.err != nil {
		t.Fatalf("dial through the non-compliant proxy failed: %v", res.err)
	}
	defer func() { _ = res.conn.Close() }()
	// The tunnel carries traffic: a plain GET over the TLS conn comes back
	// with the origin's SSE.
	if _, err := res.conn.Write([]byte("GET /zen/v1/chat/completions HTTP/1.1\r\nHost: " + origin.Listener.Addr().String() + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write over the tunnel: %v", err)
	}
	raw, err := io.ReadAll(res.conn)
	if err != nil {
		t.Fatalf("read over the tunnel: %v", err)
	}
	if !strings.Contains(string(raw), "data: tunneled") {
		t.Fatalf("tunneled body = %q, want the origin's SSE chunk", raw)
	}
}

// ---- Credential hygiene ----

// sekritUser/sekritPass are the fake credentials every row of the hygiene
// test embeds in the egress URL. They must never appear in any surfaced
// string.
const (
	sekritUser = "sekrit-user"
	sekritPass = "sekrit-pass"
)

func assertNoCredentialMaterial(t *testing.T, where, msg string) {
	t.Helper()
	if config.HasSecret(msg) {
		t.Errorf("%s: surfaced string embeds proxy userinfo: %q", where, msg)
	}
	for _, cred := range []string{sekritUser, sekritPass} {
		if strings.Contains(msg, cred) {
			t.Errorf("%s: surfaced string carries credential material %q: %q", where, cred, msg)
		}
	}
}

// TestProxyCredentialsNeverSurface sweeps EVERY path a proxy-protocol failure
// takes to a client-facing string: the typed CONNECT 407, a proxy dial
// refusal and TLS failure at the CONNECT boundary, the SOCKS5 RFC 1929
// rejection and dial refusal, the stdlib absolute-form proxy path, the
// executor layer built on those messages, and the JSON error envelope the
// client finally receives. Every egress URL below embeds
// sekrit-user:sekrit-pass; none of it may surface anywhere.
func TestProxyCredentialsNeverSurface(t *testing.T) {
	const credentialed = sekritUser + ":" + sekritPass
	doErr := func(c *Client, url string) *UpstreamError {
		resp, uerr, _ := c.DoClassified(context.Background(), url, staticHeaders(), []byte("{}"))
		if resp != nil {
			_ = resp.Body.Close()
		}
		return uerr
	}

	// 1. Typed CONNECT 407 (https origin through a credentialed http proxy).
	pxy, _ := connect407Proxy(t)
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://" + credentialed + "@" + strings.TrimPrefix(pxy.URL, "http://")}))
	if uerr := doErr(c, "https://origin.invalid/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the CONNECT refusal to surface")
	} else {
		assertNoCredentialMaterial(t, "CONNECT 407 message", uerr.Message)
		assertNoCredentialMaterial(t, "CONNECT 407 Error()", uerr.Error())
	}

	// 2. Proxy dial refusal at the CONNECT boundary.
	c = noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://" + credentialed + "@127.0.0.1:1"}))
	if uerr := doErr(c, "https://origin.invalid/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the proxy dial refusal to surface")
	} else {
		assertNoCredentialMaterial(t, "proxy dial refusal", uerr.Message)
	}

	// 3. Proxy TLS failure at the CONNECT boundary (https proxy, untrusted cert).
	tlsPxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer tlsPxy.Close()
	c = noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTPS, URL: "https://" + credentialed + "@" + strings.TrimPrefix(tlsPxy.URL, "https://")}))
	if uerr := doErr(c, "https://origin.invalid/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the proxy TLS failure to surface")
	} else {
		assertNoCredentialMaterial(t, "proxy TLS failure", uerr.Message)
	}

	// 4. SOCKS5 RFC 1929 rejection with credentialed egress.
	sf := newSocks5Fake(t, 0x02, 0x01, 0x00) // auth demanded and rejected
	c = noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5://" + credentialed + "@" + strings.TrimPrefix(sf.url, "socks5://")}))
	if uerr := doErr(c, "http://127.0.0.1:1/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the RFC 1929 rejection to surface")
	} else {
		assertNoCredentialMaterial(t, "socks5 auth rejection", uerr.Message)
	}

	// 5. SOCKS5 proxy dial refusal.
	c = noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5://" + credentialed + "@127.0.0.1:1"}))
	if uerr := doErr(c, "http://127.0.0.1:1/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the socks5 dial refusal to surface")
	} else {
		assertNoCredentialMaterial(t, "socks5 dial refusal", uerr.Message)
	}

	// 6. The stdlib absolute-form proxy path (plain-http origin): Go derives
	// Proxy-Authorization from the userinfo itself; its own error text must
	// stay credential-free too.
	c = noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://" + credentialed + "@127.0.0.1:1"}))
	if uerr := doErr(c, "http://origin.invalid/zen/v1/chat/completions"); uerr == nil {
		t.Fatal("fixture flaw: expected the absolute-form proxy dial to fail")
	} else {
		assertNoCredentialMaterial(t, "stdlib absolute-form proxy dial", uerr.Message)
	}

	// 7. Executor layer: the same typed 407 verdict one layer up, unchanged.
	a, err := NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://" + credentialed + "@" + strings.TrimPrefix(pxy.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	a.Sleep = func(time.Duration) {}
	egA, _ := fixtureRuntime(map[string]int{"a": 200}).Egress("a")
	exec := NewExecutor(func(*config.Egress) (*Client, bool) { return a, true }, health.New(), NewLimiter())
	plan := routing.RoutePlan{RouteID: "r", Strategy: config.StrategyRoundRobin, Attempts: []string{"a"}, Egresses: []*config.Egress{egA}}
	_, id, attempts, class, uerr := exec.Execute(
		context.Background(), "https://origin.invalid/zen/v1/chat/completions",
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), plan, policy(true, 3))
	if class != ClassProxyAuthError || attempts != 1 || id != "a" || uerr == nil {
		t.Fatalf("class=%s attempts=%d id=%q, want one typed dial and no more", class, attempts, id)
	}
	assertNoCredentialMaterial(t, "executor-surfaced 407", uerr.Message)

	// 8. The client-facing envelope built from that message stays clean.
	env, err := json.Marshal(BuildErrorBody(http.StatusBadGateway, uerr.Message))
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredentialMaterial(t, "error envelope", string(env))
}

// ---- JS-parity redirect behavior ----

// TestRedirectFollowedLikeJSFetch pins the redirect PARITY: base.js passes no
// `redirect:` option to fetch (base.js:144-149, forwarded untouched by
// utils/proxyFetch.js:203-257), so the JS router follows redirects by default
// — Go's http.Client default (CheckRedirect unset, follow up to 10) is the
// same contract, deliberately NOT changed to ErrUseLastResponse. The 308
// keeps the method and re-POSTs the body: the target receives the full POST —
// including `Authorization: Bearer public` — and its response is served.
func TestRedirectFollowedLikeJSFetch(t *testing.T) {
	var mu sync.Mutex
	targetHits, targetBody, targetAuth := 0, "", ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		targetHits++
		targetBody = string(b)
		targetAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: redirected\n\n")
	}))
	defer target.Close()

	redirects := 0
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		redirects++
		mu.Unlock()
		w.Header().Set("Location", target.URL+"/zen/v1/chat/completions")
		w.WriteHeader(http.StatusPermanentRedirect) // 308: method + body preserved
	}))
	defer moved.Close()

	c := NewClient()
	headers := map[string]string{"Authorization": "Bearer " + config.PublicBearer}
	resp, uerr := c.Do(context.Background(), moved.URL+"/zen/v1/chat/completions", func() map[string]string { return headers }, []byte(`{"model":"big-pickle"}`))
	if uerr != nil {
		t.Fatalf("the followed redirect must serve the target's response, got %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "data: redirected") {
		t.Fatalf("status=%d body=%q, want the redirect target's 200 SSE", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if redirects != 1 || targetHits != 1 {
		t.Fatalf("redirects=%d target hits=%d, want 1/1 (308 followed, request re-POSTed)", redirects, targetHits)
	}
	if targetBody != `{"model":"big-pickle"}` {
		t.Fatalf("the target saw body %q, want the original POST body re-sent", targetBody)
	}
	if targetAuth != "Bearer "+config.PublicBearer {
		t.Fatalf("the target saw Authorization %q — the accepted consequence is credentials following the redirect", targetAuth)
	}
}

// ---- Bounded terminal error body ----

// TestTerminalErrorBodyReadIsBounded: a hostile upstream answering a
// non-retryable status with an endless body must not grow the process — the
// terminal read is capped at maxErrorBodyBytes (a deliberate divergence from
// utils/error.js:61 `response.text()`, which is unbounded), and the truncated
// text becomes the message (a truncated body is no longer valid JSON, so
// parseUpstreamError falls back to the raw text).
func TestTerminalErrorBodyReadIsBounded(t *testing.T) {
	const bodyLen = 3 << 20 // 3 MiB — well past the cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // outside the retry matrix → terminal
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("x", bodyLen)))
	}))
	defer srv.Close()

	c := NewClient()
	resp, uerr := c.Do(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if uerr == nil || uerr.Status != http.StatusTeapot {
		t.Fatalf("uerr = %+v, want the terminal 418", uerr)
	}
	if len(uerr.Message) > maxErrorBodyBytes {
		t.Fatalf("message = %d bytes, must be capped at %d — the terminal read is unbounded", len(uerr.Message), maxErrorBodyBytes)
	}
	if !strings.HasPrefix(uerr.Message, strings.Repeat("x", 4096)) {
		t.Fatalf("message is not the truncated raw body: %.80q", uerr.Message)
	}
}

// TestAllTransportsCarryIdleConnTimeout is the structural pin for
// config.IdleConnTimeout: EVERY transport this package builds must carry it.
// net/http registers no finalizer for pooled conns, so a conn that was busy
// when a generation prune called CloseIdleConnections returns to the idle
// pool and lives forever unless the transport's own idle timer reaps it (see
// config.IdleConnTimeout). A transport added without the field regresses that
// leak; this fails first.
func TestAllTransportsCarryIdleConnTimeout(t *testing.T) {
	check := func(c *Client, which string) {
		t.Helper()
		for label, hc := range map[string]*http.Client{"HTTP": c.HTTP, "tunneled": c.tunneled} {
			if hc == nil {
				continue
			}
			tr, ok := hc.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("%s %s: transport is %T, want *http.Transport", which, label, hc.Transport)
			}
			if tr.IdleConnTimeout != config.IdleConnTimeout {
				t.Errorf("%s %s: IdleConnTimeout = %v, want %v", which, label, tr.IdleConnTimeout, config.IdleConnTimeout)
			}
		}
	}
	check(NewClient(), "NewClient")
	check(noSleepClient(NewClientFor(nil)), "NewClientFor(direct)")
	check(noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: "http://127.0.0.1:9"})), "NewClientFor(http)")
	check(noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTPS, URL: "https://127.0.0.1:9"})), "NewClientFor(https)")
	check(noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: "socks5://127.0.0.1:9"})), "NewClientFor(socks5)")
}
