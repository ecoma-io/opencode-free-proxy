package router

// Generation-prune tests: the transport cache is the branch's subtlest
// concurrency seam (issue #6 adversarial review found it untested). The
// invariants pinned here: an evicted transport closes its idle conns
// (deterministic FD accounting), pruning is once per generation and
// monotonic — a STALE snapshot never prunes against its older keep-set.

import (
	"net/http"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/upstream"
)

// pruneProbe replaces a client's transport so CloseIdleConnections is
// observable (http.Client forwards the call to any transport implementing
// it). RoundTripper is embedded to satisfy the field type; no request ever
// travels through it in these tests.
type pruneProbe struct {
	http.RoundTripper
	closes int
}

func (p *pruneProbe) CloseIdleConnections() { p.closes++ }

// pruneEgress is a proxy egress whose transport signature is stable per name.
func pruneEgress(id string) *config.Egress {
	return &config.Egress{ID: id, Proxy: &config.Proxy{
		Type: config.ProxyHTTP,
		URL:  "http://" + id + ".example:3128",
	}}
}

// pruneRuntime resolves a snapshot with the given egresses and ONE route over
// them (the keep-set source), stamped with the given generation.
func pruneRuntime(t *testing.T, gen uint64, ids ...string) *config.Runtime {
	t.Helper()
	f := config.File{Routes: []config.Route{{ID: "r", Egress: ids}}}
	for _, id := range ids {
		f.Egress = append(f.Egress, *pruneEgress(id))
	}
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	rt.Generation = gen
	return rt
}

// newPruneServer is a bare Server with the named egresses cached, each
// carrying its own probe transport.
func newPruneServer(ids ...string) (*Server, map[string]*pruneProbe) {
	s := &Server{clients: map[string]*upstream.Client{}}
	probes := map[string]*pruneProbe{}
	for _, id := range ids {
		e := pruneEgress(id)
		c, err := upstream.NewClientFor(e.Proxy)
		if err != nil {
			panic(err)
		}
		p := &pruneProbe{RoundTripper: http.DefaultTransport}
		c.HTTP.Transport = p
		probes[id] = p
		s.clients[e.TransportSignature()] = c
	}
	return s, probes
}

func sigOf(t *testing.T, id string) string {
	t.Helper()
	return pruneEgress(id).TransportSignature()
}

// TestPruneEvictsAbsentTransportsAndClosesIdle: generation 2 routes [a, b] —
// c's transport is evicted, its idle conns close IMMEDIATELY (not at GC), and
// the keep-set survives untouched. A second onGeneration at the SAME
// generation is a no-op (once per generation), and an OLDER generation never
// prunes (monotonic guard).
func TestPruneEvictsAbsentTransportsAndClosesIdle(t *testing.T) {
	s, probes := newPruneServer("a", "b", "c")
	rt2 := pruneRuntime(t, 2, "a", "b")

	s.onGeneration(rt2)
	if _, ok := s.clients[sigOf(t, "c")]; ok {
		t.Fatal("c's transport must be evicted by the generation-2 prune")
	}
	if _, ok := s.clients[sigOf(t, "a")]; !ok {
		t.Fatal("a's transport must survive the prune")
	}
	if _, ok := s.clients[sigOf(t, "b")]; !ok {
		t.Fatal("b's transport must survive the prune")
	}
	if probes["c"].closes != 1 {
		t.Fatalf("evicted client CloseIdleConnections = %d, want 1", probes["c"].closes)
	}
	if probes["a"].closes != 0 || probes["b"].closes != 0 {
		t.Fatalf("keep-set probes closed: a=%d b=%d, want 0", probes["a"].closes, probes["b"].closes)
	}

	// Same generation again: re-adding the absent client must survive — the
	// prune already ran for generation 2.
	c, err := upstream.NewClientFor(pruneEgress("c").Proxy)
	if err != nil {
		t.Fatal(err)
	}
	s.clients[sigOf(t, "c")] = c
	s.onGeneration(rt2)
	if _, ok := s.clients[sigOf(t, "c")]; !ok {
		t.Fatal("a same-generation onGeneration must not prune again")
	}

	// An OLDER snapshot never prunes against its keep-set (it would drop a
	// newer generation's transports).
	rt1 := pruneRuntime(t, 1, "a")
	s.onGeneration(rt1)
	if _, ok := s.clients[sigOf(t, "c")]; !ok {
		t.Fatal("a stale generation must never prune (monotonic guard)")
	}
	if _, ok := s.clients[sigOf(t, "b")]; !ok {
		t.Fatal("a stale generation must never prune (monotonic guard)")
	}
}

// TestStaleGenerationNeverPrunesNewerKeepSet: the CAS-window regression —
// generation 3 (route [a, c]) prunes and caches c; a LATE generation-2
// snapshot (route [a, b]) must not evict c even though c is absent from ITS
// keep-set.
func TestStaleGenerationNeverPrunesNewerKeepSet(t *testing.T) {
	s, probes := newPruneServer("a", "b", "c")

	s.onGeneration(pruneRuntime(t, 3, "a", "c"))
	if _, ok := s.clients[sigOf(t, "c")]; !ok {
		t.Fatal("c must be cached after the generation-3 prune")
	}
	if probes["c"].closes != 0 {
		t.Fatalf("c must not have been closed: %d", probes["c"].closes)
	}

	// The stale generation-2 snapshot arrives late.
	s.onGeneration(pruneRuntime(t, 2, "a", "b"))
	if _, ok := s.clients[sigOf(t, "c")]; !ok {
		t.Fatal("the stale generation-2 prune must not evict the newer generation's transport")
	}
	if probes["c"].closes != 0 {
		t.Fatalf("the stale prune must not close the newer client: %d", probes["c"].closes)
	}
	if _, ok := s.clients[sigOf(t, "b")]; ok {
		t.Fatal("b was already evicted by generation 3 and must stay evicted")
	}
}

// TestGenerationZeroNeverPrunes: the built-in default runtime (generation 0)
// carries no generation semantics — onGeneration must be a no-op.
func TestGenerationZeroNeverPrunes(t *testing.T) {
	s, probes := newPruneServer("a")
	rt := config.DefaultRuntime()
	rt.Generation = 0
	s.onGeneration(rt)
	if _, ok := s.clients[sigOf(t, "a")]; !ok {
		t.Fatal("generation 0 must never prune")
	}
	if probes["a"].closes != 0 {
		t.Fatalf("generation 0 closed a client: %d", probes["a"].closes)
	}
}

// swapRuntime resolves a one-egress snapshot whose proxy URL is given, for
// health-identity reclamation tests (the identity is id + transport).
func swapRuntime(t *testing.T, gen uint64, id, url string) *config.Runtime {
	t.Helper()
	f := config.File{
		Egress: []config.Egress{{ID: id, Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: url}}},
		Routes: []config.Route{{ID: "r", Egress: []string{id}}},
	}
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	rt.Generation = gen
	return rt
}

// TestGenerationSwapReclaimsAbandonedHealthIdentities: the once-per-generation
// maintenance covers health state too (issue #9) — an identity the new runtime
// no longer names is reclaimed by the same pass that prunes the transport
// cache, and a pinned identity survives even when abandoned (an in-flight
// request may still be observing it) until its pin releases.
func TestGenerationSwapReclaimsAbandonedHealthIdentities(t *testing.T) {
	oldKey := (&config.Egress{ID: "a", Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: "http://old.example:3128"}}).HealthKey()
	p := health.Policy{Enabled: true, Threshold: 1, Cooldown: time.Minute}
	healthy := health.Policy{Enabled: true}

	s := &Server{clients: map[string]*upstream.Client{}, Health: health.New()}

	s.Health.Observe(oldKey, false, p) // old transport arms its cooldown
	s.onGeneration(swapRuntime(t, 2, "a", "http://new.example:3128"))
	if !s.Health.Healthy(oldKey, healthy) {
		t.Fatal("abandoned identity must be reclaimed by the generation swap")
	}

	// A pinned abandoned identity survives the swap; the next swap after the
	// pin releases collects it.
	s.Health.Observe(oldKey, false, p)
	release := s.Health.Pin([]string{oldKey})
	s.onGeneration(swapRuntime(t, 3, "a", "http://third.example:3128"))
	if s.Health.Healthy(oldKey, healthy) {
		t.Fatal("pinned identity must survive the generation swap")
	}
	release()
	s.onGeneration(swapRuntime(t, 4, "a", "http://fourth.example:3128"))
	if !s.Health.Healthy(oldKey, healthy) {
		t.Fatal("after the pin releases, the next swap reclaims the identity")
	}
}
