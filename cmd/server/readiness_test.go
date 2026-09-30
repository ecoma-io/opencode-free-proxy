package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/router"
)

// readyServer is a real listener serving /healthz + /readyz through the same
// mux construction main uses, with the Server the handlers read. A nil Store
// is the documented way to build a Server in tests and nothing here touches
// it — the readiness surface reads exactly one field, the Draining flag.
type readyServer struct {
	*router.Server
	base string
}

func newReadyServer(t *testing.T) *readyServer {
	t.Helper()
	s := &router.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET "+readyPath, serveReadyz(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &readyServer{Server: s, base: srv.URL}
}

// readyz answers (status, body, headers) for one GET /readyz against the REAL
// listener, not a synthetic Request — the ordering assertion is precisely that
// the socket still answers, and a recorder cannot show that.
func (rs *readyServer) readyz(t *testing.T) (int, string, http.Header) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(rs.base + readyPath)
	if err != nil {
		return 0, "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		t.Fatalf("read /readyz body: %v", err)
	}
	return resp.StatusCode, string(body), resp.Header
}

func (rs *readyServer) healthz(t *testing.T) int {
	t.Helper()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(rs.base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode
}

// TestReadinessHandler pins the endpoint contract: 200 while serving, 503 the
// instant Drain() runs, and — the invariant the whole feature exists for —
// BOTH answered off the same live connection, because Drain() does not touch
// the listener. If the second probe could not be answered at all, readiness
// went unobservable and the head start would be buying nothing.
func TestReadinessHandler(t *testing.T) {
	rs := newReadyServer(t)

	code, body, hdr := rs.readyz(t)
	if code != http.StatusOK || body != readyBody {
		t.Fatalf("while serving: status=%d body=%q, want 200 %q", code, body, readyBody)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: a cached 200 replayed after draining routes traffic into a closing socket", cc)
	}

	rs.Drain()

	if code, _, _ := rs.readyz(t); code != http.StatusServiceUnavailable {
		t.Fatalf("after Drain(): status=%d, want 503 while the listener is still accepting", code)
	}
}

// A readiness probe going unready must not take liveness with it: an
// orchestrator reading a failing liveness probe kills a process that is
// draining correctly, mid-drain, cutting the very streams the grace exists to
// protect. The two questions have separate answers.
func TestHealthzStaysHealthyWhileDraining(t *testing.T) {
	rs := newReadyServer(t)
	rs.Drain()
	if code := rs.healthz(t); code != http.StatusOK {
		t.Fatalf("/healthz after Drain() = %d, want 200 — liveness is not readiness", code)
	}
}

// The endpoint must read the ONE flag the request gate reads
// (relay()/HandleModels gate on Draining too), never a parallel state machine:
// a probe that could disagree with the gate would advertise an instance as
// ready while it is already refusing its work.
func TestReadyzFollowsTheSameFlagTheDrainGateReads(t *testing.T) {
	rs := newReadyServer(t)
	if rs.Draining.Load() {
		t.Fatal("a fresh Server reports draining")
	}
	rs.Drain()
	if !rs.Draining.Load() {
		t.Fatal("Drain() did not set the flag the readiness handler reads")
	}
}

// /readyz is not part of the OpenAI-compatible surface, so a non-GET is a
// plain 405 with an Allow header rather than the proxy's JSON method envelope.
// The ServeMux pattern already routes only GET/HEAD, so this exercises the
// handler directly — the branch is defence for a future pattern widening, not
// a path a caller can reach today.
func TestReadyzRejectsNonGet(t *testing.T) {
	h := serveReadyz(&router.Server{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, readyPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /readyz = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s /readyz Allow = %q, want %q", method, allow, http.MethodGet)
		}
	}
}

// TestReadinessHeadStartIsCappedAtHalfTheGrace pins the derivation: the head
// start is drawn FROM the grace, not added on top, and a short grace takes a
// short head start rather than one that leaves nothing for in-flight work.
// Total signal→exit must stay within OCFP_SHUTDOWN_GRACE, or a container's
// stop_grace_period would need adjusting for a change that must not need it.
func TestReadinessHeadStartIsCappedAtHalfTheGrace(t *testing.T) {
	cases := []struct {
		grace time.Duration
		want  time.Duration
		why   string
	}{
		{350 * time.Second, readinessPropagation, "the production grace carries the full 5s"},
		{config.DefaultShutdownGrace, readinessPropagation, "the 55s default carries the full 5s"},
		{10 * time.Second, readinessPropagation, "10s/2 is still above 5s"},
		{readinessPropagation * 2, readinessPropagation, "exactly at the cap"},
		{8 * time.Second, 4 * time.Second, "a short grace takes a SHORT head start, not the full one"},
		{time.Second, 500 * time.Millisecond, "a test-sized grace takes half of it"},
		{time.Nanosecond, 0, "a budget too small to halve takes none"},
		{0, 0, "a zero grace takes none — previous behaviour exactly"},
		{-time.Second, 0, "a negative grace takes none rather than sleeping"},
	}
	for _, tc := range cases {
		if got := readinessHeadStart(tc.grace); got != tc.want {
			t.Errorf("readinessHeadStart(%v) = %v, want %v (%s)", tc.grace, got, tc.want, tc.why)
		}
	}
	// The invariant, stated once rather than per case: the head start plus
	// what is left of the drain never exceeds the grace.
	for _, grace := range []time.Duration{time.Second, 8 * time.Second, config.DefaultShutdownGrace, 350 * time.Second} {
		head := readinessHeadStart(grace)
		if head > grace {
			t.Fatalf("head start %v exceeds grace %v — it must be drawn from the grace", head, grace)
		}
	}
}

// TestAwaitReadinessHeadStartSkipsWhenTheServerAlreadyStopped: otherwise every
// clean shutdown sleeps the full window for a listener that is already gone.
func TestAwaitReadinessHeadStartSkipsWhenTheServerAlreadyStopped(t *testing.T) {
	t.Run("returns immediately on a server that stopped", func(t *testing.T) {
		serveDone := make(chan error, 1)
		serveDone <- http.ErrServerClosed
		start := time.Now()
		awaitReadinessHeadStart(readinessPropagation, serveDone, testLogger())
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("waited %v on an already-stopped server, want an immediate return", elapsed)
		}
	})

	t.Run("waits out the window when the server is still up", func(t *testing.T) {
		serveDone := make(chan error, 1)
		const head = 200 * time.Millisecond
		start := time.Now()
		awaitReadinessHeadStart(head, serveDone, testLogger())
		elapsed := time.Since(start)
		if elapsed < head {
			t.Fatalf("returned after %v, want at least the %v head start", elapsed, head)
		}
		if elapsed > head+time.Second {
			t.Fatalf("waited %v for a %v head start", elapsed, head)
		}
	})

	t.Run("a zero head start never blocks", func(t *testing.T) {
		start := time.Now()
		awaitReadinessHeadStart(0, make(chan error), testLogger())
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("zero head start blocked for %v", elapsed)
		}
	})
}

// TestErrServerExited: the drain path always reaches Shutdown, which makes
// ListenAndServe return ErrServerClosed. That is the expected end and must
// never be reported as a failure; a real listen error still must be.
func TestErrServerExited(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, true},
		{http.ErrServerClosed, true},
		{io.ErrUnexpectedEOF, false},
	}
	for _, tc := range cases {
		if got := errServerExited(tc.err); got != tc.want {
			t.Errorf("errServerExited(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// testLogger is a discarded-output logger; these paths log, and nothing here
// asserts on what they say.
func testLogger() zerolog.Logger { return zerolog.Nop() }
