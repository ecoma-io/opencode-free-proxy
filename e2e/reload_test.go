//go:build e2e

// Snapshot/fallback E2E: a real server subprocess driven through real HTTP
// forward proxies, its service settings pinned by the OCFP_CONFIG document
// already written into the spawn dir (upstream.base) — so
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
	"sync/atomic"
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
	done   chan struct{} // closed once the single Process.Wait goroutine reaps it
}

// reap waits for the spawned process's single Wait goroutine to have reaped
// it. Like the reaper itself it is idempotent — any number of callers may
// block here, and after done closes, further reap calls return immediately.
func (sp *proxySpawn) reap(timeout time.Duration) bool {
	select {
	case <-sp.done:
		return true
	case <-time.After(timeout):
		return false
	}
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
	// The service settings (upstream.base) live in the OCFP_CONFIG
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
	done := make(chan struct{})
	sp := &proxySpawn{
		base:   "http://127.0.0.1:" + port,
		cmd:    cmd,
		out:    out,
		client: &http.Client{Timeout: 30 * time.Second},
		done:   done,
	}
	// Exactly one Process.Wait per spawned process, started here; waitExit
	// (during a shutdown test) and t.Cleanup (after) both block on the same
	// done channel, so a process is never reaped twice.
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		if !sp.reap(3 * time.Second) {
			_ = cmd.Process.Kill()
			sp.reap(3 * time.Second)
		}
	})
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
// would revert to the real https://opencode.ai).
func serviceHead(upstreamURL string) string {
	return fmt.Sprintf("upstream:\n  base: %q\n", upstreamURL)
}

// upstreamBase pins the upstream base in spawn docs; omitting the section
// would revert the base to the real https://opencode.ai.
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
// while a's proxy holds the request's dial open, the config file swaps to
// generation 2 (route c); the in-flight request still falls back to the
// GENERATION-1 b and never dials c; the NEXT request uses c.
func TestReloadRaceKeepsInFlightPlan(t *testing.T) {
	dir := cfgDir(t)

	// a: holds the dial until released, then fails pre-request — the only
	// failure shape that may move the request on to b.
	proxyA := newFailProxy(t, true)
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
  - {id: a, proxy: {type: socks5, url: %q}}
  - {id: b, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a, b]}
`, proxyA.url(), proxyB.URL)
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

	// Wait for the request to reach a and park inside its dial.
	proxyA.waitDialed(t)

	// Swap to generation 2 while a holds the request.
	writeCFG(t, dir, serviceHead("http://upstream.invalid")+fmt.Sprintf(`
egress:
  - {id: c, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [c]}
`, proxyC.URL))
	waitSwap(t, sp, "config reload: swapped to new config (generation 2")

	proxyA.release()
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
	// The completion line is written while the response is still being relayed,
	// so wait for it to reach the captured buffer rather than reading it once
	// (issue #67): the assertion is about log CONTENT, not about copy timing.
	waitForLog(t, sp, "the generation-1 fallback marker", func(log string) bool {
		return strings.Contains(log, "generation=1") && strings.Contains(log, "fallback=true")
	})

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

// TestEgress429IsTerminalAndMarksNoHealth: a provider verdict ENDS the logical
// call. a answers 429 with the health threshold at 1 — the harshest setting —
// so both halves of the contract are observable at once:
//
//   - no fallback, whatever the budget: b is never dialed, and the client gets
//     a's own 429 envelope verbatim;
//   - no health mark: the round-robin comes back to a and a serves. A mark
//     under threshold 1 would have removed a from the eligible heads and this
//     request would have landed on b.
//
// The counterpart — a failure that PROVABLY happened before the request was
// sent DOES move to b and DOES cool a — is pinned end-to-end by
// TestPolicyOnlyReloadKeepsHealthState, TestReloadHealthPolicyPinnedPerGeneration
// and TestConnect407ThroughRealForwardProxyFallsBack.
func TestEgress429IsTerminalAndMarksNoHealth(t *testing.T) {
	dir := cfgDir(t)

	var aCalls atomic.Int64
	aStatus := &atomic.Int64{}
	aStatus.Store(http.StatusTooManyRequests)
	proxyA := statusProxy(&aCalls, aStatus)
	defer proxyA.Close()
	var bCalls atomic.Int64
	proxyB := chatProxy(&bCalls)
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

	// req1: RR head a → 429 → delivered verbatim, one attempt, no b.
	resp := sp.post(t, streamBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 429 (body %s)", resp.StatusCode, b)
	}
	e := errorEnvelope(t, decodeJSON(t, resp))
	_ = resp.Body.Close()
	if msg, _ := e["message"].(string); !strings.HasPrefix(msg, "[429]: ") || !strings.Contains(msg, "credentials rejected") {
		t.Fatalf("error message = %q, want the [429]: envelope with the upstream text", msg)
	}
	if got := bCalls.Load(); got != 0 {
		t.Fatalf("b dialed %d times, want 0 — a provider verdict never moves egress", got)
	}
	if got := aCalls.Load(); got != 1 {
		t.Fatalf("a dialed %d times, want 1 (a verdict is not retried)", got)
	}
	assertRequestLine(t, sp, 0, "generation=1", "egress=a", "attempts=1", "class=upstream_429", "status=429", "fallback=false")

	// req2: RR head b (cursor advanced) → 200 on b.
	consume("b")

	// req3: RR head a again — a must still be eligible. It answers 200 now,
	// so a serves: the 429 cooled nothing.
	aStatus.Store(http.StatusOK)
	consume("a")
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("a dialed %d times, want 2 (req1 429 + req3 200) — a 429 must not health-mark the egress", got)
	}
	assertRequestLine(t, sp, 2, "generation=1", "egress=a", "attempts=1", "class=success", "fallback=false")
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
