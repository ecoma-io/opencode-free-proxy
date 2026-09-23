package router

// Provenance rendering, end to end through the real pipeline: a request whose
// egress cannot be reached must log where the failure happened and — the
// load-bearing part — that the request provably never went out. This is the
// chain the recovery contract will read: transport boundary → Failure →
// evidence row → log field.

import (
	"bytes"
	"net/http"
	"testing"

	"opencode-free-proxy/internal/logging"
)

// deadEgressDoc routes over a direct egress pointed at a loopback port nothing
// can be listening on. The connection is refused before any request byte
// exists, which is the one condition the contract will treat as replayable.
func deadEgressDoc() string {
	return `upstream:
  base: "http://127.0.0.1:1"
egress:
  - {id: a}
routes:
  - {id: default, egress: [a]}
fallback:
  max_attempts: 1
`
}

// TestTransportFailureLogsNotSentProvenance: a target-connect refusal renders
// as transport / target_connect / not_sent. The request never reached the
// provider, and the log says so in the fixed vocabulary the contract is
// written in.
func TestTransportFailureLogsNotSentProvenance(t *testing.T) {
	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), deadEgressDoc())

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the synthesized 502 (body=%s)", rec.Code, rec.Body.String())
	}

	errs := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(errs) == 0 {
		t.Fatal("no upstream_error event rendered")
	}
	// Every row of the failed attempt carries the same provenance — the count
	// itself is the client's retry matrix's business, not this contract's.
	for _, ev := range errs {
		for key, want := range map[string]any{
			"phase":         "transport",
			"class":         "connection_error",
			"origin":        "transport",
			"failure_phase": "target_connect",
			"request_state": "not_sent",
			"egress":        "a",
		} {
			if ev[key] != want {
				t.Fatalf("field %q = %v, want %v (event=%v)", key, ev[key], want, ev)
			}
		}
		if ev["status"] != nil {
			t.Fatalf("a transport row must carry no HTTP status, got %v", ev["status"])
		}
	}
}
