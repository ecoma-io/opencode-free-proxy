//go:build e2e

// Snapshot/fallback E2E: a real server subprocess driven through real HTTP
// forward proxies, its service settings pinned by the OCFP_CONFIG document
// already written into the spawn dir (upstream.base, auth.keys) — so
// per-egress behavior is observable on the wire through the egress proxies.
// Facts asserted come off the wire: X-OFP-Egress headers,
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// proxySpawn is one self-contained server subprocess.
type proxySpawn struct {
	base   string
	cmd    *exec.Cmd
	out    *syncBuf
	client *http.Client
}

// spawnProxy builds and runs the server binary with the given extra env
// (PORT/OCFP_CONFIG appended over a filtered environment) and waits until
// /healthz answers. t.Cleanup terminates it with SIGTERM then SIGKILL.
func spawnProxy(t *testing.T, cfgDir string, extra map[string]string) *proxySpawn {
	t.Helper()
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	out := &syncBuf{}
	cmd := exec.Command(proxyBin)
	// SSL_CERT_FILE is dropped too: tests speaking TLS to a fixture https
	// upstream pass their own root file via extra, and it must be the only
	// source of the subprocess root pool.
	cmd.Env = filteredEnv("OCFP_PORT", "OCFP_CONFIG", "OCFP_CONFIG_POLL_MS", "OCFP_SHUTDOWN_GRACE", "SSL_CERT_FILE")
	cmd.Env = append(cmd.Env, "OCFP_PORT="+port)
	// The service settings (upstream.base, auth.keys) live in the OCFP_CONFIG
	// document already written into the spawn dir; the poll interval keeps
	// hot-reload tests snappy.
	if _, ok := extra["OCFP_CONFIG"]; !ok {
		cmd.Env = append(cmd.Env, "OCFP_CONFIG=cfg.yaml")
	}
	if _, ok := extra["OCFP_CONFIG_POLL_MS"]; !ok {
		cmd.Env = append(cmd.Env, "OCFP_CONFIG_POLL_MS=200")
	}
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Dir = cfgDir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	sp := &proxySpawn{
		base:   "http://127.0.0.1:" + port,
		cmd:    cmd,
		out:    out,
		client: &http.Client{Timeout: 30 * time.Second},
	}
	if err := waitHealthy(sp.base+"/healthz", 15*time.Second); err != nil {
		t.Fatalf("spawn: %v\nlog:\n%s", err, out.String())
	}
	return sp
}

// writeCFG writes the OCFP_CONFIG document into the spawn dir (returned by
// cfgDir for the caller, kept inside the spawn for atomic rewrite by tests).
func writeCFG(t *testing.T, dir, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "cfg.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cfgDir returns a fresh temp dir under which spawnProxy runs, so relative
// config paths are stable per spawn.
func cfgDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// serviceHead is the config-document prefix every spawn doc must carry: the
// upstream base (never dialed here — every egress is proxied, but omitting it
// would revert to the real https://opencode.ai) and the inbound auth key. A
// swapped document that drops auth.keys would silently disable the gate.
func serviceHead(upstreamURL string) string {
	return fmt.Sprintf("upstream:\n  base: %q\nauth:\n  keys:\n    - {name: e2e, key: %s}\n", upstreamURL, testAPIKey)
}

// upstreamBase pins the upstream base in spawn docs that carry NO auth
// section (auth off — the removed empty-OFP_API_KEY default). Omitting the
// section would revert the base to the real https://opencode.ai.
func upstreamBase(upstreamURL string) string {
	return fmt.Sprintf("upstream:\n  base: %q\n", upstreamURL)
}

// post streams a chat completion through the spawn and returns the response
// (body left open for the caller).
func (sp *proxySpawn) post(t *testing.T, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", sp.base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	if mutate != nil {
		mutate(req)
	}
	resp, err := sp.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// waitSwap polls the server log until the store reports a swap to the wanted
// generation (both would swap to 2 in these tests). Fails on timeout.
func waitSwap(t *testing.T, sp *proxySpawn, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sp.out.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q in server log:\n%s", want, sp.out.String())
}

// fwdProxy is a real HTTP forward proxy; the given handler sees every request
// the egress routes through it (absolute-form request line).
func fwdProxy(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

const streamBody = `{"model":"qwen3-coder-free","stream":true}`

// TestReloadRaceKeepsInFlightPlan: request 1 pins generation 1 (route a,b);
// while a's proxy holds the request open, the config file swaps to
// generation 2 (route c); the in-flight request still falls back to the
// GENERATION-1 b and never dials c; the NEXT request uses c.
func TestReloadRaceKeepsInFlightPlan(t *testing.T) {
	dir := cfgDir(t)

	// a: blocks until released, then 500.
	var gateMu sync.Mutex
	gate := make(chan struct{})
	released := false
	proxyA := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		gateMu.Lock()
		released = true
		gateMu.Unlock()
		<-gate
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer proxyA.Close()
	var bCalls int
	var bMu sync.Mutex
	proxyB := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		bMu.Lock()
		bCalls++
		bMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
	defer proxyB.Close()
	var cCalls int
	var cMu sync.Mutex
	proxyC := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		cMu.Lock()
		cCalls++
		cMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
	defer proxyC.Close()
	cfg := serviceHead("http://upstream.invalid") + fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.URL, proxyB.URL)
	writeCFG(t, dir, cfg)
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_CONFIG":         "cfg.yaml",
		"OCFP_CONFIG_POLL_MS": "50",
		"OCFP_SHUTDOWN_GRACE": "2000",
	})

	done := make(chan *http.Response, 1)
	go func() {
		done <- sp.post(t, streamBody, nil)
	}()

	// Wait for the request to reach a and block on the gate.
	deadline := time.Now().Add(2 * time.Second)
	for {
		gateMu.Lock()
		r := released
		gateMu.Unlock()
		if r || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !released {
		t.Fatalf("request never reached proxy a\nlog:\n%s", sp.out.String())
	}

	// Swap to generation 2 while a holds the request.
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: c, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [c]}
`, proxyC.URL))
	waitSwap(t, sp, "config reload: swapped to new config (generation 2")

	close(gate)
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(35 * time.Second):
		// The client's own timeout is 30s; a post() failure inside the
		// goroutine (t.Fatalf) never sends on done — without this deadline
		// the receive would hang the whole suite past go test's timeout.
		t.Fatalf("in-flight request never returned after gate release (log:\n%s)", sp.out.String())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read in-flight body: %v", err)
	}
	if !strings.Contains(string(body), fakeChatID) {
		t.Fatalf("in-flight body missing stream: %s", body)
	}
	cMu.Lock()
	c := cCalls
	cMu.Unlock()
	if c != 0 {
		t.Fatalf("proxy c dialed %d times during an in-flight generation-1 request", c)
	}
	// The logs must attribute the fallback to the request's OWN generation.
	if !strings.Contains(sp.out.String(), "generation=1") ||
		!strings.Contains(sp.out.String(), "fallback=true") {
		t.Fatalf("log lacks generation=1 fallback marker:\n%s", sp.out.String())
	}

	// A fresh request pins the NEW generation.
	resp2 := sp.post(t, streamBody, nil)
	defer resp2.Body.Close()
	if eg := resp2.Header.Get("X-OFP-Egress"); eg != "c" {
		t.Fatalf("fresh X-OFP-Egress = %q, want c (generation 2)", eg)
	}
	cMu.Lock()
	c = cCalls
	cMu.Unlock()
	if c == 0 {
		t.Fatal("fresh request never dialed c")
	}
}

// TestEgress429FallsBackButStaysHealthy: a 429 must fall back to b WITHOUT
// poisoning a — a later request is served by a again. The 500 path poisons.
func TestEgress429FallsBackButStaysHealthy(t *testing.T) {
	dir := cfgDir(t)
	var aMu sync.Mutex
	aCalls := 0
	aStatus := 429
	proxyA := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		aMu.Lock()
		aCalls++
		s := aStatus
		aMu.Unlock()
		w.WriteHeader(s)
	})
	defer proxyA.Close()
	var bCalls int
	var bMu sync.Mutex
	proxyB := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		bMu.Lock()
		bCalls++
		bMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
	defer proxyB.Close()

	cfg := serviceHead("http://upstream.invalid") + fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
health:
  failure_threshold: 1
`, proxyA.URL, proxyB.URL)
	writeCFG(t, dir, cfg)
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_CONFIG":         "cfg.yaml",
		"OCFP_CONFIG_POLL_MS": "50",
	})

	consume := func(want string) {
		t.Helper()
		resp := sp.post(t, streamBody, nil)
		defer resp.Body.Close()
		if eg := resp.Header.Get("X-OFP-Egress"); eg != want {
			t.Fatalf("X-OFP-Egress = %q, want %q", eg, want)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	// req1: RR head a → 429 → fallback b.
	consume("b")
	if bCalls != 1 {
		t.Fatalf("b calls after req1 = %d, want 1", bCalls)
	}
	// req2: RR head b (cursor advanced) → 200 on b.
	consume("b")
	// a recovered (429 never poisons): req3 RR head a again serves 200 —
	// X-OFP-Egress names the SERVING egress, so a must be healthy to answer.
	aMu.Lock()
	aStatus = 200
	aMu.Unlock()
	consume("a")
	if aCalls != 2 {
		t.Fatalf("a calls after req3 = %d, want 2 (req1 429 + req3 200)", aCalls)
	}
	// Switch a to 500: req4 head b, req5 head a → 500 → falls back, and a
	// is now marked unhealthy: req6 head b again (a excluded from heads).
	aMu.Lock()
	aStatus = 500
	aMu.Unlock()
	consume("b") // req4: head b serves
	consume("b") // req5: head a → 500 → fallback b (a now unhealthy)
	// req6: the RR cursor is 5 — even WITHOUT the health exclusion, 5%2=1
	// heads b, so this alone cannot discriminate. req7 is the discriminator:
	// cursor 6 → 6%2=0 = a if a were still eligible. With the exclusion
	// working, b serves and a is NOT called: aCalls stays 3 (req1 429 +
	// req3 200 + req5 500) only if the health filter (and the executor
	// marking 500) actually kept a out of heads.
	consume("b") // req6: head b again
	consume("b") // req7: b again if a excluded → a not called
	aMu.Lock()
	calls := aCalls
	aMu.Unlock()
	if calls != 3 {
		t.Fatalf("a called %d times, want 3 (req1 429 + req3 200 + req5 500) — 5xx did not poison a", calls)
	}
}

// TestStreamingCommitmentNoFallback: after the relay has written downstream,
// a mid-stream upstream death must never fall back — the client already
// committed. a dies after one flushed chunk; b must see zero requests.
func TestStreamingCommitmentNoFallback(t *testing.T) {
	dir := cfgDir(t)
	proxyA := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"delta\":\"partial\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// return closes the conn mid-stream, no [DONE]
	})
	defer proxyA.Close()
	var bCalls int
	var bMu sync.Mutex
	proxyB := fwdProxy(func(w http.ResponseWriter, r *http.Request) {
		bMu.Lock()
		bCalls++
		bMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	})
	defer proxyB.Close()

	writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: a, proxy: {type: http, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.URL, proxyB.URL))
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_CONFIG":         "cfg.yaml",
		"OCFP_CONFIG_POLL_MS": "50",
	})

	resp := sp.post(t, streamBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream started)", resp.StatusCode)
	}
	if eg := resp.Header.Get("X-OFP-Egress"); eg != "a" {
		t.Fatalf("X-OFP-Egress = %q, want a", eg)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(body), "partial") {
		t.Fatalf("body = %q, want the partial delta relayed", body)
	}
	bMu.Lock()
	b := bCalls
	bMu.Unlock()
	if b != 0 {
		t.Fatalf("b calls = %d, want 0 (no fallback after the first byte)", b)
	}
}
