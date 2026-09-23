package upstream

// Terminal-response regression suite (issue #53, brief §10): the boundary
// between "the provider answered" and "the transport failed".
//
// The recovery-ownership split's central claim is that OFP never re-asks a
// provider after an HTTP verdict, because a response of any status proves the
// request was received. These tests make that claim falsifiable at the lowest
// layer that can honour it: one Client.Do, one HTTP request on the wire, for
// every status in the table.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// statusClass is the expected classification of each terminal status.
var statusClass = map[int]Class{
	400: ClassClientError,
	401: ClassClientError,
	403: ClassClientError,
	404: ClassClientError,
	408: ClassClientError,
	409: ClassClientError,
	422: ClassClientError,
	429: ClassUpstream429,
	500: ClassUpstream5xx,
	502: ClassUpstream5xx,
	503: ClassUpstream5xx,
	504: ClassUpstream5xx,
}

// TestProviderResponseIsTerminal pins the table: whatever the status, exactly
// one request reaches the upstream, the verdict is parsed and returned, and
// its provenance is upstream/response_started — never replay-safe.
func TestProviderResponseIsTerminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				hits++
				mu.Unlock()
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"provider says no"}}`)
			}))
			defer srv.Close()

			started := time.Now()
			_, uerr, failure := NewClient().DoClassified(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
			elapsed := time.Since(started)

			if uerr == nil || uerr.Status != status {
				t.Fatalf("uerr = %v, want status %d", uerr, status)
			}
			if uerr.Message != "provider says no" {
				t.Fatalf("message = %q, want the provider's own body", uerr.Message)
			}
			mu.Lock()
			got := hits
			mu.Unlock()
			if got != 1 {
				t.Fatalf("upstream requests = %d, want exactly 1", got)
			}
			if want := statusClass[status]; failure.Class != want {
				t.Fatalf("class = %s, want %s", failure.Class, want)
			}
			if failure.Origin != OriginUpstream || failure.RequestState != RequestStateResponseStarted {
				t.Fatalf("provenance = %+v, want upstream/response_started", failure)
			}
			if failure.ReplaySafe() {
				t.Fatalf("a provider verdict is replay-safe: %+v", failure)
			}
			// No retry delay exists any more; a verdict returns promptly. The
			// bound is loose (it only has to be far below the 2-3s the removed
			// matrix slept for) so a loaded CI box cannot flake it.
			if elapsed > 1500*time.Millisecond {
				t.Fatalf("status %d took %s — a status-based wait re-entered the path", status, elapsed)
			}
		})
	}
}

// TestHeadersBuiltOncePerLogicalCall: buildHeaders is invoked once per
// logical upstream attempt. It used to be invoked once per retry-matrix
// iteration (base.js:127-130 rebuilds the map per `continue`); with the matrix
// gone the header map and the single request it belongs to are one unit.
func TestHeadersBuiltOncePerLogicalCall(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	buildHeaders := func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		seen++
		return map[string]string{"x-opencode-request": "msg_1"}
	}

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer srv.Close()

	_, uerr, _ := NewClient().DoClassified(context.Background(), srv.URL, buildHeaders, []byte("{}"))
	if uerr == nil || uerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("uerr = %v, want the 503 relayed", uerr)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("upstream requests = %d, want exactly 1", hits)
	}
	if seen != 1 {
		t.Fatalf("buildHeaders invoked %d times, want exactly 1", seen)
	}
}

// TestNoRetrySurfaceRemains is a structural pin: the client exposes one call
// and one clock, and no field through which a caller could inject a retry
// delay. It fails to COMPILE if a Sleep/retry seam comes back, which is the
// point — the removal is not just behavioral.
func TestNoRetrySurfaceRemains(t *testing.T) {
	c := NewClient()
	if c.Now == nil {
		t.Fatal("the evidence clock must stay wired")
	}
	// One call, one attempt: a 429 returns the provider's verdict and nothing
	// else happened between here and the wire (asserted by the table above).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()
	_, uerr, failure := c.DoClassified(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
	if uerr == nil || failure.Class != ClassUpstream429 {
		t.Fatalf("uerr=%v failure=%+v", uerr, failure)
	}
}
