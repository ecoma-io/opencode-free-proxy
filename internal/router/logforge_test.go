package router

// Log-forging guard: a client-controlled model id must not be able to inject
// forged lines into the router's log stream (CWE-117). The LOG LINE quotes
// the id (%q — control bytes escaped); the upstream request body echo is
// untouched (the raw id still travels to the provider, byte for byte).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"opencode-free-proxy/internal/logging"
)

// TestRelayModelIDCannotForgeLogLines drives a model id containing a raw
// newline through relay(): every rendered log line stays ONE line, and the
// upstream body still carries the hostile id verbatim.
func TestRelayModelIDCannotForgeLogLines(t *testing.T) {
	dir := t.TempDir()
	hostile := "qwen3-coder-free\nFAKE log line: egress=ghost status=999"

	var upMu sync.Mutex
	var upstreamModel string
	upstreamSrv := forwardingProxy(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading upstream body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("upstream body is not JSON: %v", err)
		}
		upMu.Lock()
		upstreamModel, _ = body["model"].(string)
		upMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatStreamSSE)
	})
	defer upstreamSrv.Close()

	var logsMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	_, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, pxyURL(upstreamSrv)), logf)

	rec := postJSON(t, mux, "/v1/chat/completions",
		fmt.Sprintf(`{"model":%q,"stream":true}`, hostile), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// The hostile id reached the upstream untouched (the sanitizer is for the
	// LOG LINE only — the JSON body echo stays raw).
	upMu.Lock()
	got := upstreamModel
	upMu.Unlock()
	if got != hostile {
		t.Fatalf("upstream body model = %q, want the raw hostile id %q", got, hostile)
	}

	// Every rendered log line is a single line: no raw control bytes survive.
	logsMu.Lock()
	defer logsMu.Unlock()
	if len(logs) == 0 {
		t.Fatal("no log lines captured — the completion log never ran")
	}
	for i, line := range logs {
		if strings.ContainsAny(line, "\n\r") {
			t.Fatalf("log line %d contains a raw newline (forgeable): %q", i, line)
		}
	}
	// The completion line quotes the id with %q: the newline appears escaped.
	want := `model="qwen3-coder-free\nFAKE log line: egress=ghost status=999"`
	found := false
	for _, line := range logs {
		if strings.Contains(line, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no completion line quotes the escaped model id %q; logs: %v", want, logs)
	}
}

// TestUpstreamErrorMessageCannotForgeLogLines extends the guard to the
// evidence layer: an upstream error body carrying CRLF (the second
// attacker-controlled text source after the model id) must reach the
// upstream_error event sanitized — control bytes gone, message intact.
func TestUpstreamErrorMessageCannotForgeLogLines(t *testing.T) {
	hostile := "rate limited\r\nFAKE log line: egress=ghost status=200"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"message":%q}}`, hostile))
	}))
	defer up.Close()

	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), fmt.Sprintf(`
upstream:
  base: %q
egress:
  - {id: a}
routes:
  - {id: default, egress: [a]}
`, up.URL))

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.ContainsAny(line, "\r") {
			t.Fatalf("log line contains a raw CR (forgeable): %q", line)
		}
		// Every line must parse as exactly one JSON object: a forged line
		// injected via the message would break the framing.
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unparsable (possibly forged) log line: %q: %v", line, err)
		}
	}
	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1", len(errs))
	}
	if got := errs[0]["message"]; got != "rate limited FAKE log line: egress=ghost status=200" {
		t.Fatalf("message = %v, want the sanitized text with control bytes folded", got)
	}
}
