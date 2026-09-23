package router

// Health-pin lifecycle tests (issue #9 through the router): a request pins
// its route's health identities at snapshot-pin time and holds them for the
// request's whole lifetime, so a generation swap that stops referencing an
// identity can never reclaim the state the in-flight request is still
// adjudicating against. TestInFlightPinShieldsIdentityFromReclaim is the
// deterministic proof; TestRelayPinReclaimConcurrentStorm hammers the
// Get→Pin critical section (handler.go) against onGeneration's reclaim under
// -race.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/health"
)

// TestInFlightPinShieldsIdentityFromReclaim: request 1 pins generation 1
// (route [a, b]) and blocks inside a's dial; while the pin is held, a is
// armed cooling directly; the config swaps to generation 2 (route [b] only —
// a's identity is now inactive); request 2 runs the generation-2 reclaim
// THROUGH the relay. The reclaim must spare a (still pinned): its just-armed
// cooldown state survives. Only after request 1 releases does a later
// reclaim collect the identity.
func TestInFlightPinShieldsIdentityFromReclaim(t *testing.T) {
	dir := t.TempDir()

	// a: holds the dial until released, then fails PRE-REQUEST (the SOCKS5
	// greeting never completes), which is the only failure shape that may
	// move the request on to b. The proxy's own cleanup closes both the held
	// connections and the listener, so an assertion failure mid-test cannot
	// leave the request parked forever.
	proxyA := newFailingEgressProxy(t, true)
	var bMu sync.Mutex
	bCalls := 0
	proxyB := countProxy(&bCalls, &bMu)
	defer proxyB.Close()

	s, mux, store := snapshotRouter(t, dir, fmt.Sprintf(`
health:
  enabled: true
  failure_threshold: 1
  cooldown: 1h
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)), nil)

	// Request 1: pins generation 1's keys (keyA included) and blocks inside
	// a's dial. a must be HEALTHY here — a cooling a would be filtered from
	// the heads and never dialed.
	rt1 := store.Get()
	eA, ok := rt1.Egress("a")
	if !ok {
		t.Fatal("egress a missing from generation 1")
	}
	keyA := eA.HealthKey()
	hp := health.PolicyFromSnapshot(rt1)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	}()
	proxyA.waitDialed(t)

	// Now, while request 1 is mid-dial (pin held, state existing), arm a:
	// threshold 1 → cooling for 1h. Unknown identities are healthy, so
	// Healthy==false is the observable proof the cooldown STATE exists.
	s.Health.Observe(keyA, false, hp)
	if s.Health.Healthy(keyA, hp) {
		t.Fatal("precondition failed: armed a still healthy")
	}

	// Swap to generation 2: route [b] only. a's identity is now INACTIVE —
	// the next reclaim would collect it, were it not pinned.
	writeCfg(t, dir, testUpstreamBasePrefix+fmt.Sprintf(`
health:
  enabled: true
  failure_threshold: 1
  cooldown: 1h
egress:
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [b]}
`, pxyURL(proxyB)))
	waitGeneration(t, store, 2)

	// Request 2 through the relay: pins generation 2's keys and runs
	// onGeneration → Reclaim({keyB}) while request 1 still holds keyA's pin.
	rec2 := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-OFP-Egress") != "b" {
		t.Fatalf("request 2: status=%d egress=%q, want 200 via b", rec2.Code, rec2.Header().Get("X-OFP-Egress"))
	}
	if s.Health.Healthy(keyA, hp) {
		t.Fatal("a's cooldown state was reclaimed while request 1 still pinned it")
	}

	// Release request 1; a's dial fails pre-request, the request falls back
	// to b, and its pin goes away.
	proxyA.release()
	rec1 := <-done
	if rec1.Code != http.StatusOK || rec1.Header().Get("X-OFP-Egress") != "b" {
		t.Fatalf("request 1: status=%d egress=%q, want 200 via b (fallback)", rec1.Code, rec1.Header().Get("X-OFP-Egress"))
	}

	// Now the identity is neither active nor pinned: the next reclaim
	// collects exactly it, and the registry forgets the cooldown.
	dropped := s.Health.Reclaim(health.ActiveKeys(store.Get()))
	if dropped != 1 {
		t.Fatalf("reclaim dropped %d identities, want 1 (a)", dropped)
	}
	if !s.Health.Healthy(keyA, hp) {
		t.Fatal("a still cooling after its identity was reclaimed")
	}
}

// writeCfgAtomic swaps the config the way a real deploy does — write to a
// temp file in the same dir, rename over the target — so the poller never
// reads a torn document (a truncated read parses as a route-less valid YAML
// and would 400 unrelated requests; that's a test-harness artifact, not a
// product behavior).
func writeCfgAtomic(t *testing.T, dir, doc string) {
	t.Helper()
	tmp, err := os.CreateTemp(dir, ".cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := tmp.Name()
	_, werr := tmp.WriteString(doc)
	cerr := tmp.Close()
	if werr != nil {
		t.Fatal(werr)
	}
	if cerr != nil {
		t.Fatal(cerr)
	}
	if err := os.Rename(p, filepath.Join(dir, "cfg.yaml")); err != nil {
		t.Fatal(err)
	}
}

// TestRelayPinReclaimConcurrentStorm hammers the seam the deterministic test
// cannot time: concurrent relays against a config store swapping every few
// milliseconds, under -race.
//
// Shape: odd generations route [a] (a pre-request-failing egress, armed
// cooling before the storm, threshold 1 / cooldown 1h); even generations
// route [c] (a 200 proxy). An even generation's reclaim legitimately wipes
// a's unpinned identity; odd-generation requests already past their heads
// filter may then legitimately dial a (up to `workers` concurrent dials, until
// the first failure re-arms the cooldown). So the pass bound is
// aDials ≤ (generations/2 + 1) × workers — NOT zero, and the comment must be
// honest about why: the Get→Pin window the old code raced over is nanoseconds
// wide and the failure it produced (a pin landing after a reclaim) is a
// LOGICAL ordering bug, not a data race — the race detector cannot see it and
// no black-box test can order it deterministically without production hooks
// (server.go is off limits to this change). What the storm does prove: no
// deadlock, no lost wakeups, no panic, every request completes, and dial
// amplification stays within the legit-wipe bound.
func TestRelayPinReclaimConcurrentStorm(t *testing.T) {
	dir := t.TempDir()

	proxyA := newFailingEgressProxy(t, false)
	var cMu sync.Mutex
	cCalls := 0
	proxyC := countProxy(&cCalls, &cMu)
	defer proxyC.Close()

	oddDoc := fmt.Sprintf(`
fallback: {max_attempts: 1}
health: {enabled: true, failure_threshold: 1, cooldown: 1h}
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxyA.url())
	evenDoc := fmt.Sprintf(`
fallback: {max_attempts: 1}
health: {enabled: true, failure_threshold: 1, cooldown: 1h}
egress:
  - {id: c, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [c]}
`, pxyURL(proxyC))

	s, mux, store := snapshotRouter(t, dir, oddDoc, nil)

	// Arm a: odd-generation requests must find it cooling (heads empty → 502)
	// unless an even-generation reclaim legitimately wiped the state.
	rt1 := store.Get()
	eA, ok := rt1.Egress("a")
	if !ok {
		t.Fatal("egress a missing from generation 1")
	}
	hp := health.PolicyFromSnapshot(rt1)
	s.Health.Observe(eA.HealthKey(), false, hp)

	// Swap the file on a tight loop for the storm's duration.
	stopSwapping := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		doc := evenDoc
		for {
			select {
			case <-stopSwapping:
				return
			default:
			}
			writeCfgAtomic(t, dir, testUpstreamBasePrefix+doc)
			if doc == evenDoc {
				doc = oddDoc
			} else {
				doc = evenDoc
			}
			time.Sleep(4 * time.Millisecond)
		}
	}()

	const workers = 8
	stormDone := make(chan struct{})
	var reqMu sync.Mutex
	badStatus := 0
	sampleBody := ""
	requests := 0
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { stormDone <- struct{}{} }()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
				reqMu.Lock()
				requests++
				// 200 = an even-generation request served by c. 502 = an
				// odd-generation request that either found a cooling (no
				// eligible head) or dialed it and failed pre-request with
				// max_attempts: 1 (no fallback left). A provider status could
				// not appear here at all: a's only egress never answers HTTP.
				// Anything else is broken.
				if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
					badStatus++
					if sampleBody == "" {
						sampleBody = fmt.Sprintf("%d %s", rec.Code, rec.Body.String())
					}
				}
				reqMu.Unlock()
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-stormDone
	}
	close(stopSwapping)
	swapWG.Wait()

	reqMu.Lock()
	bad := badStatus
	total := requests
	sample := sampleBody
	reqMu.Unlock()
	dials := proxyA.dialCount()
	if bad != 0 {
		t.Fatalf("%d/%d storm requests returned an unexpected status (sample: %s)", bad, total, sample)
	}
	// Each even generation's reclaim can uncool a at most once; every
	// odd-generation request ALREADY past its routeHeads filter at that
	// instant (up to `workers`) may then dial a, and the first failure
	// landing re-arms the cooldown for the rest (fallback max_attempts: 1
	// caps each at one dial).
	maxDials := (int(store.Get().Generation)/2 + 1) * workers
	if dials > maxDials {
		t.Fatalf("a dialed %d times across %d generations (bound %d) — a pinned identity's state was wiped mid-request more often than legit reclaims explain", dials, store.Get().Generation, maxDials)
	}
}
