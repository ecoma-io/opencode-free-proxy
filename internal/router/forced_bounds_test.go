package router

// Bounds tests for the forced SSE→JSON body read (readBoundedSSE): the
// streaming path has always had the config.StreamStall watchdog
// (upstream.ScanLines); the aggregation path reads the WHOLE body, so it
// needs the same stall deadline plus a total byte cap
// (config.MaxForcedSSEBytes) — ResponseHeaderTimeout is satisfied once
// headers arrive and bounds nothing after that.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// blockingBody is an io.ReadCloser whose Read parks until Close unblocks it
// (io.Pipe semantics) — the shape of a real upstream body after the headers
// arrived and the proxy went quiet.
type blockingBody struct {
	pr *io.PipeReader
}

func newBlockingBody() (*blockingBody, func()) {
	pr, pw := io.Pipe()
	b := &blockingBody{pr: pr}
	return b, func() { _ = pw.Close() } // also the writer closer, for cleanup
}

func (b *blockingBody) Read(p []byte) (int, error) { return b.pr.Read(p) }
func (b *blockingBody) Close() error               { return b.pr.CloseWithError(io.EOF) }

// trickleBody emits one byte every interval until total is reached, then EOF
// — a slow-but-alive upstream whose bytes each arrive inside the stall
// window, the shape only a per-gap deadline (not an absolute read timeout)
// lets through.
type trickleBody struct {
	interval time.Duration
	total    int
	mu       sync.Mutex
	sent     int
	closed   bool
}

func (t *trickleBody) Read(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed || t.sent >= t.total {
		t.mu.Unlock()
		return 0, io.EOF
	}
	t.mu.Unlock()
	select {
	case <-time.After(t.interval):
		if len(p) == 0 {
			return 0, nil
		}
		t.mu.Lock()
		t.sent++
		t.mu.Unlock()
		p[0] = 'x'
		return 1, nil
	case <-time.After(30 * time.Second):
		return 0, io.EOF
	}
}

func (t *trickleBody) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// TestReadBoundedSSEStallAborts: no bytes within the stall deadline → the
// read aborts in bounded time (the old io.ReadAll would hang forever), and
// closing the body unblocks the reader goroutine (no leak past the request).
func TestReadBoundedSSEStallAborts(t *testing.T) {
	body, closeWriter := newBlockingBody()
	// The writer stays open for now: Read parks forever, which is exactly the
	// stuck-upstream shape. closeWriter + body.Close below play the role
	// forcedSSEToJson's deferred resp.Body.Close plays for real.
	defer closeWriter()

	before := runtime.NumGoroutine()
	start := time.Now()
	_, err := readBoundedSSE(context.Background(), body, 1<<20, 40*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, errForcedSSEStall) {
		t.Fatalf("err = %v, want errForcedSSEStall", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("stall abort took %s — the deadline does not bound the read", elapsed)
	}

	// The reader goroutine parked inside Read must exit once the body is
	// closed (forcedSSEToJson's deferred Close).
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closeWriter()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines after abort = %d, baseline %d — the stalled reader leaked", after, before)
	}
}

// TestReadBoundedSSEProgressResetsDeadline: bytes arriving inside the window
// keep the read alive — the deadline is per-gap (ScanLines semantics), not
// absolute, so a slow-but-alive upstream still aggregates.
func TestReadBoundedSSEProgressResetsDeadline(t *testing.T) {
	body := &trickleBody{interval: 3 * time.Millisecond, total: 20}
	defer func() { _ = body.Close() }()
	// 20 bytes at 3ms ≈ 60ms total, far beyond the 25ms stall window — only
	// per-gap resets let this complete.
	got, err := readBoundedSSE(context.Background(), body, 1<<20, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil (progress must reset the deadline)", err)
	}
	if len(got) != 20 {
		t.Fatalf("read %d bytes, want 20", len(got))
	}
}

// TestReadBoundedSSESizeCap: more than maxBytes → the cap error, and nothing
// beyond the cap was buffered (the accumulator never exceeds one chunk over).
func TestReadBoundedSSESizeCapAborts(t *testing.T) {
	body := strings.NewReader(strings.Repeat("a", 128*1024))
	_, err := readBoundedSSE(context.Background(), body, 32*1024, time.Second)
	if !errors.Is(err, errForcedSSETooLarge) {
		t.Fatalf("err = %v, want errForcedSSETooLarge", err)
	}
}

// TestReadBoundedSSEContextCancel: a dead request context aborts the read
// and the reader goroutine (client disconnect mid-aggregation).
func TestReadBoundedSSEContextCancel(t *testing.T) {
	body, closeWriter := newBlockingBody()
	defer closeWriter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := readBoundedSSE(ctx, body, 1<<20, time.Minute)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled read did not return")
	}
}

// TestReadBoundedSSECompleteBody: a well-formed body still reads whole and
// EOF-terminates (the bounds must not disturb the happy path).
func TestReadBoundedSSECompleteBody(t *testing.T) {
	want := "data: {\"x\":1}\n\ndata: [DONE]\n\n"
	got, err := readBoundedSSE(context.Background(), strings.NewReader(want), 1<<20, time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(got) != want {
		t.Fatalf("read = %q, want %q", got, want)
	}
}

// TestForcedSSEToJsonOversizedUpstreamReturns502 drives the cap end to end:
// an upstream whose forced-SSE body blows past config.MaxForcedSSEBytes gets
// the clean 502 envelope of any other forced-conversion failure — not an
// unbounded buffer.
func TestForcedSSEToJsonOversizedUpstreamReturns502(t *testing.T) {
	flood := strings.Repeat("x", 32*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// ~2x the cap in 32KB chunks; the router aborts at the cap and the
		// remaining writes fail against the closed connection (ignored).
		for i := 0; i < (config.MaxForcedSSEBytes/(32*1024))+2; i++ {
			if _, err := w.Write([]byte(flood)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	_, mux := newRouter(upstream.URL, "")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, mux, "/v1/chat/completions", `{"model":"qwen3-coder-free"}`, nil)
	}()
	select {
	case rec := <-done:
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Failed to convert streaming response to JSON") {
			t.Fatalf("body = %s, want the forced-conversion 502 envelope", rec.Body.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("oversized forced body did not abort — the byte cap is not enforced")
	}
}
