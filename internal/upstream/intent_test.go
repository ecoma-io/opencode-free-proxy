package upstream

// Egress-intent tests (issue #56): the executor's selection goes through
// routing.Selector, so every attempt is made under a NAMED intent and the
// intent is recorded on the attempt's evidence row.
//
// What the intent is allowed to be is the whole point: `normal` for a request
// with no history, `new-egress` for a replacement after a failure that
// provably preceded the request, and nothing else. A provider verdict never
// produces one — under the terminal-response rule it does not move egress at
// all.

import (
	"context"
	"testing"
)

// TestAttemptIntentIsNormalThenNewEgress: the first attempt asks for any
// eligible egress; every attempt after a replay-safe failure asks for a
// replacement that is not the egress that just failed. The intent is on the
// row, so the contract is checkable from logs.
func TestAttemptIntentIsNormalThenNewEgress(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200, "c": 200})
	defer f.Close()
	f.deadEgress(t, "a")
	f.deadEgress(t, "b")

	rec := NewRecorder()
	resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["c"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b", "c"), policy(true, 3), rec)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v, want the request served by c", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	if id != "c" || attempts != 3 {
		t.Fatalf("id=%q attempts=%d, want c/3", id, attempts)
	}

	rows := rec.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per failed attempt", len(rows))
	}
	if rows[0].Intent != "normal" {
		t.Fatalf("attempt 1 intent = %q, want normal — a request with no history avoids nothing", rows[0].Intent)
	}
	if rows[1].Intent != "new-egress" {
		t.Fatalf("attempt 2 intent = %q, want new-egress — the egress that failed must not be deliberately reused", rows[1].Intent)
	}
}

// TestProviderVerdictProducesNoNewEgressIntent: a 429 is the provider's answer
// about the request. It moves nothing, so it asks for nothing: one attempt,
// intent `normal`, and no second selection exists to carry `new-egress`.
// (The 429 → new-egress inference is exactly what issue #53 forbids; this
// pins that the intent names introduced by #56 did not re-open it.)
func TestProviderVerdictProducesNoNewEgressIntent(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429, "b": 200})
	defer f.Close()

	rec := NewRecorder()
	resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["b"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3), rec)
	if resp != nil || uerr == nil || uerr.Status != 429 {
		t.Fatalf("resp=%v uerr=%v, want the terminal 429", resp, uerr)
	}
	if id != "a" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want a/1 — a verdict moves nothing", id, attempts)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Intent != "normal" {
		t.Fatalf("intent = %q, want normal (the only selection this request ever made)", rows[0].Intent)
	}
	if rows[0].FallbackDecision != FallbackStop {
		t.Fatalf("fallback_decision = %q, want stop", rows[0].FallbackDecision)
	}
}

// TestFailedEgressIsNeverRedialedThroughTheHandle (issue #64): the replacement
// request carries the HANDLE of the selection that just failed, not its id, and
// the selector is the one that decodes it. The observable consequence is the
// same as before the seam changed — the failed egress is never dialed twice —
// and this is the plan shape where it has real work to do: a repeated egress,
// which config validation rejects in production, so the exclusion is the only
// thing standing between the plan and a second dial of the same egress.
//
// The budget is 2, so the budget is NOT what stops this: if the handle were
// ignored the executor would dial a again. One dial, one row, and the row's
// decision corrected to `stop` (the request ended when the plan ran out, not
// because it found a second egress).
func TestFailedEgressIsNeverRedialedThroughTheHandle(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200})
	defer f.Close()
	f.deadEgress(t, "a")

	rec := NewRecorder()
	resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "a"), policy(true, 2), rec)
	if resp != nil || uerr == nil {
		t.Fatalf("resp=%v uerr=%v, want the transport failure surfaced", resp, uerr)
	}
	if id != "a" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want a/1 — the failed egress must not be dialed twice, the budget would allow it", id, attempts)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (one selection, one dial):\n%+v", len(rows), rows)
	}
	if rows[0].Intent != "normal" {
		t.Fatalf("intent = %q, want normal — the selection that was made, never a replacement that did not happen", rows[0].Intent)
	}
	if rows[0].FallbackDecision != FallbackStop {
		t.Fatalf("fallback = %q, want stop: the plan was exhausted, so the request stopped at this row", rows[0].FallbackDecision)
	}
}

// TestSkippedSelectionKeepsItsIntent: a pass-over is a scheduling fact, not a
// failure, so a plan skipped over before the first dial never turns the next
// selection into a replacement request. The first DIALED attempt is still
// `normal` — the request has no failed egress to avoid. (The transport-build
// skip is simulated by withdrawing a's client from the fixture map — the
// executor's clientFor then reports "no transport", exactly the production
// wiring failure this skip exists for.)
func TestSkippedSelectionKeepsItsIntent(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	delete(f.clients, "a")

	rec := NewRecorder()
	resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["b"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 3), rec)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v, want the request served by b", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	if id != "b" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want b/1", id, attempts)
	}
	rows := rec.Rows()
	if len(rows) != 1 || rows[0].Phase != PhaseSkip || rows[0].Intent != "normal" {
		t.Fatalf("rows = %+v, want one skip row under intent normal — the only selection was the first", rows)
	}
}
