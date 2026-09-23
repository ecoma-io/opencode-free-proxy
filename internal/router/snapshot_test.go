package router

// Router-level snapshot tests: one request = one immutable config generation,
// even when a hot reload lands mid-request. The proxies here are REAL servers
// (absolute-URI HTTP forwarding), so the whole dial path participates.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/upstream"
)

// testUpstreamBasePrefix is prepended to every snapshot doc: the pinned
// upstream.base is part of the runtime, and a swap doc that omits `upstream`
// would revert the base to the REAL default (https://opencode.ai) — proxied
// requests would start CONNECT+TLS against a live host from unit tests.
const testUpstreamBasePrefix = "upstream:\n  base: \"http://upstream.invalid\"\n"

// writeCfg writes a config document into a temp dir (store_test's pattern,
// reused here because the store is what the router snapshots).
func writeCfg(t *testing.T, dir, doc string) string {
	t.Helper()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// waitGeneration polls the store until the snapshot generation advances past
// the given value (or fails the test).
func waitGeneration(t *testing.T, s *config.Store, want uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.Get().Generation < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Get().Generation != want {
		t.Fatalf("generation = %d, want %d", s.Get().Generation, want)
	}
}

// snapshotRouter wires a Server on a live polling store, exactly like
// cmd/server/main.go: the runtime (including upstream.base) comes from the
// config document that the store polls; the dummy upstream base pinned in
// every doc (testUpstreamBasePrefix) is NEVER dialed directly — every egress
// reaches the mesh through its configured proxies.
func snapshotRouter(t *testing.T, dir, cfgDoc string, logf func(string, ...any)) (*Server, *http.ServeMux, *config.Store) {
	t.Helper()
	doc := testUpstreamBasePrefix + cfgDoc
	p := writeCfg(t, dir, doc)
	store, err := config.NewStore(p, 20*time.Millisecond, logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	s := NewServer(
		store,
		identity.NewUserAgentCache(),
		upstream.NewClient(), // direct client: unused — every egress is proxied
		logf,
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	return s, mux, store
}

// forwardingProxy is a real HTTP forward proxy (absolute-URI). Its handler
// sees every request the egress makes through it.
func forwardingProxy(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// countProxy counts forwarded requests and answers with chatStreamSSE.
func countProxy(counter *int, mu *sync.Mutex) *httptest.Server {
	return forwardingProxy(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*counter++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatStreamSSE)
	})
}

// pxyURL converts a httptest URL to a config proxy URL (both are http://127.0.0.1:port).
func pxyURL(s *httptest.Server) string {
	return s.URL
}

// failingEgressProxy is a TCP endpoint that speaks no SOCKS5: a dial that
// lands here never gets a greeting reply, so the attempt fails INSIDE the
// dialer this package owns (socks5.go's negotiate step) — the one failure
// shape the recovery split lets move to another egress. It is the router-level
// analogue of the upstream suite's deadEgress, and the reason the reload /
// health-pin races below can still drive a fallback now that a provider
// verdict (4xx/5xx) no longer can.
//
// It also gives those races their ordering primitive: `dialed` fires the
// instant a dial arrives, so a test can hold a request inside egress a, swap
// the config, and only then release the request into a provably pre-request
// failure.
//
// The rest of the config value is deliberate:
//
//   - socks5h:// (remote resolve) so the dial reaches the proxy before any
//     name lookup that could fail for an unrelated reason: the upstream base
//     is a .invalid host on purpose (testUpstreamBasePrefix).
//   - hold = true parks each accepted conn until Release; hold = false hangs
//     up immediately, which is the "always fails fast" shape the health-policy
//     tests want on every dial.
//   - dials counts accepted connections, i.e. egress dials — the failure never
//     produces an HTTP request anywhere, so there is nothing else to count.
type failingEgressProxy struct {
	ln       net.Listener
	dialed   chan struct{} // closed by the first accepted dial
	dialedOf sync.Once
	hold     bool

	mu    sync.Mutex
	conns []net.Conn
	dials int
}

func newFailingEgressProxy(t *testing.T, hold bool) *failingEgressProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &failingEgressProxy{ln: ln, dialed: make(chan struct{}), hold: hold}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.dialedOf.Do(func() { close(p.dialed) })
			p.mu.Lock()
			p.dials++
			hold := p.hold
			if hold {
				p.conns = append(p.conns, c)
			}
			p.mu.Unlock()
			if !hold {
				_ = c.Close()
			}
		}
	}()
	t.Cleanup(p.Close)
	return p
}

// url is the config proxy URL an egress points at to fail pre-request.
func (p *failingEgressProxy) url() string { return "socks5h://" + p.ln.Addr().String() }

// waitDialed blocks until a dial has reached the proxy.
func (p *failingEgressProxy) waitDialed(t *testing.T) {
	t.Helper()
	select {
	case <-p.dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("no dial reached the failing egress proxy")
	}
}

// release hangs up every held connection and stops holding: the parked dial
// fails pre-request now, and so does every dial after it. One-shot by design —
// a released gate must never park a later attempt.
func (p *failingEgressProxy) release() {
	p.mu.Lock()
	p.hold = false
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}

func (p *failingEgressProxy) dialCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials
}

func (p *failingEgressProxy) Close() {
	p.release()
	_ = p.ln.Close()
}

// TestReloadDoesNotAffectActiveRequest is the hot-reload race proof: request
// 1 pins snapshot generation 1 (route [a, b]); while a holds the dial open,
// the config file swaps to generation 2 (route [c]); the in-flight request
// keeps its generation-1 plan, fails a pre-request, falls back to b, and never
// dials c.
func TestReloadDoesNotAffectActiveRequest(t *testing.T) {
	dir := t.TempDir()
	var logsMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	// a: holds the dial until released, then fails PRE-REQUEST (the SOCKS5
	// greeting never completes). Only such a failure may move egress.
	proxyA := newFailingEgressProxy(t, true)
	var bCalls int
	var bMu sync.Mutex
	proxyB := countProxy(&bCalls, &bMu)
	defer proxyB.Close()
	var cCalls int
	var cMu sync.Mutex
	proxyC := countProxy(&cCalls, &cMu)
	defer proxyC.Close()

	s, mux, store := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)), logf)
	_ = s

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	}()

	// Let the request reach proxyA and park inside its dial.
	proxyA.waitDialed(t)

	// Swap the config to generation 2 (route [c] only) while a is blocked.
	writeCfg(t, dir, testUpstreamBasePrefix+fmt.Sprintf(`
egress:
  - {id: c, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [c]}
`, pxyURL(proxyC)))
	waitGeneration(t, store, 2)

	// Release a; the executor falls back to the SNAPSHOT's b, not the new c.
	proxyA.release()
	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-OFP-Egress"); got != "b" {
		t.Fatalf("X-OFP-Egress = %q, want b (the generation-1 fallback)", got)
	}
	if strings.Contains(rec.Body.String(), "No eligible") {
		t.Fatalf("body = %s, want the b stream", rec.Body.String())
	}

	// The in-flight request logged its OWN generation (1), not the swapped one.
	logsMu.Lock()
	defer logsMu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "generation=1") && strings.Contains(l, "fallback=true") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no generation=1 fallback log line: %v", logs)
	}
	cMu.Lock()
	c := cCalls
	cMu.Unlock()
	if c != 0 {
		t.Fatalf("proxyC calls = %d, want 0 (an active request never sees a reload)", c)
	}
}

// TestNewRequestUsesReloadedRuntime: the NEXT request after the swap pins
// generation 2 and dials the new egress — reload applies at the request
// boundary, never mid-request.
func TestNewRequestUsesReloadedRuntime(t *testing.T) {
	dir := t.TempDir()
	var logsMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	var aMu sync.Mutex
	aCalls := 0
	proxyA := countProxy(&aCalls, &aMu)
	defer proxyA.Close()
	var cMu sync.Mutex
	cCalls := 0
	proxyC := countProxy(&cCalls, &cMu)
	defer proxyC.Close()

	_, mux, store := snapshotRouter(t, dir, fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, pxyURL(proxyA)), logf)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-OFP-Egress") != "a" {
		t.Fatalf("pre-reload: status=%d egress=%q", rec.Code, rec.Header().Get("X-OFP-Egress"))
	}

	writeCfg(t, dir, testUpstreamBasePrefix+fmt.Sprintf(`
egress:
  - {id: c, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [c]}
`, pxyURL(proxyC)))
	waitGeneration(t, store, 2)

	rec2 := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-OFP-Egress") != "c" {
		t.Fatalf("post-reload: status=%d egress=%q, want c", rec2.Code, rec2.Header().Get("X-OFP-Egress"))
	}
}

// TestOldRequestKeepsOldHealthPolicyAfterReload: the health POLICY is part of
// the pinned snapshot (issue #6 §1). A request in flight under generation 1
// (threshold 1) observes its failure under ITS policy even though the live
// config moved to a loose generation 2 (threshold 100) before the failure
// landed — the cooldown arms, and a later generation-2 request (which could
// never arm it itself: 100 failures needed) must see a cooling.
//
// The failure it observes is a pre-request transport failure: egress-path
// health is exactly what a health mark means now (a provider verdict marks
// nothing), so the policy under test is the one that decides whether an
// egress-path failure is attributed.
func TestOldRequestKeepsOldHealthPolicyAfterReload(t *testing.T) {
	dir := t.TempDir()

	// a: holds the dial until released, then fails pre-request.
	proxyA := newFailingEgressProxy(t, true)
	var bMu sync.Mutex
	bCalls := 0
	proxyB := countProxy(&bCalls, &bMu)
	defer proxyB.Close()

	_, mux, store := snapshotRouter(t, dir, fmt.Sprintf(`
health:
  enabled: true
  failure_threshold: 1
  cooldown: 1m
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)), nil)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	}()
	proxyA.waitDialed(t)

	// Reload to a LOOSE policy (100 failures to arm) while a is blocked.
	writeCfg(t, dir, testUpstreamBasePrefix+fmt.Sprintf(`
health:
  enabled: true
  failure_threshold: 100
  cooldown: 1m
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)))
	waitGeneration(t, store, 2)

	// Release a; the in-flight (generation-1) request observes its failure
	// under the PINNED threshold-1 policy and arms the cooldown.
	proxyA.release()
	rec := <-done
	if rec.Code != http.StatusOK || rec.Header().Get("X-OFP-Egress") != "b" {
		t.Fatalf("req1: status=%d egress=%q", rec.Code, rec.Header().Get("X-OFP-Egress"))
	}
	if calls := proxyA.dialCount(); calls != 1 {
		t.Fatalf("a dials = %d, want 1", calls)
	}

	// A generation-2 request must see the gen1-armed cooldown: b serves and a
	// is never dialed again. (Had the observation used the live threshold of
	// 100, a would still be eligible and this request would dial it.)
	rec2 := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-OFP-Egress") != "b" {
		t.Fatalf("req2: status=%d egress=%q, want b (a cooling under the gen1-armed state)", rec2.Code, rec2.Header().Get("X-OFP-Egress"))
	}
	if calls := proxyA.dialCount(); calls != 1 {
		t.Fatalf("a dials after req2 = %d, want 1 (the gen1-pinned observation armed the cooldown)", calls)
	}
}

// TestNewRequestUsesNewHealthPolicyAfterReload: the mirror — requests that
// START under a generation use THAT generation's policy. Under a disabled
// policy a's failures never mark (it is re-dialed every time); after the
// reload enables threshold 1, the next failure arms and a stops being dialed.
func TestNewRequestUsesNewHealthPolicyAfterReload(t *testing.T) {
	dir := t.TempDir()
	// a fails on every dial, pre-request — the failure shape egress-path
	// health is about.
	proxyA := newFailingEgressProxy(t, false)
	var bMu sync.Mutex
	bCalls := 0
	proxyB := countProxy(&bCalls, &bMu)
	defer proxyB.Close()

	_, mux, store := snapshotRouter(t, dir, fmt.Sprintf(`
health:
  enabled: false
  failure_threshold: 1
  cooldown: 1m
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)), nil)

	// Under the disabled policy a's failures must never mark it: keep making
	// requests (rotation order is the scheduler's business) until a has been
	// dialed a SECOND time — the direct proof that failure #1 marked nothing.
	// If a disabled observation marked health, a would be filtered from the
	// heads and this loop would time out with aDials frozen at 1.
	requestB := func(stage string) {
		t.Helper()
		rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free","stream":true}`, nil)
		if rec.Code != http.StatusOK || rec.Header().Get("X-OFP-Egress") != "b" {
			t.Fatalf("%s: status=%d egress=%q, want b", stage, rec.Code, rec.Header().Get("X-OFP-Egress"))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for proxyA.dialCount() < 2 {
		requestB("disabled-policy phase")
		if time.Now().After(deadline) {
			t.Fatalf("a was never re-dialed under the disabled policy (dials=%d) — a disabled observation must not mark", proxyA.dialCount())
		}
	}

	// Reload: health ON with threshold 1.
	writeCfg(t, dir, testUpstreamBasePrefix+fmt.Sprintf(`
health:
  enabled: true
  failure_threshold: 1
  cooldown: 1m
egress:
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), pxyURL(proxyB)))
	waitGeneration(t, store, 2)

	// Generation-2 requests: the FIRST failure a suffers under the new
	// policy must mark it — wait for that dial.
	deadline = time.Now().Add(5 * time.Second)
	for proxyA.dialCount() < 3 {
		requestB("enabled-policy phase")
		if time.Now().After(deadline) {
			t.Fatalf("a was never re-dialed under the enabled policy (dials=%d)", proxyA.dialCount())
		}
	}

	// Now a is cooling under the generation-2 policy: several further
	// requests, and a must never be dialed again.
	for i := 0; i < 4; i++ {
		requestB("cooldown phase")
	}
	if calls := proxyA.dialCount(); calls != 3 {
		t.Fatalf("a dials = %d, want 3 (the generation-2 policy armed the cooldown)", calls)
	}
}

// TestStreamingCommitmentNoFallback: once the relay has written downstream,
// an upstream mid-stream death must NEVER trigger a fallback — the client
// already committed. a answers with SSE then dies without a terminal frame;
// b must see zero requests.
func TestStreamingCommitmentNoFallback(t *testing.T) {
	dir := t.TempDir()
	proxyA := forwardingProxy(func(w http.ResponseWriter, r *http.Request) {
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
		t.Fatalf("status = %d, want 200 (stream started)", rec.Code)
	}
	if got := rec.Header().Get("X-OFP-Egress"); got != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", got)
	}
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
