// Command server runs the OpenCode free-tier proxy: an OpenAI-compatible
// router (chat completions + responses + models) backed solely by the
// opencode free provider. With OCFP_CONFIG set it becomes a config-driven,
// stateless multi-egress proxy: typed YAML routing with hot reload, egress
// fallback + health, per-egress concurrency limits and graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/router"
	"opencode-free-proxy/internal/upstream"
)

// version is stamped at build time (Dockerfile: -ldflags "-X main.version=…").
var version = "dev"

func main() {
	// Subcommand dispatch: the runtime image is `scratch` — no shell, no curl —
	// so the Docker HEALTHCHECK runs the entrypoint binary itself against its
	// own /healthz. Anything else (or no args) serves.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			log.Fatalf("healthcheck failed: %v", err)
		}
		return
	}

	cfg := config.FromEnv()
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
		st, err := config.NewStore(cfg.ConfigPath, cfg.ConfigPoll, log.Printf)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		store = st
		log.Printf("config: loaded %s (poll %v)", cfg.ConfigPath, cfg.ConfigPoll)
	} else {
		store = config.NewDefault()
	}

	server := router.NewServer(store, uaCache, direct, log.Printf, time.Sleep)

	// Sync the compound UA triple (opencode version, ai-sdk provider-utils,
	// bun) from GitHub: one forced warm at startup, then a background ticker
	// every cfg.UASyncInterval. The request path is a pure cache read
	// (identity.UserAgentCache documents the divergence from 9router's lazy
	// per-request warm); requests before the first successful sync use the
	// compiled-in default triple (fail-open).
	go func() {
		ua := uaCache.Warm(direct.HTTP, true)
		log.Printf("opencode UA cache warm: %s", ua)
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":" + cfg.Port
	conns := newConnTracker()
	srv := &http.Server{Addr: addr, Handler: mux, ConnState: conns.connState}
	go func() {
		log.Printf("opencode-free-proxy %s listening on %s (upstream %s)", version, addr, store.Get().UpstreamBase())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server exited: %v", err)
		}
	}()

	// Graceful shutdown, two phases:
	//
	//   Phase 1 (drain): on SIGINT/SIGTERM stop accepting NEW work first (the
	//   drain gate answers 503), stop the config poller and UA sync, then let
	//   the HTTP server finish in-flight requests/streams. Shutdown closes
	//   the listener and the idle connections and waits for the ACTIVE ones.
	//
	//   Phase 2 (force): OCFP_SHUTDOWN_GRACE bounds phase 1. Shutdown itself
	//   never closes an active connection — a stuck stream (upstream that
	//   trickles below the stall deadline) would outlive the grace — so when
	//   the grace lapses, every still-tracked connection is force-closed
	//   here, unblocking the handlers and bounding the process exit to the
	//   grace in all cases.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	<-ctx.Done()
	stop()
	log.Printf("shutdown: draining (grace %v)…", cfg.ShutdownGrace)
	server.Drain()
	store.Stop()
	uaStop()

	graceCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(graceCtx); err != nil {
		closed := conns.forceCloseAll()
		log.Printf("shutdown: grace %v elapsed, force-closed %d connection(s): %v", cfg.ShutdownGrace, closed, err)
	}
	log.Printf("shutdown complete")
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

// runHealthcheck backs the Docker HEALTHCHECK: GET the server's own /healthz
// on the configured port (OCFP_PORT) and report success. The 5 s client
// timeout matches HEALTHCHECK --timeout=5s.
func runHealthcheck() error {
	port := os.Getenv("OCFP_PORT")
	if port == "" {
		port = config.DefaultPort
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz status %d", resp.StatusCode)
	}
	return nil
}
