package router

// Caller correlation id (issue #83). The tests here pin the three properties
// the implementation is only safe because of, and the one shape the change must
// NOT take:
//
//   - accept-or-ignore, never sanitize: an out-of-charset value produces NO
//     field, never a folded one. A rewritten join key is the worst available
//     outcome — the join fails silently with nothing pointing at the reason.
//   - log-only: a caller id changes nothing but the log. Routing, egress order,
//     health state, session stickiness and session_fp are all identical with and
//     without one.
//   - never promoted: the local request_id stays authoritative, attempt_id stays
//     request_id/N, and a value shaped like a local id never passes as one.
//
// The pre-routing rejections are pinned here too: every path that can produce a
// client-visible 4xx/5xx now records a line, and it was silent before.
//
// The router fixtures (evidenceRouter, decodeEvents, eventsWith, strField,
// postJSON) and upstreamRecorder are the file-local helpers the existing
// evidence tests use — this file adds no helper of its own beyond the two
// fixtures at the bottom.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/logging"
	"opencode-free-proxy/internal/relay"
	"opencode-free-proxy/internal/upstream"

	"github.com/rs/zerolog"
)

// ---- fixtures ----

// callerIDRouter wires a server whose single direct egress dials upstreamURL,
// with one catch-all route, and a logger writing JSON into the returned buffer.
// The caller id is a pure header read, so the base does not matter for the
// accept/reject tests — those need no upstream at all.
func callerIDRouter(t *testing.T, upstreamURL string) (*Server, *http.ServeMux, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s, mux := evidenceRouter(t, logging.New(&buf), twoDirectEgressDoc(upstreamURL))
	return s, mux, &buf
}

// callerIDNoRouteRouter wires a server whose ONLY route matches a specific
// model, so a request naming anything else is rejected with "No route matched"
// — the one pre-routing rejection that needs a real config to reproduce.
func callerIDNoRouteRouter(t *testing.T) (*Server, *http.ServeMux, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s, mux := evidenceRouter(t, logging.New(&buf), `
upstream:
  base: "http://upstream.invalid"
egress:
  - {id: a}
routes:
  - {id: only, match: {models: ["qwen3-coder-free"]}, egress: [a]}
`)
	return s, mux, &buf
}

// soleCompletion asserts the request produced exactly one completion line and
// returns it. Every outcome this change touches must be exactly one line: a
// second would double-count the request, and zero would be the silent path
// being closed.
func soleCompletion(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	done := eventsWith(decodeEvents(t, buf), "request completed")
	if len(done) != 1 {
		t.Fatalf("completion lines = %d, want exactly 1: %v", len(done), eventsWith(decodeEvents(t, buf), "request completed"))
	}
	return done[0]
}

// ---- accept-or-ignore, never sanitize ----

// TestCallerRequestIDRejectedValuesProduceNoField is the table-driven reject
// matrix. Every case must leave the field ABSENT — the value is never folded,
// collapsed, truncated or escaped into something else. Note the cases a
// trimming implementation would emit something for: " abc" and "abc " are what
// a sanitize-and-keep would produce, and are exactly what must not appear.
func TestCallerRequestIDRejectedValuesProduceNoField(t *testing.T) {
	_, _, buf := callerIDRouter(t, "http://upstream.invalid")
	overLength := strings.Repeat("a", config.MaxCallerRequestIDLen+1)

	for _, tc := range []struct{ name, value string }{
		{"empty", ""},
		{"over length", overLength},
		{"carriage return", "abc\rdef"},
		{"line feed", "abc\ndef"},
		{"nul", "abc\x00def"},
		{"inner space", "abc def"},
		{"leading space", " abc"},
		{"trailing space", "abc "},
		{"tab", "abc\tdef"},
		{"quote", `abc"def`},
		{"backslash", `abc\def`},
		{"non-ascii", "abcé"},
		{"emoji", "abc🙂"},
		{"brace", "abc{def}"},
		{"percent", "abc%20def"},
		{"slash", "abc/def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh router per case: each rejection is its own request, and
			// a shared buffer would let one case's line satisfy another's
			// assertion.
			_, mux, buf := callerIDRouter(t, "http://upstream.invalid")
			res := postJSON(t, mux, "/v1/chat/completions",
				`{"model":"qwen3-coder-free","stream":true}`,
				map[string]string{config.CallerRequestIDHeader: tc.value})
			// The rejection is a diagnostic, never an error the caller sees:
			// the request proceeds exactly as it would have without the header.
			if res.Code != http.StatusServiceUnavailable && res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want the request to proceed unaffected (a malformed correlation header is not a routing fact)", res.Code)
			}
			ev := soleCompletion(t, buf)
			if got, ok := ev["caller_request_id"]; ok {
				t.Fatalf("caller_request_id = %v, want ABSENT — a rejected value must be dropped, never folded from %q", got, tc.value)
			}
			// This process's own correlation handle must survive a dropped
			// caller id: that is what makes the rejection loud rather than
			// silent, and the only safe way for a join to degrade.
			if strField(t, ev, "request_id") == "" {
				t.Fatal("request_id lost — a rejected caller id must not cost the local one")
			}
		})
	}
	_ = buf
}

// TestCallerRequestIDAcceptedVerbatim: an in-charset value is used WHOLE. The
// characters . _ : - are on the allow-list precisely so a W3C trace id or a
// UUID-ish caller id passes unescaped; this pins that they survive byte for
// byte. The one-character value is excluded from the Msgf check: a bare "a"
// occurs inside ordinary words in the frozen sentence, so a substring test
// would be meaningless for it — the long values carry that assertion.
func TestCallerRequestIDAcceptedVerbatim(t *testing.T) {
	for _, want := range []string{
		"01HQZX9K3M7NPQRSTVWXY2EFGH",
		"trace-01HQZX9K3M7NPQRSTVWXY2EFGH.0001",
		strings.Repeat("z", config.MaxCallerRequestIDLen), // exactly at the bound
		"a_b.c:d-e",
	} {
		t.Run(want[:min(len(want), 20)], func(t *testing.T) {
			rec := &upstreamRecorder{}
			up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
			defer up.Close()
			_, mux, buf := callerIDRouter(t, up.URL)

			postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
				map[string]string{config.CallerRequestIDHeader: want})

			ev := soleCompletion(t, buf)
			if got := strField(t, ev, "caller_request_id"); got != want {
				t.Fatalf("caller_request_id = %q, want %q (verbatim, never rewritten)", got, want)
			}
			// The frozen Msgf must not grow the id: correlation rides the
			// JSON fields only (AGENTS.md rule 10).
			if msg, _ := ev["msg"].(string); strings.Contains(msg, want) {
				t.Fatalf("the frozen Msgf text must not carry the caller id: %q", msg)
			}
		})
	}
}

// TestCallerRequestIDShortestAcceptedValue: the one-character case, pinned
// separately because it cannot use the substring check above.
func TestCallerRequestIDShortestAcceptedValue(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux, buf := callerIDRouter(t, up.URL)

	postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: "a"})

	if got := strField(t, soleCompletion(t, buf), "caller_request_id"); got != "a" {
		t.Fatalf("caller_request_id = %q, want %q — 1 byte is inside the bound", got, "a")
	}
}

// TestCallerRequestIDIsNeverInterchangeableWithRequestID is the collision
// hazard. Both ids are 16 hex characters BY CONSTRUCTION, so an operator
// grepping request_id across two services' merged logs can splice unrelated
// evidence rows onto a completion line. A caller-supplied value shaped exactly
// like a local one must therefore never appear as request_id, and the local id
// must never be replaced by it.
func TestCallerRequestIDIsNeverInterchangeableWithRequestID(t *testing.T) {
	const spoof = "deadbeefdeadbeef" // 16 hex: exactly a local request_id's shape
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux, buf := callerIDRouter(t, up.URL)

	postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: spoof})

	ev := soleCompletion(t, buf)
	if got := strField(t, ev, "caller_request_id"); got != spoof {
		t.Fatalf("caller_request_id = %q, want %q", got, spoof)
	}
	if got := strField(t, ev, "request_id"); got == spoof {
		t.Fatal("the caller's value was promoted into request_id — a caller may name a request, never the process's own identity")
	}
	if got := strField(t, ev, "request_id"); len(got) != 16 {
		t.Fatalf("request_id = %q, want a freshly minted 16-hex local id, uninfluenced by the caller", got)
	}
}

// ---- log-only: the value reaches no decision ----

// TestCallerRequestIDChangesNothingButTheLog is the load-bearing test: the
// value must not reach ANY decision. Two identical requests, one carrying a
// caller id and one not, must produce identical routing, egress, attempts,
// class, status, model, endpoint and session pseudonym — the caller id is the
// only observable difference between their logs.
func TestCallerRequestIDChangesNothingButTheLog(t *testing.T) {
	const body = `{"model":"qwen3-coder-free","stream":true}`

	send := func(callerID string) map[string]any {
		t.Helper()
		rec := &upstreamRecorder{}
		up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
		defer up.Close()
		_, mux, buf := callerIDRouter(t, up.URL)
		var headers map[string]string
		if callerID != "" {
			headers = map[string]string{config.CallerRequestIDHeader: callerID}
		}
		postJSON(t, mux, "/v1/chat/completions", body, headers)
		ev := soleCompletion(t, buf)
		// The upstream request is part of "every decision": the header set
		// that reached the provider must be identical too.
		calls := rec.snapshot()
		if len(calls) != 1 {
			t.Fatalf("upstream calls = %d, want 1", len(calls))
		}
		ev["_upstream_session"] = calls[0].Header.Get("X-Opencode-Session")
		return ev
	}

	plain := send("")
	withID := send("caller-one")

	for _, key := range []string{
		"route", "egress", "attempts", "class", "status", "fallback",
		"model", "endpoint", "generation", "_upstream_session",
	} {
		a, aOK := plain[key]
		b, bOK := withID[key]
		if aOK != bOK || a != b {
			t.Fatalf("%q differs between an identified and an anonymous request: %v vs %v — the caller id must never reach a decision", key, a, b)
		}
	}
	if _, ok := plain["caller_request_id"]; ok {
		t.Fatal("a request with no caller id must carry no field at all, not an empty one")
	}
	if got := strField(t, withID, "caller_request_id"); got != "caller-one" {
		t.Fatalf("caller_request_id = %q, want caller-one", got)
	}
	if strField(t, plain, "request_id") == strField(t, withID, "request_id") {
		t.Fatal("two requests minted the same local id")
	}
}

// ---- the evidence emitters ----

// TestCallerRequestIDOnEvidenceEvents: both evidence emitters carry the field,
// and a row's attempt_id is still derived from the LOCAL id — the caller id is
// a join key, never the root of the attempt sequence.
func TestCallerRequestIDOnEvidenceEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer up.Close()
	_, mux, buf := callerIDRouter(t, up.URL)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: "caller-on-evidence"})
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d (body %s), want the provider's 429 relayed", res.Code, res.Body.String())
	}

	errs := eventsWith(decodeEvents(t, buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1: %v", len(errs), eventsWith(decodeEvents(t, buf), "upstream_error"))
	}
	ev := errs[0]
	if got := strField(t, ev, "caller_request_id"); got != "caller-on-evidence" {
		t.Fatalf("evidence caller_request_id = %q, want caller-on-evidence", got)
	}
	reqID := strField(t, ev, "request_id")
	if got := strField(t, ev, "attempt_id"); got != reqID+"/1" {
		t.Fatalf("attempt_id = %q, want %s/1 — it stays derived from the LOCAL id", got, reqID)
	}
	// And the completion line agrees on both ids.
	done := soleCompletion(t, buf)
	if strField(t, done, "request_id") != reqID {
		t.Fatalf("completion request_id = %q, evidence = %q", strField(t, done, "request_id"), reqID)
	}
	if got := strField(t, done, "caller_request_id"); got != "caller-on-evidence" {
		t.Fatalf("completion caller_request_id = %q, want caller-on-evidence", got)
	}
}

// TestCallerRequestIDOnSkipEvents: the debug egress_skipped emitter is the
// second evidence site, and it must carry the same field — a skip is a
// scheduling fact about the caller's request, and an operator joining two
// services' logs needs it there too. The row is appended directly, the
// convention the existing evidence tests use for a phase the executor produces
// under concurrency.
func TestCallerRequestIDOnSkipEvents(t *testing.T) {
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(prev)

	var buf bytes.Buffer
	rec := upstream.NewRecorder()
	rec.Append(upstream.Row{Phase: upstream.PhaseSkip, Egress: "b", Reason: upstream.SkipSlotFull, EgressType: "direct"})
	ev := newEvidenceLog(logging.New(&buf), rec, "req", "caller-on-skip", 1,
		"default", "m", "chat", true, "ses", "http://up.invalid", 3, 2, nil)
	ev.Emit()

	skips := eventsWith(decodeEvents(t, &buf), "egress_skipped")
	if len(skips) != 1 {
		t.Fatalf("egress_skipped events = %d, want 1", len(skips))
	}
	if got := strField(t, skips[0], "caller_request_id"); got != "caller-on-skip" {
		t.Fatalf("egress_skipped caller_request_id = %q, want caller-on-skip", got)
	}
	if got := strField(t, skips[0], "request_id"); got != "req" {
		t.Fatalf("egress_skipped request_id = %q, want req — the local id is unaffected", got)
	}
}

// TestRejectedCallerRequestIDOnEvidenceEvents: the reject path is the same on
// the evidence emitters. A hostile value must leave no field and must not cost
// the local one.
func TestRejectedCallerRequestIDOnEvidenceEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer up.Close()
	_, mux, buf := callerIDRouter(t, up.URL)

	postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: "evil\r\nFAKE log line"})

	errs := eventsWith(decodeEvents(t, buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1", len(errs))
	}
	if got, ok := errs[0]["caller_request_id"]; ok {
		t.Fatalf("caller_request_id = %v, want ABSENT for a CR/LF value", got)
	}
	if strField(t, errs[0], "request_id") == "" {
		t.Fatal("request_id lost on the evidence row")
	}
}

// ---- pre-routing rejections: the blind spot ----

// TestPreRoutingRejectionsRecordOneLine: every path that can produce a
// client-visible 4xx now logs a line. Before this, all of these returned with
// NOTHING written, so an operator correlating the front-end service's logs
// against this process would find the request simply absent. One line each,
// carrying the caller's id when one was sent and the real status.
func TestPreRoutingRejectionsRecordOneLine(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{"unreadable body", "/v1/chat/completions", `not json`, http.StatusBadRequest},
		{"not a JSON object", "/v1/chat/completions", `[1,2,3]`, http.StatusBadRequest},
		{"missing model", "/v1/chat/completions", `{}`, http.StatusBadRequest},
		// test-connection probe: needs the exact header name the handler
		// recognizes (x-test-connection with ANY value, even empty — presence
		// is the gate). The caller id is a separate header read alongside it.
		{"test connection probe", "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mux, buf := callerIDRouter(t, "http://upstream.invalid")
			headers := map[string]string{config.CallerRequestIDHeader: "caller-pre-routing"}
			if tc.name == "test connection probe" {
				headers["X-Test-Connection"] = "1"
			}
			res := postJSON(t, mux, tc.path, tc.body, headers)
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tc.wantStatus)
			}
			ev := soleCompletion(t, buf)
			if got := strField(t, ev, "caller_request_id"); got != "caller-pre-routing" {
				t.Fatalf("caller_request_id = %q, want caller-pre-routing", got)
			}
			if got := ev["status"]; got != float64(tc.wantStatus) {
				t.Fatalf("logged status = %v, want %d", got, tc.wantStatus)
			}
			// A pre-routing rejection dialed nothing, so the no-dial facts
			// must say so rather than implying an upstream interaction.
			if got := ev["attempts"]; got != float64(0) {
				t.Fatalf("attempts = %v, want 0", got)
			}
			if got := strField(t, ev, "egress"); got != "" {
				t.Fatalf("egress = %q, want empty", got)
			}
			if strField(t, ev, "request_id") == "" {
				t.Fatal("a logged rejection must still carry a local request id")
			}
			// The endpoint is known even when the body never parsed.
			if got := strField(t, ev, "endpoint"); got != "chat" {
				t.Fatalf("endpoint = %q, want chat", got)
			}
		})
	}
}

// TestNoRouteMatchedRecordsOneLine: the "no route matched" 400 is the one
// pre-routing rejection that needs a real config to reproduce — a route that
// matches one model, and a request naming another.
func TestNoRouteMatchedRecordsOneLine(t *testing.T) {
	_, mux, buf := callerIDNoRouteRouter(t)
	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"no-such-model","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: "caller-no-route"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", res.Code, res.Body.String())
	}
	ev := soleCompletion(t, buf)
	if got := strField(t, ev, "caller_request_id"); got != "caller-no-route" {
		t.Fatalf("caller_request_id = %q, want caller-no-route", got)
	}
	if got := strField(t, ev, "route"); got != "" {
		t.Fatalf("route = %q, want empty — no route matched", got)
	}
	// The model the request ASKED for is still on the line: a rejected request
	// is exactly the one an operator wants to find by model.
	if got := strField(t, ev, "model"); got != "no-such-model" {
		t.Fatalf("model = %q, want no-such-model", got)
	}
}

// TestMethodNotAllowedRecordsOneLine pins the 405 branch of relay. Note HOW it
// is reached: cmd/server/main.go registers method-scoped patterns
// ("POST /v1/chat/completions"), so in production Go's ServeMux answers a
// method mismatch with its own 405 and relay is never entered — the branch is
// defensive. Driving it through the mux would assert a line that can never
// appear in a real deployment, so the test calls relay directly, and says so.
func TestMethodNotAllowedRecordsOneLine(t *testing.T) {
	s, _, buf := callerIDRouter(t, "http://upstream.invalid")
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set(config.CallerRequestIDHeader, "caller-405")
	rec := httptest.NewRecorder()
	s.relay(rec, req, relay.FormatChat)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	ev := soleCompletion(t, buf)
	if got := strField(t, ev, "caller_request_id"); got != "caller-405" {
		t.Fatalf("caller_request_id = %q, want caller-405", got)
	}
	if got := ev["status"]; got != float64(http.StatusMethodNotAllowed) {
		t.Fatalf("logged status = %v, want 405", got)
	}
	if got := ev["attempts"]; got != float64(0) {
		t.Fatalf("attempts = %v, want 0", got)
	}
}

// TestMethodNotAllowedThroughTheMuxLogsNothing documents the production shape
// the test above works around: the mux answers 405 before relay runs, so NO
// line is emitted for a method mismatch on a real deployment. This is a
// standing property of the wiring, not a gap in the completion line — and it
// is why the "one line per request" invariant is scoped to requests that
// REACH the handler.
func TestMethodNotAllowedThroughTheMuxLogsNothing(t *testing.T) {
	_, mux, buf := callerIDRouter(t, "http://upstream.invalid")
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set(config.CallerRequestIDHeader, "caller-405")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want the mux's 405", rec.Code)
	}
	if done := eventsWith(decodeEvents(t, buf), "request completed"); len(done) != 0 {
		t.Fatalf("completion lines = %d, want 0 — the mux answers 405 before relay is entered", len(done))
	}
}

// ---- log-only: the value reaches no wire ----

// TestCallerRequestIDNeverReachesTheWire: the value is log-only. It must not
// become a response header, must not be exposed to browser JavaScript, and must
// not be forwarded upstream. The last is the subtle half: captureDownstream is
// an explicit allow-list, and this header is deliberately not on it — which is
// what keeps it away from session resolution and prompt caching.
func TestCallerRequestIDNeverReachesTheWire(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux, _ := callerIDRouter(t, up.URL)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`,
		map[string]string{config.CallerRequestIDHeader: "caller-wire"})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	for name := range res.Header() {
		if strings.EqualFold(name, config.CallerRequestIDHeader) {
			t.Fatalf("the response carries %s — the id is log-only, never echoed (issue #77)", config.CallerRequestIDHeader)
		}
	}
	if got := res.Header().Get("Access-Control-Expose-Headers"); got != "" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want absent — the id must not become browser-readable", got)
	}
	assertNoWireProvenance(t, res.Header())

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	// Upstream must never see it: the forwarding allow-list is the thing that
	// keeps this header out of session resolution and the session_fp pseudonym.
	if got := calls[0].Header.Get(config.CallerRequestIDHeader); got != "" {
		t.Fatalf("upstream received %s = %q — this header is log-only and is not in the forwarding allow-list",
			config.CallerRequestIDHeader, got)
	}
	// The session header the executor DOES forward must be unaffected by it.
	if calls[0].Header.Get("X-Opencode-Session") == "" {
		t.Fatal("no session header reached the upstream — the fixture is not exercising the forwarding path")
	}
}
