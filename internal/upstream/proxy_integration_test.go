package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// insecureTLS disables cert verification for the loopback origin fixture
// (httptest TLS certs are self-signed for "example.com").
func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// noSleepClient stubs the retry sleep so hardening loops run instantly; every
// failure row below exercises the 502 retry budget (1 initial + 3 retries).
func noSleepClient(c *Client, err error) *Client {
	if err != nil {
		panic(err)
	}
	c.Sleep = func(time.Duration) {}
	return c
}

// TestConnect407FromProxyIsProxyAuth: an HTTP CONNECT proxy answers 407 —
// only a proxy can produce this error. stdlib strips the numeric code and
// wraps the REASON PHRASE (transport.go strings.Cut → errors.New(text)), so a
// string-contains "407" probe would MISS this; the phrase gate catches it and
// the class is proxy-auth, not a generic connection error.
func TestConnect407FromProxyIsProxyAuth(t *testing.T) {
	var connects int
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects++
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-proxy"`)
		w.WriteHeader(http.StatusProxyAuthRequired) // 407
	}))
	defer pxy.Close()

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL}))
	resp, uerr, class := c.DoClassified(context.Background(), "https://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want the 502 envelope", uerr)
	}
	if !strings.Contains(uerr.Message, "Proxy Authentication Required") {
		// Pins the stdlib shape: the number is gone, the phrase is the marker.
		t.Fatalf("error text = %q, want the reason phrase (stdlib strips the 407 code)", uerr.Message)
	}
	if connects == 0 {
		t.Fatal("proxy never saw the request")
	}
}

// TestOrigin407BehindProxyIsClientError: with an https target the proxy
// CONNECT-tunnels; a 407 AFTER the tunnel can only come from the ORIGIN — it
// must NOT be classified as proxy-auth (the tunnel means our proxy
// credentials were accepted).
func TestOrigin407BehindProxyIsClientError(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"origin auth"}}`)
	}))
	defer origin.Close()

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
	defer pxy.Close()

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL}))
	c.HTTP.Transport.(*http.Transport).TLSClientConfig = insecureTLS()
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

// TestForwardProxy407WithCredsIsProxyAuth: an http TARGET through an http
// proxy with credentials — we sent Proxy-Authorization and the proxy gated
// us (the origin never saw the request). Definitive proxy-auth.
func TestForwardProxy407WithCredsIsProxyAuth(t *testing.T) {
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
	if class != ClassProxyAuthError {
		t.Fatalf("class = %s, want ClassProxyAuthError", class)
	}
	if uerr == nil || uerr.Status != http.StatusProxyAuthRequired {
		t.Fatalf("uerr = %+v, want status 407", uerr)
	}
}

// TestForwardProxy407NoCredsIsClientError: same shape without credentials —
// ambiguous (proxy wants what we never sent, or the origin answered). The
// conservative choice: a client error, no fallback onto the next proxy with
// the same missing credentials and no health poison of a healthy egress.
func TestForwardProxy407NoCredsIsClientError(t *testing.T) {
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer pxy.Close()

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL}))
	resp, uerr, class := c.DoClassified(context.Background(), "http://origin.invalid/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
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
// (RFC 1929) and rejected our credentials — a typed proxyAuthError, so the
// class is proxy-auth regardless of any "407" text.
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
