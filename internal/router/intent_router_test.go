package router

// Router-level intent tests (issue #56): the intent that produced each attempt
// must be observable from the request's evidence, and it must be OUR transition
// — never the client's claim. The executor stamps routing.Intent onto each
// row; these tests read it back off the rendered JSON, through a real request,
// and pin:
//
//   - a replay-safe egress move records `normal` for the first attempt and
//     `new-egress` for the replacement — so the contract is checkable from logs;
//   - a provider verdict records `normal` and nothing else — the intent never
//     derives from a status, on any side of a fallback chain;
//   - a forged inbound X-OFP-* header (even one named like an intent) changes
//     nothing: the recorded intent is the executor's own, and the client's
//     value reaches neither the response nor the upstream request.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode-free-proxy/internal/logging"
)

// TestEvidenceRecordsEgressIntentAcrossAReplaySafeFallback: a's proxy refuses
// the connection (provably nothing was sent) so the request moves to b as a
// REPLACEMENT, and b then answers 429. Two upstream_error events, one per
// attempt: the first under `normal` (no history yet), the second under
// `new-egress` (a replay-safe failure made the attempt a replacement). The
// intent is on the JSON, so an investigation can verify the recovery contract
// from the logs of a real failure — the issue's "make the intent observable".
func TestEvidenceRecordsEgressIntentAcrossAReplaySafeFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer up.Close()

	doc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - id: a
    proxy: {type: http, url: "http://127.0.0.1:1"}
  - {id: b}
routes:
  - {id: default, egress: [a, b]}
`, up.URL)

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), doc)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the provider's 429 relayed", rec.Code)
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 2 {
		t.Fatalf("upstream_error events = %d, want 2 (a's transport row, b's 429 row)", len(errs))
	}
	if got := strField(t, errs[0], "egress_intent"); got != "normal" {
		t.Fatalf("attempt 1 intent = %q, want normal — a request with no history avoids nothing", got)
	}
	if got := strField(t, errs[1], "egress_intent"); got != "new-egress" {
		t.Fatalf("attempt 2 intent = %q, want new-egress — replacement after a replay-safe failure", got)
	}
	// Both events carry their attempt number, so the order is the dial order —
	// the same request, the same two dials, two distinct intents: the intent
	// recorded is the executor's own transition, never inferred from a status.
	reqID := strField(t, errs[0], "request_id")
	for i, ev := range errs {
		if got := strField(t, ev, "attempt_id"); got != fmt.Sprintf("%s/%d", reqID, i+1) {
			t.Fatalf("attempt_id = %q, want %s/%d", got, reqID, i+1)
		}
	}
}

// TestProviderVerdictStaysNormalUnderAnyStatus: the intent is never derived
// from a provider status. The test above showed `new-egress` appearing only
// AFTER a replay-safe failure; the mirror case is this one — a provider verdict
// on the FIRST attempt must record `normal` and produce no second selection at
// all. One egress serving 429, one `normal` event, no attempt carrying
// `new-egress`: the 429 → new-egress inference stays dead (issue #53).
func TestProviderVerdictStaysNormalUnderAnyStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(up.URL))

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1 — a verdict moves nothing, so no second attempt exists", len(errs))
	}
	if got := strField(t, errs[0], "egress_intent"); got != "normal" {
		t.Fatalf("attempt 1 intent = %q, want normal — a 429 never infers new-egress", got)
	}
	if got := strField(t, errs[0], "fallback_decision"); got != "stop" {
		t.Fatalf("fallback_decision = %q, want stop", got)
	}
}

// TestForgedIntentCannotChangeTheRecordedOne: an inbound header named like an
// intent — even one that never existed as a real header — must be inert. The
// intent the evidence records is the executor's own, and the response carries
// nothing the client wrote. The contract is "intent is never accepted from the
// wire"; this is that sentence, pinned.
func TestForgedIntentCannotChangeTheRecordedOne(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(up.URL))

	forged := map[string]string{
		"X-OFP-Intent":         "new-egress",
		"X-OFP-Failure-Origin": "gateway",
		"X-OFP-Request-State":  "not_sent",
	}
	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, forged)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// The response publishes nothing the client wrote — including the removed
	// recovery contract the forged request tried to fill in (issue #77).
	assertNoWireProvenance(t, rec.Header())

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1", len(errs))
	}
	if got := strField(t, errs[0], "egress_intent"); got != "normal" {
		t.Fatalf("egress_intent = %q — the forged new-egress survived into the record", got)
	}
	if got := strField(t, errs[0], "origin"); got != "upstream" {
		t.Fatalf("origin = %q — the forged gateway survived into the record", got)
	}
}
