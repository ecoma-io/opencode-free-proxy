package routing

// PlanSelector unit tests (issues #56, #64): the in-process Selector over a
// pinned RoutePlan. The intent semantics are argued in intent.go; these pin the
// mechanical contract the executor relies on:
//
//   - selections consume the plan in order;
//   - the egress a `new-egress` request asks to avoid is never returned (the
//     distinct-egress postcondition `new-egress` relies on);
//   - ok=false on exhaustion — no second pass over the plan;
//   - the egress pointer is resolved from the SAME plan slot as the id, so the
//     executor never dials a different egress than the one its evidence names;
//   - the handle is only ever consumed through Resolve, and the zero handle
//     excludes nothing (a request's first attempt).

import (
	"testing"

	"opencode-free-proxy/internal/config"
)

// resolve is the test-side decoding of a handle: (id, egress, ok).
func resolve(t *testing.T, s Selection) (string, *config.Egress, bool) {
	t.Helper()
	return s.Resolve()
}

func TestPlanSelectorConsumesInOrder(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "b", "c"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	s := NewPlanSelector(plan)
	for i, want := range []string{"a", "b", "c"} {
		sel, ok := s.Next(IntentNormal, Selection{})
		if !ok {
			t.Fatalf("selection %d: plan exhausted early", i)
		}
		id, eg, resolved := resolve(t, sel)
		if !resolved || id != want || eg == nil || eg.ID != want {
			t.Fatalf("selection %d = (%q, %v, %v), want (%q, non-nil)", i, id, eg, resolved, want)
		}
	}
	if sel, ok := s.Next(IntentNormal, Selection{}); ok {
		id, _, _ := resolve(t, sel)
		t.Fatalf("a fourth selection (%q) must not exist — the plan is three entries", id)
	}
}

// TestPlanSelectorExclusionSkipsOverTheFailedEgress: the egress named by the
// handle a `new-egress` request carries back is passed over with id AND
// pointer, so the executor's replacement selection cannot land back on the
// egress that just failed. Config validation makes production plans distinct,
// so the exclusion would be a no-op there; this pinches the path with a
// REPEATED plan — the case where the postcondition ("the egress that just
// failed is never deliberately re-offered") has real work to do.
func TestPlanSelectorExclusionSkipsOverTheFailedEgress(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "a"}, {ID: "b"}},
	}
	s := NewPlanSelector(plan)
	first, ok := s.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the plan serves its first member")
	}
	if id, eg, _ := resolve(t, first); id != "a" || eg.ID != "a" {
		t.Fatalf("first = (%q, %v)", id, eg)
	}
	second, ok := s.Next(IntentNewEgress, first)
	if !ok {
		t.Fatal("the plan has b left")
	}
	if id, eg, _ := resolve(t, second); id != "b" || eg.ID != "b" {
		t.Fatalf("second = (%q, %v), want b — the duplicate a must be passed over", id, eg)
	}
}

// TestPlanSelectorExhaustionHonorsExclusion: when every remaining entry IS the
// egress the handle names, the selection is exhausted rather than re-offered.
// Same trick as above: a repeated plan, all of whose remaining entries are the
// egress that just failed — the request ends instead of deliberately re-dialing
// its failed egress.
func TestPlanSelectorExhaustionHonorsExclusion(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "a"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "a"}},
	}
	s := NewPlanSelector(plan)
	failed, ok := s.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the plan serves its first member")
	}
	if _, ok := s.Next(IntentNewEgress, failed); ok {
		t.Fatal("remaining entries excluded → exhausted, never a re-dial of a")
	}
}

// TestZeroSelectionExcludesNothing: the handle a first attempt carries is the
// zero Selection, and it must not be read as "avoid the empty id" — a plan
// entry is always selected on the first pass. The zero value is the state
// "no previous egress", and the plan proves it by being served from its head.
func TestZeroSelectionExcludesNothing(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a"},
		Egresses: []*config.Egress{{ID: "a"}},
	}
	s := NewPlanSelector(plan)
	sel, ok := s.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the zero handle must exclude nothing")
	}
	if id, _, _ := resolve(t, sel); id != "a" {
		t.Fatalf("id = %q, want a", id)
	}
	if _, _, valid := resolve(t, Selection{}); valid {
		t.Fatal("the zero Selection must resolve to nothing — it is not a handle")
	}
}

// TestForeignSelectionIsBoundedToOneSkip: a handle this selector did not
// produce is a caller bug, and its blast radius must stay bounded. Here a
// handle from a DIFFERENT selector request names an id this plan also carries:
// the entry is skipped (never dialed twice by accident), and every other entry
// is still served in order. The handle can never select anything — it only
// ever subtracts.
func TestForeignSelectionIsBoundedToOneSkip(t *testing.T) {
	other := NewPlanSelector(RoutePlan{
		Attempts: []string{"a"},
		Egresses: []*config.Egress{{ID: "a"}},
	})
	foreign, ok := other.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the foreign selector serves its plan")
	}

	s := NewPlanSelector(RoutePlan{
		Attempts: []string{"a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}},
	})
	sel, ok := s.Next(IntentNewEgress, foreign)
	if !ok {
		t.Fatal("b is still selectable")
	}
	if id, _, _ := resolve(t, sel); id != "b" {
		t.Fatalf("id = %q, want b — a foreign handle subtracts, it never selects", id)
	}
	if _, ok := s.Next(IntentNewEgress, foreign); ok {
		t.Fatal("the plan had two entries; one skip and one selection exhaust it")
	}
}

// poolFakeSelector is a SECOND implementation of the seam, written the way a
// pool-backed one would be: it answers both intents over a pool it owns, and it
// carries its payload in the handle rather than in a signature. It exists to
// prove the interface is implementable at all without an egress identity
// crossing it — the property issue #64 is about — and to pin what the executor
// asks for, on the wire between caller and selector.
type poolFakeSelector struct {
	pool  []string          // the pool's own naming, meaningless outside
	owner map[string]string // pool member → the egress this process dials
	asked []Intent          // every intent received, in order
	seen  []string          // the egress each `new-egress` handle named
}

func (s *poolFakeSelector) Next(intent Intent, prev Selection) (Selection, bool) {
	s.asked = append(s.asked, intent)
	avoid, _, _ := prev.Resolve()
	if intent == IntentNewEgress {
		s.seen = append(s.seen, avoid)
	}
	for _, member := range s.pool {
		if member == avoid {
			continue
		}
		return Selection{id: member, egress: &config.Egress{ID: s.owner[member]}, valid: true}, true
	}
	return Selection{}, false
}

// TestSelectorSeamIsImplementableWithoutIdentity: the local PlanSelector is not
// the shape of the interface, it is one implementation of it. A pool-backed
// selector answers `normal` from its pool, answers `new-egress` by not
// deliberately reusing the handle it was given, and never sees — nor returns —
// anything the caller could interpret as a pool internal. The caller's side is
// exactly what the executor does: ask, resolve, echo.
func TestSelectorSeamIsImplementableWithoutIdentity(t *testing.T) {
	sel := &poolFakeSelector{
		pool:  []string{"slot-0", "slot-1"},
		owner: map[string]string{"slot-0": "a", "slot-1": "b"},
	}
	var asSelector Selector = sel // the seam is the interface, not the struct

	first, ok := asSelector.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the pool is not empty")
	}
	id, eg, valid := resolve(t, first)
	if !valid || id != "slot-0" || eg.ID != "a" {
		t.Fatalf("first = (%q, %v, %v), want the pool's own handle resolved to a", id, eg, valid)
	}

	// The caller echoes what it was given; it never spells an egress.
	second, ok := asSelector.Next(IntentNewEgress, first)
	if !ok {
		t.Fatal("the pool has a second member")
	}
	if id, eg, _ := resolve(t, second); id != "slot-1" || eg.ID != "b" {
		t.Fatalf("second = (%q, %v), want the pool's second member", id, eg)
	}

	if len(sel.asked) != 2 || sel.asked[0] != IntentNormal || sel.asked[1] != IntentNewEgress {
		t.Fatalf("intents asked = %v, want [normal new-egress]", sel.asked)
	}
	if len(sel.seen) != 1 || sel.seen[0] != "slot-0" {
		t.Fatalf("the handle carried to new-egress named %v, want the previous selection (slot-0)", sel.seen)
	}
}

// TestPlanSelectorNullEgressSlotStillReturnsTheHead: a head absent from the
// snapshot (nil Egresses slot) is still selected and reported — the executor
// SKIPS it as unknown rather than silently moving off it, and a nil slot must
// never shift the id/pointer alignment of later selections.
func TestPlanSelectorNullEgressSlotStillReturnsTheHead(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"ghost", "b"},
		Egresses: []*config.Egress{nil, {ID: "b"}},
	}
	s := NewPlanSelector(plan)
	sel, ok := s.Next(IntentNormal, Selection{})
	if !ok {
		t.Fatal("the head is selected even with no resolvable egress")
	}
	if id, eg, valid := resolve(t, sel); id != "ghost" || eg != nil || !valid {
		t.Fatalf("first = (%q, %v, %v), want (ghost, nil, valid) so the executor can skip it AS the head", id, eg, valid)
	}
}
