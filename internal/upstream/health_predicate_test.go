package upstream

// health_predicate_test.go — the REPLAY permission and the EGRESS-HEALTH mark
// are two decisions off one failure, not one decision (issue #62).
//
//	ReplaySafe()         may this REQUEST be re-sent without the provider
//	                     doing the work twice?      → gates the egress move
//	MarksEgressHealth()  is this EGRESS broken — would the next request fail
//	                     the same way?              → gates the health mark
//
// The two agree whenever the failing step is one this process performs against
// the egress endpoint itself: a proxy TCP connect, a proxy TLS handshake, a
// proxy auth refusal, a SOCKS5 negotiation, a CONNECT write. They diverge at
// the destination end of the path, where a failure is replay-safe and says
// nothing about the egress — a target TCP connect refusal, an origin TLS
// failure, a CONNECT refusal by a proxy that could not reach the origin.
//
// The divergence is not cosmetic. Marking those failures turns a PROVIDER
// outage into a GATEWAY outage: every egress in the pool fails at
// target_connect, every failure arms the threshold, and the pool quarantines
// itself for the whole cooldown — outliving the outage it recorded. That is
// the failure mode this file exists to keep out (TestOutageAttribution...).
//
// The fixtures are real: real listeners, real proxies, the production dial
// wrappers. A hand-built Failure proves only that a switch has cases.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// downTargetClient is an egress that fails at target_connect: its transport
// dials a closed port, so nothing is written (replay-safe) and the failing
// step is the connection to the DESTINATION.
func downTargetClient(t *testing.T) *Client {
	t.Helper()
	c := NewClient()
	c.HTTP.Transport = dialTo(closedAddr(t))
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// downProxyClient is an egress whose PROXY ENDPOINT is unreachable: the TCP
// dial to the proxy fails, which is a step this process performs against the
// egress itself. It is also replay-safe — no request byte exists — which is
// exactly why the two predicates have to be asked separately.
func downProxyClient(t *testing.T) *Client {
	t.Helper()
	return phaseClient(t, config.ProxyHTTPS, "https://"+closedAddr(t), nil)
}

// serveAttempt runs ONE request through an executor whose egress a is the
// client under test and whose egress b is a direct client that can serve the
// URL, with health ENABLED at threshold 1 (testHealthPolicy) so a single mark
// would cool the egress. It returns the winning egress, the attempts consumed,
// and the registry the decisions were recorded in.
func serveAttempt(t *testing.T, a *Client, url string) (id string, attempts int, reg *health.Registry) {
	t.Helper()
	reg = health.New()
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		switch e.ID {
		case "a":
			return a, true
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
	resp, id, attempts, _, uerr := exec.Execute(
		context.Background(), url, staticHeaders(), []byte(`{}`), plan, policy(true, 3))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if uerr != nil {
		t.Fatalf("uerr = %v, want the request served by egress b", uerr)
	}
	return id, attempts, reg
}

// TestReplaySafetyAndEgressHealthAreSeparateDecisions is the divergence proof.
// Two failures that differ in ONE respect — whose fault the failing step is —
// and agree in every decision the recovery contract used to make from them:
// both are replay-safe, both move the request to the sibling egress, and the
// request outcome is identical. Only the health verdict differs.
func TestReplaySafetyAndEgressHealthAreSeparateDecisions(t *testing.T) {
	origin := phaseOrigin(t)
	url := origin.URL + "/zen/v1/chat/completions"

	cases := []struct {
		name        string
		a           *Client
		wantPhase   string
		wantMarks   bool
		wantHealthy bool
	}{
		{
			// The destination refused the connection. Nothing was sent, so the
			// request may move — but the egress did nothing wrong.
			name:        "destination owns the failing step",
			a:           downTargetClient(t),
			wantPhase:   "target_connect",
			wantMarks:   false,
			wantHealthy: true,
		},
		{
			// The egress's own proxy endpoint was not even reachable. Same
			// replay-safety, same move, different owner.
			name:        "egress owns the failing step",
			a:           downProxyClient(t),
			wantPhase:   "proxy_connect",
			wantMarks:   true,
			wantHealthy: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := phaseProbe(t, tc.a, url)
			if failure.Origin != OriginTransport || failure.Phase.String() != tc.wantPhase {
				t.Fatalf("provenance = %+v, want transport/%s", failure, tc.wantPhase)
			}
			if !failure.ReplaySafe() {
				t.Fatalf("failure not replay-safe: %+v — both cases must move the request", failure)
			}
			if got := failure.MarksEgressHealth(); got != tc.wantMarks {
				t.Fatalf("MarksEgressHealth = %v, want %v for phase %s (ReplaySafe = %v)",
					got, tc.wantMarks, tc.wantPhase, failure.ReplaySafe())
			}

			id, attempts, reg := serveAttempt(t, tc.a, url)
			if id != "b" || attempts != 2 {
				t.Fatalf("id=%q attempts=%d, want the request moved to b in both cases", id, attempts)
			}
			if got := reg.Healthy(healthKey("a"), testHealthPolicy); got != tc.wantHealthy {
				t.Fatalf("a healthy after a %s failure = %v, want %v (threshold 1: one mark cools it)",
					tc.wantPhase, got, tc.wantHealthy)
			}
		})
	}
}

// TestOutageAttributionDecidesWhetherThePoolIsQuarantined is the availability
// proof, with its own control: the SAME three-egress pool, the same
// threshold-1 policy, the same exhausted plan — and two attributions that must
// lead to opposite registry states.
//
//   - every egress failing at target_connect (the provider's origin is
//     unreachable) is a destination-side failure: each is replay-safe, so the
//     request still walks the whole pool, and NONE may be cooled. Under the
//     pre-#62 predicate the first failure armed the cooldown for its egress,
//     and a three-egress pool went dark for the cooldown's whole duration —
//     for the provider's outage.
//   - every egress failing at proxy_connect (the shared egress layer is down)
//     is the egress's own failure, and the SAME run must quarantine all three,
//     which is what proves the policy under test really can arm.
func TestOutageAttributionDecidesWhetherThePoolIsQuarantined(t *testing.T) {
	const ids = "abc"

	cases := []struct {
		name            string
		down            func(*testing.T, *executorFixture, string)
		wantQuarantined bool
	}{
		{
			name:            "destination unreachable: nothing is the egress's fault",
			down:            func(t *testing.T, f *executorFixture, id string) { f.deadEgress(t, id) },
			wantQuarantined: false,
		},
		{
			name: "egress endpoint unreachable: all of them are",
			down: func(t *testing.T, f *executorFixture, id string) {
				f.clients[id] = downProxyClient(t)
			},
			wantQuarantined: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200, "c": 200})
			defer f.Close()
			for _, id := range []string{"a", "b", "c"} {
				tc.down(t, f, id)
			}

			rec := NewRecorder()
			resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
				context.Background(), f.servers["a"].URL,
				func() map[string]string { return map[string]string{} },
				[]byte(`{}`), f.plan("r", "a", "b", "c"), policy(true, 3), rec)

			// The request walks the pool either way: every failure is
			// replay-safe, so the budget of three is consumed and the LAST
			// egress's own verdict is what the caller sees.
			if resp != nil || uerr == nil {
				t.Fatalf("resp=%v uerr=%v, want the last real transport verdict", resp, uerr)
			}
			if attempts != 3 || id != "c" {
				t.Fatalf("attempts=%d id=%q, want all three egresses tried", attempts, id)
			}

			rows := rec.Rows()
			if len(rows) != 3 {
				t.Fatalf("rows = %d, want one per failed attempt", len(rows))
			}
			wantHealth := HealthNeutral
			if tc.wantQuarantined {
				wantHealth = HealthMarked
			}
			for i, row := range rows {
				if row.HealthDecision != wantHealth {
					t.Fatalf("row %d (%s): health = %q, want %q", i, row.FailurePhase, row.HealthDecision, wantHealth)
				}
			}
			if rows[0].FallbackDecision != FallbackYes || rows[1].FallbackDecision != FallbackYes ||
				rows[2].FallbackDecision != FallbackStop {
				t.Fatalf("fallback decisions = %q/%q/%q, want fallback/fallback/stop (the budget stopped the last one)",
					rows[0].FallbackDecision, rows[1].FallbackDecision, rows[2].FallbackDecision)
			}

			// The registry is what the ROUTER's eligibility filter reads before
			// it builds the next plan (server.go), so "quarantined" here is
			// exactly "gone from the rotation" there.
			for _, eg := range ids {
				key := healthKey(string(eg))
				want := !tc.wantQuarantined
				if got := f.health.Healthy(key, testHealthPolicy); got != want {
					t.Fatalf("egress %c healthy = %v, want %v", eg, got, want)
				}
			}
		})
	}
}

// TestMarkingFailureDoesNotMoveTheRequest is the divergence in the other
// direction, and the one that carries a safety argument: a failure can be
// attributed to the EGRESS — marked — and still not authorise a move, because
// the replay permission is about the request, not about the egress.
//
// The sequence: hop 1 is written to the proxy and answered with a 307, so the
// logical call has transmitted and nothing after it may claim not_sent
// (issue #60); the response tells the client to close the connection, so hop 2
// must dial again; the proxy endpoint is gone by then. The failure is a
// proxy_connect on a call that was already answered — marked, and not
// replayable.
func TestMarkingFailureDoesNotMoveTheRequest(t *testing.T) {
	deadHop := closedAddr(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Connection: close is LOAD-BEARING: without it the redirect would ride
		// the pooled connection, hop 2 would perform no dial at all, and the
		// failure would degrade to unattributable (FailurePhaseNone) rather
		// than naming the proxy hop.
		w.Header().Set("Connection", "close")
		w.Header().Set("Location", "http://"+deadHop+"/zen/v1/chat/completions")
		w.WriteHeader(http.StatusTemporaryRedirect)
		// The egress layer disappears with the redirect: hop 2 has to dial the
		// proxy again, and the listener is closed.
		_ = srv.Listener.Close()
	}))
	t.Cleanup(srv.Close)

	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxyHTTP, URL: srv.URL}))
	reg := health.New()
	exec := NewExecutor(func(*config.Egress) (*Client, bool) { return c, true }, reg, nil)
	plan := routing.RoutePlan{
		RouteID:  "r",
		Attempts: []string{"a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}},
	}
	rec := NewRecorder()

	resp, id, attempts, failure, uerr := exec.ExecuteObserved(
		context.Background(), "http://origin.example/zen/v1/chat/completions",
		staticHeaders(), []byte(`{}`), plan, policy(true, 3), rec)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a transport failure must not be delivered as a response")
	}
	if uerr == nil {
		t.Fatal("want the hop-2 proxy dial failure surfaced")
	}
	if attempts != 1 || id != "a" {
		t.Fatalf("attempts=%d id=%q, want a single attempt — hop 1 transmitted, so nothing may be replayed", attempts, id)
	}
	if failure.Phase != FailurePhaseProxyConnect {
		t.Fatalf("phase = %q, want proxy_connect (the failing step is the proxy hop)", failure.Phase)
	}
	// The call was answered — the 307 IS a response — so the state is
	// response_started, the strongest of the three refusals (issue #60).
	if failure.RequestState != RequestStateResponseStarted {
		t.Fatalf("request state = %s, want response_started: hop 1 was written and answered", failure.RequestState)
	}
	if failure.ReplaySafe() {
		t.Fatalf("failure reported replay-safe after hop 1 transmitted: %+v", failure)
	}
	if !failure.MarksEgressHealth() {
		t.Fatalf("failure did not mark the egress: %+v — the proxy endpoint stopped accepting", failure)
	}

	// The two decisions, side by side in the record: marked, and stopped here.
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].HealthDecision != HealthMarked || rows[0].FallbackDecision != FallbackStop {
		t.Fatalf("row = marked? %q fallback? %q, want marked + stop", rows[0].HealthDecision, rows[0].FallbackDecision)
	}
	if reg.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("the mark did not reach the registry: a proxy_connect failure is the egress's own")
	}
}

// TestPhaseAttributionDecidesTheHealthMark pins the mapping from every phase
// this package can record to the mark, including the phases whose own test
// files produce them behaviourally (proxy_phase_test.go, the socks5 and
// integration suites). It is the place a NEW phase has to be argued onto the
// marking list: the default is the neutral side, mirroring RequestStateUnknown.
func TestPhaseAttributionDecidesTheHealthMark(t *testing.T) {
	cases := []struct {
		phase FailurePhase
		marks bool
		why   string
	}{
		{FailurePhaseProxyConnect, true, "the egress endpoint did not accept the connection"},
		{FailurePhaseProxyTLS, true, "the egress endpoint's TLS is unusable"},
		{FailurePhaseProxyAuth, true, "the egress refused our credentials"},
		{FailurePhaseSocks5Greeting, true, "the egress does not speak SOCKS5"},
		{FailurePhaseSocks5Auth, true, "the egress refused our credentials"},
		{FailurePhaseConnectWrite, true, "the egress stopped taking our protocol bytes"},
		{FailurePhaseConnectRead, false, "the egress's answer about the destination"},
		{FailurePhaseSocks5Connect, false, "the egress's answer about the destination"},
		{FailurePhaseTargetConnect, false, "the destination refused"},
		{FailurePhaseOriginTLS, false, "the destination's handshake failed"},
		{FailurePhaseNone, false, "no step attributable — an honest gap is not evidence"},
		{FailurePhaseRequestWrite, false, "after transmission: no egress evidence either way"},
		{FailurePhaseResponseHeaders, false, "after transmission"},
		{FailurePhaseResponseBody, false, "after transmission"},
	}

	for _, tc := range cases {
		// The pre-transmission shape: a call that never wrote a byte.
		pre := Failure{
			Class: ClassConnectionError, Origin: OriginTransport,
			Phase: tc.phase, RequestState: RequestStateNotSent,
		}
		if got := pre.MarksEgressHealth(); got != tc.marks {
			t.Fatalf("%s: MarksEgressHealth = %v, want %v (%s)", tc.phase, got, tc.marks, tc.why)
		}
		// Origin gates the mark independently of the phase: neither a provider
		// verdict, nor an answer of unprovable authorship (issue #63), nor the
		// caller hanging up is evidence about the egress.
		for _, o := range []Origin{OriginNone, OriginUpstream, OriginAmbiguous, OriginClient} {
			f := Failure{Origin: o, Phase: tc.phase, RequestState: RequestStateNotSent}
			if f.MarksEgressHealth() {
				t.Fatalf("%s with origin %q must never mark the egress", tc.phase, o)
			}
		}
	}

	// ... and the two predicates are NOT nested, in either direction. A later
	// hop of a logical call can fail at the PROXY endpoint while an earlier hop
	// already transmitted (issue #60 × #62): marked, and not replayable. The
	// mark is a scheduling fact about the egress; it authorises no re-send.
	later := Failure{
		Class: ClassConnectionError, Origin: OriginTransport,
		Phase: FailurePhaseProxyConnect, RequestState: RequestStateUnknown,
	}
	if !later.MarksEgressHealth() {
		t.Fatal("a proxy-hop failure on a call that already transmitted is still egress evidence")
	}
	if later.ReplaySafe() {
		t.Fatal("...and it must not authorise a replay: the request may have been executed")
	}
}
