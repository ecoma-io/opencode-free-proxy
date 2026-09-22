package upstream

import (
	"context"
	"io"
	"net/http"
	"sync"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/routing"
)

// Executor drives the cross-egress attempt loop — the fallback layer. Each
// attempt is one per-egress Client.DoClassified; the retry matrix
// (RetryRules) stays untouched INSIDE the attempt. After a terminal result
// the executor classifies → maybe observes health → maybe moves to the next
// attempt. Fallback never happens once a live response exists: the calling
// relay owns commitment from the moment Execute returns a non-nil response.
type Executor struct {
	clientFor func(*config.Egress) (*Client, bool)
	health    *health.Registry // nil = no registry wired (health off)
	slots     *Limiter         // nil = unlimited
}

// NewExecutor wires the per-egress client resolver. clientFor resolves a
// SNAPSHOT-resolved egress (routing.Plan pins it at request start) to its
// transport; the executor itself never touches the config store, so a hot
// reload cannot move a client out from under an in-flight request. health
// and slots are optional — nil disables them.
func NewExecutor(clientFor func(*config.Egress) (*Client, bool), h *health.Registry, slots *Limiter) *Executor {
	return &Executor{clientFor: clientFor, health: h, slots: slots}
}

// AttemptPolicy carries the per-request (per-snapshot) bounds the executor
// honors. MaxConcurrency maps egress id → cap (0 = unlimited). HealthPolicy
// is the request's OWN snapshot policy — observations are judged under it,
// never under a global knob (issue #6).
type AttemptPolicy struct {
	FallbackEnabled bool
	MaxAttempts     int // distinct egresses including the first; < 1 = 1
	MaxConcurrency  map[string]int
	HealthPolicy    health.Policy
}

// Budget is the attempt cap the executor enforces for this policy. It is
// also what the evidence renderer reports as max_attempts — a method so the
// reported number and the enforced one can never drift apart.
func (p AttemptPolicy) Budget() int {
	if !p.FallbackEnabled || p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// Execute walks the plan honoring the policy. Returns the live response
// (commitment — the caller must not fall back after this), the winning
// egress id, the attempts consumed, the last failure class, and the
// client-facing UpstreamError. On success uerr is nil and class is
// ClassSuccess. When no egress could serve, resp is nil and uerr carries the
// LAST REAL verdict of the final dialed egress — a deliberate divergence
// from base.js:183, which synthesizes an "All N URLs failed" error in that
// spot (see the loop tail below); only a request that never dialed (every
// plan entry skipped, or an empty head set) gets the synthetic 502 envelope.
//
// Invariants (issue #3):
//   - 429 falls back to the next egress but NEVER marks health.
//   - No fallback after any downstream write — guaranteed structurally:
//     downstream writes happen only after this returns a response.
//   - A slot that fills between plan and dial is SKIPPED, never a failure.
//   - A slot acquired for an attempt is released exactly once: immediately
//     when the attempt fails, or when the success response body is closed
//     (the cap counts in-flight requests/streams, so a winning egress holds
//     its slot until the body is consumed — both relay paths close it).
//
// slotReleaseBody closes the underlying body and frees the slot exactly once
// (sync.Once — the streaming relay closes the body twice). ORDER MATTERS:
// the body is closed BEFORE the slot frees. A replacement request must never
// be admitted past the concurrency cap while this attempt's connection is
// still open — releasing first would let Acquire succeed before Close runs,
// briefly overshooting the cap by one live connection per handoff. Freeing
// after the close keeps the cap a true upper bound on held connections
// (pinned by TestSlotReleaseBodyClosesBodyBeforeFree).
type slotReleaseBody struct {
	io.ReadCloser
	once sync.Once
	free func()
}

func (b *slotReleaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.free)
	return err
}
func (x *Executor) Execute(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte, plan routing.RoutePlan, policy AttemptPolicy) (*http.Response, string, int, Class, *UpstreamError) {
	return x.ExecuteObserved(ctx, url, buildHeaders, bodyJSON, plan, policy, nil)
}

// ExecuteObserved is Execute plus the evidence recorder — behaviorally
// identical, rec strictly observational (nil = off). The executor is the only
// place attempt-level facts exist, so it owns three row kinds:
//
//   - skip rows: a plan entry passed over without dialing (scheduling fact,
//     never a failure);
//   - dial rows: captured inside DoClassifiedObserved, then ANNOTATED here
//     with egress id/type, attempt number, and in-flight occupancy — the
//     client layer dials a transport and cannot know them;
//   - decisions: the LAST row of a failed attempt receives the health and
//     retry/fallback decisions the executor just made, AFTER they were made
//     (a row never claims a decision before it exists).
//
// Ordering is therefore exactly: response → capture → classify → health →
// retry/fallback decision → (later, at the router) emit.
func (x *Executor) ExecuteObserved(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte, plan routing.RoutePlan, policy AttemptPolicy, rec *Recorder) (*http.Response, string, int, Class, *UpstreamError) {
	budget := policy.Budget()
	attempts := 0
	lastID := ""
	lastClass := ClassConnectionError
	var lastErr *UpstreamError
	// terminalRow is the recorder index of the row the most recent failed
	// attempt's decisions were stamped on (-1 = none). The post-loop
	// correction below targets exactly that row — computed when the decision
	// was stamped, never "the last row now", because skip rows can be
	// appended after a failed attempt and must never receive a retry
	// disposition.
	terminalRow := -1
	// The budget is checked exactly once per dial, at the continue guard
	// below — attempts only grows after a dial and every post-dial path
	// either returns or re-checks the budget before continuing, so no
	// top-of-loop guard is needed (and one would be dead code today).
	for i, id := range plan.Attempts {
		// A skipped head (below) is a scheduling race, never a failure — with
		// fallback ENABLED the executor moves to the next plan entry. With
		// fallback disabled no other egress may be dialed, so a skip at the
		// head ends the plan: the loop falls through to the 502 envelope.
		// The egress was resolved from the request's snapshot by the
		// scheduler; a nil slot only happens if a head left the snapshot
		// between Plan and here — skip, not failure.
		if i >= len(plan.Egresses) || plan.Egresses[i] == nil {
			rec.Append(Row{Phase: PhaseSkip, Egress: id, Reason: SkipUnknownEgress})
			if !policy.FallbackEnabled {
				break
			}
			continue
		}
		eg := plan.Egresses[i]
		client, ok := x.clientFor(eg)
		if !ok {
			rec.Append(Row{Phase: PhaseSkip, Egress: id, EgressType: proxyTypeName(eg), Reason: SkipTransportBuild})
			if !policy.FallbackEnabled {
				break // transport build failed and no fallback: nothing else to try
			}
			continue // transport build failed: skip, not failure
		}
		if x.slots != nil && !x.slots.Acquire(id, policy.MaxConcurrency[id]) {
			rec.Append(Row{Phase: PhaseSkip, Egress: id, EgressType: proxyTypeName(eg), Reason: SkipSlotFull})
			if !policy.FallbackEnabled {
				break // head's slot full and no fallback: nothing else to try
			}
			continue // slots filled between plan and dial: skip ≠ failure
		}
		attempts++
		lastID = id
		// In-flight occupancy INCLUDING this dial, captured at acquire time —
		// the concurrency snapshot the failure correlation wants. Only capped
		// egresses are tracked (the limiter does not count uncapped ones).
		inFlight := 0
		if x.slots != nil && policy.MaxConcurrency[id] > 0 {
			inFlight = x.slots.InFlight(id)
		}
		startRow := rec.Len()
		resp, uerr, class := client.DoClassifiedObserved(ctx, url, buildHeaders, bodyJSON, rec)
		lastClass = class
		// Identity annotation for every row this attempt captured.
		rec.Annotate(startRow, func(row *Row) {
			row.Egress = id
			row.EgressType = proxyTypeName(eg)
			row.Attempt = attempts
			row.InFlight = inFlight
		})
		if uerr != nil {
			lastErr = uerr
			if x.slots != nil {
				x.slots.Release(id)
			}
			if class == ClassContextCanceled {
				annotateDecision(rec, startRow, HealthNeutral, RetryStop)
				return nil, id, attempts, class, uerr
			}
			health := HealthNeutral
			if class.MarksHealth() && x.health != nil {
				// State identity is id+transport (eg.HealthKey) so a policy-
				// only reload keeps history while a transport swap starts
				// clean; the POLICY is this request's snapshot.
				x.health.Observe(eg.HealthKey(), false, policy.HealthPolicy)
				health = HealthMarked
			}
			if !class.FallbackAllowed() || attempts >= budget {
				annotateDecision(rec, startRow, health, RetryStop)
				return nil, id, attempts, class, uerr
			}
			// Only the fallback branch's row can need the post-loop
			// correction — the stop branches return before the loop can
			// exhaust the plan.
			terminalRow = annotateDecision(rec, startRow, health, RetryFallback)
			continue
		}
		if x.health != nil {
			x.health.Observe(eg.HealthKey(), true, policy.HealthPolicy)
		}
		if x.slots != nil {
			resp.Body = &slotReleaseBody{ReadCloser: resp.Body, free: func() { x.slots.Release(id) }}
		}
		return resp, id, attempts, class, uerr
	}
	// Terminal verdict: the LAST REAL one when anything was dialed — a plan
	// that ran out before the budget must not rewrite a 429/503/504 into a
	// synthetic 502 (it would invert the client's backoff semantics and make
	// the visible status depend on plan length vs budget).
	//
	// Deliberate divergence from base.js, stated as such per porting
	// discipline #4: when the URL loop completes without returning,
	// base.js:183 executes `throw lastError || new Error(\`All
	// ${fallbackCount} URLs failed with status ${lastStatus}\`)`. That DOES
	// synthesize a failure whenever every URL was passed over via
	// shouldRetry (base.js:83-85, the 429-exhausted case): lastError is
	// still null after status-only skips, so the client sees the synthesized
	// "All N URLs failed with status 429" instead of its own 429 envelope.
	// This executor keeps the last real verdict instead — the synthesized
	// string is lossy (it discards the upstream's own message body), not
	// load-bearing, and the status the client backoffs on stays the
	// upstream's. Everything else is parity: an exhausted retry matrix
	// returns the real response (base.js:163) and the last URL's real error
	// escapes (base.js:179).
	//
	// The synthetic envelope is only for a request that never dialed — every
	// plan entry skipped (slot-full / unknown egress / transport build) or an
	// empty plan; attempts==0 marks that case in the log.
	if lastErr != nil {
		// Plan exhaustion: a row annotated `fallback` at the dial was the
		// decision THEN; with no plan entry left to fall back to, the request
		// in fact STOPPED at that row, and the terminal row must say what the
		// request did — a log ending in "fallback" with no following attempt
		// reads as evidence loss. Correct exactly the row the decision
		// stamped (terminalRow, captured when it was stamped): a skip row
		// appended after the failed attempt never receives a retry
		// disposition, and an earlier attempt's row keeps its fallback label
		// (a later attempt did follow it). -1 means the cap dropped the
		// attempt's rows — nothing to correct.
		if terminalRow >= 0 {
			rec.AnnotateAt(terminalRow, func(row *Row) { row.RetryDecision = RetryStop })
		}
		return nil, lastID, attempts, lastClass, lastErr
	}
	return nil, lastID, attempts, lastClass, &UpstreamError{
		Status:  http.StatusBadGateway,
		Message: "none of the eligible egresses could serve the request",
	}
}

// proxyTypeName reduces an egress to its safe transport kind for evidence
// rows: the configured proxy type, or "direct" for the host's own network.
// The proxy URL itself is NEVER used — it can carry credentials.
func proxyTypeName(eg *config.Egress) string {
	if eg == nil || eg.Proxy == nil {
		return "direct"
	}
	return string(eg.Proxy.Type)
}

// annotateDecision stamps the executor's just-made health and retry/fallback
// decisions onto the LAST row of the failed attempt that starts at startRow,
// returning the row index it stamped (-1 when the startRow guard fired: the
// cap dropped every row of the attempt, so there is nothing to stamp).
// Retried (non-terminal) rows keep their retry_same_egress decision — only
// the terminal dial's row carries the attempt disposition.
func annotateDecision(rec *Recorder, startRow int, health, retry string) int {
	last := rec.Len() - 1
	if last < startRow {
		return -1
	}
	rec.Annotate(last, func(row *Row) {
		row.HealthDecision = health
		row.RetryDecision = retry
	})
	return last
}
