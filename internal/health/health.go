// Package health is the temporary-eligibility layer: per-egress consecutive
// failure counting and cooldown. The three layers stay separate — routing
// chooses where to start, fallback chooses what to try after a retryable
// failure, health decides whether an egress may be tried AT ALL for a while.
//
// Policy and state are deliberately split (issue #6):
//
//   - STATE (the per-identity failure counter + cooldown deadline) is
//     process-wide runtime state. It survives config swaps and is shared by
//     every generation.
//   - POLICY (enabled / threshold / cooldown duration) is immutable per
//     request: the caller passes the Policy captured from ITS config
//     snapshot on every call. A hot reload therefore affects only new
//     requests — a request pinned to generation 1 keeps adjudicating its
//     observations under generation 1's thresholds even after generation 2
//     is live.
//
// Reloading the policy never arms or clears a cooldown by itself — only an
// Observe crossing the observing request's threshold arms one. A threshold
// DECREASE (10 → 3, streak 3) thus leaves the egress healthy until its next
// failing observation, and an INCREASE (3 → 10, streak 2) keeps the streak
// for later observers to judge. See the tests for the full semantics table.
package health

import (
	"sync"
	"time"

	"opencode-free-proxy/internal/config"
)

// Policy is the immutable health policy of one config generation. It is a
// value type: a request captures it once (from its snapshot) and passes the
// same copy to every Healthy/Observe call it makes.
type Policy struct {
	Enabled   bool
	Threshold int // consecutive failures before cooldown; <= 0 = never cools
	Cooldown  time.Duration
}

// PolicyFromSnapshot resolves the generation's effective policy (defaults
// applied, explicit 0 preserved). One call per request, at snapshot pin time.
func PolicyFromSnapshot(rt *config.Runtime) Policy {
	return Policy{
		Enabled:   rt.Health.Enabled == nil || *rt.Health.Enabled,
		Threshold: rt.HealthThreshold(),
		Cooldown:  rt.HealthCooldown(),
	}
}

// Registry tracks per-identity failure state. Zero value is not usable; use
// New. The registry itself carries NO policy: every method receives the
// caller's snapshot-pinned Policy.
//
// State identity: keys are egress id + transport signature
// (config.Egress.HealthKey). A policy-only reload keeps the same key —
// history continues across generations; swapping an egress's proxy URL
// changes the key, so a fresh physical transport never inherits the old
// transport's failure streak or cooldown.
type Registry struct {
	mu     sync.Mutex
	now    func() time.Time
	states map[string]*state
}

type state struct {
	failures int
	until    time.Time // cooldown deadline; zero = not cooling
}

func New() *Registry {
	return &Registry{now: time.Now, states: map[string]*state{}}
}

// Observe records one outcome for the identity, judged under the OBSERVING
// request's policy. A disabled policy or a never-cool threshold (<= 0)
// records nothing — an operator who turned health off (or off for cooldown
// purposes) gets no history accrual either. A success resets the failure
// streak; a failure counts toward the threshold, and CROSSING it arms the
// cooldown for the observing policy's duration. Failures during an active
// cooldown never extend it (the cooldown bounds the absence — repeated
// failures can't push an egress out indefinitely).
func (r *Registry) Observe(key string, ok bool, p Policy) {
	if !p.Enabled || p.Threshold <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.states[key]
	if st == nil {
		st = &state{}
		r.states[key] = st
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
	if st.failures >= p.Threshold {
		st.until = r.now().Add(p.Cooldown)
		st.failures = 0
	}
}

// Healthy reports whether the identity may be scheduled, judged under the
// ASKING request's policy. Unknown identities are always healthy; a disabled
// policy answers healthy for everything (the state stays put — re-enabling
// health resumes from the preserved history rather than inventing a clean
// slate).
func (r *Registry) Healthy(key string, p Policy) bool {
	if !p.Enabled {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.states[key]
	if st == nil {
		return true
	}
	return st.until.IsZero() || !r.now().Before(st.until)
}
