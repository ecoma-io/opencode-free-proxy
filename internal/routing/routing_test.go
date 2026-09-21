package routing

import (
	"reflect"
	"testing"

	"opencode-free-proxy/internal/config"
)

// w is a weight pointer helper for Egress specs.
func w(n int) *int { return &n }

// egress must place the requested weight in the spec.
func egress(id string, weight int) config.Egress {
	return config.Egress{ID: id, Weight: w(weight), MaxConcurrency: 0}
}

func route(id string, strategy config.Strategy, eg ...string) config.Route {
	return config.Route{ID: id, Strategy: strategy, Egress: eg}
}

// weightedRuntime builds a resolved snapshot with the given egresses and one
// weighted_round_robin route; an egress listed on the route but absent from
// the spec gets weight 1 (weightOf default) — no crash.
func weightedRuntime(eg ...config.Egress) *config.Runtime {
	spec := config.File{
		Egress: eg,
		Routes: []config.Route{route("r", config.StrategyWeightedRR, "a", "b", "c")},
	}
	rt, err := spec.Resolve()
	if err != nil {
		panic(err)
	}
	return rt
}

// wrrRuntime resolves a weighted_round_robin snapshot with the given route
// egress list (weightedRuntime's fixed [a b c] route does not fit every row).
func wrrRuntime(routeEgress []string, eg ...config.Egress) *config.Runtime {
	spec := config.File{
		Egress: eg,
		Routes: []config.Route{route("r", config.StrategyWeightedRR, routeEgress...)},
	}
	rt, err := spec.Resolve()
	if err != nil {
		panic(err)
	}
	return rt
}

// TestModelAllowed: exact ids match themselves; globs use path.Match
// semantics; an EMPTY list admits nothing (the router's len>0 guard owns the
// "no allow-list = every model" contract); a malformed pattern never matches
// (path.ErrBadPattern is a miss, not a panic).
func TestModelAllowed(t *testing.T) {

	if !ModelAllowed([]string{"qwen3-coder-free"}, "qwen3-coder-free") {
		t.Fatal("exact id must match")
	}
	if ModelAllowed([]string{"qwen3-coder-free"}, "qwen3-coder-free-extra") {
		t.Fatal("exact id must not match longer ids")
	}
	if !ModelAllowed([]string{"muse-*-free"}, "muse-spark-1.3-contributor-free") {
		t.Fatal("glob must match")
	}
	if ModelAllowed([]string{"muse-*-free"}, "muse") {
		t.Fatal("glob must not match unrelated ids")
	}
	if ModelAllowed(nil, "anything") {
		t.Fatal("empty allow-list must admit nothing (router guards len>0)")
	}
	if ModelAllowed([]string{}, "anything") {
		t.Fatal("empty allow-list must admit nothing")
	}
	if ModelAllowed([]string{"["}, "anything") {
		t.Fatal("malformed pattern must not match")
	}
}

// TestModelAllowedBracketAndEscape: the model gates run the FULL path.Match
// syntax, not a star-only subset — [..] classes match one listed byte, "\"
// escapes a metacharacter into a literal, "?" matches one byte. Pinned in
// both directions so the doc comment cannot drift from the matcher again.
func TestModelAllowedBracketAndEscape(t *testing.T) {
	if !ModelAllowed([]string{"muse-[abc]-free"}, "muse-b-free") {
		t.Fatal("character class must match a listed member")
	}
	if ModelAllowed([]string{"muse-[abc]-free"}, "muse-d-free") {
		t.Fatal("character class must not match an unlisted member")
	}
	if !ModelAllowed([]string{`muse-\*-free`}, "muse-*-free") {
		t.Fatal("escaped * must match a literal *")
	}
	if ModelAllowed([]string{`muse-\*-free`}, "muse-x-free") {
		t.Fatal("escaped * must not act as a wildcard")
	}
	if !ModelAllowed([]string{"muse-?-free"}, "muse-1-free") {
		t.Fatal("? must match exactly one character")
	}
}

func TestPlanEmptyHeads(t *testing.T) {
	rt := config.DefaultRuntime()
	p := NewScheduler().Plan(rt, route("r", config.StrategyRoundRobin), nil)
	if p.RouteID != "r" || len(p.Attempts) != 0 {
		t.Fatalf("empty heads must yield empty plan, got %+v", p)
	}
}

func TestPlanRoundRobin(t *testing.T) {
	rt := config.DefaultRuntime()
	s := NewScheduler()
	a, b, c := "a", "b", "c"

	for i, want := range [][3]string{
		{"a", "b", "c"},
		{"b", "c", "a"},
		{"c", "a", "b"},
		{"a", "b", "c"},
	} {
		p := s.Plan(rt, route("r", config.StrategyRoundRobin, a, b, c), []string{a, b, c})
		for j := range want {
			if p.Attempts[j] != want[j] {
				t.Fatalf("round %d: attempt %d = %q, want %q (plan %v)", i, j, p.Attempts[j], want[j], p.Attempts)
			}
		}
		if p.RouteID != "r" || p.Strategy != config.StrategyRoundRobin {
			t.Fatalf("route identity lost: %+v", p)
		}
	}
}

// TestPlanRoundRobinHeadDrop: a head dropping out (health/slots) must not
// stall or error — the cursor advances over whatever remains. The ROUTE stays
// the config's route (route.Egress is fingerprint identity — shrinking it
// would be a membership change, covered by the state-lifecycle membership
// test); only the eligible head set shrinks.
func TestPlanRoundRobinHeadDrop(t *testing.T) {
	rt := config.DefaultRuntime()
	s := NewScheduler()
	r := route("r", config.StrategyRoundRobin, "a", "b", "c")
	_ = s.Plan(rt, r, []string{"a", "b", "c"})

	// cursor is at 1 after the first plan: over [b c] that starts at c.
	p := s.Plan(rt, r, []string{"b", "c"})
	if p.Attempts[0] != "c" || p.Attempts[1] != "b" {
		t.Fatalf("after head drop plan = %v, want [c b]", p.Attempts)
	}
}

// TestPlanRoundRobinCursorPerRoute: two routes share nothing — their cursors
// advance independently.
func TestPlanRoundRobinCursorPerRoute(t *testing.T) {
	rt := config.DefaultRuntime()
	s := NewScheduler()
	p1 := s.Plan(rt, route("r1", config.StrategyRoundRobin, "a", "b"), []string{"a", "b"})
	p2 := s.Plan(rt, route("r2", config.StrategyRoundRobin, "x", "y"), []string{"x", "y"})
	_ = s.Plan(rt, route("r2", config.StrategyRoundRobin, "x", "y"), []string{"x", "y"})
	p3 := s.Plan(rt, route("r1", config.StrategyRoundRobin, "a", "b"), []string{"a", "b"})
	if p1.Attempts[0] != "a" || p3.Attempts[0] != "b" {
		t.Fatalf("r1 cursor must advance independently: %v then %v", p1.Attempts, p3.Attempts)
	}
	if p2.Attempts[0] != "x" {
		t.Fatalf("r2 first plan = %v, want head x", p2.Attempts)
	}
}

func TestPlanWeightedRR(t *testing.T) {
	// nginx smooth WRR with weights a=5, b=1, c=1 over heads [a b c]: the
	// deterministic 7-round sequence is a a b a c a a.
	rt := weightedRuntime(egress("a", 5), egress("b", 1), egress("c", 1))
	s := NewScheduler()
	heads := []string{"a", "b", "c"}
	want := []string{"a", "a", "b", "a", "c", "a", "a"}
	for i, w := range want {
		p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b", "c"), heads)
		if p.Attempts[0] != w {
			t.Fatalf("round %d: head = %q, want %q (full plan %v)", i, p.Attempts[0], w, p.Attempts)
		}
	}
}

// TestPlanWeightedRRMissingEgress: Resolve VALIDATES route egress references
// — a route naming an egress that does not exist is a load error, not a
// runtime surprise.
func TestPlanWeightedRRMissingEgress(t *testing.T) {
	_, err := (&config.File{
		Egress: []config.Egress{egress("a", 3)},
		Routes: []config.Route{route("r", config.StrategyWeightedRR, "a", "ghost")},
	}).Resolve()
	if err == nil {
		t.Fatal("route referencing an unknown egress must fail validation")
	}
}

// TestPlanEgressesResolvedFromSnapshot: Plan pins every attempt to a PRIVATE
// COPY of the snapshot's egress, index-aligned with Attempts and value-
// equivalent to rt.Egress — the executor dials transports from this pinned
// list, never a runtime re-lookup (P1: one request = one snapshot). The copy
// must not alias the snapshot: mutating the plan's egress leaves rt intact.
func TestPlanEgressesResolvedFromSnapshot(t *testing.T) {
	rt := weightedRuntime(egress("a", 5), egress("b", 1), egress("c", 1))
	s := NewScheduler()
	p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b", "c"), []string{"a", "b", "c"})
	if len(p.Egresses) != len(p.Attempts) {
		t.Fatalf("Egresses len = %d, Attempts len = %d — must be index-aligned", len(p.Egresses), len(p.Attempts))
	}
	for i, id := range p.Attempts {
		want, ok := rt.Egress(id)
		if !ok {
			t.Fatalf("Attempts[%d] = %q not in snapshot", i, id)
		}
		if e := p.Egresses[i]; !reflect.DeepEqual(e, want) {
			t.Fatalf("Egresses[%d] = %+v, want a copy of the snapshot egress %+v — pinning failed",
				i, e, want)
		}
	}

	// The pin is a copy, not an alias: writing through it cannot touch the
	// snapshot (and rt.Egress re-derives the untouched value).
	p.Egresses[0].MaxConcurrency = 999999
	after, _ := rt.Egress(p.Attempts[0])
	if after.MaxConcurrency == 999999 {
		t.Fatal("mutating a plan's pinned egress must never reach the snapshot")
	}
}

// TestPlanEgressesNilSlotForAbsentHead: a head that left the snapshot before
// Plan yields a nil slot (not a panic, not a lookup against a newer runtime);
// the executor treats nil Egresses as skip, not failure.
func TestPlanEgressesNilSlotForAbsentHead(t *testing.T) {
	rt := wrrRuntime([]string{"a", "b"}, egress("a", 1), egress("b", 1))
	s := NewScheduler()
	p := s.Plan(rt, route("r", config.StrategyRoundRobin, "a", "b"), []string{"a", "ghost"})
	for i, id := range p.Attempts {
		if id == "ghost" && p.Egresses[i] != nil {
			t.Fatalf("ghost head must resolve to a nil Egresses slot, got %v", p.Egresses[i])
		}
		if id == "a" && p.Egresses[i] == nil {
			t.Fatalf("known head %q must resolve", id)
		}
	}
}

// TestPlanWeightedRREqualWeights: two equal candidates alternate strictly —
// smooth WRR degenerates to round-robin only when weights are equal.
func TestPlanWeightedRREqualWeights(t *testing.T) {
	rt := wrrRuntime([]string{"a", "b"}, egress("a", 2), egress("b", 2)) // a,b head set only
	s := NewScheduler()
	want := []string{"a", "b", "a", "b", "a", "b"}
	for i, w := range want {
		p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b"), []string{"a", "b"})
		if p.Attempts[0] != w {
			t.Fatalf("round %d: head = %q, want %q", i, p.Attempts[0], w)
		}
	}
}

// TestPlanWeightedRRHeavyVsLight: weights 10:1 produce the exact nginx
// smooth-WRR sequence a a a a a b a a a a a a over 12 rounds — the heavy
// candidate dominates without starving the light one (a smooth 11-round
// cycle, so round 12 restarts on a).
func TestPlanWeightedRRHeavyVsLight(t *testing.T) {
	rt := wrrRuntime([]string{"a", "b"}, egress("a", 10), egress("b", 1))
	s := NewScheduler()
	want := []string{"a", "a", "a", "a", "a", "b", "a", "a", "a", "a", "a", "a"}
	for i, w := range want {
		p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b"), []string{"a", "b"})
		if p.Attempts[0] != w {
			t.Fatalf("round %d: head = %q, want %q", i, p.Attempts[0], w)
		}
	}
}

// TestPlanWeightedRRHeavyPlusTwo: 10:1:1 distributes 10/1/1 over one WRR
// cycle (12 rounds) — the heavy follows the canonical smooth sequence with
// the two light candidates each scheduled exactly once.
func TestPlanWeightedRRHeavyPlusTwo(t *testing.T) {
	rt := weightedRuntime(egress("a", 10), egress("b", 1), egress("c", 1))
	s := NewScheduler()
	want := []string{"a", "a", "a", "a", "b", "a", "a", "a", "c", "a", "a", "a"}
	for i, w := range want {
		p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b", "c"), []string{"a", "b", "c"})
		if p.Attempts[0] != w {
			t.Fatalf("round %d: head = %q, want %q", i, p.Attempts[0], w)
		}
	}
}

// TestPlanWeightedRRZeroWeightNeverScheduled: an explicit weight 0 is
// "configured but never a route head" — a competing positive-weight egress
// wins every round.
func TestPlanWeightedRRZeroWeightNeverScheduled(t *testing.T) {
	file := config.File{
		Egress: []config.Egress{egress("a", 0), egress("b", 1)},
		Routes: []config.Route{route("r", config.StrategyWeightedRR, "a", "b")},
	}
	rt, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	s := NewScheduler()
	for i := 0; i < 6; i++ {
		p := s.Plan(rt, route("r", config.StrategyWeightedRR, "a", "b"), []string{"a", "b"})
		if p.Attempts[0] != "b" {
			t.Fatalf("round %d: head = %q, want b (weight-0 a must never be scheduled)", i, p.Attempts[0])
		}
	}
}

// TestPlanWeightedRRZeroWeightNeverHeadsAcrossEligibilityFlap: the head-set
// flap repro of the weight-0 invariant violation. Eligibility (health
// cooldown, slots, model gate, streaming) changes the head set WITHOUT a
// reload, so the fingerprint — and with it the current_weight reset — never
// fires, and smooth-WRR's sum-zero property only holds over one FIXED head
// set. Route [b w0, a w1, c w1] with b first (the strict `>` tie-break lets
// an earlier candidate win a tie, which is exactly how b won): phase 1 has b
// cooling, so heads [a c] for one round drive current_weight to {a:-1,c:1};
// phase 2 has b back and c gone, heads [b a]. With b inside the selection
// pool b polls 0 > -1 and a then ties it at 0 (not >) — weight-0 b heads
// while a is eligible. The exclusion of weight-0 members from selection
// keeps a the head regardless of any stale totals.
func TestPlanWeightedRRZeroWeightNeverHeadsAcrossEligibilityFlap(t *testing.T) {
	rt := wrrRuntime([]string{"b", "a", "c"}, egress("b", 0), egress("a", 1), egress("c", 1))
	r := route("r", config.StrategyWeightedRR, "b", "a", "c")
	s := NewScheduler()
	if h := s.Plan(rt, r, []string{"a", "c"}).Attempts[0]; h != "a" {
		t.Fatalf("phase 1 head = %q, want a (cw now {a:-1, c:1})", h)
	}
	p := s.Plan(rt, r, []string{"b", "a"})
	if p.Attempts[0] != "a" {
		t.Fatalf("phase 2 head = %q, want a (weight-0 b must never head while a is eligible)", p.Attempts[0])
	}
	if p.Attempts[1] != "b" {
		t.Fatalf("phase 2 attempts = %v, want [a b] — b keeps its fallback-tail place", p.Attempts)
	}
	// And once c returns the rotation continues without b ever heading.
	for i, want := range []string{"c", "a", "c", "a"} {
		if h := s.Plan(rt, r, []string{"b", "a", "c"}).Attempts[0]; h != want {
			t.Fatalf("round %d after full head set returns: head = %q, want %q", i, h, want)
		}
	}
}

// TestPlanWeightedRRZeroWeightInFallbackTail: a weight-0 egress stays in the
// plan's fallback tail at its head-order position and never takes index 0 —
// and the tail order is deterministic: head, then the remaining heads in
// route order. Weights a=0, b=2, c=1 give the smooth cycle b c b.
func TestPlanWeightedRRZeroWeightInFallbackTail(t *testing.T) {
	rt := weightedRuntime(egress("a", 0), egress("b", 2), egress("c", 1))
	r := route("r", config.StrategyWeightedRR, "a", "b", "c")
	s := NewScheduler()
	want := [][3]string{
		{"b", "a", "c"},
		{"c", "a", "b"},
		{"b", "a", "c"},
	}
	for i, w := range want {
		p := s.Plan(rt, r, []string{"a", "b", "c"})
		for j := range w {
			if p.Attempts[j] != w[j] {
				t.Fatalf("round %d: attempts = %v, want %v", i, p.Attempts, w)
			}
		}
	}
}

// TestPlanWeightedRRAllZeroHeadsYieldEmptyPlan: config validation rejects a
// WRR route with no positive-weight member ("needs at least one egress with
// weight > 0"); the runtime analogue — every positive-weight member
// temporarily ineligible, only weight-0 survivors left in the head set —
// answers the same way. No schedulable head exists, so Plan yields an empty
// plan (the executor's synthetic 502 envelope, the same verdict an empty
// head set produces) and, like the empty-head-set path, touches no rotation
// state.
func TestPlanWeightedRRAllZeroHeadsYieldEmptyPlan(t *testing.T) {
	rt := wrrRuntime([]string{"a", "b"}, egress("a", 1), egress("b", 0))
	r := route("r", config.StrategyWeightedRR, "a", "b")
	s := NewScheduler()
	p := s.Plan(rt, r, []string{"b"}) // a cooling: only weight-0 b eligible
	if len(p.Attempts) != 0 || len(p.Egresses) != 0 {
		t.Fatalf("all-weight-0 head set must yield an empty plan, got %+v", p)
	}
	if len(s.state) != 0 {
		t.Fatalf("an unservable plan must touch no rotation state, store holds %d routes", len(s.state))
	}
}
