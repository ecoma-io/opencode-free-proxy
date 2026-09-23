package upstream

// Evidence tests for ExecuteObserved: the executor annotates each attempt's
// rows with egress identity, the attempt number, in-flight occupancy, and the
// health + fallback decisions it just made — so a request's rows alone
// reconstruct request → attempts → egress → verdict → decision. The fallback
// BEHAVIOR is pinned by fallback_test.go; these pin the forensic record.

import (
	"context"
	"net/http"
	"testing"
)

// TestEvidenceAttemptCorrelationAcrossEgresses: two replay-safe failures in a
// row, then a serving third egress. Each failed attempt leaves exactly one
// row, numbered in dial order, and each carries the decisions the executor
// made at that moment: the request moved on for both (the plan had a third
// entry left), and health stayed NEUTRAL for both — the fixture's failure is a
// direct-path target_connect, which is the destination's fault, not the
// egress's (issue #62). A row reading `fallback` + `neutral` is exactly what a
// provider outage is supposed to look like.
func TestEvidenceAttemptCorrelationAcrossEgresses(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200, "c": 200})
	defer f.Close()
	f.deadEgress(t, "a")
	f.deadEgress(t, "b")

	rec := NewRecorder()
	resp, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b", "c"), policy(true, 3), rec)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	if id != "c" || attempts != 3 {
		t.Fatalf("id=%q attempts=%d", id, attempts)
	}

	rows := rec.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per failed attempt (c's success gets none)", len(rows))
	}
	for i, row := range rows {
		eg := string(rune('a' + i))
		if row.Egress != eg || row.Attempt != i+1 || row.Phase != PhaseTransport {
			t.Fatalf("row %d = %+v, want egress %s attempt %d", i, row, eg, i+1)
		}
		if row.Class != ClassConnectionError.String() {
			t.Fatalf("row %d = %+v", i, row)
		}
		// The move and the mark are separate decisions: a destination-side
		// failure authorises the first and not the second.
		if row.HealthDecision != HealthNeutral {
			t.Fatalf("row %d: a target_connect failure must not mark health, got %q", i, row.HealthDecision)
		}
		if row.FallbackDecision != FallbackYes {
			t.Fatalf("row %d: decision = %q, want fallback", i, row.FallbackDecision)
		}
		if row.Origin != OriginTransport.String() || row.RequestState != RequestStateNotSent.String() {
			t.Fatalf("row %d: provenance lost: %+v", i, row)
		}
		if row.EgressType != "direct" {
			t.Fatalf("row %d: egress type = %q, want direct", i, row.EgressType)
		}
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) || !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("row health decisions must mirror the registry: neither failed dial marks (threshold 1 would quarantine on one mark)")
	}
}

// TestEvidenceProviderVerdictMarksNoHealth: a 500 is the provider answering.
// One row, no health mark, and the decision on the row is STOP — the sibling
// egress is never dialed.
func TestEvidenceProviderVerdictMarksNoHealth(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

	rec := NewRecorder()
	_, _, attempts, _, _ := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 2), rec)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}

	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.HealthDecision != HealthNeutral || row.FallbackDecision != FallbackStop {
		t.Fatalf("row = %+v, want neutral + stop for a provider verdict", row)
	}
	if row.Origin != OriginUpstream.String() || row.RequestState != RequestStateResponseStarted.String() {
		t.Fatalf("row provenance = %+v, want upstream/response_started", row)
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("row must mirror the registry: a 500 marks nothing")
	}
	if got := f.rec.count("b"); got != 0 {
		t.Fatalf("b POSTs = %d, want 0", got)
	}
}

// TestEvidenceTerminalVerdictStops: the same shape for 429, which the removed
// matrix treated specially — the row now says what actually happened: one
// attempt, no health mark, stop.
func TestEvidenceTerminalVerdictStops(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429})
	defer f.Close()

	rec := NewRecorder()
	_, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a"), policy(true, 3), rec)
	if uerr == nil || uerr.Status != http.StatusTooManyRequests {
		t.Fatalf("uerr = %v", uerr)
	}
	if id != "a" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d", id, attempts)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].FallbackDecision != FallbackStop || rows[0].HealthDecision != HealthNeutral {
		t.Fatalf("terminal row = %+v, want stop + neutral", rows[0])
	}
}

// TestEvidenceTerminalStopAfterTrailingSkip: plan [a, b] where a fails replay-
// safely (fallback allowed at the time) and b's slot fills between plan and
// dial. The post-loop plan-exhaustion correction must hit the row the DECISION
// stamped — a's transport row (fallback → stop) — and never the skip row
// appended after it: a skip is a scheduling fact and carries no fallback
// disposition.
func TestEvidenceTerminalStopAfterTrailingSkip(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
	defer f.Close()
	f.deadEgress(t, "a")
	f.slots.Acquire("b", 1) // b's only slot is busy: plan passes over it

	rec := NewRecorder()
	_, id, attempts, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"),
		AttemptPolicy{
			FallbackEnabled: true,
			MaxAttempts:     3,
			MaxConcurrency:  map[string]int{"a": 1, "b": 1},
			HealthPolicy:    testHealthPolicy,
		}, rec)
	if uerr == nil {
		t.Fatal("want a terminal failure: both plan entries are unusable")
	}
	if id != "a" || attempts != 1 {
		t.Fatalf("id=%q attempts=%d, want a/1", id, attempts)
	}

	rows := rec.Rows()
	if len(rows) != 2 || rows[0].Phase != PhaseTransport || rows[1].Phase != PhaseSkip {
		t.Fatalf("rows = %+v, want a transport row then a b skip row", rows)
	}
	if rows[0].Egress != "a" || rows[0].FallbackDecision != FallbackStop {
		t.Fatalf("terminal dial row = %+v, want stop (plan exhausted behind it)", rows[0])
	}
	if rows[1].Egress != "b" || rows[1].Reason != SkipSlotFull || rows[1].FallbackDecision != "" {
		t.Fatalf("skip row = %+v, want slot_full with NO fallback disposition", rows[1])
	}
}

func TestEvidenceSkipRows(t *testing.T) {
	t.Run("slot full", func(t *testing.T) {
		f := newExecutorFixture(t, map[string]int{"a": 200, "b": 200})
		defer f.Close()
		f.slots.Acquire("a", 1)

		p := AttemptPolicy{
			FallbackEnabled: true,
			MaxAttempts:     3,
			MaxConcurrency:  map[string]int{"a": 1, "b": 1},
			HealthPolicy:    testHealthPolicy,
		}
		rec := NewRecorder()
		resp, id, _, _, uerr := f.exec.ExecuteObserved(
			context.Background(), f.servers["a"].URL,
			func() map[string]string { return map[string]string{} },
			[]byte(`{}`), f.plan("r", "a", "b"), p, rec)
		if uerr != nil || resp == nil || id != "b" {
			t.Fatalf("uerr=%v id=%q", uerr, id)
		}
		defer func() { _ = resp.Body.Close() }()

		rows := rec.Rows()
		if len(rows) != 1 || rows[0].Phase != PhaseSkip || rows[0].Reason != SkipSlotFull {
			t.Fatalf("rows = %+v, want one slot_full skip", rows)
		}
		if rows[0].Egress != "a" || rows[0].EgressType != "direct" || rows[0].Attempt != 0 {
			t.Fatalf("skip row = %+v", rows[0])
		}
	})
	t.Run("unknown egress", func(t *testing.T) {
		f := newExecutorFixture(t, map[string]int{"b": 200})
		defer f.Close()

		rec := NewRecorder()
		resp, id, _, _, uerr := f.exec.ExecuteObserved(
			context.Background(), f.servers["b"].URL,
			func() map[string]string { return map[string]string{} },
			[]byte(`{}`), f.plan("r", "ghost", "b"), policy(true, 3), rec)
		if uerr != nil || resp == nil || id != "b" {
			t.Fatalf("uerr=%v id=%q", uerr, id)
		}
		defer func() { _ = resp.Body.Close() }()

		rows := rec.Rows()
		if len(rows) != 1 || rows[0].Reason != SkipUnknownEgress || rows[0].Egress != "ghost" {
			t.Fatalf("rows = %+v, want one unknown_egress skip", rows)
		}
	})
}

// TestEvidenceInFlightTrackedOnlyForCappedEgresses: the concurrency snapshot
// is the occupancy INCLUDING this dial, and only meaningful when the egress is
// capped (the limiter does not count uncapped ones — 0 is omitted, not faked).
func TestEvidenceInFlightTrackedOnlyForCappedEgresses(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429})
	defer f.Close()

	t.Run("capped egress reports occupancy", func(t *testing.T) {
		p := AttemptPolicy{
			FallbackEnabled: true,
			MaxAttempts:     1,
			MaxConcurrency:  map[string]int{"a": 2},
			HealthPolicy:    testHealthPolicy,
		}
		rec := NewRecorder()
		_, _, _, _, _ = f.exec.ExecuteObserved(
			context.Background(), f.servers["a"].URL,
			func() map[string]string { return map[string]string{} },
			[]byte(`{}`), f.plan("r", "a"), p, rec)
		rows := rec.Rows()
		if len(rows) != 1 || rows[0].InFlight != 1 {
			t.Fatalf("rows = %+v, want in_flight=1 (this dial holding the capped slot)", rows)
		}
	})
	t.Run("uncapped egress reports none", func(t *testing.T) {
		rec := NewRecorder()
		_, _, _, _, _ = f.exec.ExecuteObserved(
			context.Background(), f.servers["a"].URL,
			func() map[string]string { return map[string]string{} },
			[]byte(`{}`), f.plan("r", "a"), policy(true, 1), rec)
		rows := rec.Rows()
		if len(rows) != 1 || rows[0].InFlight != 0 {
			t.Fatalf("rows = %+v, want in_flight omitted for an uncapped egress", rows)
		}
	})
}
