package identity

// Lifecycle tests for the UA sync loop: constructing the cache spawns
// nothing, StartSync is explicit, and the loop follows a LIVE cadence
// closure (re-read every cycle — a reload changes user_agent.sync_interval
// without a second StartSync). Its stop is idempotent and safe in every
// state (before any tick, after the loop exited, called twice).

import (
	"errors"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// goroutinesSettled waits for the goroutine count to fall back to baseline
// (the stopped loop's goroutine needs a moment to observe the closed done
// channel) and fails if it never does.
func goroutinesSettled(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseline {
		t.Fatalf("goroutines = %d, baseline %d — a sync goroutine leaked", after, baseline)
	}
}

// TestUserAgentCacheConstructSpawnsNothing: NewUserAgentCache + Get are pure
// — no sync goroutine exists until StartSync (the hot path never fetches, so
// a cache that is never started must cost zero goroutines).
func TestUserAgentCacheConstructSpawnsNothing(t *testing.T) {
	baseline := runtime.NumGoroutine()
	c := NewUserAgentCache()
	if ua := c.Get(); ua == "" {
		t.Fatal("Get returned an empty UA — the fail-open fallback must always render")
	}
	goroutinesSettled(t, baseline)
}

// TestStartSyncDisabledIntervalIdlesWithoutFetching: a disabled cadence (the
// OFP_CONFIG user_agent.sync_interval is <= 0 at read time) must NOT fetch —
// the loop idles on a 1 s re-check that never touches the network, so a
// reload that re-enables sync resumes in place. The stop stays idempotent.
func TestStartSyncDisabledIntervalIdlesWithoutFetching(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var fetches atomic.Int64
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		fetches.Add(1)
		return nil, errors.New("no network in tests")
	})}
	stop := c.StartSync(client, func() time.Duration { return 0 }) // disabled cadence
	time.Sleep(1300 * time.Millisecond)                            // several 1 s idle re-checks
	if n := fetches.Load(); n != 0 {
		t.Fatalf("fetches = %d, want 0 while the cadence is disabled", n)
	}
	stop()
	stop() // double stop must stay idempotent
	goroutinesSettled(t, baseline)
}

// TestStartSyncReReadsIntervalPerCycle: the loop follows the LIVE cadence
// closure. A disabled cadence idles without fetching; the moment the closure
// returns a positive interval the SAME loop resumes fetching — no second
// StartSync, no supervisor.
func TestStartSyncReReadsIntervalPerCycle(t *testing.T) {
	var fetches atomic.Int64
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		fetches.Add(1)
		return nil, errors.New("no network in tests")
	})}
	interval := int64(0)
	stop := c.StartSync(client, func() time.Duration { return time.Duration(atomic.LoadInt64(&interval)) })
	time.Sleep(1300 * time.Millisecond) // idle phase: must not fetch
	if n := fetches.Load(); n != 0 {
		t.Fatalf("fetches = %d, want 0 before the cadence is enabled", n)
	}
	atomic.StoreInt64(&interval, int64(5*time.Millisecond))
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := fetches.Load(); n == 0 {
		t.Fatal("cadence re-enabled but the loop never fetched — a reload must resume sync in place")
	}
	stop()
}

// TestStartSyncCadenceScalesDownToDisabled: the mirror of the resume test —
// a positive cadence that a reload DISABLES (closure returns 0) stops
// fetching on the next cycle (after the in-flight timer), without stopping
// the loop.
func TestStartSyncCadenceScalesDownToDisabled(t *testing.T) {
	var fetches atomic.Int64
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		fetches.Add(1)
		return nil, errors.New("no network in tests")
	})}
	interval := int64(5 * time.Millisecond)
	stop := c.StartSync(client, func() time.Duration { return time.Duration(atomic.LoadInt64(&interval)) })
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	before := fetches.Load()
	if before < 3 {
		t.Fatalf("fetches before disable = %d, want >= 3", before)
	}
	atomic.StoreInt64(&interval, 0) // reload disables sync
	time.Sleep(1400 * time.Millisecond)
	// Allow exactly ONE post-disable fetch: the in-flight timer for the last
	// positive cycle may complete before the loop re-reads the cadence. The
	// loop must then idle — an additional fetch beyond that is a disabled
	// cadence fetching.
	if n := fetches.Load(); n > before+1 {
		t.Fatalf("fetches after disable = %d, want <= %d (a disabled cadence must stop fetching)", n, before+1)
	}
	stop()
}

// TestStartSyncDoubleStop: the loop runs, a double stop neither panics nor
// double-closes the done channel, and the goroutine goes away.
func TestStartSyncDoubleStop(t *testing.T) {
	baseline := runtime.NumGoroutine()
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})}
	stop := c.StartSync(client, func() time.Duration { return 2 * time.Millisecond })
	time.Sleep(30 * time.Millisecond) // a few ticks; Warm fails open throughout
	stop()
	stop() // double stop must not panic (single close of done)
	goroutinesSettled(t, baseline)

	// The cache is untouched by the failed syncs: still the fail-open
	// fallback (all-or-nothing triples never half-apply).
	if ua := c.Get(); ua != FallbackUA() {
		t.Fatalf("UA = %q, want the fallback triple after only failed syncs", ua)
	}
}
