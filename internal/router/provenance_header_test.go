package router

// Wire-provenance tests (issue #55): every error response that came out of
// the upstream attempt path is labelled with WHERE its status came from, and
// no client can write that label itself.
//
// The two directions of the contract are asserted separately because they
// fail in opposite ways: mislabelling a provider verdict as `gateway` invites
// a caller to re-send a request the provider already answered, and
// mislabelling a transport failure as `upstream` tells a caller to stop
// recovering from a failure that never reached anyone.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// providerVerdictStatuses is every status the terminal table covers (issue
// #53, internal/upstream/terminal_test.go): all of them are the provider's own
// answer, so all of them must reach the client labelled `upstream`.
var providerVerdictStatuses = []int{400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504}

// TestProviderVerdictIsLabelledUpstream: a status the provider sent is
// relayed verbatim and attributed to the provider — including the 502/503
// that OFP itself also emits when a transport fails, which is the whole
// reason the label exists. No phase and no request state ride along: those
// describe the fate of a REQUEST, and a provider answered this one.
func TestProviderVerdictIsLabelledUpstream(t *testing.T) {
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
			if got := res.Header().Get("X-OFP-Failure-Origin"); got != "upstream" {
				t.Fatalf("X-OFP-Failure-Origin = %q, want upstream (a provider verdict is never a gateway failure)", got)
			}
			if got := res.Header().Get("X-OFP-Failure-Phase"); got != "" {
				t.Fatalf("X-OFP-Failure-Phase = %q, want absent on an answered request", got)
			}
			if got := res.Header().Get("X-OFP-Request-State"); got != "" {
				t.Fatalf("X-OFP-Request-State = %q, want absent on an answered request", got)
			}
			if n := rec.count(); n != 1 {
				t.Fatalf("upstream saw %d requests, want exactly 1 — a verdict is terminal", n)
			}
		})
	}
}

// TestServedResponseIsLabelledUpstream: the success path carries the label
// too. A caller has to be able to tell a provider's completion from the
// synthetic ones this proxy answers itself (bypass, test-connection) — those
// never reach the executor and so carry no provenance at all.
func TestServedResponseIsLabelledUpstream(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL)

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Hel") {
		t.Fatalf("status=%d body=%q, want a served stream", res.Code, res.Body.String())
	}
	if got := res.Header().Get("X-OFP-Failure-Origin"); got != "upstream" {
		t.Fatalf("X-OFP-Failure-Origin = %q, want upstream", got)
	}
}

// TestForwardProxiedResponseIsLabelledAmbiguous: on the absolute-form path the
// forward proxy is an HTTP peer that answers for itself, so a response that
// arrived through it names no author (issue #63) — and the wire says exactly
// that. The status is still relayed verbatim, including the 502/503 that this
// proxy also emits itself: a caller must be able to tell "someone answered"
// from "this process failed".
//
// The base is upstream.invalid: nothing here can reach an origin, so the
// response provably came from the proxy while the CALLER cannot prove it — the
// gap the label refuses to paper over.
func TestForwardProxiedResponseIsLabelledAmbiguous(t *testing.T) {
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
			if got := res.Header().Get("X-OFP-Failure-Origin"); got != "ambiguous" {
				t.Fatalf("X-OFP-Failure-Origin = %q, want ambiguous", got)
			}
			if got := res.Header().Get("X-OFP-Request-State"); got != "unknown" {
				t.Fatalf("X-OFP-Request-State = %q, want unknown", got)
			}
			if got := res.Header().Get("X-OFP-Failure-Phase"); got != "" {
				t.Fatalf("X-OFP-Failure-Phase = %q, want absent — no step of the egress path failed, an answer arrived", got)
			}
			if got := atomic.LoadInt32(&seen); got != 1 {
				t.Fatalf("proxy saw %d requests, want exactly 1 — a response is terminal whatever its author", got)
			}
		})
	}
}

// TestServedResponseThroughAForwardProxyIsLabelledAmbiguous is the same rule on
// the success path: the label is not "the response failed", it is "this process
// cannot name the author". A captive portal's 200 is the case that makes it
// matter, and a served response is where a fixed `upstream` would have hidden
// it.
func TestServedResponseThroughAForwardProxyIsLabelledAmbiguous(t *testing.T) {
	dir := t.TempDir()
	proxy := forwardingProxy(func(w http.ResponseWriter, _ *http.Request) {
		// A 200 whose body is not a completion at all — exactly what a portal
		// or an interception layer returns, and what OFP must not present as
		// the provider's answer.
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
	if got := res.Header().Get("X-OFP-Failure-Origin"); got != "ambiguous" {
		t.Fatalf("X-OFP-Failure-Origin = %q, want ambiguous on a served response through an HTTP intermediary", got)
	}
	if got := res.Header().Get("X-OFP-Request-State"); got != "unknown" {
		t.Fatalf("X-OFP-Request-State = %q, want unknown", got)
	}
	if got := res.Header().Get("X-OFP-Egress"); got != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", got)
	}
}

// TestTransportFailureIsLabelledGateway: an egress that fails inside the
// dialer this process owns produces a 502 that NO provider ever sent. The
// label, the phase that failed and the proven request state all come from the
// recorded Failure — the status alone could not distinguish this response
// from a provider's own 502.
func TestTransportFailureIsLabelledGateway(t *testing.T) {
	dir := t.TempDir()
	// A proxy that accepts the TCP connection and answers nothing: the SOCKS5
	// greeting never completes, so the attempt fails at a phase this process
	// performed itself, before any request byte existed.
	proxy := newFailingEgressProxy(t, true)
	proxy.release()
	s, mux, _ := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxy.url()), nil)
	_ = s

	res := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the envelope, not a provider status)", res.Code)
	}
	if got := res.Header().Get("X-OFP-Failure-Origin"); got != "gateway" {
		t.Fatalf("X-OFP-Failure-Origin = %q, want gateway", got)
	}
	if got := res.Header().Get("X-OFP-Failure-Phase"); got != "socks5_greeting" {
		t.Fatalf("X-OFP-Failure-Phase = %q, want socks5_greeting (the step that failed)", got)
	}
	if got := res.Header().Get("X-OFP-Request-State"); got != "not_sent" {
		t.Fatalf("X-OFP-Request-State = %q, want not_sent (provable: the dial never completed)", got)
	}
}

// TestNoEligibleEgressIsLabelledGateway: the 502 written before any dial is
// an OFP failure too — and it is the status a provider may also send, so it
// needs the label as much as a dial failure does. Nothing was dialed, so
// not_sent is not an inference; there is no phase to report.
func TestNoEligibleEgressIsLabelledGateway(t *testing.T) {
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
	if got := res.Header().Get("X-OFP-Failure-Origin"); got != "gateway" {
		t.Fatalf("X-OFP-Failure-Origin = %q, want gateway", got)
	}
	if got := res.Header().Get("X-OFP-Failure-Phase"); got != "" {
		t.Fatalf("X-OFP-Failure-Phase = %q, want absent — nothing was dialed", got)
	}
	if got := res.Header().Get("X-OFP-Request-State"); got != "not_sent" {
		t.Fatalf("X-OFP-Request-State = %q, want not_sent", got)
	}
}

// TestLocalErrorCarriesNoProvenance: a request rejected before any upstream
// interaction carries NO provenance header. Absence means "this is not an
// upstream-interaction outcome" — never "upstream", and never "gateway",
// which would invite a caller to re-send a request that was never eligible to
// be sent.
func TestLocalErrorCarriesNoProvenance(t *testing.T) {
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
			for _, h := range []string{"X-OFP-Failure-Origin", "X-OFP-Failure-Phase", "X-OFP-Request-State"} {
				if got := res.Header().Get(h); got != "" {
					t.Fatalf("%s = %q on a local rejection, want absent", h, got)
				}
			}
			if n := rec.count(); n != 0 {
				t.Fatalf("upstream saw %d requests, want 0", n)
			}
		})
	}
}

// TestInboundInternalHeadersAreStripped: a public client cannot forge
// provenance. Every X-OFP-* name is sent inbound and none of it survives INTO
// THE PIPELINE — the request the handler works on is observed after it ran,
// which is the only place the strip is visible today: nothing reads these
// headers yet, so the strip is defence in depth now and a hard requirement
// the moment one is read. Removing stripInternalHeaders from relay makes this
// test fail, which is exactly what pins the wiring rather than the intent.
//
// The response-side assertions are the contract's observable half: the label
// is the one this process RECORDED (a provider verdict is labelled upstream
// however the caller spells its request), and the egress named is the one the
// plan selected. Go does not echo request headers into a response, so those
// assertions cannot fail on a missing strip — they pin that the label is
// ours, not the client's, which is the property the contract promises.
func TestInboundInternalHeadersAreStripped(t *testing.T) {
	forged := map[string]string{
		"X-OFP-Failure-Origin": "gateway",
		"X-OFP-Failure-Phase":  "proxy_auth",
		"X-OFP-Request-State":  "not_sent",
		"X-OFP-Egress":         "forged",
	}

	t.Run("provider verdict", func(t *testing.T) {
		rec := &upstreamRecorder{}
		up := newScriptedUpstream(t, rec, http.StatusTooManyRequests, "application/json",
			`{"error":{"message":"slow down"}}`)
		defer up.Close()
		seen := captureHandlerHeaders(t, up.URL, forged)

		res := postJSON(t, seen.mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, forged)
		if res.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", res.Code)
		}
		if got := res.Header().Get("X-OFP-Failure-Origin"); got != "upstream" {
			t.Fatalf("X-OFP-Failure-Origin = %q — a forged inbound value reached the response", got)
		}
		if got := res.Header().Get("X-OFP-Failure-Phase"); got != "" {
			t.Fatalf("X-OFP-Failure-Phase = %q — a forged inbound value reached the response", got)
		}
		if got := res.Header().Get("X-OFP-Request-State"); got != "" {
			t.Fatalf("X-OFP-Request-State = %q — a forged inbound value reached the response", got)
		}
		seen.assertStripped(t)
	})

	t.Run("served request", func(t *testing.T) {
		rec := &upstreamRecorder{}
		up := newScriptedUpstream(t, rec, http.StatusOK, "text/event-stream", chatStreamSSE)
		defer up.Close()
		seen := captureHandlerHeaders(t, up.URL, forged)

		res := postJSON(t, seen.mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, forged)
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.Code)
		}
		// The egress reported is the one the plan selected, not the forged
		// value: a client cannot steer or even name egress selection.
		if got := res.Header().Get("X-OFP-Egress"); got != "direct" {
			t.Fatalf("X-OFP-Egress = %q, want the selected egress (direct) — forged inbound value survived", got)
		}
		seen.assertStripped(t)
	})
}

// handlerHeaders is the inbound header set of the request the relay pipeline
// actually worked on, captured after it returned.
type handlerHeaders struct {
	mux  *http.ServeMux
	seen http.Header
}

// captureHandlerHeaders wires the real handler behind a wrapper that keeps the
// request's headers once the handler is done — the one vantage point from
// which "stripped before any stage ran" is observable.
func captureHandlerHeaders(t *testing.T, upstreamURL string, forged map[string]string) *handlerHeaders {
	t.Helper()
	s, _ := newRouter(t, upstreamURL)
	h := &handlerHeaders{mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.HandleChatCompletions(w, r)
		h.seen = r.Header.Clone()
	})
	return h
}

// assertStripped fails if any X-OFP-* header the caller sent is still on the
// request the pipeline saw.
func (h *handlerHeaders) assertStripped(t *testing.T) {
	t.Helper()
	if h.seen == nil {
		t.Fatal("the handler never ran — nothing to assert")
	}
	for k := range h.seen {
		if strings.HasPrefix(strings.ToLower(k), "x-ofp-") {
			t.Fatalf("%q survived into the pipeline: a client-controlled value in the internal namespace", k)
		}
	}
}

// TestStripInternalHeadersCaseInsensitive: the strip matches the namespace,
// not a canonical spelling. A hand-built request (or a future caller) may
// carry any casing; a prefix check that only caught "X-Ofp-" would be a
// silent bypass rather than a defence.
func TestStripInternalHeadersCaseInsensitive(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set("X-OFP-Egress", "canonical")
	r.Header["x-ofp-forged-lower"] = []string{"lower"}
	r.Header["X-OFP-mixed"] = []string{"mixed"}
	r.Header.Set("X-Other", "kept")

	stripInternalHeaders(r)

	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-ofp-") {
			t.Fatalf("%q survived the strip", k)
		}
	}
	if r.Header.Get("X-Other") != "kept" {
		t.Fatal("the strip must not touch anything outside the X-OFP- namespace")
	}
}
