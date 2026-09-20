// Package routing selects WHERE a request starts: the ordered egress list
// (RoutePlan) a matched route produces. Fallback (what to try after a
// retryable failure) and health (whether an egress is temporarily eligible)
// are separate concerns — routing only matches and orders, it never decides
// failure handling. The executor consumes RoutePlan and knows nothing about
// the strategies that shaped it.
package routing

import (
	"path"
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
// winner). State is keyed by route id so cursors stay stable across
// requests; Plan must be fed the CURRENT eligible head set (health/slots
// filter before the call — a head dropping out mid-stream degrades WRR
// smoothness gracefully, it never errors).
type Scheduler struct {
	mu  sync.Mutex
	rr  map[string]int
	wrr map[string]map[string]int
}

func NewScheduler() *Scheduler {
	return &Scheduler{rr: map[string]int{}, wrr: map[string]map[string]int{}}
}

// Plan orders the head set for route. heads must be non-empty and already
// filtered to the eligible egresses of route.Egress; an empty set yields an
// empty plan the caller reports as "no eligible egress".
func (s *Scheduler) Plan(rt *config.Runtime, route config.Route, heads []string) RoutePlan {
	plan := RoutePlan{RouteID: route.ID, Strategy: route.Strategy}
	if len(heads) == 0 {
		return plan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch route.Strategy {
	case config.StrategyWeightedRR:
		cw := s.wrr[route.ID]
		if cw == nil {
			cw = map[string]int{}
			s.wrr[route.ID] = cw
		}
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
		i := s.rr[route.ID] % len(heads)
		plan.Attempts = append(append([]string{}, heads[i:]...), heads[:i]...)
		s.rr[route.ID]++
	}
	plan.Egresses = resolveEgresses(rt, plan.Attempts)
	return plan
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
