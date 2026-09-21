package router

// Transport-cache lifecycle tests (issue #6): the cache is a LOOKUP, not an
// ownership registry. A request that resolved its *Client from the cache owns
// that client by reference — a generation prune that evicts the signature can
// neither fail the in-flight request nor close its active connection, and the
// same request can rebuild the evicted transport on its next dial (self-
// healing). These tests use real loopback conns (httptest) so CloseIdleConn-
// ections semantics are observable at the connection level, not just probed.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/logging"
	"opencode-free-proxy/internal/upstream"
)

// cacheServer is a bare Server with a logger wired to the test.
func cacheServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		clients: map[string]*upstream.Client{},
		log:     logging.FromLegacy(func(format string, args ...any) { t.Logf(format, args...) }),
	}
}

// directEgress is a no-proxy egress: NewClientFor(nil) dials the origin
// directly, so httptest servers serve as the upstream.
func directEgress(id string) config.Egress {
	return config.Egress{ID: id}
}

// proxiedEgress is a named http-proxy egress (signature differs from direct).
func proxiedEgress(id, url string) config.Egress {
	return config.Egress{ID: id, Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: url}}
}

// cacheRuntime resolves a one-route runtime over the given egresses, stamped
// with the given generation.
func cacheRuntime(t *testing.T, gen uint64, egs ...config.Egress) *config.Runtime {
	t.Helper()
	ids := make([]string, len(egs))
	for i, e := range egs {
		ids[i] = e.ID
	}
	f := config.File{Egress: egs, Routes: []config.Route{{ID: "r", Egress: ids}}}
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	rt.Generation = gen
	return rt
}

// connTracker records http.ConnState transitions per remote address.
type connTracker struct {
	mu     sync.Mutex
	states map[string][]http.ConnState
}

// trackConns wires the hook on an UNSTARTED server — assigning ConnState to a
// running httptest server races its serve loop.
func trackConns(srv *httptest.Server) *connTracker {
	ct := &connTracker{states: map[string][]http.ConnState{}}
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		ct.mu.Lock()
		defer ct.mu.Unlock()
		ct.states[c.RemoteAddr().String()] = append(ct.states[c.RemoteAddr().String()], s)
	}
	srv.Start()
	return ct
}

func (ct *connTracker) saw(addr string, want http.ConnState) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	for _, s := range ct.states[addr] {
		if s == want {
			return true
		}
	}
	return false
}

// waitUpTo polls cond until it holds or the timeout passes.
func waitUpTo(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInFlightRequestSurvivesTransportPrune: the request resolves its client
// from the cache, a generation swap evicts that signature, and the request
// STILL completes through the very same *Client — cache eviction is not
// revocation (the map is only a lookup; the request owns its reference).
func TestInFlightRequestSurvivesTransportPrune(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered) // first entry only; the channel is per-test-lifetime
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := cacheServer(t)
	eg := directEgress("a")
	c, ok := s.clientForEgress(&eg)
	if !ok {
		t.Fatal("direct egress must build a client")
	}

	type result struct {
		status int
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := c.HTTP.Get(srv.URL)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		ch <- result{status: resp.StatusCode}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream handler")
	}

	// The generation swap prunes egress a's transport mid-request.
	s.onGeneration(cacheRuntime(t, 2, proxiedEgress("b", "http://b.example:3128")))
	if _, still := s.clients[eg.TransportSignature()]; still {
		t.Fatal("the old transport must be evicted from the cache")
	}

	close(release)
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("in-flight request must survive the prune: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request hung after the prune")
	}
}

// TestOldGenerationCanRebuildItsPinnedTransport: after the prune, the request
// (still holding its OLD snapshot's egress) dials again — the cache misses and
// rebuilds a FRESH client under the old signature. Stale snapshots self-heal;
// nothing needs the cache to remember a deleted generation.
func TestOldGenerationCanRebuildItsPinnedTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := cacheServer(t)
	eg := directEgress("a")
	first, ok := s.clientForEgress(&eg)
	if !ok {
		t.Fatal("first build must succeed")
	}

	s.onGeneration(cacheRuntime(t, 2, proxiedEgress("b", "http://b.example:3128")))
	if _, still := s.clients[eg.TransportSignature()]; still {
		t.Fatal("the old signature must be evicted")
	}

	rebuilt, ok := s.clientForEgress(&eg)
	if !ok {
		t.Fatal("the old-generation egress must rebuild its transport")
	}
	if rebuilt == first {
		t.Fatal("a rebuilt client must be a fresh instance, not the evicted one")
	}
	resp, err := rebuilt.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("rebuilt transport must work: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestPruneDoesNotCloseActiveConnection: CloseIdleConnections during the
// generation prune spares the ACTIVE connection — the server side never sees
// the conn torn down while the request is mid-flight, and the conn goes idle
// (not closed) once the response completes.
func TestPruneDoesNotCloseActiveConnection(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var remote atomicValue // conn addr of the in-flight request
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.Store(r.RemoteAddr)
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tracker := trackConns(srv) // starts the server with the hook wired

	s := cacheServer(t)
	eg := directEgress("a")
	c, ok := s.clientForEgress(&eg)
	if !ok {
		t.Fatal("direct egress must build a client")
	}

	done := make(chan error, 1)
	go func() {
		resp, err := c.HTTP.Get(srv.URL)
		if err != nil {
			done <- err
			return
		}
		_ = resp.Body.Close()
		done <- nil
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream handler")
	}

	pruneNow := func() {
		s.clientMu.Lock()
		s.pruneClientsLocked(map[string]struct{}{}) // evict EVERYTHING
		s.clientMu.Unlock()
	}
	pruneNow()

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the active connection must survive the prune: %v", err)
	}

	addr, _ := remote.Load().(string)
	if addr == "" {
		t.Fatal("handler never recorded its conn address")
	}
	if tracker.saw(addr, http.StateClosed) {
		t.Fatal("the active connection must not be closed by the prune")
	}
	waitUpTo(t, 2*time.Second, func() bool { return tracker.saw(addr, http.StateIdle) })
	if !tracker.saw(addr, http.StateIdle) {
		t.Fatal("the connection must return to the idle pool after the response")
	}
}

// TestTransportReplacementDoesNotLeakIdleConnections: a proxy swap evicts the
// old transport, whose idle keep-alive conn is closed for real — observable
// server-side as StateClosed AFTER the conn went idle. Eviction must not
// strand pooled conns waiting for GC.
func TestTransportReplacementDoesNotLeakIdleConnections(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tracker := trackConns(srv) // starts the server with the hook wired

	s := cacheServer(t)
	eg := directEgress("a")
	c, ok := s.clientForEgress(&eg)
	if !ok {
		t.Fatal("direct egress must build a client")
	}
	resp, err := c.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The completed request leaves ONE pooled idle conn; find its address
	// through the tracker (exactly one conn so far).
	waitUpTo(t, 2*time.Second, func() bool { return addrOfOnly(tracker) != "" && tracker.saw(addrOfOnly(tracker), http.StateIdle) })
	idleAddr := addrOfOnly(tracker)
	if idleAddr == "" {
		t.Fatal("no idle conn observed after the completed request")
	}

	// Generation 2 swaps egress a to a proxy: the old signature is evicted and
	// its idle conn must close for real.
	s.onGeneration(cacheRuntime(t, 2, proxiedEgress("a", "http://swapped.example:3128")))
	waitUpTo(t, 2*time.Second, func() bool { return tracker.saw(idleAddr, http.StateClosed) })
	if !tracker.saw(idleAddr, http.StateClosed) {
		t.Fatal("the evicted transport's idle connection must be closed, not left pooled")
	}
}

// addrOfOnly returns the single tracked conn address (empty if none/multiple).
func addrOfOnly(ct *connTracker) string {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	for addr, states := range ct.states {
		if len(states) > 0 {
			return addr
		}
	}
	return ""
}

// atomicValue is a tiny mutex-guarded any (sync/atomic.Value wants consistent
// concrete types across stores; this avoids the pitfall).
type atomicValue struct {
	mu sync.Mutex
	v  any
}

func (a *atomicValue) Store(v any) {
	a.mu.Lock()
	a.v = v
	a.mu.Unlock()
}

func (a *atomicValue) Load() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

// TestTransportCacheConcurrentReloadAndDial: dials through the cache race
// generation swaps and prunes — every dial still gets a working client (built
// fresh if its signature was just evicted), and -race stays clean.
func TestTransportCacheConcurrentReloadAndDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := cacheServer(t)
	egA := directEgress("a")
	egB := proxiedEgress("b", "http://b.example:3128")
	rtA := cacheRuntime(t, 1, egA)
	rtB := cacheRuntime(t, 2, egB)

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Four dialers loop: resolve client for the runtime's FIRST egress and
	// make a real request (only egress a can actually dial the httptest
	// origin; b's dial failures are expected and not counted — b only exercises
	// cache-miss builds for a signature that can never connect).
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				var eg config.Egress
				if j%2 == 0 {
					eg = egA
				} else {
					eg = egB
				}
				c, ok := s.clientForEgress(&eg)
				if !ok {
					errs <- fmt.Errorf("client build failed for %s", eg.ID)
					continue
				}
				if eg.ID != "a" {
					continue // b points at a fake proxy; build-coverage only
				}
				resp, err := c.HTTP.Get(srv.URL)
				if err != nil {
					errs <- err
					continue
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("status %d", resp.StatusCode)
				}
			}
		}()
	}
	// One swapper drives prune churn across the two generations.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			if j%2 == 0 {
				s.onGeneration(rtA)
			} else {
				s.onGeneration(rtB)
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
