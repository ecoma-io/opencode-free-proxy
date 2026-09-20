package main

// Shutdown force-close tests: http.Server.Shutdown never closes an ACTIVE
// connection, so a stuck stream would outlive the drain grace forever. The
// connTracker records live connections through the ConnState hook and
// forceCloseAll closes the survivors when the grace lapses (the two-phase
// shutdown in main()).

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeConn satisfies forceCloseAll's needs: identity + Close. Only Close is
// called on it — the full net.Conn interface is embedded, never invoked.
type fakeConn struct {
	net.Conn
	mu       sync.Mutex
	closes   int
	closeErr error
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return c.closeErr
}

func (c *fakeConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func TestConnTrackerTracksAndDrops(t *testing.T) {
	tr := newConnTracker()
	c1 := &fakeConn{}
	c2 := &fakeConn{}

	// New then Active: tracked once (map semantics — no double-count).
	tr.connState(c1, http.StateNew)
	tr.connState(c1, http.StateActive)
	tr.connState(c2, http.StateNew)

	// Idle → dropped (Shutdown closes idle conns itself).
	tr.connState(c1, http.StateIdle)
	tr.mu.Lock()
	n := len(tr.conns)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("tracked = %d, want 1 (c1 went idle, c2 remains)", n)
	}

	// Hijacked/Closed also drop.
	tr.connState(c2, http.StateHijacked)
	tr.mu.Lock()
	n = len(tr.conns)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatalf("tracked = %d, want 0 after hijack", n)
	}

	// The dropped c1 is not closed by forceCloseAll — it left the set.
	if got := tr.forceCloseAll(); got != 0 {
		t.Fatalf("forceCloseAll = %d, want 0", got)
	}
	if c1.closeCount() != 0 {
		t.Fatalf("idle conn closed %d times, want 0 (it was already dropped)", c1.closeCount())
	}
}

func TestForceCloseAllClosesEachOnce(t *testing.T) {
	tr := newConnTracker()
	conns := []*fakeConn{{}, {}, {}}
	for _, c := range conns {
		tr.connState(c, http.StateActive)
	}

	if got := tr.forceCloseAll(); got != 3 {
		t.Fatalf("forceCloseAll = %d, want 3", got)
	}
	for i, c := range conns {
		if c.closeCount() != 1 {
			t.Fatalf("conn %d closed %d times, want 1", i, c.closeCount())
		}
	}

	// A second pass closes nothing: the set was emptied.
	if got := tr.forceCloseAll(); got != 0 {
		t.Fatalf("second forceCloseAll = %d, want 0", got)
	}

	// A conn whose Close fails is not counted as closed.
	cErr := &fakeConn{closeErr: errors.New("already gone")}
	tr.connState(cErr, http.StateActive)
	if got := tr.forceCloseAll(); got != 0 {
		t.Fatalf("forceCloseAll with failing Close = %d, want 0", got)
	}
	if cErr.closeCount() != 1 {
		t.Fatalf("failing conn Close called %d times, want 1", cErr.closeCount())
	}
}

// TestShutdownGraceForceClosesStuckStream drives the two-phase shutdown end
// to end: a handler holds its response open past the grace, Shutdown times
// out, and forceCloseAll unblocks it — the process exit stays bounded by the
// grace in every case.
func TestShutdownGraceForceClosesStuckStream(t *testing.T) {
	tr := newConnTracker()
	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(handlerEntered)
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-release // hold the connection ACTIVE past the grace
		}),
		ConnState: tr.connState,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	client := &http.Client{Timeout: 10 * time.Second}
	bodyErr := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + ln.Addr().String() + "/stream")
		if err != nil {
			bodyErr <- err
			return
		}
		_, err = io.Copy(io.Discard, resp.Body) // blocks until the force close
		bodyErr <- err
	}()

	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never entered")
	}

	// Phase 1: the drain grace lapses while the stream is active.
	graceCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	shutdownErr := srv.Shutdown(graceCtx)
	if shutdownErr == nil {
		t.Fatal("Shutdown returned nil with a stuck active connection — the grace did not lapse")
	}

	// Phase 2: force-close the survivor.
	if got := tr.forceCloseAll(); got < 1 {
		t.Fatalf("forceCloseAll = %d, want >= 1 tracked connection", got)
	}
	close(release)

	select {
	case err := <-bodyErr:
		if err == nil {
			t.Fatal("client body read completed cleanly — the stuck stream was never cut")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("force close did not unblock the stuck stream")
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve exited with %v, want http.ErrServerClosed", err)
	}
}
