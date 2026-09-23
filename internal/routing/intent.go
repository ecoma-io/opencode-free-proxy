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

// Selection is a handle a Selector PRODUCED, carried back to it by the caller
// to express `new-egress`. The caller never builds one, reads one, or spells
// one: it receives a Selection from Next, echoes it on the next call, and
// resolves it through Resolve to learn what to dial.
//
// That indirection is the whole point (issue #64). The identity of an egress —
// its configured id, its proxy URL, its address, any pool slot — is the
// SELECTOR's business, and the INTENT is the caller's. Passing a raw id across
// the seam inverted that: it made the interface's currency an identity this
// process happens to have today, which is exactly what the cross-service
// contract forbids ("Egress identity is RPGW's private business; across a
// service boundary the only currency is the logical intent"). A pool-backed
// selector can now answer `new-egress` from a handle that names nothing OFP
// could interpret.
//
// The zero Selection means "no previous egress" — the state of a request's
// first attempt. It is never produced by Next.
//
// A future pool-backed selector would live in this package too (it is OFP's own
// client of RPGW), so the unexported fields are not a barrier to a second
// implementation carrying its own payload. What the exported seam guarantees is
// narrower and is the point: no CALLER of Selector can name an egress — not to
// request one, not to exclude one, not to read one out of a selection.
type Selection struct {
	// id and egress are the LOCAL resolution of the handle: the id to record
	// on evidence and the configured egress to dial. They are unexported so a
	// caller cannot mint a Selection or compare two of them — the handle's
	// meaning belongs to the selector that produced it.
	id     string
	egress *config.Egress
	valid  bool
}

// Resolve reports the local dial target a Selection names — the id to record
// and the egress whose transport serves it. ok=false only for the zero
// Selection (a request with no previous attempt).
//
// A pool-backed selector would answer with the transport through which THIS
// process reaches the pool, not with any pool internal: what the caller needs
// is something to dial, and the pool's own naming never has to surface.
func (s Selection) Resolve() (id string, egress *config.Egress, ok bool) {
	return s.id, s.egress, s.valid
}

// Selector resolves an intent to the egress the next attempt should use.
//
// Next receives the intent and, for `new-egress`, the Selection of the egress
// that just failed — the one the caller must not deliberately reuse. The zero
// Selection (a request's first attempt) means there is nothing to avoid. A
// pool-backed implementation may honour `new-egress` by returning its only
// member when no alternative exists — the intent explicitly permits that —
// because the pooled identity is the pool's business.
//
// ok=false means the selection has nothing left to offer and the request ends.
type Selector interface {
	Next(intent Intent, prev Selection) (sel Selection, ok bool)
}

// PlanSelector is the in-process Selector: the request's own pinned RoutePlan,
// consumed in order.
//
// It needs no pool policy because its pool IS the plan — a list this request
// owns, ordered by the scheduler for the initial attempt and by the route's
// declaration order after it. Config validation rejects a repeated egress
// reference in a route, so the plan's entries are distinct and consecutive
// selections are already different egresses; the `prev` check below states
// that as an enforced postcondition rather than an accident of validation. It
// is also what a pool-backed selector would have to honour, which is why it
// lives on the interface's contract and not only on this implementation.
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

// Next returns the next plan entry this request has not dialed, passing over
// the egress `prev` names. ok=false when the plan is exhausted.
//
// `prev` is decoded with Resolve and matched against the plan's ids: the plan
// IS a list of ids, so that is the only decoding this implementation has. The
// guarantee the caller relies on is the narrow one — a handle this selector
// produced is honoured. A zero Selection (first attempt) names nothing and
// excludes nothing. A handle from another selector is a caller bug with a
// bounded consequence: at worst one entry whose id coincides is passed over.
// It can never cause a different egress to be dialed, which is the direction
// that would matter.
func (s *PlanSelector) Next(_ Intent, prev Selection) (Selection, bool) {
	exclude, _, hasPrev := prev.Resolve()
	for ; s.next < len(s.plan.Attempts); s.next++ {
		id := s.plan.Attempts[s.next]
		if hasPrev && id == exclude {
			continue
		}
		var egress *config.Egress
		if s.next < len(s.plan.Egresses) {
			egress = s.plan.Egresses[s.next]
		}
		s.next++
		return Selection{id: id, egress: egress, valid: true}, true
	}
	return Selection{}, false
}
