// Package routing selects WHERE a request starts: the ordered egress list
// (RoutePlan) a matched route produces. Fallback (what to try after a
// retryable failure) and health (whether an egress is temporarily eligible)
// are separate concerns — routing only matches and orders, it never decides
// failure handling. The executor consumes RoutePlan and knows nothing about
// the strategies that shaped it.
package routing

import (
	"path"
	"strconv"
	"strings"
	"sync"

	"opencode-free-proxy/internal/config"
)

// Profile is the per-request class the router matches routes and filters
// egresses on. Model is the suffix-stripped id (the match key); BodyBytes is
// the INBOUND raw body size; Endpoint is observability only.
type Profile struct {
	Model     string
	Streaming bool
	BodyBytes int64
	Endpoint  string // source format: "chat" | "responses"
}

// ModelAllowed reports whether the model id matches an egress glob
// allow-list. Exact ids match themselves; otherwise path.Match semantics
// (no expression language, invalid patterns never match).
func ModelAllowed(pats []string, model string) bool {
	for _, pat := range pats {
		if pat == model {
			return true
		}
		if ok, err := path.Match(pat, model); err == nil && ok {
			return true
		}
	}
	return false
}

// RoutePlan is the executor's ordered attempt list: the head is the
// scheduler's pick, the rest are the route's other eligible egresses in
// deterministic order. IDs are distinct; the executor re-checks slots at
// dial time and may skip (skip ≠ failure) an egress that filled up.
//
// Egresses carries the same ordered ids RESOLVED against the request's
// snapshot inside Plan: index i is the *config.Egress for Attempts[i]. A
// request thereafter dials transports from this pinned list — no runtime
// re-lookup exists downstream, so a hot reload mid-request cannot swap or
// drop an egress under it. Egresses may be shorter than Attempts when a
// head no longer resolves (defensive; heads always come from the same rt);
// the executor skips unresolved ids.
type RoutePlan struct {
	RouteID  string
	Strategy config.Strategy
	Attempts []string
	Egresses []*config.Egress
}

// Scheduler owns per-route rotation state. Round-robin rotates a per-route
// cursor; weighted_round_robin is nginx's smooth algorithm (add weight to
// each candidate's running total, pick the max, subtract the total from the
// winner). State is keyed by route id AND tagged with the route's scheduler
// fingerprint (schedulerFingerprint): a hot reload that leaves the
// fingerprint intact preserves the cursor and current_weight exactly —
// rotation continuity across policy-only reloads — while a change to the
// strategy, the ordered membership or (under WRR) a weight resets that one
// route's state deterministically. Plan must be fed the CURRENT eligible
// head set (health/slots filter before the call — a head dropping out
// mid-stream degrades WRR smoothness gracefully, it never errors, and the
// fingerprint never sees it: heads are per-request, membership is config).
//
// The state lifecycle is Go-side design with no JS counterpart to cite:
// open-sse has no multi-egress scheduler at all (providers are round-robined
// as unweighted key lists in services/capacityAdapter.js, with no reload
// semantics), so per AGENTS.md porting rule 4 this file documents the
// deliberate design rather than a port.
type Scheduler struct {
	mu    sync.Mutex
	state map[string]*routeState
}

// routeState is one route's rotation state, tagged with the fingerprint it
// was built under. cursor serves round_robin (the positional index into the
// eligible head set); cw serves weighted_round_robin (nginx smooth
// current_weight per egress id). The two fields never mix: strategy is a
// fingerprint input, so a route's state is always read back by the strategy
// that wrote it.
type routeState struct {
	fingerprint string
	cursor      int
	cw          map[string]int
}

func NewScheduler() *Scheduler {
	return &Scheduler{state: map[string]*routeState{}}
}

// Plan orders the head set for route. heads must be non-empty and already
// filtered to the eligible egresses of route.Egress; an empty set yields an
// empty plan the caller reports as "no eligible egress" (and touches no
// state — an unroutable request neither advances nor resets rotation).
func (s *Scheduler) Plan(rt *config.Runtime, route config.Route, heads []string) RoutePlan {
	plan := RoutePlan{RouteID: route.ID, Strategy: route.Strategy}
	if len(heads) == 0 {
		return plan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateFor(route, rt)
	switch route.Strategy {
	case config.StrategyWeightedRR:
		// st.cw is non-nil by construction: the fingerprint matched under
		// this strategy, and stateFor allocates cw exactly for WRR state.
		cw := st.cw
		total := 0
		best := heads[0]
		bestCW := -1
		for _, id := range heads {
			w := weightOf(rt, id)
			total += w
			cw[id] += w
			if cw[id] > bestCW {
				bestCW = cw[id]
				best = id
			}
		}
		cw[best] -= total
		plan.Attempts = append([]string{best}, without(heads, best)...)
	default: // StrategyRoundRobin (and unset)
		i := st.cursor % len(heads)
		plan.Attempts = append(append([]string{}, heads[i:]...), heads[:i]...)
		st.cursor++
	}
	plan.Egresses = resolveEgresses(rt, plan.Attempts)
	return plan
}

// stateFor returns the route's live rotation state, migrating on identity
// change. A matching fingerprint returns the stored state untouched — that
// preservation is the point: rotation continuity is load-bearing for
// weighted_round_robin (its smooth current_weight IS a distribution
// guarantee; restarting it re-concentrates traffic on the heavy head) and
// for round_robin's even spread. Any identity change — first sight, changed
// strategy, membership, order, or a WRR weight — replaces the state with
// empty state for THIS route only (cursor 0, no current_weight): the next
// plan starts from the route's deterministic first position, never from a
// leftover offset of the old shape. PruneRoutes is the complement: it
// deletes state wholesale for routes the runtime no longer names.
func (s *Scheduler) stateFor(route config.Route, rt *config.Runtime) *routeState {
	fp := schedulerFingerprint(route, rt)
	if st, ok := s.state[route.ID]; ok && st.fingerprint == fp {
		return st
	}
	st := &routeState{fingerprint: fp}
	if route.Strategy == config.StrategyWeightedRR {
		st.cw = map[string]int{}
	}
	s.state[route.ID] = st
	return st
}

// schedulerFingerprint is the identity of one route's rotation state: the
// inputs that change what the stored state MEANS. Same fingerprint across a
// reload → keep the state byte-for-byte; changed fingerprint → reset that
// route deterministically (stateFor).
//
// Inputs: the strategy (it selects which field of routeState is live), the
// route id, and the route's config-level egress list IN ORDER — deliberately
// NOT the eligible-head subset Plan receives. Heads fluctuate per request
// with health/slots/streaming/body filtering; keying state identity on them
// would reset rotation on the first transient outage, destroying exactly the
// continuity the fingerprint exists to preserve. Order is part of the
// identity because rotation reads it: the rr cursor is positional over the
// head order (which follows route.Egress) and smooth-WRR breaks
// current_weight ties by that order. Each member's effective weight joins
// the fingerprint ONLY under weighted_round_robin: round_robin never reads
// weights (weightOf is consulted solely in the WRR branch), so folding them
// in would reset a strategy that ignores them — a spurious rotation loss on
// a weight-only edit.
//
// The weight input is also what kills the cross-generation WRR carryover the
// adversarial review proved: route [Z,A] with weights Z=5/A=1 drives
// current_weight to {Z:2,A:-2} after four requests; a reload to Z=0/A=1 that
// PRESERVED the state would combine the stale totals with the new weights
// and head Z — a weight-0 egress ahead of an eligible positive-weight
// sibling, contradicting the documented weight-0-never-heads invariant. The
// weight change alters the fingerprint, the reset zeroes the totals, and Z
// (adding 0 every round) can never out-poll A again. Self-correction is not
// good enough when the violation is provable on the very first post-reload
// plan.
//
// Encoding: NUL-separated fields, member count included — injective because
// Validate rejects control characters in route and egress ids (the same
// argument as config.Egress.HealthKey: the separator byte cannot appear in a
// field) and every other field is a strategy name or a decimal count. An
// unset strategy is normalized to round_robin (Resolve keeps "" as written;
// Plan schedules "" and "round_robin" identically, so they must share one
// identity or a cosmetic edit would reset rotation). This is a map key, not
// a message — never logged.
func schedulerFingerprint(route config.Route, rt *config.Runtime) string {
	strategy := route.Strategy
	if strategy == "" {
		strategy = config.StrategyRoundRobin
	}
	var b strings.Builder
	b.WriteString(string(strategy))
	b.WriteByte(0)
	b.WriteString(route.ID)
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(len(route.Egress)))
	for _, id := range route.Egress {
		b.WriteByte(0)
		b.WriteString(id)
		if strategy == config.StrategyWeightedRR {
			b.WriteByte(0)
			b.WriteString(strconv.Itoa(weightOf(rt, id)))
		}
	}
	return b.String()
}

// PruneRoutes drops rotation state for every route the keep set does not
// name — the scheduler half of the once-per-generation maintenance in
// Server.onGeneration, alongside transport pruning and health reclamation,
// under the same monotonic generation guard. A removed route's state must
// not survive: nothing would ever overwrite it, and re-adding a route with
// the same id later would resurrect a cursor or current_weight from an
// unrelated config era. The keep set holds route ids (nil drops everything);
// surviving entries stay keyed by id alone — fingerprint identity still
// guards them, so a kept route whose shape changed resets itself at its next
// Plan. Returns the number of states dropped.
func (s *Scheduler) PruneRoutes(keep map[string]struct{}) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := 0
	for id := range s.state {
		if _, ok := keep[id]; !ok {
			delete(s.state, id)
			dropped++
		}
	}
	return dropped
}

// resolveEgresses pins Attempts to their snapshot *config.Egress, one per
// id and index-aligned. An id absent from rt yields a nil slot the executor
// skips (defensive — heads are always resolved from this same snapshot).
func resolveEgresses(rt *config.Runtime, ids []string) []*config.Egress {
	out := make([]*config.Egress, len(ids))
	for i, id := range ids {
		if e, ok := rt.Egress(id); ok {
			out[i] = e
		}
	}
	return out
}

func weightOf(rt *config.Runtime, id string) int {
	if e, ok := rt.Egress(id); ok {
		return e.EffectiveWeight()
	}
	return 1
}

// without returns xs minus the first occurrence of drop, preserving order.
func without(xs []string, drop string) []string {
	out := make([]string, 0, len(xs)-1)
	for _, x := range xs {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}
