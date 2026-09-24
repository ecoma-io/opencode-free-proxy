// Package routing selects WHERE a request starts: the ordered egress list
// (RoutePlan) a matched route produces. Failover (what to try after a
// replay-safe failure) and health (whether an egress is temporarily eligible)
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
// allow-list. Exact ids match themselves; otherwise path.Match semantics —
// `*` (any run of non-separator bytes), `?` (one non-separator byte),
// `[...]` character classes and `\` escapes; an invalid pattern never
// matches (path.ErrBadPattern is a miss, not an error).
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
// drop an egress under it. Egresses is always the SAME LENGTH as Attempts;
// an id that no longer resolves in the snapshot leaves a nil slot at its
// index (defensive — heads always come from the same rt), and the executor
// treats a nil slot as a skip, never a failure.
type RoutePlan struct {
	RouteID  string
	Strategy config.Strategy
	Attempts []string
	Egresses []*config.Egress
}

// Scheduler owns per-route rotation state. Round-robin rotates a per-route
// cursor; weighted_round_robin is nginx's smooth algorithm (add weight to
// each candidate's running total, pick the max, subtract the total from the
// winner) over the POSITIVE-WEIGHT members only — a weight-0 egress is
// "configured but never scheduled as a route head" (config.Egress.Weight),
// so it never enters selection; it keeps its place in the plan's fallback
// tail. State is keyed by route id AND tagged with the route's scheduler
// fingerprint (schedulerFingerprint): a hot reload that leaves the
// fingerprint intact preserves the cursor and current_weight exactly —
// rotation continuity across policy-only reloads — while a change to the
// strategy, the ordered membership or (under WRR) a weight resets that one
// route's state deterministically. Each stored state also records the config
// GENERATION that wrote it (stateFor): a request holding a superseded
// snapshot can still advance a same-shape rotation, but it can never replace
// a newer generation's state with an older shape's — it plans against
// throwaway state instead. Plan must be fed the CURRENT eligible head set
// (health/slots filter before the call — a head dropping out mid-stream
// degrades WRR smoothness gracefully, it never errors, and the fingerprint
// never sees it: heads are per-request, membership is config).
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
// was built under and the generation that stored it. cursor serves
// round_robin (the positional index into the eligible head set); cw serves
// weighted_round_robin (nginx smooth current_weight per egress id, positive-
// weight members only — selection never creates or reads a weight-0 entry).
// The cursor/cw fields never mix: strategy is a fingerprint input, so a
// route's state is always read back by the strategy that wrote it.
type routeState struct {
	fingerprint string
	// generation is the Runtime.Generation that WROTE this state. It is not
	// updated when a same-fingerprint request advances the state (any
	// generation may participate in a shape it shares); it exists solely as
	// the stale-write guard in stateFor: a request from an OLDER generation
	// whose shape differs from the stored fingerprint must not replace the
	// newer generation's state with its own.
	generation uint64
	cursor     int
	cw         map[string]int
}

func NewScheduler() *Scheduler {
	return &Scheduler{state: map[string]*routeState{}}
}

// Plan orders the head set for route. heads must be non-empty and already
// filtered to the eligible egresses of route.Egress; an empty set yields an
// empty plan the caller reports as "no eligible egress" (and touches no
// state — an unroutable request neither advances nor resets rotation).
// Under weighted_round_robin the same empty-plan answer serves a head set
// whose every member carries weight 0: config validation rejects a WRR route
// with no positive-weight member ("needs at least one egress with weight >
// 0"), and the runtime analogue — every positive-weight member temporarily
// ineligible, only weight-0 survivors — is equally unservable, because a
// weight-0 egress is never a route head. It also touches no state.
func (s *Scheduler) Plan(rt *config.Runtime, route config.Route, heads []string) RoutePlan {
	plan := RoutePlan{RouteID: route.ID, Strategy: route.Strategy}
	if len(heads) == 0 {
		return plan
	}
	// WRR selection pool, resolved ONCE against this request's snapshot and
	// BEFORE any state is touched: only positive-weight members are
	// schedulable heads, so they are the only members the smooth algorithm
	// ever adds to, reads from or subtracts against. Weight-0 members are
	// excluded here structurally — their eligibility (they ARE in heads and
	// stay in the plan's fallback tail) is untouched; their head-ness is not
	// a scheduling question but a configuration stance, so no current_weight
	// value — fresh, stale or flapped — can put one at the head.
	var pool []wrrCandidate
	if route.Strategy == config.StrategyWeightedRR {
		for _, id := range heads {
			if w := weightOf(rt, id); w > 0 {
				pool = append(pool, wrrCandidate{id: id, w: w})
			}
		}
		if len(pool) == 0 {
			return plan
		}
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
		var best string
		bestCW := 0
		for i, c := range pool {
			total += c.w
			cw[c.id] += c.w
			// i == 0 seeds the maximum: a candidate's running total can be
			// negative after past rounds (it was served), so a -1 sentinel
			// would let a stale total fall below it and leave best unset —
			// the first candidate wins any tie, preserving nginx's order
			// tie-break over the head order.
			if i == 0 || cw[c.id] > bestCW {
				best = c.id
				bestCW = cw[c.id]
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

// wrrCandidate is one positive-weight head the WRR arm may select. The
// weight is resolved once, against the request's own snapshot, before any
// rotation state is read or written — the selection pool and the
// current_weight it feeds can never mix weights from two generations.
type wrrCandidate struct {
	id string
	w  int
}

// stateFor returns the route's rotation state for THIS snapshot: the stored
// state on a fingerprint match, fresh empty state on an identity change, or
// an unstored throwaway when the snapshot is a superseded generation trying
// to change the state's identity.
//
// A matching fingerprint returns the stored state untouched — that
// preservation is the point: rotation continuity is load-bearing for
// weighted_round_robin (its smooth current_weight IS a distribution
// guarantee; restarting it re-concentrates traffic on the heavy head) and
// for round_robin's even spread. The match ignores generations by design: a
// policy-only reload keeps the shape, so the newer generation's requests
// continue the same rotation and an in-flight request from the older
// generation may still take its turn in it — same shape, same meaning.
//
// An identity change — first sight, changed strategy, membership, order, or
// a WRR weight — replaces the state with empty state for THIS route only
// (cursor 0, no current_weight): the next plan starts from the route's
// deterministic first position, never from a leftover offset of the old
// shape. The replacement is GENERATION-GUARDED: a request holds its arrival
// snapshot for its whole lifetime, so it can reach stateFor after newer
// traffic already stored a NEWER generation's shape. An unguarded write
// there would key old-fingerprint state under the route id, and the next
// current-generation plan would see a mismatch and reset — a spurious
// rotation restart (WRR re-concentrates on its heavy head) caused by a
// request the config had already moved past. So a snapshot older than the
// stored state's generation never writes: it plans against fresh throwaway
// state (deterministic first pick for its own shape, attempts still valid
// for its own snapshot) and the live rotation is left exactly as the newer
// generation left it — the same "neither advances nor resets" stance Plan
// gives an unroutable request. Equal generation with a different
// fingerprint still replaces: one generation names one shape, so that is a
// legitimate reset (reachable only from hand-built runtimes; the store
// stamps each swap with a fresh monotonic generation). PruneRoutes is the
// complement: it deletes state wholesale for routes the runtime no longer
// names.
func (s *Scheduler) stateFor(route config.Route, rt *config.Runtime) *routeState {
	fp := schedulerFingerprint(route, rt)
	if st, ok := s.state[route.ID]; ok {
		if st.fingerprint == fp {
			return st
		}
		if rt.Generation < st.generation {
			return freshState(route, fp)
		}
	}
	st := freshState(route, fp)
	st.generation = rt.Generation
	s.state[route.ID] = st
	return st
}

// freshState builds empty rotation state for a shape: cursor 0, no
// current_weight. Callers that store it must stamp the generation.
func freshState(route config.Route, fp string) *routeState {
	st := &routeState{fingerprint: fp}
	if route.Strategy == config.StrategyWeightedRR {
		st.cw = map[string]int{}
	}
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
// The weight input is one half of what kills the weight-0 head violation
// the adversarial review proved: route [Z,A] with weights Z=5/A=1 drives
// current_weight to {Z:2,A:-2} after four requests, and a reload to Z=0/A=1
// that PRESERVED the state would combine the stale totals with the new
// weights and head Z — a weight-0 egress ahead of an eligible
// positive-weight sibling. The weight change alters the fingerprint, so the
// reset stops the stale totals from ever meeting the new weights. But the
// reset alone was never sufficient enforcement: current_weight survives
// ELIGIBILITY flaps untouched (no reload, same fingerprint), and the old
// argument that "Z adds 0 every round, so it can never out-poll A" only
// held while A's total stayed non-negative — a returning sibling can carry
// a stale negative total (A at -1 polls 0 after its weight, Z ties it at 0
// and wins on order). That is why the other half is structural, in Plan:
// weight-0 members never enter the selection pool at all, so no current_
// weight value — fresh, stale or flapped — can put one at the head.
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
	// The fingerprint is rebuilt for every Plan. Grow pre-warms capacity for
	// the fixed fields plus a small per-member budget, so normal short routes
	// avoid a Builder growth; Grow changes capacity only, and the NUL-delimited
	// identity remains byte-for-byte identical. The budget is deliberately not
	// exact (ids and decimal weights vary in length) — it trades one growth
	// for larger shapes, not byte identity.
	b.Grow(len(strategy) + len(route.ID) + len(route.Egress)*(len(strconv.Itoa(0))+1))
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

// resolveEgresses pins each attempt to a private copy of its snapshot
// *config.Egress, one per id and index-aligned — the plan owns its memory
// and never aliases the snapshot (config.Egress clone-per-call). An id
// absent from rt yields a nil slot the executor skips (defensive — heads
// are always resolved from this same snapshot).
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
