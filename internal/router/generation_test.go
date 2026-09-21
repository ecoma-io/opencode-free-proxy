package router

// Generation-binding tests (issue #24): relay() captures ONE Runtime snapshot
// at arrival and threads it through auth, routing, health, the upstream base,
// fallback and the completion log. A hot reload landing after a request has
// arrived can change nothing about that request — not its admission, not its
// upstream, not its logged generation — only requests that START after the
// swap observe the new generation. The barriers here are deterministic
// (channels): the gated upstream signals `arrived`, the test swaps the
// config, waitGeneration observes the swap, and only then is the gate
// released.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestAuthBindingSurvivesMidRequestReload: a request admitted under
// generation 1 completes under generation 1 even though the reload that
// REVOKES its key lands while the request sits at the upstream. The next
// request under the revoked key is 401; a surviving key and the newly added
// key behave per generation 2.
func TestAuthBindingSurvivesMidRequestReload(t *testing.T) {
	dir := t.TempDir()
	var logsMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	snapshotLogs := func() []string {
		logsMu.Lock()
		defer logsMu.Unlock()
		return append([]string(nil), logs...)
	}

	arrived, gate, rec, up := gatedUpstream(t)
	release := gateRelease(t, gate)

	keys1 := "    - {name: first, key: sk-1}\n" +
		"    - {name: second, key: sk-2}\n"
	mux, store := genRouter(t, dir, testUpstreamBasePrefix+authDoc(keys1, up.URL), logf)
	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// The request is admitted by generation 1 (Bearer sk-1) and parks at the
	// gated upstream.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, mux, "/v1/chat/completions", payload, map[string]string{"Authorization": "Bearer sk-1"})
	}()
	waitArrived(t, arrived)

	// Reload: sk-1/first is REVOKED, third added. The store swaps to
	// generation 2 while the request is in flight.
	writeCfg(t, dir, testUpstreamBasePrefix+authDoc("    - {name: second, key: sk-2}\n    - {name: third, key: sk-3}\n", up.URL))
	waitGeneration(t, store, 2)

	release()
	res := <-done
	if res.Code != http.StatusOK {
		t.Fatalf("in-flight request status = %d, want 200 (admission must survive the key revocation; body=%s)", res.Code, res.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", rec.count())
	}
	// The completion line must carry the request's OWN generation AND the key
	// name that admitted it — one line, one generation, no mixed facts.
	found := false
	for _, l := range snapshotLogs() {
		if strings.Contains(l, "generation=1") {
			found = true
			if !strings.Contains(l, `api_key_name="first"`) {
				t.Fatalf("generation=1 line lacks api_key_name=first: %q", l)
			}
			if !strings.Contains(l, "egress=a") {
				t.Fatalf("generation=1 line names the wrong egress: %q", l)
			}
		}
	}
	if !found {
		t.Fatalf("no generation=1 completion line; logs: %v", snapshotLogs())
	}

	// Post-reload requests follow generation 2: the revoked key 401s, the
	// surviving and the added keys are admitted.
	admit := func(key string, want int) {
		t.Helper()
		r := postJSON(t, mux, "/v1/chat/completions", payload, map[string]string{"Authorization": "Bearer " + key})
		if r.Code != want {
			t.Fatalf("key %q: status = %d, want %d (body=%s)", key, r.Code, want, r.Body.String())
		}
	}
	admit("sk-1", http.StatusUnauthorized)
	admit("sk-2", http.StatusOK)
	admit("sk-3", http.StatusOK)
	admit("sk-9", http.StatusUnauthorized)
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

	// Auth off; the upstream base IS the generation marker here.
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
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postJSON(t, mux, "/v1/chat/completions", payload, nil) }()
	waitArrived(t, arrived)

	// Swap the base to upB while the request is parked at upA.
	writeCfg(t, dir, gen2)
	waitGeneration(t, store, 2)

	release()
	res := <-done
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

// TestAuthFailureDoesNotEchoPresentedKey: the 401 envelope carries the fixed
// message only — the presented credential (valid or not) never appears in the
// response body.
func TestAuthFailureDoesNotEchoPresentedKey(t *testing.T) {
	_, up := authUpstream(t)
	keys := "    - {name: prod, key: sk-real-secret}\n"
	_, mux, _ := snapshotRouter(t, t.TempDir(), authDoc(keys, up.URL), nil)

	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`
	for _, presented := range []string{"sk-wrong-guess", "sk-real-secret "} {
		res := postJSON(t, mux, "/v1/chat/completions", payload, map[string]string{"Authorization": "Bearer " + presented})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: status = %d, want 401", presented, res.Code)
		}
		if strings.Contains(res.Body.String(), presented) || strings.Contains(res.Body.String(), "sk-real-secret") {
			t.Fatalf("401 body echoes a credential: %q", res.Body.String())
		}
	}
}
