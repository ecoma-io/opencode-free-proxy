package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"net"
	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// scriptedRecorder collects upstream calls per egress id.
type scriptedRecorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *scriptedRecorder) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

func (r *scriptedRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

// scriptedUpstream serves every request with the given status; the recorder
// tags it by egress name.
func scriptedUpstream(t *testing.T, name string, status int, rec *scriptedRecorder) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		if rec.calls == nil {
			rec.calls = map[string]int{}
		}
		rec.calls[name]++
		rec.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
	}))
}

// executorFixture wires one httptest upstream per egress, each returning its
// own fixed status. plan/policy are sized to the caller's scenario.
type executorFixture struct {
	servers map[string]*httptest.Server
	clients map[string]*Client
	rec     *scriptedRecorder
	exec    *Executor
	health  *health.Registry
	slots   *Limiter
	// rt is the fixture's snapshot: one egress per scripted server, used by
	// plan() to resolve Attempts into *config.Egress exactly like the
	// scheduler does in production.
	rt *config.Runtime
}

func newExecutorFixture(t *testing.T, statuses map[string]int) *executorFixture {
	t.Helper()
	f := &executorFixture{
		servers: map[string]*httptest.Server{},
		rec:     &scriptedRecorder{calls: map[string]int{}},
		health:  health.New(),
		slots:   NewLimiter(),
	}
	// No registry-wide Configure: since issue #6 the health POLICY rides the
	// AttemptPolicy (pinned per request snapshot), not the registry. Fixtures
	// pass testHealthPolicy (threshold 1) via policy(), so a single observed
	// failure flips eligibility in tests.
	for id, status := range statuses {
		f.servers[id] = scriptedUpstream(t, id, status, f.rec)
	}
	f.clients = map[string]*Client{}
	for id := range statuses {
		c := NewClient()
		// Pin each egress's dial to ITS OWN listener, independent of the URL
		// host (production differentiates egresses by proxy transport; here
		// the dial is the differentiator).
		c.HTTP.Transport = dialTo(f.servers[id].Listener.Addr().String())
		f.clients[id] = c
	}
	f.rt = fixtureRuntime(statuses)
	f.exec = NewExecutor(func(e *config.Egress) (*Client, bool) {
		c, ok := f.clients[e.ID]
		return c, ok
	}, f.health, f.slots)
	return f
}

// dialTo is the fixture transport: a plain http.Transport whose every dial
// lands on addr, so an egress is identified by its listener and the request
// URL's host is irrelevant.
//
// The dial is wrapped in recordingDialer exactly as production wraps it
// (client.go/transport.go). Without that wrapper the attempt carries no dial
// trace, and a fixture failure would classify as request_state=unknown — which
// the executor correctly refuses to fall back from. The wrappers are what make
// a fixture failure replay-safe, so the wrappers are part of the fixture.
func dialTo(addr string) *http.Transport {
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Transport{DialContext: recordingDialer(FailurePhaseTargetConnect, dial)}
}

// deadEgress re-points an egress's transport at a closed port: every attempt
// fails at target_connect with nothing written. That is the ONLY failure shape
// the executor may fall back from (upstream.Failure.ReplaySafe), so every test
// that wants a fallback has to build it this way — a provider STATUS can no
// longer produce one.
func (f *executorFixture) deadEgress(t *testing.T, id string) {
	t.Helper()
	c, ok := f.clients[id]
	if !ok {
		t.Fatalf("fixture has no egress %q", id)
	}
	c.HTTP.Transport = dialTo(closedAddr(t))
}

// fixtureRuntime builds a snapshot the same shape production gets from a
// config file: numeric ids sorted for determinism, one route over all of
// them. Resolve suceeds: ids are unique and routes reference only present
// egresses.
func fixtureRuntime(statuses map[string]int) *config.Runtime {
	ids := make([]string, 0, len(statuses))
	for id := range statuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	file := config.File{
		Egress: make([]config.Egress, 0, len(ids)),
		Routes: []config.Route{{ID: "r", Egress: ids}},
	}
	for _, id := range ids {
		file.Egress = append(file.Egress, config.Egress{ID: id})
	}
	rt, err := file.Resolve()
	if err != nil {
		panic(fmt.Sprintf("fixture runtime: %v", err))
	}
	return rt
}

func (f *executorFixture) Close() {
	for _, s := range f.servers {
		s.Close()
	}
}

// plan builds a RoutePlan from the route + eligible heads (no scheduler),
// resolving each head against the fixture snapshot — the production shape
// Scheduler.Plan produces. A head absent from the runtime yields a nil
// Egresses slot, which the executor skips.
func (f *executorFixture) plan(routeID string, heads ...string) routing.RoutePlan {
	p := routing.RoutePlan{RouteID: routeID, Strategy: config.StrategyRoundRobin, Attempts: heads}
	p.Egresses = make([]*config.Egress, len(heads))
	for i, id := range heads {
		if e, ok := f.rt.Egress(id); ok {
			p.Egresses[i] = e
		}
	}
	return p
}

// testHealthPolicy is the health policy every fixture request pins: enabled,
// threshold 1 (one observed failure arms), one-minute cooldown.
var testHealthPolicy = health.Policy{Enabled: true, Threshold: 1, Cooldown: time.Minute}

// healthKey maps a fixture egress id to its health identity. Fixture
// egresses are direct, so this is exactly the key the executor observes
// (id + separator + "direct").
func healthKey(id string) string {
	return (&config.Egress{ID: id}).HealthKey()
}

// policy builds an AttemptPolicy straight from the two knobs the tests vary,
// pinning the shared test health policy like the handler pins one per
// snapshot.
func policy(fallback bool, max int) AttemptPolicy {
	return AttemptPolicy{FallbackEnabled: fallback, MaxAttempts: max, HealthPolicy: testHealthPolicy}
}
func TestExecuteFirstEgressServes(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200})
	defer f.Close()

	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL+"/zen/v1/chat/completions",
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "a" || attempts != 1 || failure.Class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d failure.Class=%s", id, attempts, failure.Class)
	}
	if f.rec.total() != 1 {
		t.Fatalf("upstream calls = %d, want 1", f.rec.total())
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("success must mark the egress healthy")
	}
}

// TestExecuteFallsBackOnPreRequestTransportFailure: the ONLY shape that moves
// an egress — a dial that provably never put a request on the wire. a's
// transport points at a closed port; b serves; the request moves, and the
// winner's success marks b healthy.
//
// a is NOT marked unhealthy, and that is the rule rather than an oversight
// (issue #62): `target_connect` is the DIRECT path to the destination, so this
// failure is evidence about the destination — not about a's ability to carry a
// request. The move and the mark are separate decisions; the health-marking
// side of the split is pinned by health_predicate_test.go, which drives a
// failure the EGRESS owns.
func TestExecuteFallsBackOnPreRequestTransportFailure(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.deadEgress(t, "a")

	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 2 || failure.Class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d failure.Class=%s", id, attempts, failure.Class)
	}
	if f.rec.count("a") != 0 || f.rec.count("b") != 1 {
		t.Fatalf("calls a=%d b=%d, want 0/1 (a's dial never reached a server)", f.rec.count("a"), f.rec.count("b"))
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a target_connect failure is the destination's fault: it may move the request but must not quarantine the egress")
	}
	if !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("b's success must mark it healthy")
	}
}

// TestExecuteProviderVerdictStopsOnItsEgress: the whole point of the recovery
// split. Every provider answer — 429, 4xx, 5xx — is terminal: it is relayed
// from the egress that produced it, the healthy sibling b is never dialed, and
// the answering egress keeps its health (a provider verdict is not an outage).
func TestExecuteProviderVerdictStopsOnItsEgress(t *testing.T) {
	for _, status := range []int{400, 403, 404, 408, 422, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newExecutorFixture(t, map[string]int{"a": status, "b": 200})
			defer f.Close()

			resp, id, attempts, failure, uerr := f.exec.Execute(
				context.Background(), f.servers["a"].URL,
				func() map[string]string { return map[string]string{} },
				[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
			if resp != nil {
				_ = resp.Body.Close()
				t.Fatal("a provider verdict must not be delivered as a success response")
			}
			if uerr == nil || uerr.Status != status {
				t.Fatalf("uerr = %+v, want the provider's own %d", uerr, status)
			}
			if id != "a" || attempts != 1 {
				t.Fatalf("id=%q attempts=%d, want exactly one attempt on a", id, attempts)
			}
			if failure.Class == ClassSuccess {
				t.Fatal("failure.Class must not be success for a terminal verdict")
			}
			if got := f.rec.count("b"); got != 0 {
				t.Fatalf("b was dialed %d times — a provider verdict never moves egress", got)
			}
			if got := f.rec.count("a"); got != 1 {
				t.Fatalf("a was dialed %d times, want exactly 1", got)
			}
			if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
				t.Fatal("a provider verdict must never poison egress health")
			}
			if !f.health.Healthy(healthKey("b"), testHealthPolicy) {
				t.Fatal("b was never observed and must stay healthy")
			}
		})
	}
}

// TestExecutePlanExhaustionReturnsLastRealVerdict: when every egress fails
// with a replay-safe transport failure and the plan exhausts BEFORE the
// budget, the client sees the LAST egress's own transport verdict (a 502
// envelope) — never a synthesized "all N URLs failed" string. The response
// must still be nil: nothing was written downstream.
func TestExecutePlanExhaustionReturnsLastRealVerdict(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.deadEgress(t, "a")
	f.deadEgress(t, "b")

	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if resp != nil {
		t.Fatal("must not return a response when all egresses failed")
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want the last real transport verdict (502)", uerr)
	}
	if attempts != 2 || failure.Class != ClassConnectionError {
		t.Fatalf("attempts=%d failure.Class=%s", attempts, failure.Class)
	}
	if id != "b" {
		t.Fatalf("last id = %q, want b", id)
	}
}

// TestAllSkippedReturnsSynthetic502: the ONE case the synthetic envelope is
// for — nothing was ever dialed (every plan entry skipped on a full slot),
// so there is no real verdict to surface. attempts=0 marks it in the log.
func TestAllSkippedReturnsSynthetic502(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.slots.Acquire("a", 1)
	f.slots.Acquire("b", 1)

	p := AttemptPolicy{
		FallbackEnabled: true,
		MaxAttempts:     3,
		MaxConcurrency:  map[string]int{"a": 1, "b": 1},
		HealthPolicy:    testHealthPolicy,
	}
	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), p)
	if resp != nil {
		t.Fatal("must not return a response when nothing was dialed")
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway || uerr.Message != "none of the eligible egresses could serve the request" {
		t.Fatalf("uerr = %+v, want the synthetic 502 envelope", uerr)
	}
	if attempts != 0 || id != "" {
		t.Fatalf("attempts=%d id=%q, want 0/empty (nothing dialed)", attempts, id)
	}
	if f.rec.total() != 0 {
		t.Fatalf("upstream calls = %d, want 0", f.rec.total())
	}
	// The record half of the envelope, not just its class: this is the one
	// value no transport can report, so its provenance is fixed by construction
	// and must not drift (NoDialFailure).
	assertNoDialFailure(t, failure)
}

// TestNoDialFailureIsTheCanonicalNoDialRecord pins the value's OWN invariants,
// which both producers must share (internal/upstream fallback.go's never-dialed
// return and internal/router handler.go's pre-plan rejection). Those consumers
// read only .Class, so nothing else would notice a silent drift of origin,
// phase or request state — and the drift matters: ReplaySafe() is TRUE here by
// construction ("no request byte exists"), which is a RECORD and never an
// authorisation. MarksEgressHealth() must not be true of it either: a request
// that never dialed is no evidence about any egress path.
func TestNoDialFailureIsTheCanonicalNoDialRecord(t *testing.T) {
	assertNoDialFailure(t, NoDialFailure())
	if !NoDialFailure().ReplaySafe() {
		t.Fatal("a never-dialed request is replay-safe by construction — the value is a record, not an authorisation")
	}
	if NoDialFailure().MarksEgressHealth() {
		t.Fatal("nothing was dialed, so no egress path was exercised — this must not mark an egress")
	}
}

// assertNoDialFailure is the shared shape check for the canonical no-dial
// record.
func assertNoDialFailure(t *testing.T, f Failure) {
	t.Helper()
	if f.Class != ClassConnectionError {
		t.Fatalf("Class = %s, want %s", f.Class, ClassConnectionError)
	}
	if f.Origin != OriginTransport {
		t.Fatalf("Origin = %s, want %s", f.Origin, OriginTransport)
	}
	if f.Phase != FailurePhaseNone {
		t.Fatalf("Phase = %s, want %s (no step was performed)", f.Phase, FailurePhaseNone)
	}
	if f.RequestState != RequestStateNotSent {
		t.Fatalf("RequestState = %s, want %s (nothing was dialed)", f.RequestState, RequestStateNotSent)
	}
}

// TestExecuteBudgetCapsAttempts: MaxAttempts bounds DISTINCT egresses tried
// for ONE logical provider attempt (default 3); the rest of the plan is never
// dialed. Only replay-safe failures consume the budget, so the fixture makes
// every candidate fail that way.
func TestExecuteBudgetCapsAttempts(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200, "c": 200, "d": 200})
	defer f.Close()
	f.deadEgress(t, "a")
	f.deadEgress(t, "b")

	resp, id, attempts, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b", "c", "d"), policy(true, 2))
	if resp != nil || uerr == nil {
		t.Fatalf("resp=%v uerr=%v, want terminal 502", resp, uerr)
	}
	if attempts != 2 || id != "b" {
		t.Fatalf("attempts = %d id=%q, want 2 on b (budget cap)", attempts, id)
	}
	if f.rec.count("c") != 0 || f.rec.count("d") != 0 {
		t.Fatalf("beyond-budget egresses must never be dialed: c=%d d=%d", f.rec.count("c"), f.rec.count("d"))
	}
}

// TestExecuteFallbackDisabled: fallback off → one attempt only, even with a
// plan of many and a failure that would otherwise be replay-safe.
func TestExecuteFallbackDisabled(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.deadEgress(t, "a")

	resp, id, attempts, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(false, 3))
	if resp != nil || uerr == nil {
		t.Fatalf("resp=%v uerr=%v, want 502", resp, uerr)
	}
	if attempts != 1 || id != "a" {
		t.Fatalf("attempts=%d id=%q, want single attempt on a", attempts, id)
	}
	if f.rec.count("b") != 0 {
		t.Fatal("fallback disabled must never dial b")
	}
}

// TestExecuteFullSlotSkippedNotFailed: an egress whose slot fills between
// plan and dial is SKIPPED (not a failure) — the head moves on, and when the
// slot frees the egress serves again.
func TestExecuteFullSlotSkippedNotFailed(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.slots.Acquire("a", 1) // fill a's single slot before the request

	p := AttemptPolicy{
		FallbackEnabled: true,
		MaxAttempts:     3,
		MaxConcurrency:  map[string]int{"a": 1, "b": 1},
		HealthPolicy:    testHealthPolicy,
	}
	resp, id, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), p)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "b" {
		t.Fatalf("id = %q, want b (a's slot full → skip)", id)
	}
	if f.rec.count("a") != 0 {
		t.Fatalf("a must not have been dialed: %d calls", f.rec.count("a"))
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a must not be marked unhealthy for a skipped dial")
	}
}

// TestExecuteUnknownEgressSkipped: an egress that left the config mid-flight
// is skipped, not a failure.
func TestExecuteUnknownEgressSkipped(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"b": 200})
	defer f.Close()

	resp, id, attempts, _, uerr := f.exec.Execute(
		context.Background(), f.servers["b"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "ghost", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want b after skipping ghost", id, attempts)
	}
}

// TestExecuteContextCanceledShortCircuits: a canceled context must stop the
// attempt loop immediately and return the caller's error, not fall back.
func TestExecuteContextCanceledShortCircuits(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Force the transport to honor the canceled ctx via a closed server.
	f.servers["a"].Close()
	resp, id, attempts, failure, uerr := f.exec.Execute(
		ctx, f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if resp != nil {
		t.Fatal("canceled request must not return a response")
	}
	if failure.Class != ClassContextCanceled {
		t.Fatalf("failure.Class = %s, want ClassContextCanceled", failure.Class)
	}
	if attempts != 1 || id != "a" {
		t.Fatalf("attempts=%d id=%q; a canceled attempt must not fall back", attempts, id)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "context canceled") {
		t.Fatalf("uerr = %+v, want the canceled context error", uerr)
	}
	_ = resp
}

// TestExecuteReleasesSlotOnFailure: a slot acquired for an attempt that
// FAILS must be freed the moment the attempt ends — otherwise a capped
// egress bricks itself after max_concurrency failures (issue #3 review).
func TestExecuteReleasesSlotOnFailure(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

	p := AttemptPolicy{
		FallbackEnabled: true,
		MaxAttempts:     1, // terminal 502 on a
		MaxConcurrency:  map[string]int{"a": 1},
		HealthPolicy:    testHealthPolicy,
	}
	resp, id, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a"), p)
	if resp != nil || uerr == nil || id != "a" {
		t.Fatalf("resp=%v id=%q uerr=%v, want terminal failure on a", resp, id, uerr)
	}
	if !f.slots.Acquire("a", 1) {
		t.Fatal("a's slot must be released after its failed attempt")
	}
}

// TestExecuteHoldsSlotUntilBodyClosed: a winning egress holds its slot while
// the response body is open (the cap counts in-flight requests/streams) and
// releases it exactly once on Close — the streaming relay closes the body
// twice, so the release MUST be idempotent.
func TestExecuteHoldsSlotUntilBodyClosed(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200})
	defer f.Close()

	p := AttemptPolicy{
		FallbackEnabled: true,
		MaxAttempts:     3,
		MaxConcurrency:  map[string]int{"a": 1},
		HealthPolicy:    testHealthPolicy,
	}
	resp, id, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a"), p)
	if uerr != nil || resp == nil || id != "a" {
		t.Fatalf("uerr=%v id=%q, want success on a", uerr, id)
	}
	if f.slots.Acquire("a", 1) {
		t.Fatal("a's slot must be held while the response body is open")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := resp.Body.Close(); err != nil { // stream.go closes twice
		t.Fatalf("second close: %v", err)
	}
	if !f.slots.Acquire("a", 1) {
		t.Fatal("a's slot must be released once the body is closed")
	}
}
