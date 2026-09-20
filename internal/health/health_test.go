package health

import (
	"testing"
	"time"
)

// newTest returns a registry with a controllable clock.
func newTest(now time.Time) (*Registry, *testClock) {
	c := &testClock{now: now}
	return &Registry{now: c.Now, states: map[string]*state{}}, c
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// TestUnknownAlwaysHealthy: egresses never observed are always healthy, and
// the disabled registry reports healthy for everything.
func TestUnknownAlwaysHealthy(t *testing.T) {
	r, _ := newTest(time.Now())
	if !r.Healthy("never-seen") {
		t.Fatal("unknown egress must be healthy")
	}
	r.Configure(false, 3, time.Minute)
	if !r.Healthy("x") {
		t.Fatal("disabled registry must be healthy")
	}
}

// TestThresholdArmsCooldown: N consecutive failures arm the cooldown; the
// egress stays unhealthy THROUGH the cooldown and recovers after it.
func TestThresholdArmsCooldown(t *testing.T) {
	now := time.Now()
	r, clock := newTest(now)
	r.Configure(true, 3, 10*time.Minute)

	// 2 failures: still healthy (below threshold).
	r.Observe("a", false)
	r.Observe("a", false)
	if !r.Healthy("a") {
		t.Fatal("below threshold must stay healthy")
	}

	// 3rd failure arms the cooldown.
	r.Observe("a", false)
	if r.Healthy("a") {
		t.Fatal("threshold crossed: egress must be cooling")
	}

	// Just before the cooldown expires still unhealthy…
	clock.Advance(9*time.Minute + 59*time.Second)
	if r.Healthy("a") {
		t.Fatal("egress must stay unhealthy through the cooldown")
	}

	// …and recover the instant it passes.
	clock.Advance(time.Second)
	if !r.Healthy("a") {
		t.Fatal("egress must recover after the cooldown")
	}
}

// TestSuccessResetsStreak: an observed success before the threshold clears
// the streak — no cooldown ever arms.
func TestSuccessResetsStreak(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	r.Configure(true, 3, time.Minute)

	r.Observe("a", false)
	r.Observe("a", false)
	r.Observe("a", true)
	r.Observe("a", false)
	if !r.Healthy("a") {
		t.Fatal("success must reset the failure streak")
	}
	r.Observe("a", false)
	r.Observe("a", false)
	r.Observe("a", false)
	if r.Healthy("a") {
		t.Fatal("streak after a reset must still arm the cooldown")
	}
}

// TestFailuresDuringCooldownDoNotExtend: repeated failures while cooling are
// no-ops — the cooldown bounds the absence; an egress can't be pushed out
// indefinitely by a flood.
func TestFailuresDuringCooldownDoNotExtend(t *testing.T) {
	now := time.Now()
	r, clock := newTest(now)
	r.Configure(true, 3, time.Minute)

	for i := 0; i < 3; i++ {
		r.Observe("a", false)
	}
	// Flood while cooling: the deadline must not move.
	for i := 0; i < 50; i++ {
		r.Observe("a", false)
	}
	clock.Advance(59 * time.Second)
	if r.Healthy("a") {
		t.Fatal("egress must still be cooling at 59s")
	}
	clock.Advance(time.Second)
	if !r.Healthy("a") {
		t.Fatal("egress must recover at 60s — the flood must not extend the cooldown")
	}
}

// TestReloadThresholdTakesEffect: Configure is applied per request, so a
// snapshot swap changes future behavior without touching state. A stale
// threshold of 10 with only 3 failures observed stays healthy.
func TestReloadThresholdTakesEffect(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	r.Configure(true, 10, time.Minute)
	for i := 0; i < 3; i++ {
		r.Observe("a", false)
	}
	r.Configure(true, 3, time.Minute)
	if !r.Healthy("a") {
		t.Fatal("existing streak must be evaluated against the NEW threshold (3 of 3 should arm)")
	}
}

// TestDisabledRegistryIgnoresObservations: switching health off makes all
// egresses healthy again regardless of history.
func TestDisabledRegistryIgnoresObservations(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	r.Configure(true, 2, time.Minute)
	for i := 0; i < 2; i++ {
		r.Observe("a", false)
	}
	if r.Healthy("a") {
		t.Fatal("cooling before disabling")
	}
	r.Configure(false, 2, time.Minute)
	if !r.Healthy("a") {
		t.Fatal("disabled registry must be healthy regardless of history")
	}
}

// TestCooldownZeroNeverCools: threshold <= 0 means "never cool" — failures
// accumulate but eligibility never flips.
func TestCooldownZeroNeverCools(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	r.Configure(true, 0, time.Minute)
	for i := 0; i < 100; i++ {
		r.Observe("a", false)
	}
	if !r.Healthy("a") {
		t.Fatal("threshold <= 0 must never cool an egress")
	}
}

// TestStatesKeyedPerEgress: failure state is per-id, never shared.
func TestStatesKeyedPerEgress(t *testing.T) {
	now := time.Now()
	r, _ := newTest(now)
	r.Configure(true, 2, time.Minute)
	r.Observe("a", false)
	r.Observe("b", false)
	if !r.Healthy("a") || !r.Healthy("b") {
		t.Fatal("both egresses at 1/2 must still be healthy")
	}
	r.Observe("a", false)
	if !r.Healthy("b") {
		t.Fatal("b's streak must be independent of a")
	}
	if r.Healthy("a") {
		t.Fatal("a must be cooling")
	}
}
