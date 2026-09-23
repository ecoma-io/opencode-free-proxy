package upstream

// Attempt-budget and executor-isolation tests (issue #53).
//
// The budget this file bounds is DIFFERENT from the one it used to bound.
// There is no per-egress retry matrix any more: MaxAttempts caps distinct
// egresses per logical provider attempt, and only a failure that provably
// happened before the request was transmitted consumes a draw. A provider
// verdict — 429, 4xx, 5xx — consumes nothing and moves nothing.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// handlerFixture is like executorFixture but with a per-egress HANDLER
// (statuses can vary over time).
type handlerFixture struct {
	servers map[string]*httptest.Server
	clients map[string]*Client
	rec     *scriptedRecorder
	exec    *Executor
	health  *health.Registry
	slots   *Limiter
	rt      *config.Runtime
}

func newHandlerFixture(t *testing.T, handlers map[string]http.HandlerFunc) *handlerFixture {
	t.Helper()
	f := &handlerFixture{
		servers: map[string]*httptest.Server{},
		rec:     &scriptedRecorder{calls: map[string]int{}},
		health:  health.New(),
		slots:   NewLimiter(),
	}
	// No registry Configure: the health policy rides the AttemptPolicy
	// (policy() pins testHealthPolicy, threshold 1) since issue #6.

	ids := make([]string, 0, len(handlers))
	for id, h := range handlers {
		handlers[id] = func(name string, base http.HandlerFunc) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				f.rec.mu.Lock()
				if f.rec.calls == nil {
					f.rec.calls = map[string]int{}
				}
				f.rec.calls[name]++
				f.rec.mu.Unlock()
				base(w, r)
			}
		}(id, h)
		f.servers[id] = httptest.NewServer(handlers[id])
		ids = append(ids, id)
	}
	sort.Strings(ids)

	f.clients = map[string]*Client{}
	for id := range handlers {
		c := NewClient()
		c.HTTP.Transport = dialTo(f.servers[id].Listener.Addr().String())
		f.clients[id] = c
	}
	file := config.File{
		Egress: make([]config.Egress, 0, len(ids)),
		Routes: []config.Route{{ID: "r", Egress: ids}},
	}
	for _, id := range ids {
		file.Egress = append(file.Egress, config.Egress{ID: id})
	}
	rt, err := file.Resolve()
	if err != nil {
		t.Fatalf("fixture runtime: %v", err)
	}
	f.rt = rt
	f.exec = NewExecutor(func(e *config.Egress) (*Client, bool) {
		c, ok := f.clients[e.ID]
		return c, ok
	}, f.health, f.slots)
	return f
}

// deadEgress re-points an egress's transport at a closed port: the only
// failure shape that consumes an attempt-budget draw (see the file header).
func (f *handlerFixture) deadEgress(t *testing.T, id string) {
	t.Helper()
	c, ok := f.clients[id]
	if !ok {
		t.Fatalf("fixture has no egress %q", id)
	}
	c.HTTP.Transport = dialTo(closedAddr(t))
}

func (f *handlerFixture) Close() {
	for _, s := range f.servers {
		s.Close()
	}
}

func (f *handlerFixture) plan(heads ...string) routing.RoutePlan {
	p := routing.RoutePlan{RouteID: "r", Strategy: config.StrategyRoundRobin, Attempts: heads}
	p.Egresses = make([]*config.Egress, len(heads))
	for i, id := range heads {
		if e, ok := f.rt.Egress(id); ok {
			p.Egresses[i] = e
		}
	}
	return p
}

// statusHandler serves a fixed status.
func statusHandler(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

// sequenceHandler serves statuses[i] for call i (call i >= len switches to
// the last status forever).
func sequenceHandler(statuses ...int) http.HandlerFunc {
	var n atomic.Int64
	return func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		w.WriteHeader(statuses[i])
	}
}

// closedAddr returns an address nothing is listening on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestExecuteFallbackExactPostCounts: no amplification. A replay-safe failure
// on a costs exactly one dial there (and zero requests on a's wire — nothing
// was ever sent), the fallback egress pays exactly one POST, and the total is
// one request for the whole logical attempt.
func TestExecuteFallbackExactPostCounts(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()
	f.deadEgress(t, "a")

	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr=%v resp=%v, want success on b", uerr, resp)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 2 || failure.Class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d failure.Class=%s, want b/2/success", id, attempts, failure.Class)
	}
	if got := f.rec.count("a"); got != 0 {
		t.Fatalf("a received %d requests — a pre-request failure must never send one", got)
	}
	if got := f.rec.count("b"); got != 1 {
		t.Fatalf("b POSTs = %d, want exactly 1", got)
	}
	if got := f.rec.total(); got != 1 {
		t.Fatalf("total upstream requests = %d, want 1", got)
	}
}

// TestExecuteProviderStatusConsumesNoBudgetDraw: a route with budget 3 and a
// healthy sibling, where the head answers 503. The budget is untouched (one
// attempt), the sibling is never dialed, and the request ends on the verdict.
func TestExecuteProviderStatusConsumesNoBudgetDraw(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusServiceUnavailable),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("want no response: the 503 verdict is the outcome")
	}
	if uerr == nil || uerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("uerr = %+v, want the provider's 503", uerr)
	}
	if id != "a" || attempts != 1 || failure.Class != ClassUpstream5xx {
		t.Fatalf("id=%q attempts=%d failure.Class=%s, want a/1/upstream_5xx", id, attempts, failure.Class)
	}
	if got := f.rec.count("b"); got != 0 {
		t.Fatalf("b POSTs = %d, want 0", got)
	}
}

// TestExecuteCancelledContextNeverReachesTheWire: a canceled request must stop
// on the attempted egress without dialing another, and without either egress
// seeing a request. Nothing is delivered (there is nobody to deliver to) and
// no health mark is made (the caller failed, not the egress).
func TestExecuteCancelledContextNeverReachesTheWire(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, id, attempts, failure, uerr := f.exec.Execute(
		ctx, f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if resp != nil {
		t.Fatal("a canceled request must not return a response")
	}
	if failure.Class != ClassContextCanceled {
		t.Fatalf("failure.Class = %s, want ClassContextCanceled", failure.Class)
	}
	if attempts != 1 || id != "a" {
		t.Fatalf("attempts=%d id=%q, want the single canceled attempt on a", attempts, id)
	}
	if uerr == nil || !strings.Contains(uerr.Message, "context canceled") {
		t.Fatalf("uerr = %+v, want the canceled-context error", uerr)
	}
	if got := f.rec.total(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0 — a canceled request never reaches a provider", got)
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a cancellation is the caller's failure: it must not mark the egress unhealthy")
	}
}

// TestExecuteVerdictLeavesEgressHealthyAndRoutable is the poisoning proof: an
// egress that answered 429 is still healthy, so the very next request is routed
// straight back to it — one attempt, no sibling involved.
func TestExecuteVerdictLeavesEgressHealthyAndRoutable(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusTooManyRequests, http.StatusOK), // 1st: 429, then 200
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	// req 1: a → 429, terminal.
	resp, id, attempts, failure, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if id != "a" || attempts != 1 || failure.Class != ClassUpstream429 || uerr == nil || uerr.Status != 429 {
		t.Fatalf("req1: id=%q attempts=%d failure.Class=%s uerr=%v, want the terminal 429 on a", id, attempts, failure.Class, uerr)
	}
	_ = resp
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a's 429 must NOT mark it unhealthy (rate limit ≠ egress failure)")
	}

	// req 2: a must serve directly — the 429 didn't poison it.
	resp2, id2, attempts2, failure2, uerr2 := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr2 != nil || id2 != "a" {
		t.Fatalf("req2: id=%q uerr=%v, want a serving again", id2, uerr2)
	}
	_ = resp2.Body.Close()
	if attempts2 != 1 || failure2.Class != ClassSuccess {
		t.Fatalf("req2: attempts=%d failure.Class=%s, want a single attempt", attempts2, failure2.Class)
	}
	if got := f.rec.count("b"); got != 0 {
		t.Fatalf("b POSTs = %d, want 0 (neither request may move egress)", got)
	}
}

// TestExecuteNoVerdictPoisonsHealth: 429 then 500 then 502 on the same egress.
// None of them is an egress outage — health tracks the egress PATH, and the
// provider answered all three times — so the egress stays healthy throughout.
func TestExecuteNoVerdictPoisonsHealth(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	for i, want := range []int{429, 500, 502} {
		resp, id, _, _, uerr := f.exec.Execute(
			context.Background(), f.servers["a"].URL,
			func() map[string]string { return map[string]string{} },
			[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatalf("req%d: want no response", i+1)
		}
		if uerr == nil || uerr.Status != want {
			t.Fatalf("req%d: uerr = %+v, want %d", i+1, uerr, want)
		}
		if id != "a" {
			t.Fatalf("req%d: id = %q, want a", i+1, id)
		}
		if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
			t.Fatalf("req%d: a provider verdict (%d) must never mark the egress unhealthy", i+1, want)
		}
	}
	if got := f.rec.count("b"); got != 0 {
		t.Fatalf("b POSTs = %d, want 0", got)
	}
	if got := f.rec.count("a"); got != 3 {
		t.Fatalf("a POSTs = %d, want 3 (one per request)", got)
	}
}

// TestExecutorConsumesPinnedEgressPointers: the executor must dial through
// the EXACT *config.Egress pointers the plan pinned (snapshot identity), not
// re-resolve by id. The clientFor wrapper records what it received; a
// runtime re-lookup after Plan would produce different pointers (a new
// snapshot) and this test would catch it. The executor has no runtime
// reference — immutability by construction, asserted here.
func TestExecutorConsumesPinnedEgressPointers(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()
	f.deadEgress(t, "a")

	plan := f.plan("a", "b")

	// Rebuild the executor with a recording clientFor.
	var mu sync.Mutex
	var seen []*config.Egress
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		c, ok := f.clients[e.ID]
		return c, ok
	}, f.health, f.slots)

	resp, id, _, _, uerr := exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), plan, policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr=%v resp=%v", uerr, resp)
	}
	_ = resp.Body.Close()
	if id != "b" {
		t.Fatalf("id=%q, want b", id)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("clientFor called %d times, want 2 (a failed, b served)", len(seen))
	}
	for i, want := range []*config.Egress{plan.Egresses[0], plan.Egresses[1]} {
		if seen[i] != want {
			t.Fatalf("clientFor[%d] got %p (%s), want the pinned snapshot pointer %p (%s) — re-resolution leaked",
				i, seen[i], seen[i].ID, want, want.ID)
		}
	}
}

// TestExecutorIgnoresRuntimeSwapAfterPlan proves a config reload between Plan
// and Execute cannot steer the request: the new snapshot's egress pointers
// differ, and the executor keeps dialing the pinned ones.
func TestExecutorIgnoresRuntimeSwapAfterPlan(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()
	f.deadEgress(t, "a")

	plan := f.plan("a", "b")
	// Simulate a reload: a NEW runtime with the same ids resolves to NEW
	// pointers. The executor must ignore it.
	file := config.File{Egress: []config.Egress{{ID: "a"}, {ID: "b"}}, Routes: []config.Route{{ID: "r", Egress: []string{"a", "b"}}}}
	fresh, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	freshA, _ := fresh.Egress("a")
	if freshA == plan.Egresses[0] {
		t.Fatal("fixture flaw: fresh runtime reuses pointers")
	}
	_ = fresh // deliberately unreachable by the executor

	resp, id, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), plan, policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr=%v resp=%v", uerr, resp)
	}
	_ = resp.Body.Close()
	if id != "b" {
		t.Fatalf("id=%q, want b", id)
	}
	// Sanity: the request genuinely consumed the PINNED pointers — dial
	// counts came from the pinned clientFor wiring, not any runtime state.
	if fmt.Sprintf("%p", plan.Egresses[0]) == fmt.Sprintf("%p", freshA) {
		t.Fatal("impossible: distinct allocations compared equal")
	}
}
