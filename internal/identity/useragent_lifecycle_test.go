package identity

// Lifecycle tests for the UA sync loop: constructing the cache spawns
// nothing, StartSync is explicit, and its stop is idempotent and safe in
// every state (before any tick, after the loop exited, called twice).

import (
	"errors"
	"net/http"
	"runtime"
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

// TestStartSyncNonPositiveIntervalStartsNothing: a misconfigured cadence
// (time.NewTicker would panic on <= 0) degrades to a no-op stop and the
// compiled-in fallback triple.
func TestStartSyncNonPositiveIntervalStartsNothing(t *testing.T) {
	baseline := runtime.NumGoroutine()
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})}
	for _, interval := range []time.Duration{0, -1 * time.Second} {
		stop := c.StartSync(client, interval)
		stop()
		stop() // the no-op stop must be idempotent too
	}
	goroutinesSettled(t, baseline)
}

// TestStartSyncDoubleStop: the loop runs, a double stop neither panics nor
// double-closes the done channel, and the goroutine goes away.
func TestStartSyncDoubleStop(t *testing.T) {
	baseline := runtime.NumGoroutine()
	c := NewUserAgentCache()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})}
	stop := c.StartSync(client, 2*time.Millisecond)
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
