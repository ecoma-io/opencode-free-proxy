//go:build e2e

// The public wire surface, end to end (issue #77): a real server subprocess
// against real fixtures, asserted the way an OpenAI-compatible CLIENT sees it.
//
// This file replaces the wire-provenance E2E that pinned the removed recovery
// contract (X-OFP-Failure-Origin / X-OFP-Failure-Phase / X-OFP-Request-State).
// The contract is gone, so the assertions are now the inverse: every response
// path a caller can reach is a plain OpenAI-compatible answer, and none of the
// three names appears anywhere on the wire. What these tests must still prove
// is that NOTHING ELSE changed — statuses relayed verbatim, the gateway
// envelope intact, the SSE commitment boundary intact, and the one genuinely
// operational header (X-OFP-Egress) still there.

package e2e

import (
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// removedWireContract is the vendor recovery protocol, in full. Any of these
// appearing on a response is the contract coming back.
var removedWireContract = []string{
	"X-OFP-Failure-Origin",
	"X-OFP-Failure-Phase",
	"X-OFP-Request-State",
}

// assertNoRemovedContract fails if the response still publishes provenance.
// Value-blind on purpose: `upstream`, `gateway` and `ambiguous` are all
// equally forbidden, because what was removed is the publication.
//
// Presence is checked with Values(), not `Get(name) != ""`: a header sent with
// an empty value IS published, and the removed setter's one empty-value case
// (`FailurePhase`'s zero String()) is precisely the shape a reintroduction
// would take, so a value-based check would miss it.
func assertNoRemovedContract(t *testing.T, resp *http.Response) {
	t.Helper()
	for _, name := range removedWireContract {
		if got := resp.Header.Values(name); len(got) != 0 {
			t.Fatalf("%s = %q — the removed vendor recovery contract reached the client (issue #77)", name, got)
		}
	}
}

// TestPublicSurfaceIsPlainOpenAICompatible walks the response paths a caller
// can reach through a real subprocess. Provider verdicts (including the 502
// and 503 that this proxy also emits itself), an ambiguous-origin response
// relayed off an intermediated hop, and a gateway transport failure are all
// indistinguishable in SHAPE — status, OpenAI error envelope, nothing else —
// which is the point: a client that has never heard of this proxy is correct
// against every one of them.
func TestPublicSurfaceIsPlainOpenAICompatible(t *testing.T) {
	t.Run("429 answered over an intermediated hop", func(t *testing.T) {
		dir := cfgDir(t)
		var calls atomic.Int64
		var status atomic.Int64
		status.Store(http.StatusTooManyRequests)
		proxy := statusProxy(&calls, &status)
		defer proxy.Close()

		writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, egress: [a]}
`)
		sp := spawnProxy(t, dir, nil)

		resp := sp.post(t, streamBody, nil)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 relayed verbatim", resp.StatusCode)
		}
		assertNoRemovedContract(t, resp)
		if !strings.Contains(string(body), `"error"`) {
			t.Fatalf("body = %q, want the OpenAI-compatible error envelope", body)
		}
	})

	// A verdict from a DIRECT origin: nothing between the client and the
	// provider, so this is the plain upstream-answered case a generic OpenAI
	// client sees. 429 is the status a caller is most likely to have built
	// retry logic around, which is exactly why it must arrive as itself — with
	// the provider's own body and no OFP vocabulary attached.
	t.Run("provider verdicts from a direct origin", func(t *testing.T) {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
				dir := cfgDir(t)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"origin unavailable"}}`)
				}))
				defer origin.Close()

				writeCFG(t, dir, upstreamBase(origin.URL)+`
egress:
  - {id: direct}
routes:
  - {id: r, egress: [direct]}
`)
				sp := spawnProxy(t, dir, nil)

				resp := sp.post(t, streamBody, nil)
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != status {
					t.Fatalf("status = %d, want %d relayed verbatim", resp.StatusCode, status)
				}
				assertNoRemovedContract(t, resp)
				if !strings.Contains(string(body), "origin unavailable") {
					t.Fatalf("body = %q, want the provider's own error body relayed", body)
				}
			})
		}
	})

	t.Run("gateway transport failure", func(t *testing.T) {
		dir := cfgDir(t)
		// A proxy address nothing listens on: the dial fails at the egress
		// path's first step, before any request byte exists.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := ln.Addr().String()
		_ = ln.Close()

		writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "http://`+dead+`"}}
routes:
  - {id: r, egress: [a]}
`)
		sp := spawnProxy(t, dir, nil)

		resp := sp.post(t, streamBody, nil)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d (body %s), want the 502 gateway envelope", resp.StatusCode, body)
		}
		assertNoRemovedContract(t, resp)
		if !strings.Contains(string(body), `"error"`) {
			t.Fatalf("body = %q, want the OpenAI-compatible gateway error", body)
		}
	})
}

// TestNoEligibleEgressIsTheCanonicalGatewayError: a route that matches but
// whose egress refuses the model. An operator debugging this now reads the
// completion line (one per request, `attempts=0`, no egress) instead of a
// header the client had to be trusted with; the CLIENT still gets exactly the
// canonical OpenAI-compatible gateway error, which is what a caller of any
// gateway expects.
func TestNoEligibleEgressIsTheCanonicalGatewayError(t *testing.T) {
	dir := cfgDir(t)
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "http://127.0.0.1:1"}, models: ["other-*"]}
routes:
  - {id: r, egress: [a]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d (body %s), want 502", resp.StatusCode, body)
	}
	assertNoRemovedContract(t, resp)
	if !strings.Contains(string(body), "No eligible egress") {
		t.Fatalf("body = %q, want the canonical gateway error message", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want the JSON error envelope", ct)
	}
	// The internal record: one completion line, naming the route, with no
	// egress and no attempt — the no-dial fact, where the operator can see it
	// instead of on a header the client had to be trusted with.
	log := waitForLog(t, sp, "the no-egress completion line", func(l string) bool {
		return strings.Contains(l, "request completed") && strings.Contains(l, "route=r")
	})
	var line string
	for _, ln := range requestLinesOf(log) {
		if strings.Contains(ln, "route=r") {
			line = ln
		}
	}
	if line == "" {
		t.Fatalf("no completion line for route r in:\n%s", log)
	}
	for _, want := range []string{"attempts=0", "class=connection_error", `"egress":""`} {
		if !strings.Contains(line, want) {
			t.Fatalf("completion line missing %q:\n%s", want, line)
		}
	}
}

// TestServedStreamIsUnchangedApartFromProvenance: the SSE path. A relayed
// stream still arrives as a stream, still carries the egress diagnostic, and
// carries nothing else — the removal touched the classification headers only.
func TestServedStreamIsUnchangedApartFromProvenance(t *testing.T) {
	dir := cfgDir(t)
	var calls atomic.Int64
	proxy := chatProxy(&calls)
	defer proxy.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: real, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, egress: [real]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-OFP-Egress"); got != "real" {
		t.Fatalf("X-OFP-Egress = %q, want the selected egress — the diagnostic is not part of the removed contract", got)
	}
	assertNoRemovedContract(t, resp)
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("body = %q, want the relayed stream", body)
	}
}

// TestServedResponseThroughAConnectTunnelCarriesOnlyTheEgress: the path where
// the proxy CANNOT answer for the origin — an https origin behind the same
// http proxy, reached through the hand-rolled CONNECT boundary. This was the
// case that produced the `upstream` label (the tunnel is end-to-end, so the
// proxy moves ciphertext); the label is gone and the response is bare apart
// from the egress.
func TestServedResponseThroughAConnectTunnelCarriesOnlyTheEgress(t *testing.T) {
	dir := cfgDir(t)

	var zenCalls atomic.Int64
	tlsZen := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		zenCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	}))
	defer tlsZen.Close()
	// The subprocess trusts the fixture origin through SSL_CERT_FILE (Go's
	// Linux root pool reads the file when it first builds roots), written
	// before the first request.
	certPath := filepath.Join(dir, "fake-zen-cert.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsZen.Certificate().Raw})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	var connects atomic.Int64
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects.Add(1)
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(up, client) }()
		go func() { _, _ = io.Copy(client, up) }()
	}))
	defer tunnel.Close()

	writeCFG(t, dir, upstreamBase(tlsZen.URL)+`
egress:
  - {id: a, proxy: {type: http, url: "`+tunnel.URL+`"}}
routes:
  - {id: r, egress: [a]}
`)
	sp := spawnProxy(t, dir, map[string]string{"SSL_CERT_FILE": certPath})

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-OFP-Egress"); got != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", got)
	}
	assertNoRemovedContract(t, resp)
	if connects.Load() == 0 {
		t.Fatal("the request never rode the CONNECT tunnel — the case proved nothing")
	}
	if got := zenCalls.Load(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("body = %q, want the relayed stream", body)
	}
}

// TestClientCannotLabelItsOwnResponse: the namespace is no longer reserved
// inbound, so this is no longer about stripping — it is about the two
// directions of the boundary holding on their own. A client that sends the
// removed names (and a plausible `X-OFP-Egress`) gets a response carrying the
// egress THIS process selected, no classification at all, and a healthy stream.
func TestClientCannotLabelItsOwnResponse(t *testing.T) {
	dir := cfgDir(t)
	var calls atomic.Int64
	proxy := chatProxy(&calls)
	defer proxy.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: real, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, egress: [real]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, func(r *http.Request) {
		r.Header.Set("X-OFP-Egress", "forged")
		r.Header.Set("X-OFP-Failure-Origin", "gateway")
		r.Header.Set("X-OFP-Failure-Phase", "proxy_auth")
		r.Header.Set("X-OFP-Request-State", "not_sent")
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-OFP-Egress"); got != "real" {
		t.Fatalf("X-OFP-Egress = %q, want the selected egress — a forged inbound value reached the response", got)
	}
	assertNoRemovedContract(t, resp)
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("body = %q, want the relayed stream", body)
	}
}

// TestCallerRequestIDJoinsTwoServicesLogs is the black-box half of issue #83:
// a caller-supplied X-Request-Id must join this process's lines to the
// front-end service's, on BOTH the completion line and the evidence event, and
// without disturbing anything else. The value is LOG-ONLY — the response is
// byte-identical to the same request without the header, and no new header
// appears — so a client learns nothing from it and this process still exposes
// no protocol of its own.
func TestCallerRequestIDJoinsTwoServicesLogs(t *testing.T) {
	dir := cfgDir(t)
	var aCalls atomic.Int64
	aStatus := &atomic.Int64{}
	aStatus.Store(http.StatusTooManyRequests)
	proxyA := statusProxy(&aCalls, aStatus)
	defer proxyA.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, proxyA.URL))
	sp := spawnProxy(t, dir, nil)

	const callerID = "01HQZX9K3M7NPQRSTVWXY2EFGH.0001"
	resp := sp.post(t, streamBody, func(r *http.Request) {
		r.Header.Set("X-Request-Id", callerID)
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d (body %s), want the provider's 429 relayed", resp.StatusCode, body)
	}

	// The response is untouched: the id is a log field, never an echo, and the
	// removed contract plus the expose-headers escape stay absent.
	assertNoRemovedContract(t, resp)
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want absent — the id must not become browser-readable", got)
	}
	for name := range resp.Header {
		if strings.EqualFold(name, "X-Request-Id") {
			t.Fatalf("the response echoes X-Request-Id — this process writes nothing back (issue #77)")
		}
	}

	// The join itself: the evidence event and the completion line BOTH name the
	// caller's id, and both still name this process's own id.
	events := parseEvents(waitForLog(t, sp, "the caller's id on the evidence event",
		func(log string) bool { return upstreamErrors(log) == 1 }))
	errs := withMsg(events, "upstream_error")
	if len(errs) != 1 {
		t.Fatalf("upstream_error events = %d, want 1", len(errs))
	}
	done := withMsg(events, "request completed")
	if len(done) != 1 {
		t.Fatalf("completion lines = %d, want 1", len(done))
	}
	for _, ev := range []map[string]any{errs[0], done[0]} {
		if got := evStr(t, ev, "caller_request_id"); got != callerID {
			t.Fatalf("%q: caller_request_id = %q, want %q verbatim", ev["msg"], got, callerID)
		}
	}
	// The local identity is independent and still derived locally: the caller's
	// value never became request_id, and attempt_id is still request_id/N.
	if evStr(t, errs[0], "caller_request_id") == evStr(t, errs[0], "request_id") {
		t.Fatal("the caller id was promoted into request_id")
	}
	if got, want := evStr(t, errs[0], "attempt_id"), evStr(t, errs[0], "request_id")+"/1"; got != want {
		t.Fatalf("attempt_id = %q, want %s/1", got, want)
	}
	if evStr(t, done[0], "request_id") != evStr(t, errs[0], "request_id") {
		t.Fatal("the completion line lost the local request_id correlation")
	}
	// The frozen Msgf text is unchanged — the id rides the JSON fields only.
	line := requestLinesOf(sp.out.String())[0]
	if !strings.Contains(line, `request completed generation=1 route=r egress=a attempts=1 class=upstream_429`) {
		t.Fatalf("the completion line's frozen text changed:\n%s", line)
	}
	if !strings.Contains(line, `"caller_request_id":"`+callerID+`"`) {
		t.Fatalf("completion line missing the caller id as a JSON field:\n%s", line)
	}
}

// TestCallerRequestIDRejectedValueIsDroppedNotFolded is the quiet-direction
// guard, black-box: a value outside the allow-list must leave the field
// ABSENT — never shortened, trimmed or escaped into something an operator
// would then grep for and fail to find. The request still succeeds, because an
// unparseable correlation header is not a routing fact.
//
// The CR/LF/NUL cases are NOT here, and that is a fact about the wire rather
// than an omission: Go's http.Client refuses to transmit a header value
// containing them ("net/http: invalid header field value"), so no conforming
// client can put one on the wire in the first place. Those cases are still
// covered where they can actually be constructed — the unit table in
// caller_id_test.go, which calls the validator directly.
func TestCallerRequestIDRejectedValueIsDroppedNotFolded(t *testing.T) {
	dir := cfgDir(t)
	var calls atomic.Int64
	proxy := chatProxy(&calls)
	defer proxy.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, egress: [a]}
`)
	sp := spawnProxy(t, dir, nil)

	// A trailing space cannot be tested as a REJECTION here: HTTP trims the
	// optional whitespace around a field value, so the server receives
	// "trim-me" — an in-charset value that is not the one the client wrote.
	// Reject-on-arrival is exactly right, and is asserted by
	// TestCallerRequestIDHTTPTrailingSpaceArrivesTrimmed below.
	rejected := []string{
		strings.Repeat("a", 129), // one over the bound
		"has space",
		"a\tb",
		"quote\"here",
		"café",
		"a/b",
	}
	for _, bad := range rejected {
		resp := sp.post(t, streamBody, func(r *http.Request) { r.Header.Set("X-Request-Id", bad) })
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%q: status = %d, want 200 — a malformed correlation header must not fail the request", bad, resp.StatusCode)
		}
	}

	lines := waitForLog(t, sp, "one completion line per rejected value",
		func(log string) bool { return len(requestLinesOf(log)) >= len(rejected) })
	for _, ln := range requestLinesOf(lines) {
		if strings.Contains(ln, "caller_request_id") {
			t.Fatalf("a rejected caller id reached the log — it must be dropped, never folded:\n%s", ln)
		}
		if !strings.Contains(ln, `"request_id"`) {
			t.Fatalf("the local request_id must survive a dropped caller id:\n%s", ln)
		}
	}
}

// TestPreRoutingRejectionIsVisibleInTheLog: a client-visible 400 that used to
// produce NO line in this process at all. Joining the front-end service's logs
// against this one used to dead-end here; now the request names itself, and the
// caller's id rides along when one was sent.
func TestPreRoutingRejectionIsVisibleInTheLog(t *testing.T) {
	dir := cfgDir(t)
	var calls atomic.Int64
	proxy := chatProxy(&calls)
	defer proxy.Close()

	// The route matches one model; anything else is rejected before routing.
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, match: {models: ["qwen3-coder-free"]}, egress: [a]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, `{"model":"no-such-model","stream":true}`, func(r *http.Request) {
		r.Header.Set("X-Request-Id", "caller-no-route")
	})
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (body %s), want 400", resp.StatusCode, b)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream saw %d requests, want 0", calls.Load())
	}

	log := waitForLog(t, sp, "the pre-routing rejection's line",
		func(l string) bool { return strings.Contains(l, "caller-no-route") })
	lines := requestLinesOf(log)
	if len(lines) != 1 {
		t.Fatalf("completion lines = %d, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{`"status":400`, `"attempts":0`, `"egress":""`, `"model":"no-such-model"`} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("pre-routing line missing %q:\n%s", want, lines[0])
		}
	}
}

// TestCallerRequestIDHTTPTrailingSpaceArrivesTrimmed pins the one place where
// "verbatim" has a precise meaning: what the field carries is what ARRIVED, not
// what the client wrote. HTTP trims the optional whitespace around a field
// value, so "trim-me " reaches the server as "trim-me" — in-charset, accepted,
// and logged as it came. The log field is a record of this process's input, not
// a reconstruction of the sender's, and it is never re-padded to match an
// original this process never saw.
func TestCallerRequestIDHTTPTrailingSpaceArrivesTrimmed(t *testing.T) {
	dir := cfgDir(t)
	var calls atomic.Int64
	proxy := chatProxy(&calls)
	defer proxy.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+`
egress:
  - {id: a, proxy: {type: http, url: "`+proxy.URL+`"}}
routes:
  - {id: r, egress: [a]}
`)
	sp := spawnProxy(t, dir, nil)

	resp := sp.post(t, streamBody, func(r *http.Request) { r.Header.Set("X-Request-Id", "trim-me ") })
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	log := waitForLog(t, sp, "the arrival-trimmed caller id",
		func(l string) bool { return strings.Contains(l, "caller_request_id") })
	lines := requestLinesOf(log)
	if len(lines) != 1 {
		t.Fatalf("completion lines = %d, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"caller_request_id":"trim-me"`) {
		t.Fatalf("want the value that ARRIVED, unpadded:\n%s", lines[0])
	}
	if strings.Contains(lines[0], `"caller_request_id":"trim-me "`) {
		t.Fatalf("the field re-padded a value this process never received:\n%s", lines[0])
	}
}
