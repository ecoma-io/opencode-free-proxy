//go:build e2e

// Wire-provenance E2E (issue #55): the recovery contract as a caller sees it,
// through a real server subprocess and real proxies. The unit tests pin the
// mapping; these pin that it survives the whole pipeline — headers set before
// the first downstream write, no stage rewriting them, and the same answer
// whether the failure was a provider verdict or this process's own transport
// failure.

package e2e

import (
	"encoding/pem"
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

// TestWireProvenanceIsAttributedEndToEnd: a 429 relayed through a forward
// proxy, a 429 from an origin no intermediary can answer for, and a dead egress
// all produce an error the client could mistake for another. The label tells
// them apart — `ambiguous` (a response exists, but a proxy may have authored
// it), `upstream` (only the origin could have), `gateway` (this process's own
// failure, with the step that failed and a proven request state).
func TestWireProvenanceIsAttributedEndToEnd(t *testing.T) {
	t.Run("forward-proxied http origin", func(t *testing.T) {
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
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 relayed verbatim", resp.StatusCode)
		}
		// The 429 is real, and the proxy that carried it is an HTTP peer that
		// answers for itself — so the provider cannot be named as its author
		// (issue #63). It is relayed all the same: the status cannot be
		// re-litigated once it exists.
		if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "ambiguous" {
			t.Fatalf("X-OFP-Failure-Origin = %q, want ambiguous (an HTTP intermediary carried this hop)", got)
		}
		if got := resp.Header.Get("X-OFP-Request-State"); got != "unknown" {
			t.Fatalf("X-OFP-Request-State = %q, want unknown (the request reached the proxy; what became of it is not observable here)", got)
		}
		if got := resp.Header.Get("X-OFP-Failure-Phase"); got != "" {
			t.Fatalf("X-OFP-Failure-Phase = %q, want absent — no step of the egress path failed", got)
		}
	})

	t.Run("direct origin", func(t *testing.T) {
		dir := cfgDir(t)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"origin rate limited"}}`)
		}))
		defer origin.Close()

		// No intermediary in this path, so the response is the origin's and the
		// label says so — the case that proves the ambiguity rule is a rule
		// about paths and not a blanket demotion of every proxied request.
		writeCFG(t, dir, upstreamBase(origin.URL)+`
egress:
  - {id: direct}
routes:
  - {id: r, egress: [direct]}
`)
		sp := spawnProxy(t, dir, nil)

		resp := sp.post(t, streamBody, nil)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 relayed verbatim", resp.StatusCode)
		}
		if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "upstream" {
			t.Fatalf("X-OFP-Failure-Origin = %q, want upstream", got)
		}
		for _, h := range []string{"X-OFP-Failure-Phase", "X-OFP-Request-State"} {
			if got := resp.Header.Get(h); got != "" {
				t.Fatalf("%s = %q, want absent on a proven provider verdict", h, got)
			}
		}
	})

	t.Run("dead egress", func(t *testing.T) {
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
			t.Fatalf("status = %d (body %s), want 502", resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "gateway" {
			t.Fatalf("X-OFP-Failure-Origin = %q, want gateway", got)
		}
		if got := resp.Header.Get("X-OFP-Failure-Phase"); got != "proxy_connect" {
			t.Fatalf("X-OFP-Failure-Phase = %q, want proxy_connect", got)
		}
		if got := resp.Header.Get("X-OFP-Request-State"); got != "not_sent" {
			t.Fatalf("X-OFP-Request-State = %q, want not_sent", got)
		}
	})
}

// TestForgedProvenanceDoesNotSurviveTheProxy: a public client cannot label
// its own response. The forged values are sent inbound and the proxy answers
// with the provenance IT recorded — the egress it actually selected, and the
// authorship its own attempt path proves.
func TestForgedProvenanceDoesNotSurviveTheProxy(t *testing.T) {
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
		t.Fatalf("X-OFP-Egress = %q, want the selected egress", got)
	}
	// A 200 streamed over the absolute-form hop: the forged `gateway` is gone,
	// and what replaces it is this process's own answer about the path, not the
	// attacker's — the same authorship rule the failure path uses (issue #63),
	// applied to a served response.
	if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "ambiguous" {
		t.Fatalf("X-OFP-Failure-Origin = %q — a forged inbound value reached the response", got)
	}
	if got := resp.Header.Get("X-OFP-Request-State"); got != "unknown" {
		t.Fatalf("X-OFP-Request-State = %q — a forged inbound value reached the response", got)
	}
	if got := resp.Header.Get("X-OFP-Failure-Phase"); got != "" {
		t.Fatalf("X-OFP-Failure-Phase = %q — a forged inbound value reached the response", got)
	}
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("body = %q, want the relayed stream", body)
	}
}

// TestServedResponseThroughAConnectTunnelIsTheProviders: the mirror of the
// test above on the path where the proxy CANNOT answer for the origin — an
// https origin behind the same http proxy, reached through the hand-rolled
// CONNECT boundary. A 200 there is the provider's, and the wire says upstream
// with no request state. Without this case the ambiguity rule would be
// untestable as a rule: it would pass equally well if every proxied response
// were demoted.
func TestServedResponseThroughAConnectTunnelIsTheProviders(t *testing.T) {
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
	if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "upstream" {
		t.Fatalf("X-OFP-Failure-Origin = %q, want upstream (the tunnel is end-to-end: the proxy moves ciphertext)", got)
	}
	for _, h := range []string{"X-OFP-Failure-Phase", "X-OFP-Request-State"} {
		if got := resp.Header.Get(h); got != "" {
			t.Fatalf("%s = %q, want absent on a proven provider response", h, got)
		}
	}
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
