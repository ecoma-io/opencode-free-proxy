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
	clients := map[string]*Client{}
	for id := range statuses {
		c := NewClient()
		// Pin each egress's dial to ITS OWN listener, independent of the URL
		// host (production differentiates egresses by proxy transport; here
		// the dial is the differentiator).
		addr := f.servers[id].Listener.Addr().String()
		c.HTTP.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		clients[id] = c
	}
	f.rt = fixtureRuntime(statuses)
	f.exec = NewExecutor(func(e *config.Egress) (*Client, bool) {
		c, ok := clients[e.ID]
		return c, ok
	}, f.health, f.slots)
	return f
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

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL+"/zen/v1/chat/completions",
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "a" || attempts != 1 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s", id, attempts, class)
	}
	if f.rec.total() != 1 {
		t.Fatalf("upstream calls = %d, want 1", f.rec.total())
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("success must mark the egress healthy")
	}
}

// TestExecuteFallsBackOn5xx: a 500-class failure on the first egress falls
// back to the next; health is marked; the winner's success clears it.
func TestExecuteFallsBackOn5xx(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s", id, attempts, class)
	}
	if f.rec.count("a") != 1 || f.rec.count("b") != 1 {
		t.Fatalf("calls a=%d b=%d, want 1 each", f.rec.count("a"), f.rec.count("b"))
	}
	if f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a's 500 must mark it unhealthy")
	}
	if !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("b's success must mark it healthy")
	}
}

// TestExecute429FallsBackWithoutMarkingHealth: 429 falls back but is a
// verdict about the request, never about the egress.
func TestExecute429FallsBackWithoutMarkingHealth(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429, "b": 200})
	defer f.Close()

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s", id, attempts, class)
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("429 must NOT mark the egress unhealthy")
	}
}

// TestExecuteAllUnhealthyReturns502: when every egress fails, Execute
// returns the terminal error WITHOUT a response — the caller must not have
// written anything downstream yet, so the 502 envelope is safe.
func TestExecuteAllUnhealthyReturns502(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 500})
	defer f.Close()

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if resp != nil {
		t.Fatal("must not return a response when all egresses failed")
	}
	if uerr == nil || uerr.Status != http.StatusBadGateway {
		t.Fatalf("uerr = %+v, want 502", uerr)
	}
	if attempts != 2 || class != ClassUpstream5xx {
		t.Fatalf("attempts=%d class=%s", attempts, class)
	}
	if id != "b" {
		t.Fatalf("last id = %q, want b", id)
	}
}

// Test429ChainFallsBackToThirdEgress: the adversarial 429 row — two chained
// rate limits still fall through to the third egress, and NEITHER 429'd
// egress is poisoned (the next request may be routed back to either).
func Test429ChainFallsBackToThirdEgress(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429, "b": 429, "c": 200})
	defer f.Close()

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b", "c"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	_ = resp.Body.Close()
	if id != "c" || attempts != 3 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s, want c/3/success", id, attempts, class)
	}
	if f.rec.count("a") != 1 || f.rec.count("b") != 1 || f.rec.count("c") != 1 {
		t.Fatalf("calls a=%d b=%d c=%d, want 1 each (429s never retry)", f.rec.count("a"), f.rec.count("b"), f.rec.count("c"))
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) || !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("429s must not mark either egress unhealthy")
	}
}

// TestExecuteBudgetCapsAttempts: MaxAttempts bounds DISTINCT egresses tried
// (default 3); the rest of the plan is never dialed.
func TestExecuteBudgetCapsAttempts(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 500, "c": 200, "d": 200})
	defer f.Close()

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
// plan of many.
func TestExecuteFallbackDisabled(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

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
	resp, id, attempts, class, uerr := f.exec.Execute(
		ctx, f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3))
	if resp != nil {
		t.Fatal("canceled request must not return a response")
	}
	if class != ClassContextCanceled {
		t.Fatalf("class = %s, want ClassContextCanceled", class)
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
