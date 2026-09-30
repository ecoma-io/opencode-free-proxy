// Command server runs the OpenCode free-tier proxy: an OpenAI-compatible
// router (chat completions + responses + models) backed solely by the
// opencode free provider. With OCFP_CONFIG set it becomes a config-driven,
// stateless multi-egress proxy: typed YAML routing with hot reload, egress
// fallback + health, per-egress concurrency limits and graceful shutdown.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/logging"
	"opencode-free-proxy/internal/router"
	"opencode-free-proxy/internal/upstream"
)

// version is stamped at build time (Dockerfile: -ldflags "-X main.version=…").
var version = "dev"

// fatalLog reports failures before a loaded runtime can establish the process
// level. It shares stdout with the runtime JSON logger for Docker's log driver.
var fatalLog = logging.New(os.Stdout)

func main() {
	// Subcommand dispatch: the runtime image is `scratch` — no shell, no curl —
	// so the Docker HEALTHCHECK runs the entrypoint binary itself against its
	// own /healthz. Anything else (or no args) serves.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			fatalLog.Error().Err(err).Msg("healthcheck failed")
			os.Exit(1)
		}
		return
	}

	log := logging.New(os.Stdout)
	// A malformed OCFP_SHUTDOWN_GRACE or OCFP_CONFIG_POLL_MS is fatal here,
	// never a silent fallback to the default (issue #86): the operator
	// believes they configured a value, and the failure mode of not getting
	// it is one-directional — a drain window they asked to be long would run
	// short, force-closing live streams. config.FromEnv reports the key and
	// the value's length, never the value. This is the same fatalLog +
	// os.Exit(1) shape every other bootstrap failure below uses.
	cfg, err := config.FromEnv()
	if err != nil {
		fatalLog.Error().Err(err).Msg("bootstrap env load failed")
		os.Exit(1)
	}
	uaCache := identity.NewUserAgentCache()

	// The DIRECT client is used only by UA warm/sync (GitHub is reached
	// directly, never through an egress proxy); per-egress transports are
	// built lazily by the router.
	direct := upstream.NewClient()

	// Routing config: OCFP_CONFIG set → typed file with a hot-reload poller
	// (a startup load failure is fatal — there is no prior snapshot to fall
	// back on); unset → the default single-egress runtime, byte-identical to
	// the pre-routing proxy.
	var store *config.Store
	if cfg.ConfigPath != "" {
		st, err := config.NewStore(cfg.ConfigPath, cfg.ConfigPoll, log)
		if err != nil {
			fatalLog.Error().Err(err).Msg("config load failed")
			os.Exit(1)
		}
		store = st
	} else {
		store = config.NewDefault()
	}
	// Bootstrap installs the first level; Store.tick is the only subsequent
	// writer, so all emitted events read one globally consistent level.
	zerolog.SetGlobalLevel(config.ParseZerologLevel(store.Get().LogLevel()))
	if cfg.ConfigPath != "" {
		log.Info().Str("path", cfg.ConfigPath).Str("poll", cfg.ConfigPoll.String()).Msg("config loaded")
	}

	server := router.NewServer(store, uaCache, direct, log)

	// Sync the compound UA triple (opencode version, ai-sdk provider-utils,
	// bun) from GitHub: one forced warm at startup, then a background ticker
	// every cfg.UASyncInterval. The request path is a pure cache read
	// (identity.UserAgentCache documents the divergence from 9router's lazy
	// per-request warm); requests before the first successful sync use the
	// compiled-in default triple (fail-open).
	go func() {
		ua := uaCache.Warm(direct.HTTP, true)
		log.Debug().Str("user_agent", ua).Msg("opencode UA cache warm")
	}()
	// The UA sync cadence is read LIVE from the current store snapshot on
	// every cycle (user_agent.sync_interval may be changed by a reload); the
	// loop follows the store, never a captured value — StartSync is called
	// exactly once here, re-arm happens inside the loop, not via a second
	// call (see identity.UserAgentCache.StartSync).
	uaStop := uaCache.StartSync(direct.HTTP, func() time.Duration {
		return store.Get().UASyncInterval()
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", server.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", server.HandleResponses)
	mux.HandleFunc("GET /v1/models", server.HandleModels)
	mux.HandleFunc("OPTIONS /", server.HandleOptions)
	// Liveness, and liveness ONLY: an unconditional 200 for the whole
	// remaining life of the process, drain window included. It deliberately
	// does NOT report the drain — a liveness probe that failed while a
	// process is stopping correctly tells an orchestrator to kill it
	// mid-drain, and Docker marks containers unhealthy from this endpoint.
	// Readiness is the other question, answered by /readyz below
	// (readiness.go).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness: 200 while the process will take new work, 503 from the
	// instant Drain() runs. Both handlers read the SAME flag
	// (server.Draining), so the probe and the request gate cannot disagree.
	mux.HandleFunc("GET "+readyPath, serveReadyz(server))

	addr := ":" + cfg.Port
	conns := newConnTracker()
	srv := newHTTPServer(addr, mux, conns.connState)
	// serveDone lets the head start below give up early if the server dies on
	// its own, and lets the listen goroutine's outcome be collected once.
	serveDone := make(chan error, 1)
	go func() {
		log.Info().Str("version", version).Str("addr", addr).Str("upstream", store.Get().UpstreamBase()).Msg("opencode-free-proxy listening")
		serveDone <- srv.ListenAndServe()
	}()

	// Graceful shutdown, two phases:
	//
	//   Phase 1 (drain): on SIGINT/SIGTERM stop accepting NEW work first (the
	//   drain gate answers 503), stop the config poller and UA sync, PAUSE so
	//   whatever routes here can observe that, then let the HTTP server finish
	//   in-flight requests/streams. Shutdown closes the listener and the idle
	//   connections and waits for the ACTIVE ones.
	//
	//   Phase 2 (force): OCFP_SHUTDOWN_GRACE bounds phase 1 INCLUDING the
	//   readiness head start, which is drawn from the grace rather than added
	//   to it (readinessHeadStart). Shutdown itself never closes an active
	//   connection — a stuck stream (upstream that trickles below the stall
	//   deadline) would outlive the grace — so when the drain budget lapses,
	//   every still-tracked connection is force-closed here, unblocking the
	//   handlers and bounding the process exit to the grace in all cases.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	<-ctx.Done()
	// stop() restores the default disposition, so a SECOND SIGTERM still kills
	// the process outright. That is intentional: an operator who thinks the
	// drain is wedged must be able to force the issue. Do not wrap this in a
	// signal.Notify channel loop that swallows it.
	stop()
	log.Info().Str("grace", cfg.ShutdownGrace.String()).Msg("shutdown: draining")
	server.Drain()
	store.Stop()
	uaStop()

	// Readiness drops HERE — before the listener is touched. From this instant
	// /readyz answers 503 while /healthz keeps answering 200 and the API keeps
	// serving, which is the window a load balancer or a Docker HEALTHCHECK
	// needs in order to take this instance out of rotation before its socket
	// goes away. Without the pause below these two instants are microseconds
	// apart and nothing outside the process can observe the drain at all
	// (issue #87).
	head := readinessHeadStart(cfg.ShutdownGrace)
	log.Info().Dur("grace", cfg.ShutdownGrace).Dur("propagation", head).Msg("readiness_unready")
	awaitReadinessHeadStart(head, serveDone, log)

	// The head start came OUT of the grace, so the drain gets what is left of
	// it rather than the whole thing: total signal→exit stays within
	// OCFP_SHUTDOWN_GRACE, and a container's stop_grace_period needs no
	// adjustment for this change.
	budget := cfg.ShutdownGrace - head
	graceCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := srv.Shutdown(graceCtx); err != nil {
		closed := conns.forceCloseAll()
		log.Warn().Dur("grace", cfg.ShutdownGrace).Dur("drain", budget).Int("force_closed", closed).Err(err).
			Msg("shutdown: grace elapsed, force-closed connections")
	}
	// Collect the listener's outcome so it cannot be left unread. The drain
	// path above always reaches Shutdown, which makes ListenAndServe return
	// ErrServerClosed — that is the expected end, not a failure to report.
	// A REAL listen failure (a bind error, an accept error) is still worth a
	// line, and is the one case where the process should not claim a clean
	// shutdown it did not have.
	if err := <-serveDone; !errServerExited(err) {
		log.Error().Err(err).Msg("shutdown: server exited with an error")
	}
	log.Info().Msg("shutdown complete")
}

// newHTTPServer builds the process's one http.Server. Standing alone so the
// timeout contract is asserted directly in tests (servertimeout_test.go).
//
// Go-side hardening, no JS counterpart: the JS server rides Node/undici
// platform defaults, which bound header reads and idle keep-alives implicitly;
// Go's http.Server defaults to NO timeouts, so a slow client trickling bytes
// would otherwise pin a goroutine and a connection indefinitely — bounded
// only by the shutdown grace. Set (values + rationale in internal/config):
//
//   - ReadHeaderTimeout: header reads only; the connection's read deadline
//     is reset after the headers (GOROOT readRequest), so streaming request
//     BODIES are never touched by it.
//   - IdleTimeout: keep-alive connections idle BETWEEN requests only; it is
//     armed after a response completes and cleared when the next request's
//     first bytes arrive (GOROOT conn.serve), so an in-flight SSE response
//     can never be truncated by it.
//
// Deliberately NOT set — ReadTimeout and WriteTimeout must stay zero:
//
//   - ReadTimeout bounds the ENTIRE request incl. body with one global
//     deadline the handler cannot override — a slow legitimate body upload
//     (up to the 8 MiB route cap) would be killed mid-read.
//   - WriteTimeout bounds response writes from the moment a request's
//     headers are read — an SSE stream that outlives it is killed
//     MID-STREAM (the one deadline is never extended between flushes).
//
// Both look like obvious hardening to a future maintainer and both break
// this proxy's primary workload (long-lived SSE). The real bounds on slow
// upstreams/clients here are the per-egress ConnectTimeout/StreamStall and
// the shutdown-grace force-close.
func newHTTPServer(addr string, handler http.Handler, connState func(net.Conn, http.ConnState)) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ConnState:         connState,
		ReadHeaderTimeout: config.HeaderReadTimeout,
		IdleTimeout:       config.IdleTimeout,
	}
}

// connTracker records the server's live client connections through
// http.Server.ConnState so the shutdown path can force-close the survivors of
// the drain grace. A connection is tracked from StateNew/StateActive and
// dropped on StateIdle (Shutdown closes idle conns itself), StateHijacked
// (no longer the server's to manage — none today, the SSE relays use flush,
// not hijack) and StateClosed. forceCloseAll closes whatever remains; Close
// is safe to call concurrently with the transport's own reads/writes.
type connTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: map[net.Conn]struct{}{}}
}

// connState is the http.Server.ConnState hook.
func (t *connTracker) connState(c net.Conn, cs http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch cs {
	case http.StateNew, http.StateActive:
		t.conns[c] = struct{}{}
	case http.StateIdle, http.StateHijacked, http.StateClosed:
		delete(t.conns, c)
	}
}

// forceCloseAll closes every still-tracked connection and returns the count.
func (t *connTracker) forceCloseAll() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	closed := 0
	for c := range t.conns {
		if err := c.Close(); err == nil {
			closed++
		}
	}
	t.conns = map[net.Conn]struct{}{}
	return closed
}

// runHealthcheck backs the Docker HEALTHCHECK: GET the server's own /readyz
// on the configured port (OCFP_PORT) and report success. The 5 s client
// timeout matches HEALTHCHECK --timeout=5s.
//
// Readiness, not liveness. A process that is draining is alive and behaving
// correctly; the fact that matters is that it must not be sent more traffic —
// which is exactly what /readyz answers 503 for. Probing /healthz instead
// (as this did until issue #87) reported a draining instance as perfectly
// healthy until its listener finally closed, and an orchestrator acting on
// that signal would be restarting a process that was stopping properly.
//
// The BODY is checked as well as the status: 200 from anything OTHER than this
// process on that port would otherwise pass a status-only probe. The response
// body is never logged — a probe aimed at the wrong port can hit anything —
// only the status and the body's length.
func runHealthcheck() error {
	port := os.Getenv("OCFP_PORT")
	if port == "" {
		port = config.DefaultPort
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s%s", port, readyPath))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded read: /readyz answers two bytes, but a probe aimed at the wrong
	// port can hit anything, so neither the body nor its size is trusted past
	// this cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || string(body) != readyBody {
		return fmt.Errorf("%s status %d body_len %d", readyPath, resp.StatusCode, len(body))
	}
	return nil
}
