package main

// Inbound-listener timeout contract. The server main serves is built by
// newHTTPServer; these tests pin the wiring (the behavioral slowloris cut is
// covered black-box in e2e/timeouts_test.go against the real binary).

import (
	"net/http"
	"testing"

	"opencode-free-proxy/internal/config"
)

// TestNewHTTPServerTimeouts: header reads and idle keep-alives must be
// bounded (a slow client otherwise pins a goroutine + connection
// indefinitely), while ReadTimeout/WriteTimeout must stay ZERO — both would
// kill long-lived SSE streams mid-flight (see newHTTPServer).
func TestNewHTTPServerTimeouts(t *testing.T) {
	tr := newConnTracker()
	handler := http.NotFoundHandler()
	srv := newHTTPServer(":0", handler, tr.connState)

	if srv.ReadHeaderTimeout != config.HeaderReadTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v (slowloris bound)", srv.ReadHeaderTimeout, config.HeaderReadTimeout)
	}
	if srv.IdleTimeout != config.IdleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v (idle keep-alive reclamation)", srv.IdleTimeout, config.IdleTimeout)
	}
	// The SSE-hostile pair is deliberately unset; a zero check keeps a
	// well-meaning "harden this" edit from landing silently.
	if srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, want 0 — it bounds the whole request incl. body and would cut slow legitimate uploads", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 — it bounds response writes per request and would cut SSE streams mid-flight", srv.WriteTimeout)
	}
	if srv.Handler == nil {
		t.Fatal("handler not wired")
	}
	if srv.ConnState == nil {
		t.Fatal("ConnState hook not wired — shutdown force-close would lose its connection tracking")
	}
}
