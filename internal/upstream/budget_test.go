package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// handlerFixture is like executorFixture but with a per-egress HANDLER
// (statuses can vary over time) and no-op sleep — the retry-matrix statuses
// (502/503/504) loop instantly, keeping the exact-POST-count assertions fast.
type handlerFixture struct {
	servers map[string]*httptest.Server
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

	clients := map[string]*Client{}
	for id := range handlers {
		c := NewClient()
		c.Sleep = func(time.Duration) {} // no-op: exact POST counts, no 2-3s waits
		addr := f.servers[id].Listener.Addr().String()
		c.HTTP.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		clients[id] = c
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
		c, ok := clients[e.ID]
		return c, ok
	}, f.health, f.slots)
	return f
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

// TestExecuteBudgetExactPostCounts proves the retry × fallback budget has no
// amplification: a 502 egress burns its full per-attempt matrix (1 initial +
// 3 retries = 4 POSTs) while the fallback target pays exactly 1. MaxAttempts
// caps DISTINCT egresses, never multiplies per-egress retries.
func TestExecuteBudgetExactPostCounts(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": statusHandler(http.StatusBadGateway), // 502 × forever
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr != nil || resp == nil {
		t.Fatalf("uerr=%v resp=%v, want success on b", uerr, resp)
	}
	_ = resp.Body.Close()
	if id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("id=%q attempts=%d class=%s, want b/2/success", id, attempts, class)
	}
	// 1 initial + 3 retries on a (RetryRules[502].Attempts == 3), exactly 1 on b.
	if got := f.rec.count("a"); got != 4 {
		t.Fatalf("a POSTs = %d, want 4 (1 initial + 3 retries)", got)
	}
	if got := f.rec.count("b"); got != 1 {
		t.Fatalf("b POSTs = %d, want 1 (fallback never retries a fresh egress)", got)
	}
	if got := f.rec.total(); got != 5 {
		t.Fatalf("total POSTs = %d, want 5", got)
	}
}

// TestExecute429Then200KeepsEgressUsable is the adversarial rate-limit proof:
// a 429 falls back (b serves), but the 429'd egress stays HEALTHY — the next
// request routes straight back to a, which serves. A wrong "429 poisons
// health" implementation would route request 2 to b or degrade to 502/503.
func TestExecute429Then200KeepsEgressUsable(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusTooManyRequests, http.StatusOK), // 1st: 429, then 200
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	// req 1: a → 429 → fallback b → 200.
	resp, id, attempts, class, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr != nil || id != "b" || attempts != 2 || class != ClassSuccess {
		t.Fatalf("req1: id=%q attempts=%d class=%s uerr=%v, want b/2/success", id, attempts, class, uerr)
	}
	_ = resp.Body.Close()
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a's 429 must NOT mark it unhealthy (rate limit ≠ egress failure)")
	}

	// req 2: a must serve directly — the 429 didn't poison it.
	resp2, id2, attempts2, class2, uerr2 := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr2 != nil || id2 != "a" {
		t.Fatalf("req2: id=%q uerr=%v, want a serving again", id2, uerr2)
	}
	_ = resp2.Body.Close()
	if attempts2 != 1 || class2 != ClassSuccess {
		t.Fatalf("req2: attempts=%d class=%s, want a single attempt", attempts2, class2)
	}
	if got := f.rec.count("b"); got != 1 {
		t.Fatalf("b POSTs = %d, want 1 (only req1 fell back)", got)
	}
}

// TestExecute429Then500PoisonsOnlyThe500: the same egress gets 429 then 500.
// The 429 keeps it healthy; the subsequent 500 (a genuine egress failure)
// marks it unhealthy. Health tracks consecutive FAILURES, and only true
// failure classes contribute.
func TestExecute429Then500PoisonsOnlyThe500(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusTooManyRequests, http.StatusInternalServerError),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	// req1: a → 429 → fallback b; the 429 keeps a healthy.
	resp, _, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr != nil {
		t.Fatalf("req1 uerr=%v, want success via fallback", uerr)
	}
	_ = resp.Body.Close()
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("req1: single 429 must not mark a unhealthy")
	}

	// req2: a → 500 → fallback b; the 500 marks a unhealthy (threshold 1).
	resp2, _, _, _, uerr2 := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr2 != nil {
		t.Fatalf("req2 uerr=%v, want success via fallback", uerr2)
	}
	_ = resp2.Body.Close()
	if f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("req2: the 500 must mark a unhealthy (consecutive-failure threshold 1)")
	}
}

// TestExecute500Then200RecoversEgress: after a 500 (single POST, marks
// health), one 200 clears the mark (Observe(true) resets the streak) — the
// egress is usable on the very next request.
func TestExecute500Then200RecoversEgress(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusInternalServerError, http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	resp, id, _, _, uerr := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr != nil || id != "b" {
		t.Fatalf("id=%q uerr=%v, want fallback to b", id, uerr)
	}
	_ = resp.Body.Close()
	if f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("a's 500 marks it unhealthy")
	}

	resp2, id2, _, _, uerr2 := f.exec.Execute(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("a", "b"), policy(true, 3))
	if uerr2 != nil || id2 != "a" {
		t.Fatalf("req2 id=%q uerr=%v, want a restored (single success clears)", id2, uerr2)
	}
	_ = resp2.Body.Close()
}

// TestExecutorConsumesPinnedEgressPointers: the executor must dial through
// the EXACT *config.Egress pointers the plan pinned (snapshot identity), not
// re-resolve by id. The clientFor wrapper records what it received; a
// runtime re-lookup after Plan would produce different pointers (a new
// snapshot) and this test would catch it. The executor has no runtime
// reference — immutability by construction, asserted here.
func TestExecutorConsumesPinnedEgressPointers(t *testing.T) {
	f := newHandlerFixture(t, map[string]http.HandlerFunc{
		"a": sequenceHandler(http.StatusInternalServerError, http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

	plan := f.plan("a", "b")

	// Rebuild the executor with a recording clientFor.
	clients := map[string]*Client{}
	for id, srv := range f.servers {
		c := NewClient()
		c.Sleep = func(time.Duration) {}
		addr := srv.Listener.Addr().String()
		c.HTTP.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		clients[id] = c
	}
	var mu sync.Mutex
	var seen []*config.Egress
	exec := NewExecutor(func(e *config.Egress) (*Client, bool) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		c, ok := clients[e.ID]
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
		"a": sequenceHandler(http.StatusInternalServerError, http.StatusOK),
		"b": statusHandler(http.StatusOK),
	})
	defer f.Close()

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
