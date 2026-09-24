package health

import (
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// newTest returns a registry with a controllable clock.
func newTest(now time.Time) (*Registry, *testClock) {
	c := &testClock{now: now}
	return &Registry{now: c.Now, states: map[string]*state{}, refs: map[string]int{}}, c
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// policy is a shorthand for the tests.
func policy(enabled bool, threshold int, cooldown time.Duration) Policy {
	return Policy{Enabled: enabled, Threshold: threshold, Cooldown: cooldown}
}

// keyA/keyB are stable health identities for tests.
var (
	keyA = (&config.Egress{ID: "a"}).HealthKey()
	keyB = (&config.Egress{ID: "b"}).HealthKey()
)

// TestUnknownAlwaysHealthy: egresses never observed are always healthy, and
// a disabled policy answers healthy for everything.
func TestUnknownAlwaysHealthy(t *testing.T) {
	r, _ := newTest(time.Now())
	if !r.Healthy("never-seen", policy(true, 3, time.Minute)) {
		t.Fatal("unknown egress must be healthy")
	}
	if !r.Healthy("x", policy(false, 3, time.Minute)) {
		t.Fatal("disabled policy must be healthy")
	}
}

// TestThresholdArmsCooldown: N consecutive failures arm the cooldown; the
// egress stays unhealthy THROUGH the cooldown and recovers after it.
func TestThresholdArmsCooldown(t *testing.T) {
	now := time.Now()
	r, clock := newTest(now)
	p := policy(true, 3, 10*time.Minute)

	// 2 failures: still healthy (below threshold).
	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)
	if !r.Healthy(keyA, p) {
		t.Fatal("below threshold must stay healthy")
	}

	// 3rd failure arms the cooldown.
	r.Observe(keyA, false, p)
	if r.Healthy(keyA, p) {
		t.Fatal("threshold crossed: egress must be cooling")
	}

	// Just before the cooldown expires still unhealthy…
	clock.Advance(9*time.Minute + 59*time.Second)
	if r.Healthy(keyA, p) {
		t.Fatal("egress must stay unhealthy through the cooldown")
	}

	// …and recover the instant it passes.
	clock.Advance(time.Second)
	if !r.Healthy(keyA, p) {
		t.Fatal("egress must recover after the cooldown")
	}
}

// TestSuccessResetsStreak: an observed success before the threshold clears
// the streak — no cooldown ever arms.
func TestSuccessResetsStreak(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 3, time.Minute)

	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)
	r.Observe(keyA, true, p)
	r.Observe(keyA, false, p)
	if !r.Healthy(keyA, p) {
		t.Fatal("success must reset the failure streak")
	}
	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)
	if r.Healthy(keyA, p) {
		t.Fatal("streak after a reset must still arm the cooldown")
	}
}

// TestFailuresDuringCooldownDoNotExtend: repeated failures while cooling are
// no-ops — the cooldown bounds the absence; an egress can't be pushed out
// indefinitely by a flood.
func TestFailuresDuringCooldownDoNotExtend(t *testing.T) {
	now := time.Now()
	r, clock := newTest(now)
	p := policy(true, 3, time.Minute)

	for i := 0; i < 3; i++ {
		r.Observe(keyA, false, p)
	}
	// Flood while cooling: the deadline must not move.
	for i := 0; i < 50; i++ {
		r.Observe(keyA, false, p)
	}
	clock.Advance(59 * time.Second)
	if r.Healthy(keyA, p) {
		t.Fatal("egress must still be cooling at 59s")
	}
	clock.Advance(time.Second)
	if !r.Healthy(keyA, p) {
		t.Fatal("egress must recover at 60s — the flood must not extend the cooldown")
	}
}

// TestCooldownZeroNeverCools: threshold <= 0 means "never cool" — and per
// the issue-#6 semantics a never-cool policy records NOTHING (an operator
// who disabled cooling gets no history accrual either).
func TestCooldownZeroNeverCools(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 0, time.Minute)
	for i := 0; i < 100; i++ {
		r.Observe(keyA, false, p)
	}
	if !r.Healthy(keyA, p) {
		t.Fatal("threshold <= 0 must never cool an egress")
	}
	// A later policy that DOES cool starts from an empty streak.
	strict := policy(true, 1, time.Minute)
	if !r.Healthy(keyA, strict) {
		t.Fatal("never-cool observations must not accrue history")
	}
}

// TestThresholdDecreaseDoesNotArmRetroactively pins the reload semantics of
// issue #6 §2: only an OBSERVE crossing the observing request's threshold
// arms a cooldown. Shrinking the threshold (10 → 3 over a 3-failure streak)
// must NOT flip eligibility for anyone — the next failing observation is
// what arms.
func TestThresholdDecreaseDoesNotArmRetroactively(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	old := policy(true, 10, time.Minute)
	for i := 0; i < 3; i++ {
		r.Observe(keyA, false, old)
	}
	new := policy(true, 3, time.Minute)
	if !r.Healthy(keyA, new) {
		t.Fatal("a threshold decrease must never arm a cooldown by itself")
	}
	// The NEXT failure — the first observed under the new threshold — arms:
	// streak 3 + 1 = 4 >= 3.
	r.Observe(keyA, false, new)
	if r.Healthy(keyA, new) {
		t.Fatal("the first failing observation under the decreased threshold must arm")
	}
}

// TestThresholdIncreaseKeepsStreak: the mirror row — raising the threshold
// (3 → 10) keeps the 2-failure streak for later observers instead of
// discarding it, but nothing arms until the NEW threshold is crossed.
func TestThresholdIncreaseKeepsStreak(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	old := policy(true, 3, time.Minute)
	r.Observe(keyA, false, old)
	r.Observe(keyA, false, old)
	new := policy(true, 10, time.Minute)
	if !r.Healthy(keyA, new) {
		t.Fatal("2 failures stay below the raised threshold of 10")
	}
	for i := 0; i < 7; i++ {
		r.Observe(keyA, false, new)
	}
	if !r.Healthy(keyA, new) {
		t.Fatal("9 total failures must still be below 10")
	}
	r.Observe(keyA, false, new)
	if r.Healthy(keyA, new) {
		t.Fatal("the 10th failure must arm under the raised threshold")
	}
}

// TestPolicyIsPinnedPerObservation: two generations share the state but each
// observation is judged under ITS OWN policy — a strict old-generation
// request arms the cooldown even while loose new-generation requests observe
// the same identity, and the armed cooldown is visible to everyone (state is
// global, policy is per call).
func TestPolicyIsPinnedPerObservation(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	gen1 := policy(true, 1, time.Hour)  // strict, long cooldown
	gen2 := policy(true, 10, time.Hour) // loose

	// R1 (gen1) fails once — arms under ITS threshold of 1…
	r.Observe(keyA, false, gen1)
	// …even though R2 (gen2) would need ten.
	r.Observe(keyA, false, gen2)
	if r.Healthy(keyA, gen2) {
		t.Fatal("the gen1-armed cooldown must be visible to gen2 (state is global)")
	}
}

// TestDisabledPolicyRecordsNothing: a disabled policy makes Observe a no-op
// and Healthy always true; history from BEFORE the disable survives and is
// resumed when health is re-enabled.
func TestDisabledPolicyRecordsNothing(t *testing.T) {
	now := time.Now()
	r, clock := newTest(now)
	on := policy(true, 2, time.Minute)
	off := policy(false, 2, time.Minute)

	r.Observe(keyA, false, on) // streak 1
	r.Observe(keyA, false, off)
	r.Observe(keyA, false, off)
	if !r.Healthy(keyA, off) {
		t.Fatal("disabled policy must be healthy regardless of history")
	}
	if !r.Healthy(keyA, on) {
		t.Fatal("disabled-policy observations must not accrue history (streak still 1)")
	}
	// Re-enabled: the preserved streak of 1 arms on the next failure.
	r.Observe(keyA, false, on)
	if r.Healthy(keyA, on) {
		t.Fatal("history must resume after re-enabling")
	}
	clock.Advance(time.Minute)
	if !r.Healthy(keyA, on) {
		t.Fatal("cooldown must expire")
	}
}

// TestStatesKeyedPerHealthIdentity: state is per id+transport identity —
// separate ids are independent, and the SAME id on a different transport is
// a different identity that never inherits the old transport's streak or
// cooldown (issue #6 §7).
func TestStatesKeyedPerHealthIdentity(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 1, time.Minute)

	oldURL := (&config.Egress{ID: "a", Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: "http://old:8080"}}).HealthKey()
	newURL := (&config.Egress{ID: "a", Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: "http://new:8080"}}).HealthKey()
	sameOldAgain := (&config.Egress{ID: "a", Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: "http://old:8080"}}).HealthKey()

	if oldURL == newURL {
		t.Fatal("different transports must map to different health identities")
	}
	if oldURL != sameOldAgain {
		t.Fatal("the same transport across reloads must keep one identity")
	}
	if oldURL == keyB {
		t.Fatal("different ids must not share an identity")
	}

	r.Observe(oldURL, false, p)
	if r.Healthy(oldURL, p) {
		t.Fatal("old transport must be cooling after its failure")
	}
	if !r.Healthy(newURL, p) {
		t.Fatal("a fresh transport on the same id must NOT inherit the old transport's cooldown")
	}
	if !r.Healthy(keyB, p) {
		t.Fatal("other ids must be unaffected")
	}
}

// ---- lifecycle (issue #9): active identities + in-flight pins + per-swap GC ----

// healthRuntime builds a runtime-shaped fixture through the real loader
// (File.Resolve): one route referencing every given egress, so ActiveKeys
// names exactly those egresses' identities.
func healthRuntime(t *testing.T, egs ...config.Egress) *config.Runtime {
	t.Helper()
	ids := make([]string, len(egs))
	for i, e := range egs {
		ids[i] = e.ID
	}
	rt, err := (&config.File{
		Egress: egs,
		Routes: []config.Route{{ID: "r", Egress: ids}},
	}).Resolve()
	if err != nil {
		t.Fatalf("fixture runtime: %v", err)
	}
	return rt
}

// directProxy is a small shorthand for a proxied egress fixture.
func directProxy(id, url string) config.Egress {
	return config.Egress{ID: id, Proxy: &config.Proxy{Type: config.ProxyHTTP, URL: url}}
}

// TestHealthStateSurvivesPolicyOnlyReload: a policy-only reload names the
// SAME identities, so the reclaim pass must drop nothing — the failure
// streak carries across the swap and arms at the carried count, exactly as
// if no reload had happened (issue #9: history continuity).
func TestHealthStateSurvivesPolicyOnlyReload(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 3, time.Minute)

	active := ActiveKeys(healthRuntime(t,
		config.Egress{ID: "a"}, config.Egress{ID: "b"},
	))

	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)

	if dropped := r.Reclaim(active); dropped != 0 {
		t.Fatalf("policy-only reload: active identities must never be reclaimed, dropped %d", dropped)
	}
	// The streak survived: the next failure arms.
	r.Observe(keyA, false, p)
	if r.Healthy(keyA, p) {
		t.Fatal("history must continue across a policy-only reload")
	}
}

// TestHealthStateSeparatesTransportReplacement: swapping an egress's proxy
// URL changes its identity. The new identity starts clean; once no runtime
// references the old one, its state is reclaimable — and until then it stays
// (a stale-generation request may still be observing it).
func TestHealthStateSeparatesTransportReplacement(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 1, time.Minute)

	oldEg := directProxy("a", "http://old:8080")
	newEg := directProxy("a", "http://new:8080")
	oldK := oldEg.HealthKey()
	newK := newEg.HealthKey()
	if oldK == newK {
		t.Fatal("a proxy swap must change the health identity")
	}

	r.Observe(oldK, false, p) // old transport arms its cooldown

	// Generation 1 still serves the old identity: not reclaimable.
	if dropped := r.Reclaim(ActiveKeys(healthRuntime(t, oldEg))); dropped != 0 {
		t.Fatalf("active identity must not be reclaimed, dropped %d", dropped)
	}
	// Generation 2 swaps the proxy: old identity reclaimable, new one clean.
	if dropped := r.Reclaim(ActiveKeys(healthRuntime(t, newEg))); dropped != 1 {
		t.Fatalf("abandoned transport identity must be reclaimed exactly once, dropped %d", dropped)
	}
	if !r.Healthy(newK, p) {
		t.Fatal("the fresh transport must start clean")
	}
	if !r.Healthy(oldK, p) {
		t.Fatal("the reclaimed identity must read as unknown (healthy) afterwards")
	}
	if dropped := r.Reclaim(ActiveKeys(healthRuntime(t, newEg))); dropped != 0 {
		t.Fatal("reclaim must be idempotent")
	}
}

// TestHealthStateReclaimsUnusedIdentity: an identity absent from the active
// set and unpinned is reclaimed; reclamation is exactly the unknown-state
// semantics (healthy, fresh streak on the next observation).
func TestHealthStateReclaimsUnusedIdentity(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 2, time.Minute)

	r.Observe(keyA, false, p)                      // streak 1
	rt := healthRuntime(t, config.Egress{ID: "b"}) // gen2 dropped egress a
	if dropped := r.Reclaim(ActiveKeys(rt)); dropped != 1 {
		t.Fatalf("unused identity must be reclaimed, dropped %d", dropped)
	}
	if !r.Healthy(keyA, p) {
		t.Fatal("reclaimed identity must be healthy again (unknown)")
	}
	// Observations after reclamation start a fresh streak — the identity is
	// gone, not poisoned: one failure stays below the threshold of 2 (an
	// inherited streak would arm here), and the SECOND failure arms.
	r.Observe(keyA, false, p)
	if !r.Healthy(keyA, p) {
		t.Fatal("post-reclaim observation must start from zero, not inherit the old streak")
	}
	r.Observe(keyA, false, p)
	if r.Healthy(keyA, p) {
		t.Fatal("a fresh streak must still arm at the threshold")
	}
}

// TestHealthStateDoesNotReclaimInFlightIdentity: a request pins its route's
// identities at snapshot time; even when a swap removes them from the active
// set, the pinned state must survive until the request's release — the
// request keeps adjudicating against the history it planned against.
func TestHealthStateDoesNotReclaimInFlightIdentity(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 1, time.Minute)

	r.Observe(keyA, false, p) // cooling
	release := r.Pin([]string{keyA})

	rt := healthRuntime(t, config.Egress{ID: "b"}) // gen2 no longer references egress a
	if dropped := r.Reclaim(ActiveKeys(rt)); dropped != 0 {
		t.Fatalf("a pinned identity must never be reclaimed, dropped %d", dropped)
	}
	if r.Healthy(keyA, p) {
		t.Fatal("the in-flight request must still see its armed cooldown")
	}

	// The state must survive while ANY pin is held, and become reclaimable
	// after the last release.
	release2 := r.Pin([]string{keyA})
	release()
	if dropped := r.Reclaim(ActiveKeys(rt)); dropped != 0 {
		t.Fatal("state must survive while any pin is held")
	}
	release2()
	if dropped := r.Reclaim(ActiveKeys(rt)); dropped != 1 {
		t.Fatal("after the last release the identity must be reclaimable")
	}
}

// TestConcurrentHealthObserveAndIdentityGC: observations, health checks,
// pin/release churn and reclaim passes race under -race; a pinned identity
// must survive every concurrent reclaim while held. Go's map hazard
// (concurrent write + iteration) is exactly what this test exists to catch.
func TestConcurrentHealthObserveAndIdentityGC(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	p := policy(true, 2, time.Minute)
	stale := ActiveKeys(healthRuntime(t, config.Egress{ID: "ghost"}))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Observers hammer the pinned identity and an unpinned one.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := keyA
			if i%2 == 1 {
				key = keyB
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.Observe(key, i%2 == 0, p)
				r.Healthy(key, p)
			}
		}(i)
	}
	// A request-shaped pin holder: keyA must survive reclaims while held.
	pinned := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		release := r.Pin([]string{keyA})
		close(pinned)
		<-stop
		release()
	}()
	// The reclaimer loops GC passes for an active set that never names the
	// observed identities.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-pinned
		for {
			select {
			case <-stop:
				return
			default:
				r.Reclaim(stale)
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	_, alive := r.states[keyA]
	r.mu.Unlock()
	if !alive {
		t.Fatal("pinned identity must survive concurrent reclaims")
	}
	close(stop)
	wg.Wait()

	// After the pin released and the observers stopped, one pass reclaims
	// whatever is unused; nothing panics and the map stays walkable.
	r.Reclaim(stale)
}

// TestSameEgressTwoTransportsIsolation: the same logical egress id under two
// different transport signatures (a proxy URL swap) must keep two separate
// failure states, and a Reclaim driven by the current runtime's active set
// must not mix them — the abandoned transport is reclaimed, the live one
// keeps its history.
func TestSameEgressTwoTransportsIsolation(t *testing.T) {
	r, c := newTest(time.Unix(100, 0))
	p := policy(true, 1, time.Minute)

	direct := (&config.Egress{ID: "a"}).HealthKey()
	proxied := (&config.Egress{
		ID: "a",
		Proxy: &config.Proxy{
			Type: config.ProxyHTTP,
			URL:  "http://proxy:8080",
		},
	}).HealthKey()
	if direct == proxied {
		t.Fatal("the two transport signatures must differ")
	}

	// The proxied transport fails and arms a cooldown.
	r.Observe(proxied, false, p)
	if r.Healthy(proxied, p) {
		t.Fatal("proxied transport must be cooling")
	}
	// The direct transport was never observed — still healthy, and observing
	// a success on it must NOT clear the proxied cooldown.
	if !r.Healthy(direct, p) {
		t.Fatal("direct transport must stay healthy")
	}
	r.Observe(direct, true, p)
	if r.Healthy(proxied, p) {
		t.Fatal("a success on direct must not clear the proxied cooldown")
	}

	// Time moves past the proxied cooldown; both are healthy again.
	c.Advance(2 * time.Minute)
	if !r.Healthy(proxied, p) || !r.Healthy(direct, p) {
		t.Fatal("cooldown must expire for the proxied transport only")
	}

	// A Reclaim naming only the direct transport drops the proxied state but
	// preserves the direct one.
	dropped := r.Reclaim(map[string]struct{}{direct: {}})
	if dropped != 1 {
		t.Fatalf("Reclaim dropped %d, want exactly 1 (the abandoned transport)", dropped)
	}
	r.mu.Lock()
	_, proxiedAlive := r.states[proxied]
	_, directAlive := r.states[direct]
	r.mu.Unlock()
	if proxiedAlive {
		t.Fatal("abandoned transport must be reclaimed")
	}
	if !directAlive {
		t.Fatal("active transport must survive reclaim")
	}
}
