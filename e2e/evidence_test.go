//go:build e2e

// Black-box evidence-layer e2e: the spawned server's JSON log stream must let
// an operator reconstruct request → attempts → egress → verdict → decision
// from the `upstream_error` events alone (issue #45). Every assertion goes
// through the wire and the process's stdout — nothing internal is imported.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// jsonEvents parses every JSON log line the spawned server emitted.
func (sp *proxySpawn) jsonEvents() []map[string]any {
	var events []map[string]any
	for _, ln := range strings.Split(sp.out.String(), "\n") {
		if !strings.Contains(ln, `"msg"`) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(ln), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// withMsg returns every parsed event whose msg starts with prefix.
func withMsg(events []map[string]any, prefix string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if msg, _ := ev["msg"].(string); strings.HasPrefix(msg, prefix) {
			out = append(out, ev)
		}
	}
	return out
}

// evStr fetches a string field or fails.
func evStr(t *testing.T, ev map[string]any, key string) string {
	t.Helper()
	v, ok := ev[key].(string)
	if !ok {
		t.Fatalf("event field %q missing/not a string: %v", key, ev)
	}
	return v
}

// TestEvidence429TerminalReconstructsFromLogs: egress a answers 429 with
// rate-limit headers — the provider's verdict, which ENDS the logical call.
// The log must carry exactly one warn upstream_error event for attempt 1 on a
// (rate-limit observation, health-neutral, STOP) and the completion line must
// relay the 429 with no egress move; the healthy sibling b is never dialed.
func TestEvidence429TerminalReconstructsFromLogs(t *testing.T) {
	dir := cfgDir(t)

	var aCalls atomic.Int64
	proxyA := fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		aCalls.Add(1)
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-RateLimit-Limit", "100")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	})
	defer proxyA.Close()
	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
	defer proxyB.Close()

	writeCFG(t, dir, upstreamBase("http://upstream.invalid/zen")+fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.URL, proxyB.URL))
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want the provider's 429 relayed (body %s)", resp.StatusCode, b)
	}
	_ = resp.Body.Close()
	if got := bCalls.Load(); got != 0 {
		t.Fatalf("b dialed %d times, want 0 (a provider verdict is terminal)", got)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=a", "attempts=1", "class=upstream_429", "status=429", "fallback=false")

	events := sp.jsonEvents()
	errs := withMsg(events, "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want exactly 1 (the 429 attempt; b's success is silent):\n%s", len(errs), sp.out.String())
	}
	ev := errs[0]

	// Correlation: the evidence event and the completion line name the same
	// request, and the attempt id is derived from it.
	done := withMsg(events, "request completed")
	if len(done) != 1 {
		t.Fatalf("completion lines = %d, want 1", len(done))
	}
	reqID := evStr(t, done[0], "request_id")
	if evStr(t, ev, "request_id") != reqID {
		t.Fatalf("evidence request_id = %q, completion = %q", evStr(t, ev, "request_id"), reqID)
	}
	if got := evStr(t, ev, "attempt_id"); got != reqID+"/1" {
		t.Fatalf("attempt_id = %q, want %s/1", got, reqID)
	}

	for key, want := range map[string]any{
		"level":                 "warn",
		"phase":                 "response",
		"egress":                "a",
		"egress_type":           "http",
		"status":                float64(429),
		"class":                 "upstream_429",
		"error_type":            "rate_limit_error",
		"message":               "rate limited",
		"retry_after":           "17",
		"x-ratelimit-limit":     "100",
		"x-ratelimit-remaining": "0",
		"health_decision":       "neutral",
		"fallback_decision":     "stop",
		"route":                 "r",
		"upstream_host":         "upstream.invalid",
	} {
		if ev[key] != want {
			t.Fatalf("field %q = %v, want %v\nfull event: %v", key, ev[key], want, ev)
		}
	}
	for _, key := range []string{"session_fp", "request_body_sha256", "error_fingerprint", "body_peek"} {
		if v := evStr(t, ev, key); v == "" {
			t.Fatalf("field %q empty", key)
		}
	}
	// No credential or session material may ride the event: the raw session
	// header value and the request body never appear anywhere in the log.
	if strings.Contains(sp.out.String(), "Bearer ") {
		t.Fatal("authorization material leaked into the log")
	}
}

// TestEvidenceStreamDeathIsPhaseNotVerdict: an egress that delivers a 200
// start and dies mid-stream is recorded as a response_started STREAM failure
// (the delivered status stays 200) — never reclassified into an HTTP verdict,
// with no attempt id and no health decision.
func TestEvidenceStreamDeathIsPhaseNotVerdict(t *testing.T) {
	dir := cfgDir(t)

	proxyA := fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\""+fakeChatID+"\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // die mid-stream after the 200 start
	})
	defer proxyA.Close()

	writeCFG(t, dir, upstreamBase("http://upstream.invalid/zen")+fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxyA.URL))
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, nil)
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil && !strings.Contains(string(b), "Hel") {
		t.Fatalf("the already-delivered delta must reach the client: %q", b)
	}

	events := sp.jsonEvents()
	errs := withMsg(events, "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1 (the stream death):\n%s", len(errs), sp.out.String())
	}
	ev := errs[0]
	for key, want := range map[string]any{
		"level":  "warn",
		"phase":  "stream",
		"class":  "response_started",
		"status": float64(200),
		"reason": "read_error",
		"egress": "a",
	} {
		if ev[key] != want {
			t.Fatalf("field %q = %v, want %v\nfull event: %v", key, ev[key], want, ev)
		}
	}
	for _, absent := range []string{"attempt_id", "attempt", "health_decision", "fallback_decision", "error_fingerprint"} {
		if _, has := ev[absent]; has {
			t.Fatalf("stream rows must not carry %q: %v", absent, ev[absent])
		}
	}
}
