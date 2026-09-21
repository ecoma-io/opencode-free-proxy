package router

// Generation-binding tests (issue #24): relay() captures ONE Runtime snapshot
// at arrival and threads it through routing, health, the upstream base,
// fallback and the completion log. A hot reload landing after a request has
// arrived can change nothing about that request — not its upstream, not its
// logged generation — only requests that START after the swap observe the new
// generation. The barriers here are deterministic (channels): the gated
// upstream signals `arrived`, the test swaps the config, waitGeneration
// observes the swap, and only then is the gate released.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/upstream"
)

// gatedUpstream answers with chatStreamSSE but holds every request until the
// gate channel is closed; each arrival is signaled on the returned channel so
// a test can barrier on "the request reached the upstream" without sleeping.
func gatedUpstream(t *testing.T) (arrived <-chan struct{}, gate chan struct{}, rec *upstreamRecorder, srv *httptest.Server) {
	t.Helper()
	rec = &upstreamRecorder{}
	gate = make(chan struct{})
	arrivedCh := make(chan struct{}, 8)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, upstreamCall{Path: r.URL.Path, Body: raw})
		rec.mu.Unlock()
		arrivedCh <- struct{}{}
		<-gate
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, chatStreamSSE)
	}))
	t.Cleanup(srv.Close)
	return arrivedCh, gate, rec, srv
}

// genRouter wires a Server on a live polling store over a FULL config doc
// (unlike snapshotRouter it does not pin a dummy upstream.base — the
// generation-binding tests swap the real base between generations). The
// caller owns dir and rewrites the doc in it to trigger reloads.
func genRouter(t *testing.T, dir, doc string, logf func(string, ...any)) (*http.ServeMux, *config.Store) {
	t.Helper()
	p := writeCfg(t, dir, doc)
	store, err := config.NewStore(p, 20*time.Millisecond, logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	s := NewServer(
		store,
		identity.NewUserAgentCache(),
		upstream.NewClient(), // direct client: every egress here is direct or proxied
		logf,
		func(time.Duration) {}, // no-op sleep: retry matrices run instantly
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	return mux, store
}

// waitArrived barriers on the request reaching the upstream (bounded, so a
// broken pipeline fails the test instead of hanging it).
func waitArrived(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the upstream")
	}
}

// gateRelease returns an idempotent release for a gated upstream and wires
// it into t.Cleanup: httptest.Server.Close waits for outstanding handlers,
// so a handler still parked on the gate when the test fails would hang
// cleanup forever.
func gateRelease(t *testing.T, gate chan struct{}) func() {
	t.Helper()
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	return release
}

// TestUpstreamBaseBindingSurvivesMidRequestReload: the upstream base rides
// the arrival snapshot. An in-flight request keeps dialing generation 1's
// base across a reload that points upstream.base elsewhere; only the NEXT
// request dials the new base.
func TestUpstreamBaseBindingSurvivesMidRequestReload(t *testing.T) {
	dir := t.TempDir()
	arrived, gate, recA, upA := gatedUpstream(t)
	release := gateRelease(t, gate)
	recB := &upstreamRecorder{}
	upB := newScriptedUpstream(t, recB, http.StatusOK, "text/event-stream", chatStreamSSE)
	defer upB.Close()

	// The upstream base IS the generation marker here.
	gen1 := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`, upA.URL)
	gen2 := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`, upB.URL)
	mux, store := genRouter(t, dir, gen1, nil)

	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`
	// The response travels through a variable + closed channel, not a
	// channel send: if postJSON ever t.Fatal-exits this goroutine
	// (runtime.Goexit), the deferred close still releases the waiter below
	// instead of leaving it blocked forever.
	var res *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		res = postJSON(t, mux, "/v1/chat/completions", payload, nil)
	}()
	waitArrived(t, arrived)

	// Swap the base to upB while the request is parked at upA.
	writeCfg(t, dir, gen2)
	waitGeneration(t, store, 2)

	release()
	<-done
	if res == nil {
		t.Fatal("request goroutine ended without a response — see the failure it recorded above")
	}
	if res.Code != http.StatusOK {
		t.Fatalf("in-flight request status = %d, want 200 (body=%s)", res.Code, res.Body.String())
	}
	if got := res.Header().Get("X-OFP-Egress"); got != "direct" {
		t.Fatalf("X-OFP-Egress = %q, want direct", got)
	}
	if recA.count() != 1 {
		t.Fatalf("generation-1 upstream calls = %d, want 1 (the in-flight request must keep its base)", recA.count())
	}
	if recB.count() != 0 {
		t.Fatalf("generation-2 upstream saw %d calls during the in-flight request, want 0", recB.count())
	}

	// The NEXT request dials the new base.
	res2 := postJSON(t, mux, "/v1/chat/completions", payload, nil)
	if res2.Code != http.StatusOK {
		t.Fatalf("post-reload status = %d, want 200 (body=%s)", res2.Code, res2.Body.String())
	}
	if recB.count() != 1 {
		t.Fatalf("generation-2 upstream calls after a fresh request = %d, want 1", recB.count())
	}
	if recA.count() != 1 {
		t.Fatalf("generation-1 upstream calls after reload = %d, want 1", recA.count())
	}
}
