//go:build e2e

// Body-limit and wire-surface hardening E2E. The three body limits are
// separate LAYERS that must each fire independently (AGENTS.md rule 4):
//
//   - server cap: 8 MiB, rejects the request with 400 before routing
//     (internal/router/handler.go).
//   - route match.max_body_bytes: excludes the ROUTE (no route matches →
//     400 "No route matched this request").
//   - egress max_body_bytes: the route matched but the head is ineligible →
//     502 "No eligible egress".
//
// Each sub-test trips exactly ONE layer and asserts the other two stayed
// quiet, so a regression that merged two layers surfaces as the failure
// instead of a green-but-wrong pass. The egress each request rode is read
// off X-OFP-Egress (the served-path diagnostic, internal/router/response_headers).
//
// Also here: the never-dial path publishes no X-OFP-Egress at all, a set
// log-level actually silences the info-level completion line, and a same-host
// redirect is still followed over a real subprocess (GHSA-5472-vw5j-wjvg —
// the cross-host refusal is unit-pinned already).
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// chatProxyCount is a 200-SSE fake upstream that counts what it served.
func chatProxyCount(c *atomic.Int64) *httptest.Server {
	return fwdProxy(func(w http.ResponseWriter, _ *http.Request) {
		c.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
}

// paddedChatBody is a valid chat JSON body whose total byte size grows with n.
// The router parses it (model + messages + stream:false) and sizes the raw
// body against the configured thresholds, all of which sit far below the 8 MiB
// server cap.
func paddedChatBody(n int) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"stream":false}`,
		testedModel, strings.Repeat("x", n))
}

// TestBodyLimitLayersAreIndependent drives one layer per sub-test. The
// fixture is the same shape in all three: a route with body limits and
// egresses behind an http forward proxy. Only the layer that is TRIPPED
// changes, so the three outcomes are attributable to exactly one layer each.
func TestBodyLimitLayersAreIndependent(t *testing.T) {
	t.Run("server cap fires with 400 before any routing", func(t *testing.T) {
		dir := cfgDir(t)
		var up atomic.Int64
		proxy := chatProxyCount(&up)
		defer proxy.Close()
		// No body limits anywhere — only the 8 MiB server cap can stop it.
		writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.URL))
		sp := spawnProxy(t, dir, nil)

		big := []byte(paddedChatBody(9 << 20)) // >8 MiB raw body
		req, err := http.NewRequest("POST", sp.base+"/v1/chat/completions", strings.NewReader(string(big)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := sp.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (server cap)\nbody: %s", resp.StatusCode, b)
		}
		if !strings.Contains(string(b), "unreadable body") {
			t.Fatalf("body = %q, want the server-cap message", b)
		}
		if got := resp.Header.Get("X-OFP-Egress"); got != "" {
			t.Fatalf("X-OFP-Egress = %q, want absent (rejected before routing)", got)
		}
		if up.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0 — the 400 must not dial", up.Load())
		}
	})

	t.Run("route match body bytes skips the route", func(t *testing.T) {
		dir := cfgDir(t)
		var bCalls atomic.Int64
		proxyB := chatProxyCount(&bCalls)
		defer proxyB.Close()
		// Route p has higher priority but match.max_body_bytes: 100; route
		// default matches everything. A ~200-byte body must SKIP p (route-level
		// gate) and land on default — observable via X-OFP-Egress: b.
		writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: p, priority: 100, match: {streaming: false, max_body_bytes: 100}, egress: [b]}
  - {id: default, priority: 0, egress: [b]}
`, proxyB.URL))
		sp := spawnProxy(t, dir, nil)

		body := paddedChatBody(150) // ~220 bytes total — > 100, < 8 MiB
		resp := sp.post(t, body, nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, b)
		}
		if got := resp.Header.Get("X-OFP-Egress"); got != "b" {
			t.Fatalf("X-OFP-Egress = %q, want b (route default)", got)
		}
		if bCalls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", bCalls.Load())
		}
	})

	t.Run("egress max_body_bytes prunes the head", func(t *testing.T) {
		dir := cfgDir(t)
		var bCalls atomic.Int64
		proxyB := chatProxyCount(&bCalls)
		defer proxyB.Close()
		// A catch-all route whose sole egress caps max_body_bytes at 100. The
		// route MATCHES, but the only head is ineligible → 502, no dial.
		writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: b, max_body_bytes: 100, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [b]}
`, proxyB.URL))
		sp := spawnProxy(t, dir, nil)

		resp := sp.post(t, paddedChatBody(150), nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (no eligible egress)\nbody: %s", resp.StatusCode, b)
		}
		if !strings.Contains(string(b), "No eligible egress") {
			t.Fatalf("body = %q, want the no-eligible-egress envelope", b)
		}
		if got := resp.Header.Get("X-OFP-Egress"); got != "" {
			t.Fatalf("X-OFP-Egress = %q, want absent (never dialed)", got)
		}
		if bCalls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0 — the head prune must not dial", bCalls.Load())
		}
	})
}

// TestXOFPEgressAbsentOnNeverDialPath: the pre-executor 502 — route matched,
// head set filtered to empty by an EXCLUSION that is not a body cap (so it is
// a distinct empty-head shape from the egress-layer test above) — publishes
// no X-OFP-Egress and no classification headers at all: the plain OpenAI 502
// envelope and nothing else.
func TestXOFPEgressAbsentOnNeverDialPath(t *testing.T) {
	dir := cfgDir(t)
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: dead, streaming: false}
routes:
  - {id: r, egress: [dead]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, `{"model":"qwen3-coder-free","stream":true}`, nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502\nbody: %s", resp.StatusCode, b)
	}
	assertNoRemovedContract(t, resp)
	if got := resp.Header.Get("X-OFP-Egress"); got != "" {
		t.Fatalf("X-OFP-Egress = %q, want absent on the never-dial path", got)
	}
}

// TestLogLevelErrorSilencesCompletion: log-level is a process-global zerolog
// threshold (AGENTS.md rule 9). Setting it to error on a REAL spawn must hide
// the info-level completion line while the request still completes. Absence
// has no positive anchor in the log (that is the point), so the assertion
// polls a bounded window and fails the moment a request line appears.
func TestLogLevelErrorSilencesCompletion(t *testing.T) {
	dir := cfgDir(t)
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+"log-level: error\n"+
		"egress:\n  - {id: dead, streaming: false}\nroutes:\n  - {id: r, egress: [dead]}\n")
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, `{"model":"qwen3-coder-free","stream":true}`, nil)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want the 502 that the completion line would have logged at info", resp.StatusCode)
	}

	// The completion line for that request is INFO; at error level it must
	// never appear. Poll bounded; the request already completed (we read the
	// response), so a request line appearing at ANY point in the window is a
	// level leak.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if lines := sp.requestLines(); len(lines) != 0 {
			t.Fatalf("log-level: error must hide the info completion line, saw:\n%s", strings.Join(lines, "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSameHostRedirectFollowedE2E: a redirect that stays on the request
// hostname is followed over a real subprocess (GHSA-5472-vw5j-wjvg). The
// redirecting target answers the first hop with a 302 + Location to a second
// same-host listener; the proxy must re-dispatch and deliver the second hop's
// SSE as the completion. The cross-host refusal is already unit-pinned.
func TestSameHostRedirectFollowedE2E(t *testing.T) {
	dir := cfgDir(t)
	var targetCalls atomic.Int64
	target := chatProxyCount(&targetCalls)
	defer target.Close()

	// The forward proxy is ALSO the redirecting server. Hop 1 (the initial
	// dispatch) gets a 302 whose Location is BACK at the same hostname
	// (127.0.0.1, current port as upstream.base) with a marker query — the
	// same-host cross-path shape the allow-list must not break. Hop 2 (the
	// redirected dispatch, same egress proxy) carries the marker and is served.
	var movedCalls atomic.Int64
	moved := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		movedCalls.Add(1)
		if r.URL.RawQuery == "redirected=1" {
			// Hop 2 — the proxy itself is the target now.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE)
			return
		}
		w.Header().Set("Location", target.URL+"/zen/v1/chat/completions?redirected=1")
		w.WriteHeader(http.StatusFound)
	})
	defer moved.Close()

	// upstream.base IS the loopback host the request starts on, so the
	// redirect stays on the SAME hostname (127.0.0.1) — the legal cross-port
	// shape that must keep working after GHSA-5472-vw5j-wjvg.
	writeCFG(t, dir, serviceHead(target.URL)+fmt.Sprintf(`
egress:
  - {id: hop, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [hop]}
`, moved.URL))
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "data:") {
		t.Fatalf("body = %q, want the relayed stream", b)
	}
	if movedCalls.Load() != 2 {
		t.Fatalf("redirect proxy calls = %d, want 2 (initial + followed hop)", movedCalls.Load())
	}
}
