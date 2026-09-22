package upstream

// Evidence tests for ExecuteObserved: the executor annotates each attempt's
// rows with egress identity, the attempt number, in-flight occupancy, and the
// health + retry/fallback decisions it just made — so a request's rows alone
// reconstruct request → attempts → egress → verdict → decision. The fallback
// BEHAVIOR is pinned by fallback_test.go; these pin the forensic record.

import (
	"context"
	"net/http"
	"testing"
)

func TestEvidenceAttemptCorrelation429Chain(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 429, "b": 429, "c": 200})
	defer f.Close()

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
		if row.Egress != eg || row.Attempt != i+1 || row.Phase != PhaseResponse {
			t.Fatalf("row %d = %+v, want egress %s attempt %d", i, row, eg, i+1)
		}
		if row.Status != 429 || row.Class != ClassUpstream429.String() {
			t.Fatalf("row %d = %+v", i, row)
		}
		if row.HealthDecision != HealthNeutral {
			t.Fatalf("row %d: 429 must be health-neutral, got %q", i, row.HealthDecision)
		}
		if row.RetryDecision != RetryFallback {
			t.Fatalf("row %d: decision = %q, want fallback", i, row.RetryDecision)
		}
		if row.EgressType != "direct" {
			t.Fatalf("row %d: egress type = %q, want direct", i, row.EgressType)
		}
	}
	if !f.health.Healthy(healthKey("a"), testHealthPolicy) || !f.health.Healthy(healthKey("b"), testHealthPolicy) {
		t.Fatal("row health decisions must mirror the registry: 429 poisons nothing")
	}
}

func TestEvidence5xxMarksHealth(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 500, "b": 200})
	defer f.Close()

	rec := NewRecorder()
	resp, _, _, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 2), rec)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()

	rows := rec.Rows()
	if len(rows) != 1 || rows[0].HealthDecision != HealthMarked || rows[0].RetryDecision != RetryFallback {
		t.Fatalf("rows = %+v, want 500 → marked + fallback", rows)
	}
	if f.health.Healthy(healthKey("a"), testHealthPolicy) {
		t.Fatal("row must mirror the registry: 500 marks health")
	}
}

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
	if rows[0].RetryDecision != RetryStop || rows[0].HealthDecision != HealthNeutral {
		t.Fatalf("terminal row = %+v, want stop + neutral (plan exhausted, nothing marked)", rows[0])
	}
}

// TestEvidenceRetriedMatrixSharesAttempt: a 502 that burns the whole matrix
// inside ONE egress attempt yields 4 rows sharing attempt=1 with distinct dial
// numbers — the attempt_id derivation input (reqID/1, reqID/1.2, …). Only the
// terminal dial carries the executor's fallback decision; matrix-retried dials
// keep retry_same_egress.
func TestEvidenceRetriedMatrixSharesAttempt(t *testing.T) {
	f := newExecutorFixture(t, map[string]int{"a": 502, "b": 200})
	defer f.Close()

	rec := NewRecorder()
	resp, _, _, _, uerr := f.exec.ExecuteObserved(
		context.Background(), f.servers["a"].URL,
		func() map[string]string { return map[string]string{} },
		[]byte(`{}`), f.plan("r", "a", "b"), policy(true, 2), rec)
	if uerr != nil || resp == nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()

	rows := rec.Rows()
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 3 matrix retries + terminal on attempt 1", len(rows))
	}
	for i, row := range rows {
		if row.Egress != "a" || row.Attempt != 1 || row.Dial != i+1 {
			t.Fatalf("row %d = %+v, want a attempt 1 dial %d", i, row, i+1)
		}
	}
	for i := 0; i < 3; i++ {
		if rows[i].RetryDecision != RetryRetrySameEgress || !rows[i].Retried {
			t.Fatalf("matrix row %d = %+v, want retry_same_egress", i, rows[i])
		}
	}
	if rows[3].RetryDecision != RetryFallback || rows[3].Retried {
		t.Fatalf("terminal row = %+v, want fallback after matrix exhausted", rows[3])
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
