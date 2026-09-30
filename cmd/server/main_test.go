package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunHealthcheck drives the Docker HEALTHCHECK subcommand against a real
// listener via the OCFP_PORT env var (the subcommand's only configuration
// input). Since issue #87 the probe reads READINESS (/readyz) and checks the
// body as well as the status — see runHealthcheck.
func TestRunHealthcheck(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(readyBody))
	}))
	defer srv.Close()
	t.Setenv("OCFP_PORT", srvPort(t, srv))

	if err := runHealthcheck(); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	// Liveness must NOT be the probe target any more: a drain is invisible to
	// /healthz by design, and Docker marks the container unhealthy from it.
	if gotPath != readyPath {
		t.Fatalf("probe path = %q, want %q", gotPath, readyPath)
	}

	// 503 — the drain — must fail the probe. This is the whole point of
	// pointing it at /readyz.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := runHealthcheck(); err == nil {
		t.Fatal("503 server: want error, got nil")
	}

	// A 200 whose body is NOT "ok" must fail too. A status-only probe passes
	// a 200 from anything else that happens to be listening on this port,
	// which is precisely the hole the body check closes.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some other service"))
	})
	if err := runHealthcheck(); err == nil {
		t.Fatal("200 with a foreign body: want error, got nil")
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
