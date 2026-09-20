package routing

import (
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
// stall or error — the cursor advances over whatever remains.
func TestPlanRoundRobinHeadDrop(t *testing.T) {
	rt := config.DefaultRuntime()
	s := NewScheduler()
	_ = s.Plan(rt, route("r", config.StrategyRoundRobin, "a", "b", "c"), []string{"a", "b", "c"})

	// cursor is at 1 after the first plan: over [b c] that starts at c.
	p := s.Plan(rt, route("r", config.StrategyRoundRobin, "b", "c"), []string{"b", "c"})
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

// TestPlanEgressesResolvedFromSnapshot: Plan pins every attempt to the
// snapshot's *config.Egress, index-aligned with Attempts and pointer-identical
// to rt.Egress — the executor dials transports from this pinned list, never a
// runtime re-lookup (P1: one request = one snapshot).
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
		if e := p.Egresses[i]; e != want {
			t.Fatalf("Egresses[%d] = %p (%s), want the snapshot egress %p (%s) — pinning failed",
				i, e, e.ID, want, want.ID)
		}
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
