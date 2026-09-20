// Package health is the temporary-eligibility layer: per-egress consecutive
// failure counting and cooldown. The three layers stay separate — routing
// chooses where to start, fallback chooses what to try after a retryable
// failure, health decides whether an egress may be tried AT ALL for a while.
//
// The registry is keyed by egress id and survives config swaps: state for an
// egress that leaves the config is inert until its id returns, at which
// point its history resumes.
package health

import (
	"sync"
	"time"
)

// Registry tracks per-egress failure state. Zero value is not usable; use
// New. Configure applies the current snapshot's policy — it is called on
// every request (a mutex-protected field write), so a hot reload of
// threshold/cooldown takes effect on the next request.
type Registry struct {
	mu        sync.Mutex
	enabled   bool
	threshold int // consecutive failures before cooldown; <= 0 = never cools
	cooldown  time.Duration
	now       func() time.Time
	states    map[string]*state
}

type state struct {
	failures int
	until    time.Time // cooldown deadline; zero = not cooling
}

func New() *Registry {
	return &Registry{now: time.Now, states: map[string]*state{}}
}

// Configure applies the policy (nil-safe: a disabled registry makes Healthy
// always true and Observe a no-op).
func (r *Registry) Configure(enabled bool, threshold int, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
	r.threshold = threshold
	r.cooldown = cooldown
}

// Observe records one outcome. A success resets the failure streak. A
// failure counts toward the threshold; crossing it arms the cooldown.
// Failures during an active cooldown never extend it (the cooldown bounds
// the absence — repeated failures can't push an egress out indefinitely).
func (r *Registry) Observe(id string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled || r.threshold <= 0 {
		return
	}
	st := r.states[id]
	if st == nil {
		st = &state{}
		r.states[id] = st
	}
	if ok {
		st.failures = 0
		st.until = time.Time{}
		return
	}
	if r.now().Before(st.until) {
		return // already cooling; don't extend
	}
	st.failures++
	if st.failures >= r.threshold {
		st.until = r.now().Add(r.cooldown)
		st.failures = 0
	}
}

// Healthy reports whether the egress may be scheduled. Unknown egresses and
// the disabled registry are always healthy.
func (r *Registry) Healthy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return true
	}
	st := r.states[id]
	if st == nil {
		return true
	}
	return st.until.IsZero() || !r.now().Before(st.until)
}
