package routing

import "opencode-free-proxy/internal/config"

// intent.go — egress selection as a REQUEST, not as an index walk.
//
// The recovery contract (docs/recovery-semantics.md, "Cross-service
// contracts") splits egress selection from egress proving: RPGW owns the pool
// and selection, OFP owns proving whether a request was transmitted. OFP
// expresses what it wants with a logical intent — never with a proxy identity,
// an address, or a pool internal — and this file is the OFP side of that
// interface.
//
// The seam exists so the intent is REPRESENTABLE. Today the selector is
// implemented over the request's own pinned plan (PlanSelector below), which
// is where this proxy's egress set lives. A pool-backed implementation — the
// RPGW one — is a recorded cross-repo dependency, not invented here: RPGW does
// not expose an egress-selection API, and OFP must not ship a speculative
// client to an endpoint that does not exist. What OFP can do is state the
// contract, name the intents, and route every selection through the one
// interface such an implementation would plug into.
//
// The intents are deliberately coarse. "Which egress" is a pool question; OFP
// asks only the two questions its own knowledge supports:
//
//   - have no history (this request has not tried anything) — `normal`;
//   - do not deliberately reuse the egress that just failed — `new-egress`.
//
// OFP never derives an intent from a provider status. A 429 or a 5xx is the
// provider's answer about the request, not evidence about an egress: under the
// terminal-response rule a provider verdict does not move egress at all, so it
// cannot produce a selection request either. The ONLY thing that produces
// `new-egress` is a failure proven to precede the request
// (upstream.Failure.ReplaySafe) — the same predicate that gates the move.
type Intent string

const (
	// IntentNormal: any eligible egress; the selection policy decides. Used
	// for the request's FIRST attempt, where nothing has failed yet.
	IntentNormal Intent = "normal"
	// IntentNewEgress: do not deliberately reuse the egress that served the
	// immediately previous attempt, if an alternative is available. Used for
	// every attempt after a replay-safe failure.
	IntentNewEgress Intent = "new-egress"
)

// Selector resolves an intent to the egress the next attempt should use.
//
// exclude is the egress the caller must not deliberately reuse (the one the
// previous attempt failed on); empty means nothing to avoid. A pool-backed
// implementation may honour `new-egress` by returning its only member when no
// alternative exists — the intent explicitly permits that — because the pooled
// identity is the pool's business. THIS proxy's implementation is stricter,
// and deliberately so: an entry this request already dialed is never returned,
// whatever the intent, because `fallback.max_attempts` bounds DISTINCT
// egresses (one egress is tried at most once per request, config validation
// included). A one-member route therefore ends after one attempt rather than
// re-dialing — the exclusion is a postcondition of this selector, not a
// preference.
//
// ok=false means the selection has nothing left to offer and the request ends.
type Selector interface {
	Next(intent Intent, exclude string) (id string, egress *config.Egress, ok bool)
}

// PlanSelector is the in-process Selector: the request's own pinned RoutePlan,
// consumed in order.
//
// It needs no pool policy because its pool IS the plan — a list this request
// owns, ordered by the scheduler for the initial attempt and by the route's
// declaration order after it. Every entry is a distinct egress (config
// validation rejects a repeated reference), so consecutive selections are
// already different egresses; the exclude check below states that as an
// enforced postcondition rather than an accident of validation. It is also
// what a pool-backed selector would have to honour, which is why it lives on
// the interface's contract and not only on this implementation.
//
// The intent itself does not change what PlanSelector returns: with distinct
// entries, `normal` and `new-egress` select the same next entry. It is
// recorded (the executor stamps it on every attempt's evidence row) because it
// is what the request ASKED FOR, and a seam whose argument is unobservable is
// a seam nobody can verify.
type PlanSelector struct {
	plan RoutePlan
	next int
}

// NewPlanSelector starts a consumption of one request's plan.
func NewPlanSelector(plan RoutePlan) *PlanSelector { return &PlanSelector{plan: plan} }

// Next returns the next plan entry this request has not dialed, skipping the
// excluded egress. ok=false when the plan is exhausted.
func (s *PlanSelector) Next(_ Intent, exclude string) (string, *config.Egress, bool) {
	for ; s.next < len(s.plan.Attempts); s.next++ {
		id := s.plan.Attempts[s.next]
		if exclude != "" && id == exclude {
			continue
		}
		var egress *config.Egress
		if s.next < len(s.plan.Egresses) {
			egress = s.plan.Egresses[s.next]
		}
		s.next++
		return id, egress, true
	}
	return "", nil, false
}
