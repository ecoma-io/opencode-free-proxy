//go:build e2e

// Wire-provenance E2E (issue #55): the recovery contract as a caller sees it,
// through a real server subprocess and real proxies. The unit tests pin the
// mapping; these pin that it survives the whole pipeline — headers set before
// the first downstream write, no stage rewriting them, and the same answer
// whether the failure was a provider verdict or this process's own transport
// failure.

package e2e

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestWireProvenanceIsAttributedEndToEnd: a provider 429 and a dead egress
// both produce an error the client could mistake for the other. The label
// tells them apart — the provider verdict is `upstream` with no phase, the
// transport failure is `gateway` with the step that failed and a proven
// request state.
func TestWireProvenanceIsAttributedEndToEnd(t *testing.T) {
	t.Run("provider verdict", func(t *testing.T) {
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
		if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "upstream" {
			t.Fatalf("X-OFP-Failure-Origin = %q, want upstream", got)
		}
		for _, h := range []string{"X-OFP-Failure-Phase", "X-OFP-Request-State"} {
			if got := resp.Header.Get(h); got != "" {
				t.Fatalf("%s = %q, want absent on an answered request", h, got)
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
// origin the provider's own answer implies.
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
	if got := resp.Header.Get("X-OFP-Failure-Origin"); got != "upstream" {
		t.Fatalf("X-OFP-Failure-Origin = %q — a forged inbound value reached the response", got)
	}
	for _, h := range []string{"X-OFP-Failure-Phase", "X-OFP-Request-State"} {
		if got := resp.Header.Get(h); got != "" {
			t.Fatalf("%s = %q — a forged inbound value reached the response", h, got)
		}
	}
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("body = %q, want the relayed stream", body)
	}
}
