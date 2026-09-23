package upstream

// Provenance tests (issue #51): every failure must report WHERE it happened
// and what the transport can PROVE about whether the request went out.
//
// The load-bearing direction is the NEGATIVE one. A wrong not_sent is what
// authorises a replay, so the tests below pin, explicitly, that a post-dial
// failure, a post-transmission failure and a pooled-connection failure can
// never claim it — while the dial phases this package performs itself
// (proxy connect, proxy protocol, origin TLS) must.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// observedProvenance runs one observed call and returns every row's
// provenance (the retry matrix may have dialed more than once).
func observedProvenance(t *testing.T, c *Client, url string) []Row {
	t.Helper()
	rec := NewRecorder()
	resp, _, _ := c.DoClassifiedObserved(context.Background(), url, staticHeaders(), []byte("{}"), rec)
	if resp != nil {
		_ = resp.Body.Close()
	}
	rows := rec.Rows()
	if len(rows) == 0 {
		t.Fatal("no evidence row recorded")
	}
	return rows
}

// terminalProvenance returns the last row of a call — the terminal dial's.
func terminalProvenance(t *testing.T, c *Client, url string) Row {
	t.Helper()
	rows := observedProvenance(t, c, url)
	return rows[len(rows)-1]
}

func wantProvenance(t *testing.T, row Row, origin, phase, state string) {
	t.Helper()
	if row.Origin != origin || row.FailurePhase != phase || row.RequestState != state {
		t.Fatalf("provenance = origin=%q phase=%q state=%q, want %q/%q/%q",
			row.Origin, row.FailurePhase, row.RequestState, origin, phase, state)
	}
}

// noSleepClientFor builds an egress client whose retry sleeps are stubbed, so
// the multi-dial hardening cases below do not pay the matrix's wall clock.
func noSleepClientFor(t *testing.T, p *config.Proxy) *Client {
	t.Helper()
	c, err := NewClientFor(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Sleep = func(time.Duration) {}
	return c
}

// TestProvenanceUpstreamVerdictIsResponseStarted: an HTTP verdict of any kind
// is the provider's own answer. The request was received, so the state is
// response_started — never a replay candidate — and the origin is upstream,
// not transport. Pinned for the exact statuses the recovery contract turns on
// (429 = the Injector's business, 5xx = a provider verdict).
func TestProvenanceUpstreamVerdictIsResponseStarted(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			}))
			defer srv.Close()

			row := terminalProvenance(t, noSleepClientFor(t, nil), srv.URL+"/zen/v1/chat/completions")
			wantProvenance(t, row, "upstream", "response_headers", "response_started")
		})
	}
}

// TestProvenanceTargetConnectRefusedIsNotSent: the direct egress cannot reach
// the origin at all. The dial is performed by this package, so the state is
// provable — and EVERY dial of the retry matrix reports it, not just the
// terminal one.
func TestProvenanceTargetConnectRefusedIsNotSent(t *testing.T) {
	// Port 1 on loopback: reserved, nothing listens, refusal is immediate.
	rows := observedProvenance(t, noSleepClientFor(t, nil), "http://127.0.0.1:1/zen/v1/chat/completions")
	for i, row := range rows {
		if row.Class != ClassConnectionError.String() {
			t.Fatalf("row %d class = %s, want connection_error", i, row.Class)
		}
		wantProvenance(t, row, "transport", "target_connect", "not_sent")
	}
}

// TestProvenanceProxyConnectRefusedIsNotSent: the TCP connection to the proxy
// endpoint itself is refused. Phase is proxy_connect (the dial goes to the
// proxy, not the origin) and the request is provably unsent.
func TestProvenanceProxyConnectRefusedIsNotSent(t *testing.T) {
	c := noSleepClientFor(t, &config.Proxy{Type: config.ProxyHTTP, URL: "http://127.0.0.1:1"})
	row := terminalProvenance(t, c, "https://origin.invalid/zen/v1/chat/completions")
	wantProvenance(t, row, "transport", "proxy_connect", "not_sent")
}

// TestProvenanceProxyAuthIsNotSent: the proxy refused credentials. The 407 is
// read off the proxy's own CONNECT status line (connect.go), which is before
// any request byte exists — provably unsent, and typed as a credential
// verdict.
func TestProvenanceProxyAuthIsNotSent(t *testing.T) {
	pxy, _ := connect407Proxy(t)
	c := noSleepClientFor(t, &config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL})
	row := terminalProvenance(t, c, "https://origin.invalid/zen/v1/chat/completions")
	if row.Class != ClassProxyAuthError.String() {
		t.Fatalf("class = %s, want proxy_auth_error", row.Class)
	}
	wantProvenance(t, row, "transport", "proxy_auth", "not_sent")
}

// TestProvenanceOriginTLSFailureIsNotSent: the tunnel came up and the origin
// TLS handshake failed. Still pre-transmission — the request cannot be written
// before the handshake completes — so it is provable and replayable.
func TestProvenanceOriginTLSFailureIsNotSent(t *testing.T) {
	pxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// Establish the tunnel, then drop it: the client's ClientHello gets
		// EOF instead of a ServerHello.
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		_ = conn.Close()
	}))
	defer pxy.Close()

	c := noSleepClientFor(t, &config.Proxy{Type: config.ProxyHTTP, URL: pxy.URL})
	row := terminalProvenance(t, c, "https://origin.invalid/zen/v1/chat/completions")
	wantProvenance(t, row, "transport", "origin_tls", "not_sent")
}

// TestProvenanceSocks5HandshakeFailureIsNotSent: the SOCKS5 proxy accepts the
// TCP connection and closes before answering the greeting. The phase is the
// boundary that spoke the protocol, and the request is provably unsent.
func TestProvenanceSocks5HandshakeFailureIsNotSent(t *testing.T) {
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
			_ = conn.Close()
		}
	}()

	c := noSleepClientFor(t, &config.Proxy{Type: config.ProxySOCKS5, URL: "socks5://" + ln.Addr().String()})
	row := terminalProvenance(t, c, "https://origin.invalid/zen/v1/chat/completions")
	wantProvenance(t, row, "transport", "socks5_greeting", "not_sent")
}

// TestResponseHeaderTimeoutIsUnknownNotNotSent is the test the whole design
// exists for. The connection was established and the request was WRITTEN; the
// response headers never arrived. This process cannot prove whether the
// provider received and started executing the request, so the state must be
// unknown — claiming not_sent here is exactly the bug that duplicates a POST.
func TestResponseHeaderTimeoutIsUnknownNotNotSent(t *testing.T) {
	// A listener that accepts and then says nothing, ever.
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
			// Hold the conn open (read and discard) so the client's write
			// succeeds and its header wait is what times out.
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()

	// Same wiring as NewClient, with the response-header wait shortened: the
	// production value is config.ConnectTimeout (60s) and is not compressible
	// from a test.
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	c := &Client{Sleep: func(time.Duration) {}, Now: time.Now}
	c.HTTP = &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 200 * time.Millisecond,
		IdleConnTimeout:       config.IdleConnTimeout,
		DialContext:           recordingDialer(FailurePhaseTargetConnect, dialer.DialContext),
	}}

	row := terminalProvenance(t, c, "http://"+ln.Addr().String()+"/zen/v1/chat/completions")
	if row.Class != ClassTimeout.String() {
		t.Fatalf("class = %s, want timeout", row.Class)
	}
	wantProvenance(t, row, "transport", "response_headers", "unknown")
}

// TestRequestStateNeverClaimsNotSentWithoutADial pins the state machine
// directly, because the pooled-connection case cannot be forced end to end:
// net/http skips the dial entirely when it reuses a live conn, so a failure on
// that conn has no phase and must degrade to unknown. The trace is the only
// place this distinction can be made, and this table is its contract.
func TestRequestStateNeverClaimsNotSentWithoutADial(t *testing.T) {
	timeout := &timeoutErr{}
	cases := []struct {
		name      string
		trace     *dialTrace
		err       error
		wantPhase FailurePhase
		wantState RequestState
	}{
		{
			name:      "pooled connection, no dial in this attempt",
			trace:     &dialTrace{},
			err:       errors.New("connection reset by peer"),
			wantPhase: FailurePhaseNone,
			wantState: RequestStateUnknown,
		},
		{
			name:      "no trace at all (caller bypassed the wrapper)",
			trace:     nil,
			err:       errors.New("connection reset by peer"),
			wantPhase: FailurePhaseNone,
			wantState: RequestStateUnknown,
		},
		{
			name:      "dial failed at the recorded phase",
			trace:     &dialTrace{dialed: true, phase: FailurePhaseProxyConnect},
			err:       errors.New("connection refused"),
			wantPhase: FailurePhaseProxyConnect,
			wantState: RequestStateNotSent,
		},
		{
			name:      "connection established, transport never wrote through it",
			trace:     &dialTrace{dialed: true, dialOK: true, phase: FailurePhaseOriginTLS},
			err:       errors.New("use of closed network connection"),
			wantPhase: FailurePhaseNone,
			wantState: RequestStateNotSent,
		},
		{
			name:      "request written, header wait timed out",
			trace:     &dialTrace{dialed: true, dialOK: true, wrote: true},
			err:       timeout,
			wantPhase: FailurePhaseResponseHeaders,
			wantState: RequestStateUnknown,
		},
		{
			name:      "request written, read error before headers",
			trace:     &dialTrace{dialed: true, dialOK: true, wrote: true},
			err:       &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")},
			wantPhase: FailurePhaseResponseHeaders,
			wantState: RequestStateUnknown,
		},
		{
			name:      "request written, write error",
			trace:     &dialTrace{dialed: true, dialOK: true, wrote: true},
			err:       &net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")},
			wantPhase: FailurePhaseRequestWrite,
			wantState: RequestStateUnknown,
		},
		{
			name:      "request written, unattributable error type",
			trace:     &dialTrace{dialed: true, dialOK: true, wrote: true},
			err:       errors.New("something opaque"),
			wantPhase: FailurePhaseNone,
			wantState: RequestStateUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phase, state := tc.trace.provenance(tc.err)
			if phase != tc.wantPhase || state != tc.wantState {
				t.Fatalf("provenance = (%s, %s), want (%s, %s)",
					phase, state, tc.wantPhase, tc.wantState)
			}
			if state == RequestStateNotSent && tc.trace != nil && !tc.trace.dialed {
				t.Fatal("not_sent claimed without a dial — the exact quiet failure")
			}
		})
	}
}

// TestReplaySafeIsExactlyTransportNotSent: the one predicate the recovery
// contract is built on. Anything else — an HTTP verdict, an unproven state, a
// caller that went away — is not replayable.
func TestReplaySafeIsExactlyTransportNotSent(t *testing.T) {
	cases := []struct {
		f    Failure
		want bool
	}{
		{Failure{Origin: OriginTransport, RequestState: RequestStateNotSent}, true},
		{Failure{Origin: OriginTransport, RequestState: RequestStateUnknown}, false},
		{Failure{Origin: OriginTransport, RequestState: RequestStateResponseStarted}, false},
		{Failure{Origin: OriginUpstream, RequestState: RequestStateNotSent}, false},
		{Failure{Origin: OriginUpstream, RequestState: RequestStateResponseStarted}, false},
		{Failure{Origin: OriginClient}, false},
		{Failure{}, false},
	}
	for i, tc := range cases {
		if got := tc.f.ReplaySafe(); got != tc.want {
			t.Fatalf("case %d: ReplaySafe(%+v) = %t, want %t", i, tc.f, got, tc.want)
		}
	}
}

// TestClassifyTransportFailureZeroStateIsUnknown: the failure value built for
// an attempt whose trace proves nothing must carry unknown, not the zero-value
// optimisation someone might be tempted into. (not_sent is intentionally NOT
// the zero of RequestState.)
func TestClassifyTransportFailureZeroStateIsUnknown(t *testing.T) {
	f := classifyTransportFailure(context.Background(), errors.New("boom"), &dialTrace{})
	if f.Origin != OriginTransport || f.RequestState != RequestStateUnknown {
		t.Fatalf("failure = %+v, want transport/unknown", f)
	}
	if f.ReplaySafe() {
		t.Fatal("an unproven failure must not be replay-safe")
	}
}

// TestClassifyTransportFailureContextCancelIsClient: a downstream caller that
// went away is the caller's origin, and its state never authorises anything.
func TestClassifyTransportFailureContextCancelIsClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := classifyTransportFailure(ctx, context.Canceled, &dialTrace{dialed: true, phase: FailurePhaseTargetConnect})
	if f.Class != ClassContextCanceled || f.Origin != OriginClient {
		t.Fatalf("failure = %+v, want context_canceled/client", f)
	}
	if f.RequestState != RequestStateUnknown {
		t.Fatalf("state = %s, want unknown (a cancel authorises nothing)", f.RequestState)
	}
}

// TestProvenanceStringsAreStable pins the wire/log spellings: these strings are
// what the evidence rows and the cross-service contract are written in.
func TestProvenanceStringsAreStable(t *testing.T) {
	origins := map[Origin]string{
		OriginNone: "", OriginUpstream: "upstream",
		OriginTransport: "transport", OriginClient: "client",
	}
	for o, want := range origins {
		if got := o.String(); got != want {
			t.Fatalf("Origin(%d) = %q, want %q", o, got, want)
		}
	}
	states := map[RequestState]string{
		RequestStateUnknown: "unknown", RequestStateNotSent: "not_sent",
		RequestStateResponseStarted: "response_started",
	}
	for s, want := range states {
		if got := s.String(); got != want {
			t.Fatalf("RequestState(%d) = %q, want %q", s, got, want)
		}
	}
	if got := FailurePhaseNone.String(); got != "" {
		t.Fatalf("FailurePhaseNone = %q, want the empty string (absent field)", got)
	}
	for _, tc := range []struct {
		p    FailurePhase
		want string
	}{
		{FailurePhaseProxyConnect, "proxy_connect"},
		{FailurePhaseProxyTLS, "proxy_tls"},
		{FailurePhaseProxyAuth, "proxy_auth"},
		{FailurePhaseSocks5Greeting, "socks5_greeting"},
		{FailurePhaseSocks5Auth, "socks5_auth"},
		{FailurePhaseSocks5Connect, "socks5_connect"},
		{FailurePhaseConnectWrite, "connect_write"},
		{FailurePhaseConnectRead, "connect_read"},
		{FailurePhaseTargetConnect, "target_connect"},
		{FailurePhaseOriginTLS, "origin_tls"},
		{FailurePhaseRequestWrite, "request_write"},
		{FailurePhaseResponseHeaders, "response_headers"},
		{FailurePhaseResponseBody, "response_body"},
	} {
		if got := tc.p.String(); got != tc.want {
			t.Fatalf("FailurePhase(%d) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// TestRecordingConnMarksTheFirstWrite: the wrapper is what makes not_sent
// provable at all — a conn that carried request bytes must say so, before the
// underlying write even runs.
func TestRecordingConnMarksTheFirstWrite(t *testing.T) {
	trace := &dialTrace{}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	rc := &recordingConn{Conn: client, trace: trace}
	go func() { _, _ = io.Copy(io.Discard, server) }()

	if trace.provenance(nil); trace.wrote {
		t.Fatal("wrote flag set before any write")
	}
	if _, err := rc.Write([]byte("POST / HTTP/1.1\r\n")); err != nil {
		t.Fatal(err)
	}
	if !trace.wrote {
		t.Fatal("write not recorded — not_sent would be claimable after transmission")
	}
}

// TestRecordingDialerIsTransparentWithoutATrace: with no trace in the context
// the wrapper must return the dialer's conn untouched, so a direct caller (and
// every pre-migration test) observes no change at all.
func TestRecordingDialerIsTransparentWithoutATrace(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	dial := func(context.Context, string, string) (net.Conn, error) { return client, nil }
	conn, err := recordingDialer(FailurePhaseTargetConnect, dial)(context.Background(), "tcp", "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.(*recordingConn); ok {
		t.Fatal("conn was wrapped without a trace")
	}
	if conn != client {
		t.Fatal("conn was replaced")
	}
}

// timeoutErr is a net.Error that reports Timeout() — the shape net/http gives
// the ResponseHeaderTimeout failure.
type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "timeout awaiting response headers" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

var _ net.Error = (*timeoutErr)(nil)

// TestProvenanceFieldsCarryNoEgressIdentity: the provenance fields are new
// surfaces on the log line, so they must not become a channel for the egress's
// identity. They carry fixed vocabulary only — a credentialed proxy URL in the
// config must not reach them.
func TestProvenanceFieldsCarryNoEgressIdentity(t *testing.T) {
	c := noSleepClientFor(t, &config.Proxy{Type: config.ProxyHTTP, URL: "http://user:s3cret@127.0.0.1:1"})
	row := terminalProvenance(t, c, "https://origin.invalid/zen/v1/chat/completions")

	joined := row.Origin + " " + row.FailurePhase + " " + row.RequestState
	for _, bad := range []string{"s3cret", "user", "127.0.0.1", ":"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("provenance %q leaks %q", joined, bad)
		}
	}
}
