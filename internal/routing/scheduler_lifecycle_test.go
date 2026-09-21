package routing

// Scheduler state lifecycle tests. Rotation state carries a fingerprint of
// the config it was built from (routing.go schedulerFingerprint). The
// invariants pinned here, in order: a policy-only reload preserves rotation
// EXACTLY (continuity — the reason the fingerprint exists); a change to
// strategy, ordered membership or WRR weights deterministically resets that
// one route; a removed route's state is pruned so the same id can never
// resurrect an unrelated config's rotation; and the whole seam is race-clean
// under concurrent Plan + prune + runtime swap, every plan valid for the
// snapshot it was planned from.

import (
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/config"
)

// lifecycleRuntime resolves a snapshot through the real loader and stamps
// the store-style generation on it (the hot-reload version the
// once-per-generation maintenance is keyed by).
func lifecycleRuntime(t *testing.T, gen uint64, f config.File) *config.Runtime {
	t.Helper()
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	rt.Generation = gen
	return rt
}

// headOf plans once and returns only the chosen head.
func headOf(s *Scheduler, rt *config.Runtime, r config.Route, heads []string) string {
	return s.Plan(rt, r, heads).Attempts[0]
}

// routeKeepSet derives the route-id keep set Server.onGeneration passes to
// PruneRoutes (the scheduler half of the router's generationKeepSets).
func routeKeepSet(rt *config.Runtime) map[string]struct{} {
	keep := make(map[string]struct{}, len(rt.Routes()))
	for _, r := range rt.Routes() {
		keep[r.ID] = struct{}{}
	}
	return keep
}

// sameMembers reports whether xs is a permutation of ys.
func sameMembers(xs, ys []string) bool {
	if len(xs) != len(ys) {
		return false
	}
	a := slices.Clone(xs)
	b := slices.Clone(ys)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// TestSchedulerStateSurvivesEquivalentReload: a reload that leaves the
// fingerprint intact (same route id, strategy, ordered members, weights —
// only the fallback budget and the generation differ) must preserve rotation
// EXACTLY. The smooth-WRR cycle for weights 5,1,1 over [a b c] is
// a a b a c a a (7 rounds); plans 4-7 issued against the reloaded snapshot
// must CONTINUE it (a c a a), not restart it (a a b …). The round_robin
// cursor is pinned on the same swap.
func TestSchedulerStateSurvivesEquivalentReload(t *testing.T) {
	egs := []config.Egress{egress("a", 5), egress("b", 1), egress("c", 1), {ID: "x"}, {ID: "y"}}
	wrr := route("r", config.StrategyWeightedRR, "a", "b", "c")
	rr := route("rr", config.StrategyRoundRobin, "x", "y")
	rt1 := lifecycleRuntime(t, 1, config.File{Egress: egs, Routes: []config.Route{wrr, rr}})
	// The "reload": identical scheduler inputs, a policy-only fallback edit.
	rt2 := lifecycleRuntime(t, 2, config.File{
		Egress:   egs,
		Routes:   []config.Route{wrr, rr},
		Fallback: config.FallbackPolicy{MaxAttempts: 7},
	})

	s := NewScheduler()
	heads := []string{"a", "b", "c"}
	var got []string
	for i := 0; i < 3; i++ { // warm-up on generation 1
		got = append(got, headOf(s, rt1, wrr, heads))
	}
	for i := 0; i < 4; i++ { // the same rotation, continued on generation 2
		got = append(got, headOf(s, rt2, wrr, heads))
	}
	if seq := strings.Join(got, ""); seq != "aabacaa" {
		t.Fatalf("rotation sequence across the reload = %q, want %q (a reset would restart the 7-round cycle at plan 4: aaba…)", seq, "aabacaa")
	}

	rrHeads := []string{"x", "y"}
	if h := headOf(s, rt1, rr, rrHeads); h != "x" { // cursor 0 → 1 on gen 1
		t.Fatalf("round_robin warm-up head = %q, want x", h)
	}
	if h := headOf(s, rt2, rr, rrHeads); h != "y" {
		t.Fatalf("round_robin head after the equivalent reload = %q, want y (the cursor must survive a policy-only reload)", h)
	}
}

// TestSchedulerStateResetsWhenStrategyChanges: strategy is a fingerprint
// input — it selects which half of routeState is live — so switching a
// route's strategy discards its rotation state, and reverting to the earlier
// strategy must NOT resurrect it. The current_weight left by the two warm-up
// plans is {a:-2, b:2}; from that stale state b would out-poll a (3 vs 1), so
// a head of "b" after the revert is a resurrected state, not a fresh one.
func TestSchedulerStateResetsWhenStrategyChanges(t *testing.T) {
	egs := []config.Egress{egress("a", 3), egress("b", 1)}
	wrrRoute := route("r", config.StrategyWeightedRR, "a", "b")
	rrRoute := route("r", config.StrategyRoundRobin, "a", "b")
	rt1 := lifecycleRuntime(t, 1, config.File{Egress: egs, Routes: []config.Route{wrrRoute}})
	rt2 := lifecycleRuntime(t, 2, config.File{Egress: egs, Routes: []config.Route{rrRoute}})
	rt3 := lifecycleRuntime(t, 3, config.File{Egress: egs, Routes: []config.Route{wrrRoute}})

	s := NewScheduler()
	heads := []string{"a", "b"}
	if seq := headOf(s, rt1, wrrRoute, heads) + headOf(s, rt1, wrrRoute, heads); seq != "aa" {
		t.Fatalf("warm-up sequence = %q, want aa", seq)
	}
	if h := headOf(s, rt2, rrRoute, heads); h != "a" {
		t.Fatalf("first round_robin head after the strategy change = %q, want a (deterministic from a zero cursor)", h)
	}
	if h := headOf(s, rt3, wrrRoute, heads); h != "a" {
		t.Fatalf("head after reverting to the original strategy = %q, want a (a resurrected current_weight {a:-2,b:2} would head b)", h)
	}
}

// TestSchedulerStateResetsWhenEgressMembershipChanges: the fingerprint
// carries the route's CONFIG-LEVEL egress list IN ORDER — adding a member,
// removing one, or reordering the same members is a state identity change.
// Each part is built so a preserved state picks a DIFFERENT head than a
// deterministic reset.
func TestSchedulerStateResetsWhenEgressMembershipChanges(t *testing.T) {
	// (a) ADD a member (wrr): the preserved {a:-1,b:1} plus new c would head
	// b; a reset starts from uniform totals and heads a.
	egsABC := []config.Egress{egress("a", 1), egress("b", 1), egress("c", 1)}
	addFrom := route("r", config.StrategyWeightedRR, "a", "b")
	addTo := route("r", config.StrategyWeightedRR, "a", "b", "c")
	rt1 := lifecycleRuntime(t, 1, config.File{Egress: egsABC, Routes: []config.Route{addFrom}})
	rt2 := lifecycleRuntime(t, 2, config.File{Egress: egsABC, Routes: []config.Route{addTo}})
	s := NewScheduler()
	if h := headOf(s, rt1, addFrom, []string{"a", "b"}); h != "a" {
		t.Fatalf("warm-up head = %q, want a", h)
	}
	if h := headOf(s, rt2, addTo, []string{"a", "b", "c"}); h != "a" {
		t.Fatalf("head after a member was added = %q, want a (a preserved current_weight {a:-1,b:1} would head b)", h)
	}

	// (b) REMOVE a member (rr): after three plans over [a b c] the cursor is
	// 3; shrinking the route to [a b] a PRESERVED cursor would land on b
	// (3 % 2 = 1) — a reset lands on a. The cursor is positional over the
	// route's members, so membership size is part of its meaning.
	egsAB := []config.Egress{{ID: "a"}, {ID: "b"}}
	removeFrom := route("q", config.StrategyRoundRobin, "a", "b", "c")
	removeTo := route("q", config.StrategyRoundRobin, "a", "b")
	rt3 := lifecycleRuntime(t, 3, config.File{Egress: append(egsAB, config.Egress{ID: "c"}), Routes: []config.Route{removeFrom}})
	rt4 := lifecycleRuntime(t, 4, config.File{Egress: egsAB, Routes: []config.Route{removeTo}})
	s2 := NewScheduler()
	for i := 0; i < 3; i++ {
		if h := headOf(s2, rt3, removeFrom, []string{"a", "b", "c"}); h != []string{"a", "b", "c"}[i] {
			t.Fatalf("warm-up round %d head = %q, want %q", i, h, []string{"a", "b", "c"}[i])
		}
	}
	if h := headOf(s2, rt4, removeTo, []string{"a", "b"}); h != "a" {
		t.Fatalf("head after a member was removed = %q, want a (a preserved cursor 3 would land on b)", h)
	}

	// (c) REORDER the SAME members (wrr): order is rotation semantics — the
	// cursor indexes it and smooth-WRR breaks ties by it. The preserved
	// {a:-2,b:2} under the swapped list would head b; a reset heads a.
	egsW := []config.Egress{egress("a", 3), egress("b", 1)}
	orderFrom := route("r", config.StrategyWeightedRR, "a", "b")
	orderTo := route("r", config.StrategyWeightedRR, "b", "a")
	rt5 := lifecycleRuntime(t, 5, config.File{Egress: egsW, Routes: []config.Route{orderFrom}})
	rt6 := lifecycleRuntime(t, 6, config.File{Egress: egsW, Routes: []config.Route{orderTo}})
	s3 := NewScheduler()
	if seq := headOf(s3, rt5, orderFrom, []string{"a", "b"}) + headOf(s3, rt5, orderFrom, []string{"a", "b"}); seq != "aa" {
		t.Fatalf("warm-up sequence = %q, want aa", seq)
	}
	if h := headOf(s3, rt6, orderTo, []string{"b", "a"}); h != "a" {
		t.Fatalf("head after reordering the same members = %q, want a (a preserved current_weight {a:-2,b:2} under the swapped order would head b)", h)
	}
}

// TestSchedulerStateResetsWhenWeightChanges: the adversarial-review case.
// Route [z a] with weights z=5,a=1 drives smooth-WRR to current_weight
// {z:2,a:-2} after four requests; reloading z to weight 0 must RESET — a
// preserved state combined with the new weights heads z, a weight-0 egress
// ahead of an eligible positive-weight sibling, contradicting the documented
// weight-0-never-heads invariant. round_robin is pinned on the other side:
// weights are NOT rr fingerprint inputs, so a weight-only edit must NOT
// reset its cursor.
func TestSchedulerStateResetsWhenWeightChanges(t *testing.T) {
	// (a) wrr: the cross-generation carryover violation.
	wrrRoute := route("r", config.StrategyWeightedRR, "z", "a")
	rt1 := lifecycleRuntime(t, 1, config.File{
		Egress: []config.Egress{egress("z", 5), egress("a", 1)},
		Routes: []config.Route{wrrRoute},
	})
	s := NewScheduler()
	heads := []string{"z", "a"}
	seq := ""
	for i := 0; i < 4; i++ { // drives current_weight to {z:2, a:-2}
		seq += headOf(s, rt1, wrrRoute, heads)
	}
	if seq != "zzza" {
		t.Fatalf("warm-up sequence = %q, want zzza", seq)
	}
	rt2 := lifecycleRuntime(t, 2, config.File{
		Egress: []config.Egress{egress("z", 0), egress("a", 1)},
		Routes: []config.Route{wrrRoute},
	})
	for i := 0; i < 6; i++ {
		if h := headOf(s, rt2, wrrRoute, heads); h != "a" {
			t.Fatalf("round %d after the weight change: head = %q, want a (weight-0 z must never head while a is eligible; the preserved {z:2,a:-2} would head z)", i, h)
		}
	}

	// (b) round_robin: a weight-only edit is fingerprint-equivalent.
	rrRoute := route("q", config.StrategyRoundRobin, "x", "y")
	rt3 := lifecycleRuntime(t, 3, config.File{
		Egress: []config.Egress{egress("x", 5), egress("y", 1)},
		Routes: []config.Route{rrRoute},
	})
	if h := headOf(s, rt3, rrRoute, []string{"x", "y"}); h != "x" { // cursor 0 → 1
		t.Fatalf("round_robin warm-up head = %q, want x", h)
	}
	rt4 := lifecycleRuntime(t, 4, config.File{
		Egress: []config.Egress{egress("x", 1), egress("y", 9)},
		Routes: []config.Route{rrRoute},
	})
	if h := headOf(s, rt4, rrRoute, []string{"x", "y"}); h != "y" {
		t.Fatalf("round_robin head after a weight-only edit = %q, want y (weights are not rr fingerprint inputs; a reset would restart the cursor on x)", h)
	}
}

// TestSchedulerStaleSnapshotDoesNotClobberNewerState: a request holds its
// arrival snapshot for its whole lifetime, so it can reach Plan AFTER newer
// traffic already stored a newer generation's rotation state. The stale
// write used to key the OLD fingerprint under the route id, so the next
// current-generation plan saw a mismatch and reset — a spurious rotation
// restart mid-cycle. Shapes: gen 1 [a w1, c w3], gen 2 flips to [a w3,
// c w1] (fingerprint change → reset, then the 3:1 cycle a a c a), and the
// stale gen-1 plan lands after gen 2's second plan. Without the guard that
// write stores fingerprint(gen 1) and the next gen-2 plan restarts the
// cycle on a; with it the stale request answers from fresh throwaway state
// (its own deterministic first pick) and gen 2 continues on c.
func TestSchedulerStaleSnapshotDoesNotClobberNewerState(t *testing.T) {
	r := route("r", config.StrategyWeightedRR, "a", "c")
	rt1 := lifecycleRuntime(t, 1, config.File{
		Egress: []config.Egress{egress("a", 1), egress("c", 3)},
		Routes: []config.Route{r},
	})
	rt2 := lifecycleRuntime(t, 2, config.File{
		Egress: []config.Egress{egress("a", 3), egress("c", 1)},
		Routes: []config.Route{r},
	})
	s := NewScheduler()
	heads := []string{"a", "c"}
	if h := headOf(s, rt1, r, heads); h != "c" { // gen-1 state {a:1, c:-1}
		t.Fatalf("gen-1 warm-up head = %q, want c", h)
	}
	if h := headOf(s, rt2, r, heads); h != "a" { // reset, cycle position 1
		t.Fatalf("gen-2 plan 1 head = %q, want a", h)
	}
	if h := headOf(s, rt2, r, heads); h != "a" { // cycle position 2 (cw {a:-2, c:2})
		t.Fatalf("gen-2 plan 2 head = %q, want a", h)
	}
	// The stale gen-1 request, planning after gen-2 traffic.
	stale := s.Plan(rt1, r, heads)
	if stale.Attempts[0] != "c" {
		t.Fatalf("stale plan head = %q, want c (deterministic first pick for its own shape)", stale.Attempts[0])
	}
	if st, ok := s.state[r.ID]; !ok || st.fingerprint != schedulerFingerprint(r, rt2) {
		t.Fatalf("stale plan clobbered the newer generation's rotation state: %+v", s.state[r.ID])
	}
	if h := headOf(s, rt2, r, heads); h != "c" { // cycle position 3, not a restart
		t.Fatalf("gen-2 head after the stale plan = %q, want c (a stored old fingerprint would reset and restart on a)", h)
	}

	// The guard refuses REPLACEMENT, never participation: gen 3 is a
	// policy-only reload of gen 2's shape (same fingerprint), and a gen-2
	// request planning after gen-3 traffic still advances the shared
	// rotation — same shape, same meaning, any generation.
	rt3 := lifecycleRuntime(t, 3, config.File{
		Egress:   []config.Egress{egress("a", 3), egress("c", 1)},
		Routes:   []config.Route{r},
		Fallback: config.FallbackPolicy{MaxAttempts: 5},
	})
	if h := headOf(s, rt3, r, heads); h != "a" { // cycle position 4
		t.Fatalf("gen-3 head = %q, want a", h)
	}
	if h := headOf(s, rt2, r, heads); h != "a" { // cycle position 1 of the next round
		t.Fatalf("gen-2 head after gen-3 traffic = %q, want a (a same-shape request participates, it is not fenced off)", h)
	}
}

// TestSchedulerStateDoesNotLeakAcrossRemovedRoute: once-per-generation
// pruning (wired into Server.onGeneration) drops a removed route's state, so
// re-adding a route with the SAME id later starts fresh — a resurrected
// cursor would hand the new config the old one's rotation position. The kept
// route's rotation continues untouched across the same pass, and the pass is
// idempotent.
func TestSchedulerStateDoesNotLeakAcrossRemovedRoute(t *testing.T) {
	r := route("r", config.StrategyRoundRobin, "a", "b")
	keep := route("keep", config.StrategyRoundRobin, "c", "d")
	all := []config.Egress{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	rt1 := lifecycleRuntime(t, 1, config.File{Egress: all, Routes: []config.Route{r, keep}})
	s := NewScheduler()
	rHeads := []string{"a", "b"}
	kHeads := []string{"c", "d"}
	if h := headOf(s, rt1, r, rHeads); h != "a" { // cursor 1
		t.Fatalf("route r warm-up head = %q, want a", h)
	}
	if h := headOf(s, rt1, keep, kHeads); h != "c" { // cursor 1
		t.Fatalf("route keep warm-up head = %q, want c", h)
	}

	// Generation 2 removes route r entirely; the generation pass prunes it.
	rt2 := lifecycleRuntime(t, 2, config.File{
		Egress: all,
		Routes: []config.Route{keep},
	})
	if n := s.PruneRoutes(routeKeepSet(rt2)); n != 1 {
		t.Fatalf("prune dropped %d route states, want 1 (route r)", n)
	}
	if n := s.PruneRoutes(routeKeepSet(rt2)); n != 0 {
		t.Fatalf("prune must be idempotent within a generation, dropped %d on the second pass", n)
	}

	// Generation 3 re-adds r with the same id and members: FRESH, not
	// resurrected (a surviving cursor 1 would head b).
	rt3 := lifecycleRuntime(t, 3, config.File{Egress: all, Routes: []config.Route{r, keep}})
	if h := headOf(s, rt3, r, rHeads); h != "a" {
		t.Fatalf("re-added route head = %q, want a (a cursor resurrected from the removed generation would head b)", h)
	}
	if h := headOf(s, rt3, keep, kHeads); h != "d" {
		t.Fatalf("kept route head after the prune = %q, want d (a kept route's rotation must survive pruning)", h)
	}
}

// TestSchedulerReloadRace: Plan under concurrent hot reloads must be
// race-clean, and every plan must be valid for the snapshot it was planned
// FROM — attempts are a permutation of that snapshot's eligible heads, every
// attempt pins pointer-identically to that snapshot's egress, and a
// weight-0 member never heads a plan built from the weight-0 snapshot (the
// invariant the fingerprint reset exists to keep; it holds from empty state
// under ANY interleaving). A concurrent pruner walks the same lock, and
// sometimes passes an empty keep set (a mid-flight wipe is safe: the next
// Plan re-derives state from its own snapshot).
func TestSchedulerReloadRace(t *testing.T) {
	r := route("r", config.StrategyWeightedRR, "z", "a")
	heavy := lifecycleRuntime(t, 1, config.File{
		Egress: []config.Egress{egress("z", 5), egress("a", 1)},
		Routes: []config.Route{r},
	})
	zeroed := lifecycleRuntime(t, 2, config.File{
		Egress: []config.Egress{egress("z", 0), egress("a", 1)},
		Routes: []config.Route{r},
	})
	// Equivalent to heavy (same fingerprint), newer generation: the
	// policy-only reload leg of the swap cycle.
	heavyPrime := lifecycleRuntime(t, 3, config.File{
		Egress:   []config.Egress{egress("z", 5), egress("a", 1)},
		Routes:   []config.Route{r},
		Fallback: config.FallbackPolicy{MaxAttempts: 5},
	})

	var cur atomic.Pointer[config.Runtime]
	cur.Store(heavy)
	s := NewScheduler()
	heads := []string{"z", "a"}

	const workers = 8
	const plansPerWorker = 300
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < plansPerWorker; j++ {
				rt := cur.Load()
				p := s.Plan(rt, r, heads)
				if p.RouteID != "r" || p.Strategy != config.StrategyWeightedRR {
					t.Errorf("plan identity lost: %+v", p)
					continue
				}
				if !sameMembers(p.Attempts, heads) {
					t.Errorf("attempts %v are not a permutation of the heads %v", p.Attempts, heads)
					continue
				}
				for k, id := range p.Attempts {
					want, ok := rt.Egress(id)
					if !ok || !reflect.DeepEqual(p.Egresses[k], want) {
						t.Errorf("attempt %q did not pin to the snapshot it was planned from", id)
					}
				}
				if e, ok := rt.Egress("z"); ok && e.EffectiveWeight() == 0 && p.Attempts[0] == "z" {
					t.Errorf("weight-0 z headed a plan from the weight-0 snapshot: %v (weight-0-never-heads violated)", p.Attempts)
				}
			}
		}()
	}

	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 3 {
			case 0:
				cur.Store(zeroed)
			case 1:
				cur.Store(heavyPrime)
			default:
				cur.Store(heavy)
			}
		}
	}()

	var pruner sync.WaitGroup
	pruner.Add(1)
	go func() {
		defer pruner.Done()
		with := routeKeepSet(zeroed) // {"r"} — what onGeneration passes
		for i := 0; i < 500; i++ {
			if i%7 == 0 {
				s.PruneRoutes(nil) // a route vanished: drop everything
				continue
			}
			s.PruneRoutes(with)
		}
	}()

	wg.Wait()
	close(stop)
	swapper.Wait()
	pruner.Wait()
}
