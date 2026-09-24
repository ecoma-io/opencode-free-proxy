package router

// Wire-surface tests: OFP is an OpenAI-compatible API and nothing else.
//
// These assert the ABSENCE of a contract rather than its values. Until issue
// #77 every response that came out of the upstream attempt path carried
// X-OFP-Failure-Origin / X-OFP-Failure-Phase / X-OFP-Request-State — a
// namespaced recovery protocol that told a trusted caller where a status came
// from and whether it might re-send. That protocol is gone: provenance is
// internal (see response_headers.go), and these tests pin that it does not
// reappear on ANY response path, while the one genuinely operational header
// (X-OFP-Egress) is untouched.
//
// The distinction matters because the two are easy to confuse by prefix. They
// are not the same kind of thing: X-OFP-Egress names the configured egress a
// SERVED request went out through — a diagnostic. The removed trio classified
// OFP's own failure handling — provenance. Only three names were ever the
// contract, and all three are asserted absent below on every path that used to
// carry them.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/logging"
)

// wireProvenanceHeaders is the removed contract, in full. A response carrying
// any of these is a regression regardless of its value.
var wireProvenanceHeaders = []string{
	"X-OFP-Failure-Origin",
	"X-OFP-Failure-Phase",
	"X-OFP-Request-State",
}

// assertNoWireProvenance fails if the response carries any header of the
// removed recovery contract. It is deliberately value-blind: a `gateway` and
// an `upstream` are equally forbidden, because the problem is the publication,
// not the reading.
//
// Presence is checked with Values(), not with `Get(name) != ""`: an EMPTY value
// is still a published header, and the old setter guarded exactly one case of
// it (`FailurePhase`'s zero String() is "", so a naively re-added phase setter
// writes the header bare). Get() cannot tell that apart from absence, so a
// value test would wave through the most plausible way for the contract to
// come back.
func assertNoWireProvenance(t *testing.T, h http.Header) {
	t.Helper()
	for _, name := range wireProvenanceHeaders {
		if got := h.Values(name); len(got) != 0 {
			t.Fatalf("%s = %q — the removed vendor recovery contract reached the response (issue #77)", name, got)
		}
	}
}

// providerVerdictStatuses is every status the terminal table covers (issue
// #53, internal/upstream/terminal_test.go): all of them are the provider's own
// answer, relayed verbatim, and all of them are now published bare.
var providerVerdictStatuses = []int{400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504}

// TestProviderVerdictIsPublishedBare: a status the provider sent is relayed
// verbatim with the envelope and NOTHING else — including the 502/503 that OFP
// itself also emits when a transport fails. That collision was the original
// reason the label existed; the answer is now that the caller reads the body
// and its own policy, exactly as it would against any OpenAI-compatible
// upstream.
func TestProviderVerdictIsPublishedBare(t *testing.T) {
	for _, status := range providerVerdictStatuses {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			rec := &upstreamRecorder{}
			up := newScriptedUpstream(t, rec, status, "application/json",
				fmt.Sprintf(`{"error":{"message":"upstream said %d"}}`, status))
			defer up.Close()
			_, mux := newRouter(t, up.URL)

			res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
			if res.Code != status {
				t.Fatalf("status = %d, want %d relayed verbatim", res.Code, status)
			}
			assertNoWireProvenance(t, res.Header())
			// The envelope is still the OpenAI-compatible one, so the response
			// is usable by a client that has never heard of this proxy.
			if ct := res.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want the JSON error envelope", ct)
			}
			if body := res.Body.String(); !strings.Contains(body, `"error"`) {
				t.Fatalf("body = %q, want the OpenAI-compatible error envelope", body)
			}
			if n := rec.count(); n != 1 {
				t.Fatalf("upstream saw %d requests, want exactly 1 — a verdict is terminal", n)
			}
		})
	}
}

// TestServedResponseIsPublishedBare: the success path. A served request still
// carries the egress diagnostic — the one header that is not provenance — and
// no classification of any kind.
func TestServedResponseIsPublishedBare(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Hel") {
		t.Fatalf("status=%d body=%q, want a served stream", res.Code, res.Body.String())
	}
	if got := res.Header().Get(headerEgress); got != "direct" {
		t.Fatalf("X-OFP-Egress = %q, want the selected egress (direct) — the diagnostic is not part of the removed contract", got)
	}
	assertNoWireProvenance(t, res.Header())
}

// TestAmbiguousOriginResponseIsPublishedBare: on the absolute-form path the
// forward proxy is an HTTP peer that answers for itself, so authorship is
// provably unprovable (issue #63) — and that fact stays internal. The status
// is still relayed verbatim, including the 502/503 that this proxy also emits
// itself; the caller reads exactly what an OpenAI-compatible upstream sent.
//
// The base is upstream.invalid: nothing here can reach an origin, so the
// response provably came from the proxy.
func TestAmbiguousOriginResponseIsPublishedBare(t *testing.T) {
	for _, status := range []int{403, 407, 429, 502, 503} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			dir := t.TempDir()
			var seen int32
			proxy := forwardingProxy(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&seen, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"answered by the proxy"}}`)
			})
			defer proxy.Close()

			_, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.URL), nil)

			res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
			if res.Code != status {
				t.Fatalf("status = %d, want %d relayed verbatim", res.Code, status)
			}
			assertNoWireProvenance(t, res.Header())
			if got := atomic.LoadInt32(&seen); got != 1 {
				t.Fatalf("proxy saw %d requests, want exactly 1 — a response is terminal whatever its author", got)
			}
		})
	}
}

// TestAmbiguousOriginServedResponseIsPublishedBare is the same rule on the
// success path, where the removed `ambiguous` label did the most work: a
// captive portal answers 200 with a sign-in page. OFP relays it — it is the
// only answer there is — and says nothing about who wrote it. A caller that
// wants to detect that case must do what it would do against any upstream:
// validate the body it can parse.
func TestAmbiguousOriginServedResponseIsPublishedBare(t *testing.T) {
	dir := t.TempDir()
	proxy := forwardingProxy(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Sign in to the network</title></html>")
	})
	defer proxy.Close()

	_, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.URL), nil)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	assertNoWireProvenance(t, res.Header())
	if got := res.Header().Get(headerEgress); got != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", got)
	}
}

// TestGatewayTransportFailureIsPublishedBare: an egress that fails inside the
// dialer this process owns produces a 502 that NO provider ever sent. The
// caller sees a 502 and an error envelope — the same thing any gateway emits —
// while the classification, the failed phase and the proven request state stay
// in the evidence rows and the completion line where this process can use
// them.
func TestGatewayTransportFailureIsPublishedBare(t *testing.T) {
	dir := t.TempDir()
	// A proxy that accepts the TCP connection and answers nothing: the SOCKS5
	// greeting never completes, so the attempt fails at a phase this process
	// performed itself, before any request byte existed.
	proxy := newFailingEgressProxy(t, true)
	proxy.release()
	_, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.url()), nil)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the envelope, not a provider status)", res.Code)
	}
	assertNoWireProvenance(t, res.Header())
	if body := res.Body.String(); !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %q, want the OpenAI-compatible gateway error", body)
	}
}

// TestNoEligibleEgressIsPublishedBare: the 502 written before any dial. It is
// OFP's own failure and it is the status a provider may also send — the exact
// collision the removed label was invented for. The canonical gateway error
// envelope is the whole of the answer now.
func TestNoEligibleEgressIsPublishedBare(t *testing.T) {
	dir := t.TempDir()
	// The egress is configured but refuses this model, so the route matches
	// and its head set filters to empty — the pre-executor 502.
	_, mux, _ := snapshotRouter(t, dir, `
egress:
  - {id: a, proxy: {type: http, url: "http://127.0.0.1:1"}, models: ["other-*"]}
routes:
  - {id: r, egress: [a]}
`, nil)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.Code)
	}
	assertNoWireProvenance(t, res.Header())
	if body := res.Body.String(); !strings.Contains(body, "No eligible egress") {
		t.Fatalf("body = %q, want the canonical gateway error message", body)
	}
	if ct := res.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want the JSON error envelope", ct)
	}
}

// TestNoEligibleEgressRecordsTheNoDialFactInternally is the other half of
// TestNoEligibleEgressIsPublishedBare: the response says nothing, so the fact
// "this 502 is mine and nothing was dialed" has to live somewhere this process
// can read. It lives on the completion line, with the same canonical no-dial
// record the executor's never-dialed envelope carries (NoDialFailure) — no
// egress, no attempt, the connection_error class. That is also the documented
// shape for a request that dialed nothing (`attempts=0` in the completion
// line, docs/configuration.md "Routing"), and it restores the one-line-per-
// request invariant this path used to break by returning silently.
func TestNoEligibleEgressRecordsTheNoDialFactInternally(t *testing.T) {
	var buf bytes.Buffer
	_, mux := evidenceRouter(t, logging.New(&buf), `
egress:
  - {id: a, proxy: {type: http, url: "http://127.0.0.1:1"}, models: ["other-*"]}
routes:
  - {id: r, egress: [a]}
`)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.Code)
	}
	assertNoWireProvenance(t, res.Header())

	done := eventsWith(decodeEvents(t, &buf), "request completed")
	if len(done) != 1 {
		t.Fatalf("completion lines = %d, want exactly 1 — every request records one outcome", len(done))
	}
	ev := done[0]
	if got := strField(t, ev, "class"); got != "connection_error" {
		t.Fatalf("class = %q, want connection_error (the no-dial record's class)", got)
	}
	if got := strField(t, ev, "egress"); got != "" {
		t.Fatalf("egress = %q, want empty — nothing was selected, let alone dialed", got)
	}
	if got := ev["attempts"]; got != float64(0) {
		t.Fatalf("attempts = %v, want 0 — nothing was dialed", got)
	}
	if got := strField(t, ev, "route"); got != "r" {
		t.Fatalf("route = %q, want r", got)
	}
	// No evidence row: a row describes an upstream interaction, and there was
	// none. The completion line is the whole internal record of this outcome.
	if errs := eventsWith(decodeEvents(t, &buf), "upstream_error"); len(errs) != 0 {
		t.Fatalf("upstream_error events = %d, want 0 — no interaction happened", len(errs))
	}
}

// TestLocalRejectionIsPublishedBare: a request rejected before any upstream
// interaction carries no provenance — it never did (absence meant "not an
// upstream-interaction outcome"), and the removal must not have introduced
// any.
func TestLocalRejectionIsPublishedBare(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL)

	for _, tc := range []struct{ name, body string }{
		{"unreadable body", `not json`},
		{"missing model", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := postJSON(t, mux, "/v1/chat/completions", tc.body, nil)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.Code)
			}
			assertNoWireProvenance(t, res.Header())
			if n := rec.count(); n != 0 {
				t.Fatalf("upstream saw %d requests, want 0", n)
			}
		})
	}
}

// TestResponseStartedFailuresArePublishedBare covers the response_started
// family the forced SSE→JSON path produces (issue #72): a 502 this process
// synthesizes OVER a live 2xx stream it already received. That state is the
// sharpest thing the removed contract used to publish — it is what told a
// caller "do not re-send, this call was already answered" — and it is now
// evidence-only. The client sees a 502 and a message, exactly like any gateway.
func TestResponseStartedFailuresArePublishedBare(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want string
	}{
		{
			// Unparseable: no chunk, no error frame → the generic 502.
			name: "unparseable stream",
			sse:  "data: not-json-1\n\nnot a data line at all\n",
			want: "Invalid SSE response for non-streaming request",
		},
		{
			// The stream carried the provider's own mid-stream error frame.
			name: "error frame",
			sse:  "data: {\"error\":{\"message\":\"quota exceeded mid-stream\"}}\n\n",
			want: "quota exceeded mid-stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.sse)
			}))
			defer upstream.Close()

			_, mux := newRouter(t, upstream.URL)
			rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want %q", rec.Body.String(), tc.want)
			}
			assertNoWireProvenance(t, rec.Header())
		})
	}
}

// TestSSEStreamDeathIsPublishedBareWithoutFallback: an upstream that dies
// mid-stream, after its 200 arrived and after the first byte reached the
// client. The response is committed — status, egress diagnostic, headers — so
// there is nothing left to label and nothing left to recover: the client gets
// the partial stream it already started receiving, and egress b is never
// dialed. This is the commitment boundary (issue #53) with the wire contract
// removed from it.
func TestSSEStreamDeathIsPublishedBareWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	proxyA := forwardingProxy(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"delta\":\"partial\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// return = conn closes mid-stream, no [DONE]
	})
	defer proxyA.Close()
	var bCalls int
	var bMu sync.Mutex
	proxyB := countProxy(&bCalls, &bMu)
	defer proxyB.Close()

	_, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, pxyURL(proxyA), pxyURL(proxyB)), nil)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the stream started)", rec.Code)
	}
	if got := rec.Header().Get(headerEgress); got != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", got)
	}
	assertNoWireProvenance(t, rec.Header())
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("body = %q, want the partial delta relayed", rec.Body.String())
	}
	bMu.Lock()
	b := bCalls
	bMu.Unlock()
	if b != 0 {
		t.Fatalf("b calls = %d, want 0 (no fallback after the first byte)", b)
	}
}

// TestAmbiguousOriginReachesTheRelayPhaseRows: `OriginAmbiguous` survived the
// removal — but not as a wire label, so the test that used to pin it on the
// response is gone with the contract (issue #77). What must still be pinned is
// that the ROUTER plumbs the executor's delivered origin into the phase rows a
// live relay can produce, and does not quietly substitute a constant for it.
// The deleted wire tests were the only router-level proof of that seam.
//
// The base is plain http behind an http forward proxy, so the hop is
// intermediated by construction (internal/upstream hopPathOf): the stream that
// dies here may have been the intermediary's, not the provider's, and the
// record must keep refusing to name the provider.
func TestAmbiguousOriginReachesTheRelayPhaseRows(t *testing.T) {
	proxy := forwardingProxy(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// panic(ErrAbortHandler) kills the connection mid-stream, after the
		// first delta reached the client and with no [DONE]. A plain return
		// would end the chunked response cleanly — a normal end of stream, not
		// the death this test is about.
		panic(http.ErrAbortHandler)
	})
	defer proxy.Close()

	var buf bytes.Buffer
	// evidenceRouter takes the document verbatim (no testUpstreamBasePrefix), so
	// the intermediated base is spelled here.
	doc := fmt.Sprintf(`upstream:
  base: "http://upstream.invalid"
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.URL)
	_, mux := evidenceRouter(t, logging.New(&buf), doc)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Fatalf("the already-delivered delta must reach the client: %q", rec.Body.String())
	}
	assertNoWireProvenance(t, rec.Header())

	rows := eventsWith(decodeEvents(t, &buf), "upstream_error")
	if len(rows) != 1 {
		t.Fatalf("upstream_error events = %d, want 1 (the stream death)", len(rows))
	}
	// `origin` is the field under test: it must be the DELIVERED authorship,
	// not a constant. `request_state` here is the phase row's own taxonomy, not
	// the response record's: a post-header death is `response_started` by
	// construction (the reserved boundary produced in evidence_log.go), and it
	// says a response was delivered, not that the PROVIDER wrote one — which is
	// exactly what `origin` still refuses to claim.
	for key, want := range map[string]string{
		"phase":         "stream",
		"class":         "response_started",
		"origin":        "ambiguous",
		"request_state": "response_started",
	} {
		if got := strField(t, rows[0], key); got != want {
			t.Fatalf("row %s = %q, want %q — the delivered authorship must reach the row (issue #63)", key, got, want)
		}
	}
}

// TestForgedInboundHeadersAreInert: with the contract gone there is no longer
// a namespaced inbound surface to defend, so the blanket strip is gone with
// it. What must remain true — and is asserted here instead — is the property
// the strip only ever approximated: a client cannot influence this process
// through an X-OFP-* header, in EITHER direction.
//
//   - the response carries the egress THIS process selected, never the value
//     the client sent (a request header map is not a response header map, and
//     nothing copies between them);
//   - the upstream request carries no X-OFP-* header at all — the executor
//     forwards a fixed allow-list of client headers (captureDownstream), so
//     the namespace never reaches a provider either;
//   - the recorded evidence is the executor's own classification, never the
//     forged one.
func TestForgedInboundHeadersAreInert(t *testing.T) {
	forged := map[string]string{
		"X-OFP-Failure-Origin": "gateway",
		"X-OFP-Failure-Phase":  "proxy_auth",
		"X-OFP-Request-State":  "not_sent",
		"X-OFP-Egress":         "forged",
		"X-OFP-Intent":         "new-egress",
	}

	t.Run("provider verdict", func(t *testing.T) {
		rec := &upstreamRecorder{}
		up := newScriptedUpstream(t, rec, http.StatusTooManyRequests, "application/json",
			`{"error":{"message":"slow down"}}`)
		defer up.Close()
		_, mux := newRouter(t, up.URL)

		res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, forged)
		if res.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", res.Code)
		}
		assertNoWireProvenance(t, res.Header())
		assertNoOFPEgressUpstream(t, rec)
	})

	t.Run("served request", func(t *testing.T) {
		rec := &upstreamRecorder{}
		up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
		defer up.Close()
		_, mux := newRouter(t, up.URL)

		res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, forged)
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.Code)
		}
		// The egress reported is the one the plan selected, not the forged
		// value: a client cannot steer or even name egress selection.
		if got := res.Header().Get(headerEgress); got != "direct" {
			t.Fatalf("X-OFP-Egress = %q, want the selected egress (direct)", got)
		}
		assertNoWireProvenance(t, res.Header())
		assertNoOFPEgressUpstream(t, rec)
	})
}

// assertNoOFPEgressUpstream fails if any X-OFP-* header reached the upstream
// request. The check is namespace-wide on purpose: the point is that the
// whole namespace is client-facing only in the response direction, so a
// forged value cannot become an upstream header even for a name this process
// does write.
func assertNoOFPEgressUpstream(t *testing.T, rec *upstreamRecorder) {
	t.Helper()
	calls := rec.snapshot()
	if len(calls) == 0 {
		t.Fatal("the upstream saw no request — nothing to assert")
	}
	for _, c := range calls {
		for k, v := range c.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-ofp-") {
				t.Fatalf("%s: %v reached the upstream request — the namespace must not travel upstream", k, v)
			}
		}
	}
}
