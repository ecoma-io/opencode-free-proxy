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
	health    *health.Registry // nil = health disabled
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
//   - A slot acquired for an attempt is released exactly once: immediately
//     when the attempt fails, or when the success response body is closed
//     (the cap counts in-flight requests/streams, so a winning egress holds
//     its slot until the body is consumed — both relay paths close it).
//
// slotReleaseBody closes the underlying body and frees the slot exactly
// once (sync.Once — the streaming relay closes the body twice).
type slotReleaseBody struct {
	io.ReadCloser
	once sync.Once
	free func()
}

func (b *slotReleaseBody) Close() error {
	b.once.Do(b.free)
	return b.ReadCloser.Close()
}
func (x *Executor) Execute(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte, plan routing.RoutePlan, policy AttemptPolicy) (*http.Response, string, int, Class, *UpstreamError) {
	budget := policy.MaxAttempts
	if !policy.FallbackEnabled || budget < 1 {
		budget = 1
	}
	attempts := 0
	lastID := ""
	lastClass := ClassConnectionError
	for i, id := range plan.Attempts {
		if attempts >= budget {
			break
		}
		// A skipped head (below) is a scheduling race, never a failure — with
		// fallback ENABLED the executor moves to the next plan entry. With
		// fallback disabled no other egress may be dialed, so a skip at the
		// head ends the plan: the loop falls through to the 502 envelope.
		// The egress was resolved from the request's snapshot by the
		// scheduler; a nil slot only happens if a head left the snapshot
		// between Plan and here — skip, not failure.
		if i >= len(plan.Egresses) || plan.Egresses[i] == nil {
			if !policy.FallbackEnabled {
				break
			}
			continue
		}
		eg := plan.Egresses[i]
		client, ok := x.clientFor(eg)
		if !ok {
			if !policy.FallbackEnabled {
				break // transport build failed and no fallback: nothing else to try
			}
			continue // transport build failed: skip, not failure
		}
		if x.slots != nil && !x.slots.Acquire(id, policy.MaxConcurrency[id]) {
			if !policy.FallbackEnabled {
				break // head's slot full and no fallback: nothing else to try
			}
			continue // slots filled between plan and dial: skip ≠ failure
		}
		attempts++
		lastID = id
		resp, uerr, class := client.DoClassified(ctx, url, buildHeaders, bodyJSON)
		lastClass = class
		if uerr != nil {
			if x.slots != nil {
				x.slots.Release(id)
			}
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
		if x.slots != nil {
			resp.Body = &slotReleaseBody{ReadCloser: resp.Body, free: func() { x.slots.Release(id) }}
		}
		return resp, id, attempts, class, uerr
	}
	return nil, lastID, attempts, lastClass, &UpstreamError{
		Status:  http.StatusBadGateway,
		Message: "none of the eligible egresses could serve the request",
	}
}
