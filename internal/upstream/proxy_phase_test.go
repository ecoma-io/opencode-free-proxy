package upstream

// proxy_phase_test.go — the PROXY HOP's failure phases (issue #61).
//
// A forward proxy is two hops this process can prove it owns — the TCP dial to
// the proxy, and (for an https proxy endpoint) the TLS handshake on top of it —
// and the contract turns on telling them apart:
//
//	proxy TCP connect      the egress path is unreachable
//	proxy TLS handshake    reached the endpoint, could not speak to it
//	proxy auth (407)       the egress refused our credentials
//	CONNECT reply          the proxy could not reach the ORIGIN
//	origin TLS             the tunnel came up, the origin's handshake failed
//
// Every one of them is pre-transmission, so each is `not_sent` and authorises an
// egress move (docs/recovery-semantics.md, the proof-boundary table). The two
// absolute-form failures net/http used to own on its own — the TLS hop to an
// https proxy endpoint — are now recorded by this package (DialProxyTLSContext,
// connect.go); before issue #61 they arrived as an opaque dial error with no
// phase at all, which is the difference between "this egress is down" and "this
// egress cannot serve" disappearing from the record.
//
// Each case asserts four things about the failure — Origin, Phase, RequestState
// and ReplaySafe() — and then the behaviour those imply: whether the executor
// moved the request to the second egress.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// phaseOrigin is a 200-speaking plain-http origin: the target of the
// ABSOLUTE-FORM proxy path (http origins are never tunneled).
func phaseOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// tlsProxy is an https proxy ENDPOINT: TLS terminates at the fixture, so the
// CONNECT (or the absolute-form request) travels inside it. It is the fixture
// that makes the proxy-hop TLS handshake a real step of the request.
func tlsProxy(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	return s
}

// phasePool trusts every fixture server's certificate. It is injected through
// Client.TLSConfig — the seam every ORIGIN handshake already uses — which since
// issue #61 also supplies the trust anchors of an https PROXY hop
// (proxyTLSConfig, connect.go). One pool therefore covers a fixture whose proxy
// endpoint and whose origin are both self-signed.
func phasePool(t *testing.T, servers ...*httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	return pool
}

// phaseClient builds the egress client for a proxy endpoint. A nil pool leaves
// the client on the system trust store — the production shape, and the fixture
// for "the proxy endpoint is reachable but its certificate is not trusted".
func phaseClient(t *testing.T, typ config.ProxyType, proxyURL string, pool *x509.CertPool) *Client {
	t.Helper()
	c := noSleepClient(NewClientFor(&config.Proxy{Type: typ, URL: proxyURL}))
	if pool != nil {
		c.TLSConfig = &tls.Config{RootCAs: pool}
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// phasePolicy is the executor policy these cases pin: fallback allowed, three
// egress attempts, health marking OFF.
//
// Health is off on purpose. What is measured here is the REPLAY PERMISSION —
// whether the recorded failure may move the request at all — and the probe
// below fails egress a before the executor ever sees it, so a threshold-1
// policy would quarantine a and hide the decision. Splitting the marking
// predicate from the replay predicate is issue #62's subject, not this file's.
func phasePolicy() AttemptPolicy {
	return AttemptPolicy{
		FallbackEnabled: true,
		MaxAttempts:     3,
		HealthPolicy:    health.Policy{Enabled: false},
	}
}

// phaseMove drives ONE attempt through the executor with egress a (the client
// under test) and egress b (a client that can serve the same URL), and reports
// which egress was tried and whether the request was served. A replay-safe
// failure moves to b; anything else ends the attempt on a.
func phaseMove(t *testing.T, a, b *Client, url string) (id string, attempts int, served bool) {
	t.Helper()
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		switch e.ID {
		case "a":
			return a, true
		case "b":
			return b, true
		}
		return nil, false
	}, health.New(), NewLimiter())
	plan := routing.RoutePlan{
		RouteID:  "r",
		Attempts: []string{"a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}},
	}
	resp, id, attempts, _, uerr := exec.Execute(
		context.Background(), url, staticHeaders(), []byte(`{}`), plan, phasePolicy())
	if resp != nil {
		_ = resp.Body.Close()
	}
	return id, attempts, resp != nil && uerr == nil
}

// phaseProbe sends the request through ONE client and returns the recorded
// failure — the raw record, before any fallback decision can rewrite it.
func phaseProbe(t *testing.T, c *Client, url string) Failure {
	t.Helper()
	resp, _, failure := c.DoClassified(context.Background(), url, staticHeaders(), []byte(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a transport failure must not be delivered as a response")
	}
	return failure
}

// TestForwardProxyPhaseAttribution: every proxy-path failure is attributed to
// the step that actually failed, is not_sent (the request provably never went
// out), and is therefore allowed to move to another egress.
func TestForwardProxyPhaseAttribution(t *testing.T) {
	plain := phaseOrigin(t)
	plainURL := plain.URL + "/zen/v1/chat/completions"
	tlsOrigin := httpsOKOrigin(t)
	tlsURL := tlsOrigin.URL + "/zen/v1/chat/completions"
	tlsOriginPool := originRootPool(t, tlsOrigin)

	// The fallback egress: a direct client to the same origin, so "the request
	// moved" is observable as b serving it. (Case 7 is the exception: its URL
	// is unreachable by construction, so only the egress tried can be asserted.)
	directToPlain := func() *Client { return NewClient() }
	directToTLS := func() *Client {
		c := NewClient()
		c.TLSConfig = &tls.Config{RootCAs: tlsOriginPool}
		t.Cleanup(c.CloseIdleConnections)
		return c
	}

	// Two https proxy endpoints whose TLS hop must SUCCEED for the case to reach
	// the CONNECT at all, so the client under test trusts them.
	connect502Proxy := tlsProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	connect407Proxy := tlsProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-proxy"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))

	cases := []struct {
		name      string
		a         *Client
		b         *Client
		url       string
		wantPhase string
		// wantServed is false only for case 7, where b cannot reach the URL and
		// the assertion is that b was TRIED (attempts=2, id=b) and nothing more.
		wantServed bool
	}{
		{
			// The proxy endpoint itself is not listening. Both transports dial
			// it, so both report the same phase: the tunneled client for an
			// https origin, the absolute-form client for an http one.
			name:       "https proxy TCP connect refused, absolute form (http origin)",
			a:          phaseClient(t, config.ProxyHTTPS, "https://"+closedAddr(t), nil),
			b:          directToPlain(),
			url:        plainURL,
			wantPhase:  "proxy_connect",
			wantServed: true,
		},
		{
			name:       "https proxy TCP connect refused, CONNECT tunnel (https origin)",
			a:          phaseClient(t, config.ProxyHTTPS, "https://"+closedAddr(t), nil),
			b:          directToTLS(),
			url:        tlsURL,
			wantPhase:  "proxy_connect",
			wantServed: true,
		},
		{
			// Reachable but untrusted: the handshake is the failing step, and
			// NOTHING has been sent anywhere. Before issue #61 these two cases
			// were stdlib's opaque handshake error — the phase the record could
			// not name.
			name:       "https proxy TLS handshake fails, absolute form (http origin)",
			a:          phaseClient(t, config.ProxyHTTPS, tlsProxy(t, http.NotFoundHandler()).URL, nil),
			b:          directToPlain(),
			url:        plainURL,
			wantPhase:  "proxy_tls",
			wantServed: true,
		},
		{
			name:       "https proxy TLS handshake fails, CONNECT tunnel (https origin)",
			a:          phaseClient(t, config.ProxyHTTPS, tlsProxy(t, http.NotFoundHandler()).URL, nil),
			b:          directToTLS(),
			url:        tlsURL,
			wantPhase:  "proxy_tls",
			wantServed: true,
		},
		{
			// TLS to the proxy succeeded, so the CONNECT itself is spoken
			// inside it. A refusal here means the PROXY could not reach the
			// origin: pre-transmission, but a different phase from a dial
			// failure, and a different fact about the egress.
			name:       "proxy refuses CONNECT with 502 (https origin)",
			a:          phaseClient(t, config.ProxyHTTPS, connect502Proxy.URL, phasePool(t, connect502Proxy)),
			b:          directToTLS(),
			url:        tlsURL,
			wantPhase:  "connect_read",
			wantServed: true,
		},
		{
			// The one 407 that is provably the proxy's own: this package read
			// the CONNECT reply itself, through the TLS hop.
			name:       "proxy refuses CONNECT with 407 (https origin, https proxy endpoint)",
			a:          phaseClient(t, config.ProxyHTTPS, connect407Proxy.URL, phasePool(t, connect407Proxy)),
			b:          directToTLS(),
			url:        tlsURL,
			wantPhase:  "proxy_auth",
			wantServed: true,
		},
		{
			// A working tunnel to something that is not a TLS server: the
			// tunnel is up and the ORIGIN's handshake fails, which is the last
			// phase that can still be called not_sent.
			name:       "origin TLS fails inside a working tunnel",
			a:          phaseClient(t, config.ProxyHTTP, tunnelingProxy(t).URL, nil),
			b:          directToPlain(),
			url:        strings.Replace(plainURL, "http://", "https://", 1),
			wantPhase:  "origin_tls",
			wantServed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := phaseProbe(t, tc.a, tc.url)
			if failure.Origin != OriginTransport {
				t.Fatalf("origin = %s, want transport (no HTTP response exists on this path)", failure.Origin)
			}
			if got := failure.Phase.String(); got != tc.wantPhase {
				t.Fatalf("phase = %q, want %q (failure: %+v)", got, tc.wantPhase, failure)
			}
			if failure.RequestState != RequestStateNotSent {
				t.Fatalf("request state = %s, want not_sent — every proxy-path phase completes before a request byte exists", failure.RequestState)
			}
			if !failure.ReplaySafe() {
				t.Fatalf("failure not replay-safe: %+v", failure)
			}

			id, attempts, served := phaseMove(t, tc.a, tc.b, tc.url)
			if attempts != 2 || id != "b" {
				t.Fatalf("attempts=%d id=%q, want the request moved to egress b", attempts, id)
			}
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (id=%q attempts=%d)", served, tc.wantServed, id, attempts)
			}
		})
	}
}

// TestProxyPathPostTransmissionNeverReplays is the other half of the contract:
// once a request byte has been handed to a connection, the proxy path can
// produce no not_sent at all, so a death after the write can never authorise an
// egress move — however much it looks like an egress problem.
//
// The fixture is an https proxy endpoint that accepts the TLS handshake, READS
// the request (the hijack runs only after net/http parsed the request line and
// headers), and then closes the connection. The request is therefore provably
// transmitted, the response provably never arrives, and what the record must
// say is `unknown`.
func TestProxyPathPostTransmissionNeverReplays(t *testing.T) {
	plain := phaseOrigin(t)
	var seen atomic.Int64
	proxy := tlsProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close() // the request is in, the response never comes
	}))

	a := phaseClient(t, config.ProxyHTTPS, proxy.URL, phasePool(t, proxy))
	url := plain.URL + "/zen/v1/chat/completions"

	failure := phaseProbe(t, a, url)
	if got := seen.Load(); got != 1 {
		t.Fatalf("proxy saw %d requests, want 1 — the fixture must have consumed the request for this to be a post-transmission failure", got)
	}
	if failure.Origin != OriginTransport {
		t.Fatalf("origin = %s, want transport", failure.Origin)
	}
	if failure.RequestState != RequestStateUnknown {
		t.Fatalf("request state = %s, want unknown: the request was written and the peer read it", failure.RequestState)
	}
	if failure.ReplaySafe() {
		t.Fatalf("failure reported replay-safe after the request was written: %+v", failure)
	}
	// The exact phase depends on which side of the write the close lands
	// (request_write vs response_headers), or is unattributable when the error
	// type carries no direction. What it may never be is a PRE-transmission
	// phase: those are the ones that would contradict the state above.
	switch failure.Phase {
	case FailurePhaseRequestWrite, FailurePhaseResponseHeaders, FailurePhaseNone:
	default:
		t.Fatalf("phase = %q, want a post-dial phase (request_write | response_headers | none)", failure.Phase)
	}

	if id, attempts, served := phaseMove(t, a, plainDirect(t), url); attempts != 1 || id != "a" || served {
		t.Fatalf("attempts=%d id=%q served=%v, want a single attempt on a (no egress move after transmission)", attempts, id, served)
	}
}

// plainDirect is the direct client the move assertions use: it is a working
// egress, so a move would be visible as it serving the request.
func plainDirect(t *testing.T) *Client {
	t.Helper()
	c := NewClient()
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// TestHTTPSProxyAbsoluteFormServed is the positive control for the absolute-form
// TLS hop: the request must actually travel, in absolute form, over TLS to the
// proxy endpoint. A wiring mistake in DialProxyTLSContext (a conn handed back
// without the handshake, or a handshake whose state net/http cannot read) would
// fail here rather than only in production.
func TestHTTPSProxyAbsoluteFormServed(t *testing.T) {
	plain := phaseOrigin(t)
	var forwards atomic.Int64
	proxy := tlsProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Errorf("https forward proxy expected an absolute-URI request, got %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		forwards.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	c := phaseClient(t, config.ProxyHTTPS, proxy.URL, phasePool(t, proxy))
	resp, uerr, failure := c.DoClassified(context.Background(), plain.URL+"/zen/v1/chat/completions", staticHeaders(), []byte(`{}`))
	if uerr != nil || failure.Class != ClassSuccess {
		t.Fatalf("uerr = %v failure = %+v, want the request served through the https proxy endpoint", uerr, failure)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("body = %q, want the proxy's response", body)
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("proxy forwards = %d, want 1 (the request must ride the egress, not dial around it)", got)
	}
}

// TestHTTPSProxyTunnelServed is the same control for the CONNECT tunnel through
// an https proxy endpoint: TLS to the proxy, CONNECT inside it, TLS to the
// origin — the full chain dialProxy refactored (issue #61).
func TestHTTPSProxyTunnelServed(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tunneled":true}`)
	}))
	t.Cleanup(origin.Close)

	var connects atomic.Int64
	proxy := tlsProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects.Add(1)
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

	c := phaseClient(t, config.ProxyHTTPS, proxy.URL, phasePool(t, proxy, origin))
	resp, uerr, failure := c.DoClassified(context.Background(), origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte(`{}`))
	if uerr != nil || failure.Class != ClassSuccess {
		t.Fatalf("uerr = %v failure = %+v, want the request served through the tunnel", uerr, failure)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if !strings.Contains(string(body), `"tunneled":true`) {
		t.Fatalf("body = %q, want the origin's response through the tunnel", body)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("proxy CONNECTs = %d, want 1 (the https hop must tunnel)", got)
	}
}
