package upstream

// response_ownership_test.go — WHO AUTHORED an HTTP response is a property of
// the PATH it arrived on, never of the status code it carries (issue #63).
//
// The defect this file pins shut: a plain-http target behind an HTTP forward
// proxy rides Go's absolute-form path, where the proxy is an HTTP-level PEER
// that answers for itself — its own 407, its own 502/503 when it cannot reach
// the origin, a policy 403, a captive portal's 200 — and whose replies are
// indistinguishable on the wire from a relayed origin answer. Labelling every
// response `upstream` told the Injector, which owns provider-level retry, that
// the provider had answered when nothing proved it had.
//
// The fix is not a better guess. A status code cannot carry authorship (issue
// #6 forbids content sniffing: no header separates a proxy's 502 from an
// origin's), so the answer is read off the path, and where the path cannot
// prove the provider wrote the reply the origin is OriginAmbiguous — an
// explicit refusal to claim, relayed verbatim because a status cannot be
// re-litigated once it exists.
//
// The fixtures are real: real listeners, real proxies, real origins, the
// production dial wrappers. A hand-built Failure proves only that a switch has
// cases.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// originUnreachable is a base whose host cannot resolve, so a request that
// reaches the origin at all is impossible: anything that answers did so
// somewhere on the path. It is the witness that makes "the provider never saw
// this request" a FACT rather than an assumption — which is exactly why the
// label cannot claim it.
const originUnreachable = "http://origin.invalid"

// answeringProxy is an HTTP forward proxy that answers every request ITSELF
// with the given status and body, and counts what it saw. It never contacts the
// target: through originUnreachable it could not.
func answeringProxy(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	srv := fwdProxyForTest(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		// The absolute-form request line is what makes this hop the proxy's to
		// answer: it received a full HTTP message, not a tunnel.
		if !r.URL.IsAbs() {
			t.Errorf("request line = %q, want an absolute-form URL on the proxy path", r.RequestURI)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return srv, &seen
}

// relayingProxy is an HTTP forward proxy that GENUINELY forwards absolute-form
// requests to their target and returns the origin's own response — the shape
// that makes an origin verdict arrive over an intermediated path.
func relayingProxy(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	srv := fwdProxyForTest(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	return srv, &seen
}

// fwdProxyForTest is the plain httptest listener the two proxies above run on
// (kept separate from e2e's own helper of the same shape — different packages).
func fwdProxyForTest(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// statusFamily is the class each status must keep carrying on an intermediated
// path. The point of the table: the CLASS still sorts the response by what the
// status was, while the ORIGIN states who may have written it. Renaming or
// hiding the family would lose information this layer does have.
var statusFamily = map[int]Class{
	200: ClassSuccess,
	403: ClassClientError,
	407: ClassClientError,
	429: ClassUpstream429,
	502: ClassUpstream5xx,
	503: ClassUpstream5xx,
}

// TestProxyAuthoredResponseIsNeverAttributedToTheProvider: a forward proxy
// answers with its own status and body and the origin is UNREACHABLE by
// construction. The label must not claim the provider wrote it — and must not
// claim the proxy did either, because the wire cannot tell this response apart
// from a relayed one (the next test is that relayed one, and it carries the
// same label on purpose).
//
// Every row is also terminal: relayed to the caller as the only answer there
// is, with no fallback, no health mark, and no replay — the request byte
// provably left this process, and what became of it is not knowable here.
func TestProxyAuthoredResponseIsNeverAttributedToTheProvider(t *testing.T) {
	for _, status := range []int{200, 403, 407, 429, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			proxy, seen := answeringProxy(t, status, `{"error":{"message":"answered by the proxy"}}`)
			c := phaseClient(t, config.ProxyHTTP, proxy.URL, nil)

			resp, uerr, failure := c.DoClassified(context.Background(),
				originUnreachable+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))

			if status == 200 {
				if uerr != nil || resp == nil {
					t.Fatalf("uerr=%v resp=%v, want the proxy's 200 served", uerr, resp)
				}
				_ = resp.Body.Close()
			} else {
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatal("a >=400 response must not be delivered as a served response")
				}
				if uerr == nil || uerr.Status != status {
					t.Fatalf("uerr = %v, want the proxy's %d surfaced verbatim", uerr, status)
				}
				// The body is relayed as-is, because the wire cannot say whose
				// it is: this is the proxy's own text, and it is what the
				// caller gets.
				if uerr.Message != "answered by the proxy" {
					t.Fatalf("message = %q, want the answering side's own body", uerr.Message)
				}
			}

			if happened := failure; happened.Origin != OriginAmbiguous {
				t.Fatalf("origin = %s (%+v), want ambiguous: nothing on this path proves the PROVIDER wrote this", happened.Origin, happened)
			}
			if failure.RequestState != RequestStateUnknown {
				t.Fatalf("request_state = %s, want unknown", failure.RequestState)
			}
			if failure.Class != statusFamily[status] {
				t.Fatalf("class = %s, want %s (the status family is still carried)", failure.Class, statusFamily[status])
			}
			if failure.ReplaySafe() {
				t.Fatalf("an ambiguous response is replay-safe: %+v — a re-send could duplicate provider work", failure)
			}
			if failure.MarksEgressHealth() {
				t.Fatalf("an ambiguous response marked the egress: %+v — a proxy answering FOR an origin is not evidence the egress is broken", failure)
			}
			if got := seen.Load(); got != 1 {
				t.Fatalf("proxy requests = %d, want exactly 1 (a response is terminal, ambiguous or not)", got)
			}
		})
	}
}

// TestRelayedOriginVerdictIsIndistinguishableOnAnIntermediatedPath is the other
// half of the honest answer, and the reason the label above is `ambiguous`
// rather than a proxy verdict: here the ORIGIN's own 429 arrives over the same
// kind of hop, and there is nothing on the wire that separates it from the
// proxy's own 429 above. Two failures with opposite causes, one label — because
// the alternative is picking one and being wrong half the time.
func TestRelayedOriginVerdictIsIndistinguishableOnAnIntermediatedPath(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"origin is rate limiting","type":"rate_limit_error"}}`)
	}))
	defer origin.Close()

	proxy, seen := relayingProxy(t)
	c := phaseClient(t, config.ProxyHTTP, proxy.URL, nil)

	resp, uerr, failure := c.DoClassified(context.Background(),
		origin.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a 429 must not be delivered as a served response")
	}
	if uerr == nil || uerr.Status != http.StatusTooManyRequests {
		t.Fatalf("uerr = %v, want the origin's 429 relayed", uerr)
	}
	// The origin's OWN body, which is what distinguishes this case in
	// substance and never in authorship.
	if uerr.Message != "origin is rate limiting" {
		t.Fatalf("message = %q, want the origin's own text", uerr.Message)
	}
	if failure.Origin != OriginAmbiguous || failure.RequestState != RequestStateUnknown {
		t.Fatalf("provenance = %+v, want ambiguous/unknown — the same label the proxy's own 429 got", failure)
	}
	if failure.Class != ClassUpstream429 {
		t.Fatalf("class = %s, want upstream_429", failure.Class)
	}
	if failure.ReplaySafe() {
		t.Fatalf("a relayed origin verdict is replay-safe: %+v", failure)
	}
	if got := seen.Load(); got != 1 {
		t.Fatalf("proxy requests = %d, want exactly 1", got)
	}
}

// TestEndToEndPathsAttributeTheOrigin is the complementary pin: the label must
// not be applied to every proxied request, only to the intermediated ones. A
// direct hop, a SOCKS5 tunnel and a CONNECT tunnel all hand the request to the
// target over a transport-level tunnel an intermediary cannot write HTTP
// messages on, so a response on any of them is the origin's.
//
// Both a 200 and a 429 are exercised per path: authorship is a property of the
// path, so it must not move with the status.
func TestEndToEndPathsAttributeTheOrigin(t *testing.T) {
	plain := phaseOrigin(t) // 200-speaking plain-http origin
	tlsOrigin := httpsOKOrigin(t)

	// Two origins per scheme, so the same path is walked for a served response
	// and for a verdict.
	plainErr := statusOrigin(t, http.StatusTooManyRequests)
	tlsErr := tlsStatusOrigin(t, http.StatusTooManyRequests)

	connectProxy := tunnelingProxy(t) // real CONNECT tunnels to r.Host
	socks := newSocks5Fake(t, 0x00, 0x00, 0x00)

	paths := []struct {
		name   string
		client *Client
		ok     string
		err    string
	}{
		{
			name:   "direct",
			client: noSleepClient(NewClientFor(nil)),
			ok:     plain.URL,
			err:    plainErr.URL,
		},
		{
			name:   "socks5 tunnel",
			client: phaseClient(t, config.ProxySOCKS5, socks.url, nil),
			ok:     plain.URL,
			err:    plainErr.URL,
		},
		{
			name:   "connect tunnel",
			client: phaseClient(t, config.ProxyHTTP, connectProxy.URL, phasePool(t, tlsOrigin, tlsErr)),
			ok:     tlsOrigin.URL,
			err:    tlsErr.URL,
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("served", func(t *testing.T) {
				resp, uerr, failure := tc.client.DoClassified(context.Background(),
					tc.ok+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
				if uerr != nil {
					t.Fatalf("uerr = %v, want the origin's 200", uerr)
				}
				defer func() { _ = resp.Body.Close() }()
				_, _ = io.ReadAll(resp.Body)
				if failure.Origin != OriginUpstream || failure.RequestState != RequestStateResponseStarted {
					t.Fatalf("provenance = %+v, want upstream/response_started", failure)
				}
				if failure.Class != ClassSuccess {
					t.Fatalf("class = %s, want success", failure.Class)
				}
			})
			t.Run("verdict", func(t *testing.T) {
				resp, uerr, failure := tc.client.DoClassified(context.Background(),
					tc.err+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatal("a 429 must not be delivered as a served response")
				}
				if uerr == nil || uerr.Status != http.StatusTooManyRequests {
					t.Fatalf("uerr = %v, want the origin's 429", uerr)
				}
				if failure.Origin != OriginUpstream || failure.RequestState != RequestStateResponseStarted {
					t.Fatalf("provenance = %+v, want upstream/response_started", failure)
				}
				if failure.Class != ClassUpstream429 {
					t.Fatalf("class = %s, want upstream_429", failure.Class)
				}
			})
		})
	}
}

// TestHopPathTable pins the scheme→path table itself: the ONLY shape with an
// intervening HTTP peer is a plain-http hop on a client that has a CONNECT
// transport (an http/https proxy egress). Everything else is end-to-end.
func TestHopPathTable(t *testing.T) {
	httpProxyURL := "http://127.0.0.1:1"
	httpsProxyURL := "https://127.0.0.1:1"
	cases := []struct {
		name     string
		client   *Client
		http     bool // plain-http hop is intermediated
		https    bool // https hop is intermediated
		whyItIsA bool
	}{
		{
			name:   "direct",
			client: noSleepClient(NewClientFor(nil)),
		},
		{
			name:   "no proxy at all (NewClient)",
			client: NewClient(),
		},
		{
			// The absolute-form path: the proxy receives a full HTTP message
			// and is free to answer it.
			name:   "http proxy egress",
			client: phaseClient(t, config.ProxyHTTP, httpProxyURL, nil),
			http:   true,
		},
		{
			name:   "https proxy egress",
			client: phaseClient(t, config.ProxyHTTPS, httpsProxyURL, nil),
			http:   true,
		},
		{
			// SOCKS5 CONNECTs a transport-level tunnel: the proxy never sees an
			// HTTP message, so it cannot write one.
			name:   "socks5 egress",
			client: phaseClient(t, config.ProxySOCKS5, "socks5://127.0.0.1:1", nil),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, hop := range []struct {
				scheme        string
				intermediated bool
			}{
				{"http", tc.http},
				{"https", tc.https},
				// Not a real scheme; the table must fall to end-to-end for
				// anything it does not know — the zero value authorises nothing
				// stronger (provenance.go).
				{"ftp", false},
			} {
				got := tc.client.hopPathOf(hop.scheme).intermediated
				if got != hop.intermediated {
					t.Fatalf("hopPathOf(%q).intermediated = %v, want %v", hop.scheme, got, hop.intermediated)
				}
			}
		})
	}
}

// TestAmbiguousResponseMovesNothingAndMarksNothing is the executor-level
// consequence, with health ENABLED at threshold 1 (the harshest setting) and a
// healthy sibling egress available: an ambiguous 502 must not fall back (a
// re-send could duplicate provider work) and must not cool the egress (a proxy
// answering for an origin says nothing about whether it can carry traffic).
//
// The evidence row must say the same thing the wire does.
func TestAmbiguousResponseMovesNothingAndMarksNothing(t *testing.T) {
	proxy, _ := answeringProxy(t, http.StatusBadGateway, `{"error":{"message":"proxy cannot reach the origin"}}`)

	reg := health.New()
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		switch e.ID {
		case "a":
			return phaseClient(t, config.ProxyHTTP, proxy.URL, nil), true
		case "b":
			return plainDirect(t), true
		}
		return nil, false
	}, reg, NewLimiter())
	plan := routing.RoutePlan{
		RouteID:  "r",
		Attempts: []string{"a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}},
	}

	rec := NewRecorder()
	resp, id, attempts, failure, uerr := exec.ExecuteObserved(context.Background(),
		originUnreachable+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"), plan, policy(true, 3), rec)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("an ambiguous 502 must not be delivered as a served response")
	}
	if id != "a" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want a/1 — an ambiguous response ends the request where it arrived", id, attempts)
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %v, want the ambiguous 502 surfaced", uerr)
	}
	if failure.Origin != OriginAmbiguous {
		t.Fatalf("origin = %s, want ambiguous", failure.Origin)
	}
	if !reg.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("egress a was cooled by an ambiguous response — a forward proxy answering for an origin is not egress evidence")
	}

	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("evidence rows = %d, want 1 (one logical attempt):\n%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Origin != OriginAmbiguous.String() || row.RequestState != RequestStateUnknown.String() {
		t.Fatalf("row provenance = %s/%s, want ambiguous/unknown", row.Origin, row.RequestState)
	}
	if row.HealthDecision != HealthNeutral {
		t.Fatalf("row health = %q, want neutral", row.HealthDecision)
	}
	if row.FallbackDecision != FallbackStop {
		t.Fatalf("row fallback = %q, want stop", row.FallbackDecision)
	}
	if row.Class != ClassUpstream5xx.String() {
		t.Fatalf("row class = %q, want upstream_5xx (the status family is still recorded)", row.Class)
	}
}

// statusOrigin is a plain-http origin that answers one status with a JSON error
// body — the target end of the absolute-form path.
func statusOrigin(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"origin says no"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tlsStatusOrigin is the same origin over TLS — the target end of a CONNECT
// tunnel.
func tlsStatusOrigin(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"origin says no"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}
