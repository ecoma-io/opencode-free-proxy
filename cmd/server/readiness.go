package main

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"opencode-free-proxy/internal/router"
)

// The readiness surface, and the pause that makes it observable.
//
// A process that is going away has two different things to say, and they
// must not be the same thing:
//
//   - liveness — "this process is alive". That is GET /healthz, and it stays
//     true for as long as the listener exists. A liveness probe that failed
//     during a drain would tell an operator to restart a process that is
//     stopping correctly, or worse, tell an orchestrator to kill it
//     mid-drain; the shipped Docker HEALTHCHECK reads exactly this endpoint.
//   - readiness — "send me traffic". That is GET /readyz, and it goes false
//     BEFORE the listener closes.
//
// The ordering is the entire point, and it is the one thing the shutdown
// sequence may not trade away: readiness must drop while the socket the
// balancer is routing to is still answering. A poller that has not noticed
// yet cannot be told anything by a closed port — it learns the drain only as
// a refused connection, which is the failure this exists to prevent (issue
// #87).
//
// Readiness here reads the ONE flag that already exists. router.Server.Draining
// is set by Drain() and is already the gate relay() and HandleModels consult
// (internal/router/handler.go:110, models.go:45), so the endpoint and the
// request gate cannot disagree: there is no second state machine that could
// drift from the first. That flag is the whole state; there is no "starting"
// or "stopped" phase to model, because nothing outside this process observes
// the window before the listener exists, and after it closes there is nothing
// left to answer a probe.

// readyBody is the success body, matching GET /healthz's "ok" so an operator
// reads one convention instead of two. runHealthcheck below compares against
// exactly this literal.
const readyBody = "ok"

// readyPath is the readiness endpoint path.
const readyPath = "/readyz"

// serveReadyz answers the readiness probe: 200 "ok" while the process is
// willing to serve new work, 503 the moment Drain() has been called — and
// Drain() is called while the listener is still open, so the 503 is
// genuinely observable before anything goes away.
//
// A non-GET is 405 with an Allow header, and the plain-text body carries no
// OpenAI-shaped error envelope: /readyz is not part of the OpenAI-compatible
// surface, so it makes no promise about JSON error bodies. The ServeMux
// pattern below already routes only GET/HEAD here (a HEAD is a GET without a
// body, and reports the same status), so this branch is unreachable through
// the mux — it exists so a direct handler call, or a future pattern change
// that widens the match, cannot turn a POST into a 200.
//
// The response is explicitly uncacheable. A cached 200 replayed after the
// process began draining would route traffic into a socket that is about to
// close — the exact failure this endpoint exists to prevent.
func serveReadyz(s *router.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.Draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, readyBody)
	}
}

// readinessPropagation is the head start the process gives whatever routes to
// it, between "this instance is no longer ready" and "this instance stops
// accepting connections". Without it the two happen at the same instant, and
// every health check already scheduled, every in-flight probe, and every
// load-balancer view one interval out of date lands on a closed port — a
// connection refused where a 503 or a served request was available.
//
// Five seconds is derived from the probe cadence the deployment ships: a 2s
// interval plus a 2s timeout means the worst-case notification is 4s, plus
// slack for scheduling. It is drawn from the shutdown grace rather than added
// to it (see readinessHeadStart), so the total time from signal to exit is
// unchanged and a container's stop_grace_period needs no adjustment.
//
// This one lives in cmd/server rather than internal/config because it is a
// property of the DEPLOYMENT's probe cadence, not of the proxy protocol — and
// because AGENTS.md rule 3 reserves internal/config for upstream-shaped
// constants and a 9router-mirrored runtime surface. It is stated here next to
// the code that waits on it.
const readinessPropagation = 5 * time.Second

// readinessHeadStart is the readiness head start this process will take,
// capped at half the drain budget so the drain keeps the majority of it. The
// cap is what makes the window safe to spend in every deployment: a server
// configured with a short grace (a test, or an operator who wants a fast
// stop) takes a short head start instead of one that leaves nothing for
// in-flight work. A budget that cannot carry a head start at all — zero, or a
// negative no configuration produces, since envDuration rejects non-positive
// values at boot — takes none, which is the previous behaviour exactly.
func readinessHeadStart(grace time.Duration) time.Duration {
	half := grace / 2
	if half <= 0 {
		return 0
	}
	if readinessPropagation > half {
		return half
	}
	return readinessPropagation
}

// awaitReadinessHeadStart waits out the head start, returning early if the
// server stopped on its own in the meantime — otherwise every clean shutdown
// would sleep the full window for a listener that is already gone.
//
// serveDone is the ListenAndServe goroutine's result channel. Reading it here
// also means the drain phase that follows starts from a known state: if the
// listener is already dead, Shutdown has nothing left to close and in-flight
// connections have already been dealt with by the http.Server itself.
func awaitReadinessHeadStart(head time.Duration, serveDone <-chan error, log zerolog.Logger) {
	if head <= 0 {
		return
	}
	timer := time.NewTimer(head)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-serveDone:
		log.Debug().Msg("shutdown: server stopped during the readiness head start, nothing left to drain")
	}
}

// errServerExited reports whether a ListenAndServe error is the ordinary
// "someone called Shutdown/Close" outcome rather than a real failure.
func errServerExited(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed)
}
