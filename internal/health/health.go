// Package health is the temporary-eligibility layer: per-egress consecutive
// failure counting and cooldown. The three layers stay separate — routing
// chooses where to start, failover chooses what to try after a replay-safe
// failure, health decides whether an egress may be tried AT ALL for a while.
// Health measures EGRESS-PATH health only: it is marked by
// Failure.MarksEgressHealth, which is the failing step's own answer to "was
// this the egress endpoint's fault?" (issue #62). That is deliberately narrower
// than the replay-safety that permits the move (Failure.ReplaySafe), so neither
// a provider verdict — 429, 4xx, 5xx alike — nor a destination-side transport
// failure — target TCP connect, origin TLS, CONNECT refusal — poisons an
// egress. Callers pass only failures the predicate admitted; this package has
// no opinion about which they are.
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
//
// Lifecycle (issue #9): an entry is reclaimed when its identity is neither
// ACTIVE (derivable from the current runtime's routes — health.ActiveKeys)
// nor PINNED by an in-flight request (Pin/Release, taken at the request's
// snapshot pin and held for the request's whole lifetime). The router runs
// one Reclaim per generation swap, next to its transport-cache prune, so
// reclamation is deterministic and testable — never a timer. A pinned
// identity can never vanish mid-request, and an identity absent from every
// new generation does not linger forever.
type Registry struct {
	mu  sync.Mutex
	now func() time.Time
	// states holds one entry per identity SEEN since its last reclaim.
	// Entries survive reloads (the identity contract) and are dropped only by
	// Reclaim. Growth between reclaims is bounded by the number of distinct
	// transports configured in the generations live in that window — an
	// operator-driven input (proxy URL/credential rotation), not a
	// per-request one — and each entry is two small fields.
	states map[string]*state
	// refs counts in-flight Pin holds per identity. A nonzero count shields
	// the identity's state from Reclaim even when no current runtime names
	// it — an old-generation request keeps adjudicating against the history
	// it planned against.
	refs map[string]int
}

type state struct {
	failures int
	until    time.Time // cooldown deadline; zero = not cooling
}

func New() *Registry {
	return &Registry{now: time.Now, states: map[string]*state{}, refs: map[string]int{}}
}

// Pin takes one in-flight reference on each key and returns the matching
// release. The caller pins the health keys of its matched route at snapshot
// pin time — BEFORE eligibility consults the registry — and releases exactly
// once at request end, so Reclaim can never drop a state the request is
// mid-way through using. Pinning does not create state; it only shields
// whatever exists (or will be created by this request's observations).
func (r *Registry) Pin(keys []string) (release func()) {
	r.mu.Lock()
	for _, k := range keys {
		r.refs[k]++
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		for _, k := range keys {
			if r.refs[k] > 0 {
				r.refs[k]--
				if r.refs[k] == 0 {
					delete(r.refs, k)
				}
			}
		}
		r.mu.Unlock()
	}
}

// Reclaim deletes the state of every identity that is neither in the active
// set (the identities the CURRENT runtime's routes can reach —
// health.ActiveKeys) nor pinned by an in-flight request. One call per
// generation swap, from the router's generation maintenance next to the
// transport-cache prune. A policy-only reload's active set names the same
// identities, so history continues; a transport swap's set names only the
// new identity, so the abandoned one is reclaimed here; a pinned identity is
// skipped whatever the active set says, and a later Reclaim — after the
// pin's release — collects it. Returns the number of entries dropped (a
// count only: the keys embed transport signatures and are never logged).
func (r *Registry) Reclaim(active map[string]struct{}) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	dropped := 0
	for k := range r.states {
		if _, isActive := active[k]; isActive {
			continue
		}
		if r.refs[k] > 0 {
			continue
		}
		delete(r.states, k)
		dropped++
	}
	return dropped
}

// ActiveKeys derives the identity set a runtime can still reach: every
// route-referenced egress's HealthKey. Route-referenced, not every defined
// egress — eligibility and observation are only ever consulted for egresses
// a route lists, mirroring the transport cache's keep-set; an egress no
// route references has no live identity to protect.
func ActiveKeys(rt *config.Runtime) map[string]struct{} {
	active := make(map[string]struct{}, len(rt.File.Egress))
	for _, route := range rt.Routes() {
		for _, id := range route.Egress {
			if e, ok := rt.Egress(id); ok {
				active[e.HealthKey()] = struct{}{}
			}
		}
	}
	return active
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
