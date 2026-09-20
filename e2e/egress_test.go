//go:build e2e

// Issue #6 hardening E2E: the health-policy and proxy-auth semantics proven
// end to end through the real server binary. Per-egress behavior stays
// observable on the wire even though OFP_UPSTREAM_BASE is a single value —
// each egress reaches a DIFFERENT local listener (the reload_test.go trick),
// so per-egress call counts are exact. Every wait here is an observable
// condition (an upstream counter, the reload log line, a closed gate); none
// is a fixed sleep.
//
// Wire facts under test, one test each:
//
//   - a request's health POLICY is the one captured from its OWN snapshot —
//     a hot reload never re-judges an in-flight observation (health.go).
//   - a CONNECT answered 407 by a REAL forward proxy is a typed proxy-auth
//     failure: one dial, no 502-matrix retries, executor falls back
//     (connect.go + client.go DoClassified).
//   - a 407 that arrives as a RESPONSE status is a client-error verdict: no
//     fallback, no health mark (failure.go classifyStatusFor).
//   - health state is keyed by egress id + transport signature: swapping the
//     proxy URL starts a clean identity, a policy-only reload keeps history
//     (config.Egress.HealthKey).
package e2e

import (
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// requestLines returns the server's per-request outcome lines. Every request
// logs exactly one line and each carries "generation=" — the reload banner
// says "generation N" (space, not equals), so it never matches.
func (sp *proxySpawn) requestLines() []string {
	var out []string
	for _, ln := range strings.Split(sp.out.String(), "\n") {
		if strings.Contains(ln, "generation=") {
			out = append(out, ln)
		}
	}
	return out
}

// assertRequestLine requires every want substring in the idx-th (0-based)
// request outcome line. Requests in these tests are strictly sequential, so
// the line order is deterministic.
func assertRequestLine(t *testing.T, sp *proxySpawn, idx int, want ...string) {
	t.Helper()
	lines := sp.requestLines()
	if idx >= len(lines) {
		t.Fatalf("request line %d missing (have %d):\n%s", idx, len(lines), sp.out.String())
	}
	for _, w := range want {
		if !strings.Contains(lines[idx], w) {
			t.Fatalf("request line %d = %q, missing %q\nlog:\n%s", idx, lines[idx], w, sp.out.String())
		}
	}
}

// drainChat asserts a 200 streamed chat completion (the canned fakeUpstream
// stream, relayed) and returns the X-OFP-Egress header — the SERVING egress.
func drainChat(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(b), fakeChatID) {
		t.Fatalf("body missing the canned stream id:\n%s", b)
	}
	return resp.Header.Get("X-OFP-Egress")
}

// chatProxy is the per-egress fake upstream: an http forward proxy that
// answers every relayed request with the canned SSE stream (the reload_test.go
// trick) and counts what it saw.
func chatProxy(count *atomic.Int64) *httptest.Server {
	return fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
}

// statusProxy answers every relayed request with the status last written to
// status plus a count — the scriptable failure source. A 200 answer is a real
// streamed chat completion, so an egress can "recover" mid-test and prove it
// still serves; anything else carries a JSON error body the envelope test can
// match on.
func statusProxy(count *atomic.Int64, status *atomic.Int64) *httptest.Server {
	return fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		if status.Load() == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `{"error":{"message":"credentials rejected"}}`)
	})
}

// healthYAML is the policy shared by the reload/identity tests: one failure
// is enough to cool an egress, and the window outlives the test so a single
// observation decides everything.
const healthYAML = "health:\n  failure_threshold: 1\n  cooldown: 60s\n"

// TestReloadHealthPolicyPinnedPerGeneration: a request adjudicates its own
// observations under the health policy of ITS config snapshot. Generation 1
// ships health disabled; while a request is parked inside egress a (which
// answers 500 when released), the file swaps to generation 2 (health enabled,
// threshold 1, 60s cooldown). The parked request's failure must NOT mark a —
// otherwise generation 2's threshold would cool it on the spot — while the
// FIRST generation-2 failure of a does arm the cooldown.
func TestReloadHealthPolicyPinnedPerGeneration(t *testing.T) {
	dir := cfgDir(t)

	// a 500s, but every call parks on the gate first: holding the request
	// open proves its snapshot was pinned (the dial already happened) before
	// the reload is written.
	gate := make(chan struct{})
	var aCalls atomic.Int64
	proxyA := fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		aCalls.Add(1)
		<-gate
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer proxyA.Close()
	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
	defer proxyB.Close()

	// Both generations share egresses and routes byte-for-byte; ONLY the
	// health block differs — the swap is a policy-only reload.
	egressYAML := fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.URL, proxyB.URL)
	writeCFG(t, dir, egressYAML+"health:\n  enabled: false\n")
	sp := spawnProxy(t, dir, map[string]string{
		"OFP_UPSTREAM_BASE":  "http://upstream.invalid/zen",
		"OFP_CONFIG":         "cfg.yaml",
		"OFP_CONFIG_POLL_MS": "50",
	})

	done := make(chan *http.Response, 1)
	go func() { done <- sp.post(t, streamBody, nil) }()
	waitFor(t, 5*time.Second, func() bool { return aCalls.Load() == 1 },
		"request never parked inside proxy a\nlog:\n"+sp.out.String())

	// Swap to generation 2 while the generation-1 request sits in a.
	writeCFG(t, dir, egressYAML+healthYAML)
	waitSwap(t, sp, "config reload: swapped to new config (generation 2")

	close(gate)
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(35 * time.Second):
		t.Fatalf("in-flight request never returned after gate release\nlog:\n%s", sp.out.String())
	}
	if eg := drainChat(t, resp); eg != "b" {
		t.Fatalf("in-flight X-OFP-Egress = %q, want b (generation-1 fallback)", eg)
	}
	// The fallback is attributed to the request's OWN generation.
	assertRequestLine(t, sp, 0, "generation=1", "egress=b", "attempts=2", "class=success", "fallback=true")

	// Round-robin over [a, b]: the parked request consumed cursor 0 (head a),
	// so this turn heads b — and advances the cursor to 2, making a the head
	// of the request after it.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b", eg)
	}

	// THE PINNING PROOF: a is dialable again. If the parked generation-1
	// failure had been judged under generation 2's policy, a would already be
	// cooling and this request would never reach it.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b (a failed, fell back)", eg)
	}
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("a dialed %d times, want 2 (the parked gen-1 dial + this gen-2 dial) — the in-flight failure must not mark health", got)
	}
	assertRequestLine(t, sp, 2, "generation=2", "attempts=2", "fallback=true")

	// That generation-2 failure DOES arm: a drops out of the eligible head
	// set, so the next two round-robin turns serve from b without dialing a.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b", eg)
	}
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b", eg)
	}
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("a dialed %d times after its generation-2 failure, want 2 — the cooldown did not arm", got)
	}
	assertRequestLine(t, sp, 4, "generation=2", "egress=b", "attempts=1", "fallback=false")
	if got := bCalls.Load(); got != 5 {
		t.Fatalf("b dialed %d times, want 5 (every request ends on b)", got)
	}
}

// TestConnect407ThroughRealForwardProxyFallsBack: egress a is a REAL local
// HTTP forward proxy that refuses CONNECT with 407 and the upstream base is a
// REAL local https server, so the request rides the hand-rolled CONNECT
// boundary (connect.go). The typed refusal must end egress a's attempt after
// EXACTLY ONE dial — never the 502 retry matrix, which would CONNECT 1+3
// times and sleep ~9s — and the executor must fall back to direct egress b,
// which serves the request through a real TLS session.
func TestConnect407ThroughRealForwardProxyFallsBack(t *testing.T) {
	dir := cfgDir(t)

	// Real https fake zen: the CONNECT tunnel's target.
	var zenCalls atomic.Int64
	tlsZen := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		zenCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	}))
	defer tlsZen.Close()
	// The subprocess trusts the fixture origin through SSL_CERT_FILE (Go's
	// Linux root pool reads the file when it first builds roots). The cert
	// must exist before the first request — it is written here, before spawn.
	certPath := filepath.Join(dir, "fake-zen-cert.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsZen.Certificate().Raw})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// Real forward proxy: counts CONNECTs, refuses every one with 407.
	var connects atomic.Int64
	var lastTarget atomic.Value // string — the CONNECT authority
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lastTarget.Store(r.Host)
		connects.Add(1)
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-proxy"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer refusing.Close()

	// b is a direct egress, so the fake zen's own counter is b's counter.

	writeCFG(t, dir, fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b}
routes:
  - {id: r, egress: [a, b]}
`, refusing.URL))
	sp := spawnProxy(t, dir, map[string]string{
		"OFP_UPSTREAM_BASE":  tlsZen.URL,
		"OFP_CONFIG":         "cfg.yaml",
		"OFP_CONFIG_POLL_MS": "50",
		"SSL_CERT_FILE":      certPath,
	})

	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b (the fallback egress)", eg)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECTs seen by the proxy = %d, want exactly 1 — proxy-auth must bypass the 502 retry matrix", got)
	}
	if target, _ := lastTarget.Load().(string); target != strings.TrimPrefix(tlsZen.URL, "https://") {
		t.Fatalf("CONNECT authority = %q, want %q (the https fake zen)", target, strings.TrimPrefix(tlsZen.URL, "https://"))
	}
	// a's single attempt died at the proxy; b served the request for real.
	if got := zenCalls.Load(); got != 1 {
		t.Fatalf("https fake zen calls = %d, want 1 (the direct b attempt)", got)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=b", "attempts=2", "class=success", "fallback=true")
}

// TestHTTPOrigin407IsClientErrorNoFallback: a 407 that arrives as a RESPONSE
// status (http origin behind an http forward proxy — the proxy relays bytes,
// so the 407's owner is unknowable from the wire) is a client-error verdict:
// the client gets the 407 envelope, egress b sees ZERO requests (no
// fallback), a is not retried (407 has no entry in the retry matrix), and a
// is not health-marked (it still serves when the rotation comes back to it).
func TestHTTPOrigin407IsClientErrorNoFallback(t *testing.T) {
	dir := cfgDir(t)

	var aCalls atomic.Int64
	aStatus := &atomic.Int64{}
	aStatus.Store(http.StatusProxyAuthRequired)
	proxyA := statusProxy(&aCalls, aStatus)
	defer proxyA.Close()
	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
	defer proxyB.Close()

	writeCFG(t, dir, fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`+healthYAML, proxyA.URL, proxyB.URL))
	sp := spawnProxy(t, dir, map[string]string{
		"OFP_UPSTREAM_BASE":  "http://upstream.invalid/zen",
		"OFP_CONFIG":         "cfg.yaml",
		"OFP_CONFIG_POLL_MS": "50",
	})

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407 (the status verdict reaches the client verbatim)", resp.StatusCode)
	}
	e := errorEnvelope(t, decodeJSON(t, resp))
	if msg, _ := e["message"].(string); !strings.HasPrefix(msg, "[407]: ") || !strings.Contains(msg, "credentials rejected") {
		t.Fatalf("error message = %q, want the [407]: envelope with the upstream text", msg)
	}
	if got := bCalls.Load(); got != 0 {
		t.Fatalf("b dialed %d times, want 0 — a 407 status is a client error and must never fall back", got)
	}
	if got := aCalls.Load(); got != 1 {
		t.Fatalf("a dialed %d times, want 1 (a 407 status is not retried either)", got)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=a", "attempts=1", "class=client_error", "status=407", "fallback=false")

	// No health mark either: threshold is 1, so any mark would have cooled a
	// — yet it still serves when the round-robin cursor returns to it.
	aStatus.Store(http.StatusOK)
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b (rotation)", eg)
	}
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a — a 407 status must not health-mark the egress", eg)
	}
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("a dialed %d times, want 2", got)
	}
}

// TestTransportReplacementDoesNotInheritCooldown: health state is keyed by
// egress id + transport signature. Generation 1's a is a dead local port —
// the connection refusal marks it (threshold 1) and it cools. Swapping a's
// proxy URL to a working one (same egress id) is a NEW transport identity:
// the next request must be served by a's replacement transport, carrying no
// inherited cooldown.
func TestTransportReplacementDoesNotInheritCooldown(t *testing.T) {
	dir := cfgDir(t)

	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
	defer proxyB.Close()
	var a2Calls atomic.Int64
	proxyA2 := chatProxy(&a2Calls)
	defer proxyA2.Close()
	// The dead port is reserved LAST, after every other listener exists, so
	// nothing in this suite can recycle it for a live listener while the test
	// dials it.
	dead, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	egressYAML := func(aURL string) string {
		return fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, aURL, proxyB.URL)
	}
	writeCFG(t, dir, egressYAML("http://127.0.0.1:"+dead)+healthYAML)
	sp := spawnProxy(t, dir, map[string]string{
		"OFP_UPSTREAM_BASE":  "http://upstream.invalid/zen",
		"OFP_CONFIG":         "cfg.yaml",
		"OFP_CONFIG_POLL_MS": "50",
	})

	// req1: the dead port refuses; the dial failure rides the 502 retry
	// matrix INSIDE the attempt (that is what a real dead proxy looks like),
	// then generation 1's threshold-1 policy cools a and the executor falls
	// back to b.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("req1 X-OFP-Egress = %q, want b", eg)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=b", "attempts=2", "class=success", "fallback=true")

	// req2: a is cooling under its old transport identity.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("req2 X-OFP-Egress = %q, want b (a cooling)", eg)
	}
	assertRequestLine(t, sp, 1, "egress=b", "attempts=1", "fallback=false")

	// Generation 2: same egress id, NEW proxy URL.
	writeCFG(t, dir, egressYAML(proxyA2.URL)+healthYAML)
	waitSwap(t, sp, "config reload: swapped to new config (generation 2")

	// req3: the replacement transport starts clean — a serves.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "a" {
		t.Fatalf("req3 X-OFP-Egress = %q, want a (fresh transport, no inherited cooldown)", eg)
	}
	if got := a2Calls.Load(); got != 1 {
		t.Fatalf("a's replacement proxy dialed %d times, want 1", got)
	}
	assertRequestLine(t, sp, 2, "generation=2", "egress=a", "attempts=1", "fallback=false")
}

// TestPolicyOnlyReloadKeepsHealthState: the mirror of the transport swap — a
// reload that changes ONLY the health settings keeps a's transport signature
// (identical proxy URL), so its health identity — and the cooldown armed
// under generation 1 — must survive into generation 2.
func TestPolicyOnlyReloadKeepsHealthState(t *testing.T) {
	dir := cfgDir(t)

	var aCalls atomic.Int64
	proxyA := fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		aCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer proxyA.Close()
	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
	defer proxyB.Close()

	// Byte-identical egress/routes across both generations; ONLY the health
	// threshold changes (1 → 5).
	egressYAML := fmt.Sprintf(`egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.URL, proxyB.URL)
	writeCFG(t, dir, egressYAML+healthYAML)
	sp := spawnProxy(t, dir, map[string]string{
		"OFP_UPSTREAM_BASE":  "http://upstream.invalid/zen",
		"OFP_CONFIG":         "cfg.yaml",
		"OFP_CONFIG_POLL_MS": "50",
	})

	// req1 arms a's cooldown (a 500, threshold 1); req2 confirms a is out of
	// the head set.
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("req1 X-OFP-Egress = %q, want b", eg)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=b", "attempts=2", "fallback=true")
	if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
		t.Fatalf("req2 X-OFP-Egress = %q, want b (a cooling)", eg)
	}
	assertRequestLine(t, sp, 1, "egress=b", "attempts=1", "fallback=false")

	writeCFG(t, dir, egressYAML+"health:\n  failure_threshold: 5\n  cooldown: 60s\n")
	waitSwap(t, sp, "config reload: swapped to new config (generation 2")

	// Two post-reload requests: still b only — a still cooling. Had the
	// reload cleared the state, the first of them would have round-robinned
	// straight onto a and dialed its (unchanged) proxy again.
	for i := 3; i <= 4; i++ {
		if eg := drainChat(t, sp.post(t, streamBody, nil)); eg != "b" {
			t.Fatalf("req%d X-OFP-Egress = %q, want b — a policy-only reload must keep the armed cooldown", i, eg)
		}
		assertRequestLine(t, sp, i-1, "generation=2", "egress=b", "attempts=1", "fallback=false")
	}
	if got := aCalls.Load(); got != 1 {
		t.Fatalf("a dialed %d times after a policy-only reload, want 1 — the reload cleared its cooldown", got)
	}
	if got := bCalls.Load(); got != 4 {
		t.Fatalf("b dialed %d times, want 4 (every request ends on b)", got)
	}
}
