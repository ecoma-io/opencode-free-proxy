package routing

// PlanSelector unit tests (issue #56): the in-process Selector over a pinned
// RoutePlan. The intent semantics are argued in intent.go; these pin the
// mechanical contract the executor relies on:
//
//   - selections consume the plan in order;
//   - an excluded egress is never returned (the distinct-egress postcondition
//     `new-egress` relies on);
//   - ok=false on exhaustion — no second pass over the plan;
//   - the egress pointer is resolved from the SAME plan slot as the id, so the
//     executor never dials a different egress than the one its evidence names.

import (
	"testing"

	"opencode-free-proxy/internal/config"
)

func TestPlanSelectorConsumesInOrder(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "b", "c"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	s := NewPlanSelector(plan)
	for i, want := range []string{"a", "b", "c"} {
		id, eg, ok := s.Next(IntentNormal, "")
		if !ok || id != want || eg == nil || eg.ID != want {
			t.Fatalf("selection %d = (%q, %v, %v), want (%q, non-nil)", i, id, eg, ok, want)
		}
	}
	if _, _, ok := s.Next(IntentNormal, ""); ok {
		t.Fatal("a fourth selection must not exist — the plan is three entries")
	}
}

// TestPlanSelectorExclusionSkipsOverTheFailedEgress: an excluded egress is
// passed over with id AND pointer, so the executor's `new-egress` request
// cannot land back on the egress that just failed. Config validation makes
// production plans distinct, so the exclusion would be a no-op there; this
// pinches the path with a REPEATED plan — the case where the postcondition
// ("an entry this request already dialed is never returned") has real work to
// do. The selector must pass over the duplicate rather than re-offer it.
func TestPlanSelectorExclusionSkipsOverTheFailedEgress(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "a", "b"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "a"}, {ID: "b"}},
	}
	s := NewPlanSelector(plan)
	first, eg, ok := s.Next(IntentNormal, "")
	if !ok || first != "a" || eg.ID != "a" {
		t.Fatalf("first = (%q, %v, %v)", first, eg, ok)
	}
	second, eg, ok := s.Next(IntentNewEgress, "a")
	if !ok || second != "b" || eg.ID != "b" {
		t.Fatalf("second = (%q, %v, %v), want b — the duplicate a must be passed over", second, eg, ok)
	}
}

// TestPlanSelectorExhaustionHonorsExclusion: when every remaining entry IS the
// excluded egress, the selection is exhausted rather than re-offered. Same
// trick as above: a repeated plan, all of whose remaining entries are the
// egress that just failed — the request ends instead of deliberately re-dialing
// its failed egress.
func TestPlanSelectorExhaustionHonorsExclusion(t *testing.T) {
	plan := RoutePlan{
		Attempts: []string{"a", "a"},
		Egresses: []*config.Egress{{ID: "a"}, {ID: "a"}},
	}
	s := NewPlanSelector(plan)
	if _, _, ok := s.Next(IntentNormal, ""); !ok {
		t.Fatal("the plan serves its first member")
	}
	if _, _, ok := s.Next(IntentNewEgress, "a"); ok {
		t.Fatal("remaining entries excluded → exhausted, never a re-dial of a")
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
	id, eg, ok := s.Next(IntentNormal, "")
	if !ok || id != "ghost" || eg != nil {
		t.Fatalf("first = (%q, %v, %v), want (ghost, nil) so the executor can skip it AS the head", id, eg, ok)
	}
}
