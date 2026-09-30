//go:build e2e

// Readiness E2E (issue #87). Two things are pinned here that no unit test can
// show, because both are about SEQUENCE rather than state:
//
//  1. /readyz drops to 503 BEFORE the listener closes, with a measurable gap
//     between them. That gap is the entire feature: a probe that has not
//     noticed yet cannot be told anything by a closed port, it can only learn
//     about the drain as a refused connection.
//  2. A request arriving inside that gap still gets a complete HTTP answer,
//     and /healthz still answers 200. The head start is a window on a live
//     socket, not a closed door — a client that lands there must receive a
//     real response (the drain gate's 503), never a refused connection. That
//     is the difference between the two failure shapes this issue is about.
//
// The existing shutdown tests poll for "503 OR connection refused" because the
// two raced; that race is gone now, and shutdown_test.go asserts the 503
// directly.
package e2e

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runHealthcheckSubcommand runs the entrypoint binary's `healthcheck`
// subcommand against a spawn's port, exactly as the Docker HEALTHCHECK does —
// the same argv, the same OCFP_PORT, the same exit-code contract (0 healthy,
// 1 anything else). Running the real subcommand rather than reimplementing
// the probe is the point: a test that reimplements it cannot catch the probe
// being pointed at the wrong endpoint.
func runHealthcheckSubcommand(t *testing.T, port string) error {
	t.Helper()
	cmd := exec.Command(proxyBin, "healthcheck")
	cmd.Env = filteredEnv("OCFP_PORT")
	cmd.Env = append(cmd.Env, "OCFP_PORT="+port)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	// The subcommand's own log line is safe to surface (status and body
	// length only — it never echoes the body), but its exit is what the
	// assertion is about, so the wrapped error names the code.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("exit %d (stderr: %s)", exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return err
}

// okUpstream is a minimal non-streaming upstream: enough for a chat request to
// be SERVED (which is what the drain-gate sweep needs to distinguish "let
// through" from "refused"). It answers with a minimal JSON body.
func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newStallingUpstream commits the response (headers + one SSE frame) and then
// hangs, so the process holds an in-flight stream across the drain and the
// shutdown grace cannot be skipped. done unblocks the handler for teardown.
func newStallingUpstream(t *testing.T, done chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"delta\":\"chunk0\"}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// readyzAt GETs /readyz and reports (status, body). Status 0 means the
// listener refused the connection — the state this suite exists to prove does
// NOT happen first. A refusal mid-sweep is EXPECTED (it is how the sweep
// learns the listener closed), so it is not a test failure here; the ordering
// assertion is made by the caller from the recorded timestamps.
func readyzAt(t *testing.T, client *http.Client, base string) (int, string) {
	t.Helper()
	resp, err := client.Get(base + "/readyz")
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		t.Fatalf("read /readyz body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// healthzAt reports GET /healthz's status, failing the test if the endpoint
// does not answer at all.
func healthzAt(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Errorf("GET /healthz: %v", err)
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode
}

// chatStatus posts a chat completion and returns its status.
//
// A 200 proves the drain gate let the request through. A 503 is equally
// correct: the drain gate answers 503 from the SAME instant Drain() sets the
// flag that /readyz reports, so a request that reaches the handler inside the
// head start is refused with a real, complete HTTP response — not a refused
// connection. That is the property under test: inside the head start the
// client still gets an answer from a live socket. Anything else (a 502, or a
// transport error) is a failure.
func chatStatus(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	resp, err := client.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen3-coder-free","stream":false}`))
	if err != nil {
		t.Errorf("POST /v1/chat/completions: %v", err)
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode
}

// TestReadyzDropsBeforeTheListenerCloses is the invariant the whole feature
// exists for. The subprocess runs with a short grace so the head start is a
// real, bounded window rather than the production 5s, and the sweep records
// when it first saw each state.
func TestReadyzDropsBeforeTheListenerCloses(t *testing.T) {
	dir := cfgDir(t)
	// Upstream that answers immediately; the sweep only needs the drain gate's
	// verdict, and a served 200 here proves a request was let through.
	up := okUpstream(t)
	writeCFG(t, dir, upstreamBase(up.URL)+"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n")
	sp := spawnProxy(t, dir, map[string]string{
		// 6s grace → head start capped at grace/2 = 3s, and a 3s drain after.
		"OCFP_SHUTDOWN_GRACE": "6000",
	})
	client := &http.Client{Timeout: 2 * time.Second}

	// Ready before any signal.
	if code, body := readyzAt(t, client, sp.base); code != http.StatusOK {
		t.Fatalf("/readyz while serving = %d, want 200", code)
	} else if body != "ok" {
		t.Fatalf("/readyz body = %q, want %q", body, "ok")
	}

	if err := sp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	var (
		firstUnready time.Time
		firstClosed  time.Time
		servedAfter  int
		healthzAfter int
		unreadyCount int
	)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, _ := readyzAt(t, client, sp.base)
		if code == 0 {
			// Connection refused: the listener is gone.
			if firstClosed.IsZero() {
				firstClosed = time.Now()
			}
			break
		}
		if code == http.StatusServiceUnavailable {
			if firstUnready.IsZero() {
				firstUnready = time.Now()
			}
			unreadyCount++
			// Liveness must hold, and a request that arrives the moment
			// readiness drops must still be served: this is the sweep that
			// had already been scheduled against the old view. Only the FIRST
			// unready observation is probed — a chat request commits the
			// relay to an upstream and can outlast the drain budget on its
			// own, which is not what this test is about.
			if unreadyCount == 1 {
				healthzAfter++
				if healthzAt(t, client, sp.base) != http.StatusOK {
					t.Error("/healthz stopped answering while /readyz reported the drain: liveness and readiness are not the same question")
				}
				switch status := chatStatus(t, client, sp.base); status {
				case http.StatusOK, http.StatusServiceUnavailable:
					// The drain gate answers 503 from the same instant the
					// readiness flag flips, so a request inside the head
					// start is REFUSED-with-a-response, not served. What
					// matters is that it got an answer at all.
					servedAfter++
				default:
					t.Errorf("chat inside the head start = %d, want 200 or 503 (a live socket must answer)", status)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if firstUnready.IsZero() {
		t.Fatalf("/readyz never reported the drain (or never closed); log:\n%s", sp.out.String())
	}
	if firstClosed.IsZero() {
		t.Fatalf("the listener never closed within the deadline; log:\n%s", sp.out.String())
	}
	if !firstUnready.Before(firstClosed) {
		t.Fatalf("readiness dropped %v AFTER the listener closed: a load balancer could only learn about the drain from a refused connection",
			firstClosed.Sub(firstUnready))
	}
	// The window must be a WINDOW, not a single lucky poll — and it must be
	// the head start, not noise. With a 3s head start and a 10ms sweep, an
	// unready socket answering more than once is the head start being real.
	if unreadyCount < 2 {
		t.Errorf("/readyz reported the drain %d time(s) before the listener closed: the head start gave no observable window", unreadyCount)
	}
	if healthzAfter == 0 {
		t.Error("/healthz was never probed while /readyz was unready")
	}
	if servedAfter == 0 {
		t.Error("a request inside the head start got no answer: the process closed as fast as it went unready")
	}

	waitExit(t, sp, 15*time.Second)
}

// TestHealthcheckSubcommandFollowsReadiness: the shipped Docker HEALTHCHECK
// reads /readyz, so a draining instance is reported unhealthy by the probe
// while the container's liveness surface still says the process is fine. This
// runs the real entrypoint binary as a subprocess, exactly as Docker does.
func TestHealthcheckSubcommandFollowsReadiness(t *testing.T) {
	dir := cfgDir(t)
	// An upstream that stalls after headers keeps the process alive and
	// in-flight long enough for the healthcheck subprocess to be run against
	// a draining instance.
	stop := make(chan struct{})
	up := newStallingUpstream(t, stop)
	t.Cleanup(func() { close(stop) })
	t.Cleanup(up.Close) // LIFO: the close(stop) above unblocks the handler first
	writeCFG(t, dir, upstreamBase(up.URL)+"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n")
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_SHUTDOWN_GRACE": "6000",
	})

	// Healthcheck against a serving process: exit 0.
	if err := runHealthcheckSubcommand(t, sp.port); err != nil {
		t.Fatalf("healthcheck while serving: %v, want exit 0", err)
	}

	// Commit one in-flight request so the process stays up through the drain
	// (a drain with nothing in flight exits immediately).
	resp, err := sp.client.Post(sp.base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen3-coder-free","stream":true}`))
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	sawChunk := make(chan bool, 1)
	go func() {
		buf := make([]byte, 256)
		var sb strings.Builder
		for i := 0; i < 200; i++ {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), "chunk0") {
					select {
					case sawChunk <- true:
					default:
					}
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	if err := sp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	// Wait for the stream to be in flight, then probe /readyz until it flips.
	select {
	case <-sawChunk:
	case <-time.After(5 * time.Second):
		t.Fatalf("stream never delivered chunk0 (log:\n%s)", sp.out.String())
	}

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := readyzAt(t, client, sp.base); code == http.StatusServiceUnavailable {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Healthcheck against a DRAINING but listening process: exit 1.
	if err := runHealthcheckSubcommand(t, sp.port); err == nil {
		t.Errorf("healthcheck while draining: want exit 1, got 0 (the probe is still reading liveness)")
	}

	waitExit(t, sp, 15*time.Second)
}
