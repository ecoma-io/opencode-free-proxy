package upstream

import (
	"context"
	"net/http"

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
	clientFor func(id string) (*Client, bool)
	health    *health.Registry // nil = health disabled
	slots     *Limiter         // nil = unlimited
}

// NewExecutor wires the per-egress client resolver. clientFor must resolve
// egress ids to their CURRENT transport (built lazily, cached per proxy
// signature); health and slots are optional — nil disables them.
func NewExecutor(clientFor func(string) (*Client, bool), h *health.Registry, slots *Limiter) *Executor {
	return &Executor{clientFor: clientFor, health: h, slots: slots}
}

// AttemptPolicy carries the per-request (per-snapshot) bounds the executor
// honors. MaxConcurrency maps egress id → cap (0 = unlimited).
type AttemptPolicy struct {
	FallbackEnabled bool
	MaxAttempts     int // distinct egresses including the first; < 1 = 1
	MaxConcurrency  map[string]int
}

// Execute walks the plan honoring the policy. Returns the live response
// (commitment — the caller must not fall back after this), the winning
// egress id, the attempts consumed, the last failure class, and the
// client-facing UpstreamError. On success uerr is nil and class is
// ClassSuccess; when no egress could serve, resp is nil and uerr carries the
// 502 envelope.
//
// Invariants (issue #3):
//   - 429 falls back to the next egress but NEVER marks health.
//   - No fallback after any downstream write — guaranteed structurally:
//     downstream writes happen only after this returns a response.
//   - A slot that fills between plan and dial is SKIPPED, never a failure.
func (x *Executor) Execute(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte, plan routing.RoutePlan, policy AttemptPolicy) (*http.Response, string, int, Class, *UpstreamError) {
	budget := policy.MaxAttempts
	if !policy.FallbackEnabled || budget < 1 {
		budget = 1
	}
	attempts := 0
	lastID := ""
	lastClass := ClassConnectionError
	for _, id := range plan.Attempts {
		if attempts >= budget {
			break
		}
		client, ok := x.clientFor(id)
		if !ok {
			continue // egress left the config mid-flight: skip, not failure
		}
		if x.slots != nil && !x.slots.Acquire(id, policy.MaxConcurrency[id]) {
			continue // slots filled between plan and dial: skip ≠ failure
		}
		attempts++
		lastID = id
		resp, uerr, class := client.DoClassified(ctx, url, buildHeaders, bodyJSON)
		lastClass = class
		if uerr != nil {
			if class == ClassContextCanceled {
				return nil, id, attempts, class, uerr
			}
			if class.MarksHealth() && x.health != nil {
				x.health.Observe(id, false)
			}
			if !class.FallbackAllowed() || attempts >= budget {
				return nil, id, attempts, class, uerr
			}
			continue
		}
		if x.health != nil {
			x.health.Observe(id, true)
		}
		return resp, id, attempts, class, uerr
	}
	return nil, lastID, attempts, lastClass, &UpstreamError{
		Status:  http.StatusBadGateway,
		Message: "none of the eligible egresses could serve the request",
	}
}
