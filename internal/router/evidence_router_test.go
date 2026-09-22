package router

// Router-level tests for the evidence layer's emit boundary (evidence_log.go):
// the rendered `upstream_error` / `egress_skipped` events must carry the full
// attempt correlation (request_id → attempt_id → egress → verdict → decision),
// the level policy (failures at warn, skips at debug, success silent), and the
// streaming-phase rows (response_started, never an HTTP verdict).
//
// These tests capture the JSON stream directly — NOT through the legacy
// callback adapter (logging.FromLegacy renders only `msg` and would hide
// every field asserted here).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/logging"
	"opencode-free-proxy/internal/upstream"

	"github.com/rs/zerolog"
)

// evidenceRouter wires a Server whose logger writes JSON into buf, with the
// upstream base and egresses taken from the doc (the newRouter pattern, plus a
// real logger).
func evidenceRouter(t *testing.T, log zerolog.Logger, doc string) (*Server, *http.ServeMux) {
	t.Helper()
	p := writeCfg(t, t.TempDir(), doc)
	store, err := config.NewStore(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	s := NewServer(store, identity.NewUserAgentCache(), upstream.NewClient(), log, func(time.Duration) {})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.HandleResponses)
	return s, mux
}

// twoDirectEgressDoc routes over two direct egresses hitting the same base.
func twoDirectEgressDoc(base string) string {
	return fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: a}
  - {id: b}
routes:
  - {id: default, egress: [a, b]}
`, base)
}

// decodeEvents parses the captured JSON log stream, one object per line.
func decodeEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unparsable log line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

// eventsWith returns every event whose msg starts with prefix.
func eventsWith(events []map[string]any, prefix string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if msg, _ := ev["msg"].(string); strings.HasPrefix(msg, prefix) {
			out = append(out, ev)
		}
	}
	return out
}

func strField(t *testing.T, ev map[string]any, key string) string {
	t.Helper()
	v, ok := ev[key].(string)
	if !ok {
		t.Fatalf("event has no string field %q: %v", key, ev)
	}
	return v
}

// TestUpstreamErrorEvent429Terminal: a single-egress 429 renders exactly one
// warn upstream_error event carrying the full forensic record, and the
// completion line still follows unchanged.
func TestUpstreamErrorEvent429Terminal(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "17")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatStreamSSE))
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(up.URL))

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	events := decodeEvents(t, &buf)
	errs := eventsWith(events, "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1 (egress a 429s, b serves): %v", len(errs), eventsWith(events, "upstream_error"))
	}
	ev := errs[0]
	if ev["level"] != "warn" {
		t.Fatalf("level = %v, want warn", ev["level"])
	}

	reqID := strField(t, ev, "request_id")
	if got := strField(t, ev, "attempt_id"); got != reqID+"/1" {
		t.Fatalf("attempt_id = %q, want %s/1", got, reqID)
	}
	for key, want := range map[string]any{
		"attempt":               float64(1),
		"phase":                 "response",
		"egress":                "a",
		"egress_type":           "direct",
		"status":                float64(429),
		"class":                 "upstream_429",
		"error_type":            "rate_limit_error",
		"message":               "rate limited",
		"retry_after":           "17",
		"health_decision":       "neutral",
		"retry_decision":        "fallback",
		"route":                 "default",
		"model":                 "qwen3-coder-free",
		"endpoint":              "chat",
		"streaming":             true,
		"upstream_host":         strings.TrimPrefix(up.URL, "http://"),
		"generation":            float64(1),
		"x-ratelimit-remaining": "0",
	} {
		if ev[key] != want {
			t.Fatalf("field %q = %v, want %v", key, ev[key], want)
		}
	}
	for _, key := range []string{"session_fp", "request_body_sha256", "error_fingerprint", "body_peek"} {
		if v, ok := ev[key].(string); !ok || v == "" {
			t.Fatalf("field %q missing or empty: %v", key, ev[key])
		}
	}
	if ev["max_attempts"] != float64(3) { // the config default fallback budget
		t.Fatalf("max_attempts = %v, want 3", ev["max_attempts"])
	}
	// The completion line still lands, same request id, AFTER the evidence.
	done := eventsWith(events, "request completed")
	if len(done) != 1 {
		t.Fatalf("completion lines = %d, want 1", len(done))
	}
	if strField(t, done[0], "request_id") != reqID {
		t.Fatal("completion line lost the request id correlation")
	}
	if done[0]["class"] != "success" || done[0]["status"] != float64(200) {
		t.Fatalf("completion outcome = %v", done[0])
	}
}

// TestUpstreamErrorCorrelatesFallbackAttempts: 429 on a, 429 on b → two
// upstream_error events sharing one request_id with attempt ids /1 and /2.
func TestUpstreamErrorCorrelatesFallbackAttempts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(up.URL))

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 2 {
		t.Fatalf("upstream_error events = %d, want 2", len(errs))
	}
	reqID := strField(t, errs[0], "request_id")
	wantEgress := []string{"a", "b"}
	for i, ev := range errs {
		if strField(t, ev, "request_id") != reqID {
			t.Fatal("attempt events must share the request id")
		}
		if id := strField(t, ev, "attempt_id"); id != fmt.Sprintf("%s/%d", reqID, i+1) {
			t.Fatalf("attempt_id = %q, want %s/%d", id, reqID, i+1)
		}
		if ev["egress"] != wantEgress[i] {
			t.Fatalf("attempt %d → egress %v, want %q", i+1, ev["egress"], wantEgress[i])
		}
		if ev["health_decision"] != "neutral" {
			t.Fatalf("429 poisoned health: %v", ev["health_decision"])
		}
	}
	// a fell back; b's terminal verdict stopped (plan exhausted).
	if errs[0]["retry_decision"] != "fallback" || errs[1]["retry_decision"] != "stop" {
		t.Fatalf("retry decisions = %v, %v, want fallback then stop", errs[0]["retry_decision"], errs[1]["retry_decision"])
	}
}

// TestUpstreamErrorStreamDeathIsResponseStarted: an upstream that dies
// mid-stream after a 200 start is recorded as a response_started STREAM row —
// the delivered status stays 200 and is never rewritten into an HTTP verdict.
func TestUpstreamErrorStreamDeathIsResponseStarted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-stream
	}))
	defer up.Close()

	var buf bytes.Buffer
	doc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: a}
routes:
  - {id: default, egress: [a]}
`, up.URL)
	_, mux := evidenceRouter(t, logging.New(&buf), doc)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Fatalf("the already-delivered delta must reach the client: %q", rec.Body.String())
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1 (the stream death)", len(errs))
	}
	ev := errs[0]
	for key, want := range map[string]any{
		"phase":           "stream",
		"class":           "response_started",
		"status":          float64(200),
		"reason":          "read_error",
		"egress":          "a",
		"health_decision": "",
	} {
		if got, present := ev[key]; key == "health_decision" && present {
			t.Fatalf("stream rows observe no health: %v", got)
		} else if key != "health_decision" && got != want {
			t.Fatalf("field %q = %v, want %v", key, got, want)
		}
	}
	if _, hasAttempt := ev["attempt_id"]; hasAttempt {
		t.Fatalf("stream rows carry no attempt id: %v", ev["attempt_id"])
	}
}

// TestUpstreamErrorNoReEmitOnPostHeaderAbort: the emit contract is ONE event
// per row, ever. A request whose first egress 429s (row emitted at the
// post-Execute pass) and whose fallback egress then dies mid-stream (a second
// emit from StreamAbort) must yield exactly two upstream_error events — the
// 429 row must NOT be rendered a second time by the stream-phase emit.
func TestUpstreamErrorNoReEmitOnPostHeaderAbort(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-stream
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(up.URL))

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Fatalf("the already-delivered delta must reach the client: %q", rec.Body.String())
	}

	events := decodeEvents(t, &buf)
	errs := eventsWith(events, "upstream_error")
	if len(errs) != 2 {
		t.Fatalf("upstream_error events = %d, want exactly 2 (the 429 row once, the stream row once)", len(errs))
	}
	reqID := strField(t, errs[0], "request_id")
	if errs[0]["phase"] != "response" || errs[0]["egress"] != "a" || errs[0]["retry_decision"] != "fallback" {
		t.Fatalf("first event = %v, want egress a's 429 falling back", errs[0])
	}
	if errs[1]["phase"] != "stream" || errs[1]["egress"] != "b" {
		t.Fatalf("second event = %v, want egress b's stream death", errs[1])
	}
	// The hard regression: no line may repeat the 429 attempt's id — a
	// byte-identical duplicate would be indistinguishable from a second dial.
	dupes := 0
	for _, ev := range events {
		if ev["attempt_id"] == reqID+"/1" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Fatalf("attempt_id %s/1 rendered %d times, want exactly 1", reqID, dupes)
	}
}

// TestEvidenceDroppedCounterOnSkipLastRow: the dropped counter rides the LAST
// event of an emit pass whichever kind it is — when the recorder's final
// stored row is a skip, the truncation must surface on the debug egress_skipped
// event (at info level that line is suppressed, but at debug — where an
// investigation looks — the overflow is visible).
func TestEvidenceDroppedCounterOnSkipLastRow(t *testing.T) {
	rec := upstream.NewRecorder()
	for i := 0; i < config.EvidenceMaxRows-1; i++ {
		rec.Append(upstream.Row{Phase: upstream.PhaseResponse, Egress: "a", Status: 429, Class: upstream.ClassUpstream429.String()})
	}
	rec.Append(upstream.Row{Phase: upstream.PhaseSkip, Egress: "b", Reason: upstream.SkipSlotFull, EgressType: "direct"})
	rec.Append(upstream.Row{Phase: upstream.PhaseResponse, Egress: "c"}) // past the cap: dropped
	if rec.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", rec.Dropped())
	}

	var buf bytes.Buffer
	ev := newEvidenceLog(logging.New(&buf).Level(zerolog.DebugLevel), rec, "req", 1, "default", "m", "chat", true, "ses_e2e", "http://up.invalid", 3, 2, nil)
	ev.Emit()

	skips := eventsWith(decodeEvents(t, &buf), "egress_skipped")
	if len(skips) != 1 {
		t.Fatalf("egress_skipped events = %d, want 1", len(skips))
	}
	if skips[0]["evidence_dropped"] != float64(1) {
		t.Fatalf("skip event evidence_dropped = %v, want 1", skips[0]["evidence_dropped"])
	}
	if skips[0]["egress_type"] != "direct" {
		t.Fatalf("skip event must carry the egress type: %v", skips[0])
	}
	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != config.EvidenceMaxRows-1 {
		t.Fatalf("upstream_error events = %d, want %d", len(errs), config.EvidenceMaxRows-1)
	}
	for _, e := range errs {
		if _, has := e["evidence_dropped"]; has {
			t.Fatal("only the LAST event of a pass may carry evidence_dropped")
		}
	}
}

// TestUpstreamErrorForcedConvertFailure: a non-streaming client behind an SSE
// upstream whose stream never parses renders a forced-phase row with the
// bounded reason. The row's status is the status the upstream response
// STARTED with (200) — a phase row never rewrites it into the synthesized
// client 502 (that is the error-write path's fact).
func TestUpstreamErrorForcedConvertFailure(t *testing.T) {
	up := newScriptedUpstream(t, &upstreamRecorder{}, http.StatusOK, "text/event-stream", "event: ping\n\n")
	defer up.Close()

	var buf bytes.Buffer
	doc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: a}
routes:
  - {id: default, egress: [a]}
`, up.URL)
	_, mux := evidenceRouter(t, logging.New(&buf), doc)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid SSE response") {
		t.Fatalf("client body = %q", rec.Body.String())
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1", len(errs))
	}
	for key, want := range map[string]any{
		"phase":  "forced",
		"class":  "response_started",
		"status": float64(200),
		"reason": "convert",
	} {
		if errs[0][key] != want {
			t.Fatalf("field %q = %v, want %v", key, errs[0][key], want)
		}
	}
}

// TestEvidenceEmitLevelPolicy pins the level contract at the renderer: skip
// rows are debug diagnostics, failure rows are warn — a logger at info never
// emits the skip line (the volume guarantee), and success rows never exist.
func TestEvidenceEmitLevelPolicy(t *testing.T) {
	newLog := func(level zerolog.Level, buf *bytes.Buffer) zerolog.Logger {
		return logging.New(buf).Level(level)
	}
	rows := func() *upstream.Recorder {
		rec := upstream.NewRecorder()
		rec.Append(upstream.Row{Phase: upstream.PhaseSkip, Egress: "a", Reason: upstream.SkipSlotFull})
		rec.Append(upstream.Row{Phase: upstream.PhaseResponse, Egress: "a", EgressType: "direct", Attempt: 1,
			Status: 429, Class: "upstream_429", HealthDecision: "neutral", RetryDecision: "stop"})
		return rec
	}

	t.Run("debug level renders both", func(t *testing.T) {
		var buf bytes.Buffer
		ev := newEvidenceLog(newLog(zerolog.DebugLevel, &buf), rows(), "req", 1, "r", "m", "chat", false,
			"session", "http://h:1/b", 1, 2, []byte("{}"))
		ev.Emit()
		events := decodeEvents(t, &buf)
		if len(eventsWith(events, "egress_skipped")) != 1 || len(eventsWith(events, "upstream_error")) != 1 {
			t.Fatalf("events = %v", events)
		}
	})
	t.Run("info level suppresses skips only", func(t *testing.T) {
		var buf bytes.Buffer
		ev := newEvidenceLog(newLog(zerolog.InfoLevel, &buf), rows(), "req", 1, "r", "m", "chat", false,
			"session", "http://h:1/b", 1, 2, []byte("{}"))
		ev.Emit()
		events := decodeEvents(t, &buf)
		if len(eventsWith(events, "egress_skipped")) != 0 {
			t.Fatal("skip rows must stay out of the info stream")
		}
		if len(eventsWith(events, "upstream_error")) != 1 {
			t.Fatal("failure evidence must survive the info level")
		}
	})
}

// TestEvidenceDroppedCounterSurfaces: when the row cap is hit, the dropped
// count rides the last rendered event so truncation is visible where it
// happened.
func TestEvidenceDroppedCounterSurfaces(t *testing.T) {
	rec := upstream.NewRecorder()
	for i := 0; i < config.EvidenceMaxRows; i++ {
		rec.Append(upstream.Row{Phase: upstream.PhaseResponse, Attempt: i + 1, Status: 429, Class: "upstream_429"})
	}
	rec.Append(upstream.Row{Phase: upstream.PhaseResponse, Attempt: 99, Status: 429, Class: "upstream_429"})

	var buf bytes.Buffer
	ev := newEvidenceLog(logging.New(&buf), rec, "req", 1, "r", "m", "chat", false,
		"session", "http://h:1/b", 1, 16, []byte("{}"))
	ev.Emit()

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != config.EvidenceMaxRows {
		t.Fatalf("events = %d, want the cap", len(errs))
	}
	if errs[len(errs)-1]["evidence_dropped"] != float64(1) {
		t.Fatalf("dropped counter missing on the last event: %v", errs[len(errs)-1])
	}
	for _, e := range errs[:len(errs)-1] {
		if _, has := e["evidence_dropped"]; has {
			t.Fatal("dropped counter must ride only the last event")
		}
	}
}

// TestSuccessEmitsNoEvidenceEvent: the happy path is the completion line's
// job — a clean 200 SSE request renders no upstream_error and no egress_skipped.
func TestSuccessEmitsNoEvidenceEvent(t *testing.T) {
	up := newScriptedUpstream(t, &upstreamRecorder{}, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()

	var buf bytes.Buffer
	doc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: a}
routes:
  - {id: default, egress: [a]}
`, up.URL)
	_, mux := evidenceRouter(t, logging.New(&buf), doc)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	events := decodeEvents(t, &buf)
	if len(eventsWith(events, "upstream_error")) != 0 || len(eventsWith(events, "egress_skipped")) != 0 {
		t.Fatalf("clean traffic must emit zero evidence events: %v", events)
	}
	if len(eventsWith(events, "request completed")) != 1 {
		t.Fatal("the completion line still owns success telemetry")
	}
}
