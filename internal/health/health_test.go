package health

import (
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// newTest returns a registry with a controllable clock.
func newTest(now time.Time) (*Registry, *testClock) {
	c := &testClock{now: now}
	return &Registry{now: c.Now, states: map[string]*state{}}, c
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
