//go:build e2e

// Graceful-shutdown E2E: SIGTERM must stop new work with the 503 drain gate
// while in-flight streams finish under OCFP_SHUTDOWN_GRACE; past the grace the
// remaining connections are force-closed. The 503 is asserted DETERMINISTICALLY
// here because the readiness head start (issue #87) guarantees the listener is
// still open for a bounded window after the drain flag is set — this file's
// header used to accept "503 OR connection refused" because the listener close
// raced the poll; that race is gone, so a connection-refused here means the
// head start did not happen. Both cases run real subprocesses against a real
// upstream that streams slow enough to be mid-flight.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slowChunkUpstream is a fake upstream whose chat stream writes one chunk per
// tick, forever after `ticks` chunks, unless done closes (then [DONE]).
func slowChunkUpstream(t *testing.T, ticks int, tick time.Duration, done chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < ticks; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-done:
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				fl.Flush()
				return
			case <-time.After(tick):
			}
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"delta\":\"chunk%d\"}\n\n", i))
			fl.Flush()
		}
		// All ticks delivered: close the stream the same way a real upstream
		// would, so the relay flushes its guaranteed [DONE] — the passthrough
		// Flush always appends the sentinel, but our own tail makes that
		// assertion independent of relay internals.
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGracefulDrainFinishesInFlightStream: SIGTERM mid-stream; the stream
// must deliver its remaining chunks + [DONE] (grace is long enough), and NEW
// requests must be rejected while draining (503 gate or refused listener —
// the 503 branch is unit-asserted in internal/router).
func TestGracefulDrainFinishesInFlightStream(t *testing.T) {
	done := make(chan struct{})
	up := slowChunkUpstream(t, 10, 150*time.Millisecond, done)
	dir := cfgDir(t)
	writeCFG(t, dir, upstreamBase(up.URL)+"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n")
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_SHUTDOWN_GRACE": "5000",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", sp.base+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-coder-free","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := sp.client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read the first chunk to prove the stream is in flight, then SIGTERM.
	// We read until we see chunk0, then trigger shutdown while reading.
	first := make(chan struct{})
	bodyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), "chunk0") {
					select {
					case first <- struct{}{}:
					default:
					}
				}
			}
			if err != nil {
				errCh <- err
				bodyCh <- sb.String()
				return
			}
		}
	}()

	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatalf("stream never delivered chunk0")
	}

	if err := sp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	// New requests must now be rejected by the 503 drain gate. The readiness
	// head start guarantees the listener is still open for a bounded window
	// after the drain flag, so the 503 is what a poll must see — a transport
	// error here means the listener closed without the head start (issue
	// #87), which is the regression this file now exists to catch.
	deadline := time.Now().Add(5 * time.Second)
	drained := false
	var lastErr error
	for time.Now().Before(deadline) {
		r, err := sp.client.Post(sp.base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"qwen3-coder-free","stream":true}`))
		if err != nil {
			// Only acceptable if the drain already outran the poll entirely;
			// with a head start in the picture this must not be the first
			// answer, so it is recorded and retried rather than accepted.
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		status := r.StatusCode
		_ = r.Body.Close()
		if status == http.StatusServiceUnavailable {
			drained = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !drained {
		t.Fatalf("shutdown never answered new requests with the 503 drain gate (last transport error: %v)\nlog:\n%s", lastErr, sp.out.String())
	}

	// The in-flight stream keeps flowing to [DONE].
	select {
	case err := <-errCh:
		if err != nil && err != io.EOF {
			t.Fatalf("in-flight stream ended with error (want clean EOF after [DONE]): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("in-flight stream never ended (grace should allow it to finish)")
	}
	body := <-bodyCh
	if !strings.Contains(body, "chunk9") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("in-flight stream truncated: %q", body)
	}

	// The server must exit cleanly (graceful path, no forced close) within
	// grace + slack.
	waitExit(t, sp, 8*time.Second)
}

// TestGraceForcesCloseStalledStream: OCFP_SHUTDOWN_GRACE is tiny; an upstream
// that stalls mid-stream means the connection never drains; after the grace
// the server force-closes — the client sees the connection die (error or
// truncated body with no [DONE]).
func TestGraceForcesCloseStalledStream(t *testing.T) {
	// Upstream sends one SSE frame — enough for the relay to commit the
	// response downstream (headers + chunk0 — without a frame the client's
	// Do would wait on headers forever) — then hangs. The handler returns
	// only when the server process force-closes the connection (context
	// done) or the test tears down (stop) — so httptest.Close can complete.
	stop := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"delta\":\"chunk0\"}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { up.Close() })  // runs LAST (LIFO)
	t.Cleanup(func() { close(stop) }) // runs FIRST: unblock the handler
	dir := cfgDir(t)
	writeCFG(t, dir, upstreamBase(up.URL)+"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n")
	sp := spawnProxy(t, dir, map[string]string{
		"OCFP_SHUTDOWN_GRACE": "300",
	})

	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", sp.base+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-coder-free","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// First frame must arrive (the relay committed chunk0) — the stream is
	// in flight before we signal. The upstream stalls after one frame, so
	// read incrementally until chunk0 shows rather than demanding a full
	// buffer. Reads accumulate into a Builder: the per-read Contains check
	// above looked at only the latest 32-byte slice and could lose chunk0
	// across a fragmented read (e.g. "data: chu" + "nk0\n\n").
	saw0 := false
	var sb strings.Builder
	buf := make([]byte, 32)
	for i := 0; i < 100 && !saw0; i++ {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			saw0 = strings.Contains(sb.String(), "chunk0")
		}
		if err != nil {
			break
		}
	}
	if !saw0 {
		t.Fatalf("stream never delivered chunk0")
	}

	if err := sp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	sigAt := time.Now()

	// New requests are rejected by the 503 drain gate. With a 300ms grace the
	// head start is grace/2 = 150ms, so the listener is briefly still open —
	// but a 150ms window against a 30ms poll and a cold client is tight, so a
	// transport error after a 503 is tolerated here (this test's subject is
	// the force-close, not the head start; readiness_test.go pins the head
	// start deterministically). A connection refused BEFORE any 503 is the
	// signal that the head start was skipped entirely.
	deadline := time.Now().Add(3 * time.Second)
	drained := false
	var firstErr error
	for time.Now().Before(deadline) {
		r, err := sp.client.Post(sp.base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"qwen3-coder-free","stream":true}`))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		status := r.StatusCode
		_ = r.Body.Close()
		if status == http.StatusServiceUnavailable {
			drained = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !drained && firstErr != nil && !strings.Contains(firstErr.Error(), "connection refused") &&
		!strings.Contains(firstErr.Error(), "EOF") {
		t.Fatalf("new request failed with an unexpected error (want 503 or connection refused): %v", firstErr)
	}
	if !drained && firstErr == nil {
		t.Fatalf("shutdown never rejected new requests (log:\n%s)", sp.out.String())
	}

	// Body read must fail or return without [DONE] shortly after grace
	// (300ms + margin). Before the forced close it would hang.
	readErr := make(chan error, 1)
	doneRead := make(chan string, 1)
	go func() {
		b, err := io.ReadAll(resp.Body)
		readErr <- err
		doneRead <- string(b)
	}()
	var elapsed time.Duration
	select {
	case body := <-doneRead:
		elapsed = time.Since(sigAt)
		if strings.Contains(body, "[DONE]") {
			t.Fatalf("stalled stream delivered [DONE] — expected forced close")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled stream survived past grace — forced close never happened")
	}
	// The forced close must NOT happen before the grace elapsed: the server
	// waits the full grace before killing in-flight connections. A server
	// that dies instantly on SIGTERM (signal-handler regression, grace
	// ignored) would fail the read within milliseconds of the signal.
	// The grace is 300ms and the total signal→exit is still ~300ms — the
	// 150ms readiness head start comes OUT of the grace, not on top of it
	// (issue #87) — so require at least 200ms, which leaves margin for
	// scheduling without accepting instant death.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("connection closed after %v since SIGTERM, want >= ~grace (300ms) — grace was likely ignored", elapsed)
	}
	if err := <-readErr; err == nil {
		// Clean EOF without [DONE] is acceptable (force-close); so is an error.
	}

	// The process itself must be gone by now (forced close ends it).
	waitExit(t, sp, 3*time.Second)
}

// waitExit waits for the spawned process to terminate. It shares the spawn's
// single reaper (see proxySpawn.reap) so it never issues a second
// Process.Wait on the same process — the one reaper the spawn owns is the
// only one there is.
func waitExit(t *testing.T, sp *proxySpawn, timeout time.Duration) {
	t.Helper()
	if !sp.reap(timeout) {
		t.Fatalf("server process did not exit within %v\nlog:\n%s", timeout, sp.out.String())
	}
}
