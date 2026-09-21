package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunHealthcheck drives the Docker HEALTHCHECK subcommand against a real
// listener via the OCFP_PORT env var (the subcommand's only configuration input).
func TestRunHealthcheck(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OCFP_PORT", srvPort(t, srv))

	if err := runHealthcheck(); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	if gotPath != "/healthz" {
		t.Fatalf("probe path = %q, want /healthz", gotPath)
	}

	// Non-200 must fail the probe.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := runHealthcheck(); err == nil {
		t.Fatal("503 server: want error, got nil")
	}

	// Closed port (server down) must fail the probe.
	srv.Close()
	if err := runHealthcheck(); err == nil {
		t.Fatal("closed port: want error, got nil")
	}
}

func srvPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	addr := srv.Listener.Addr().String()
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	t.Fatalf("no port in listener addr %q", addr)
	return ""
}
