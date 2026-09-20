// Command server runs the OpenCode free-tier proxy: an OpenAI-compatible
// router (chat completions + responses + models) backed solely by the
// opencode free provider. With OFP_CONFIG set it becomes a config-driven,
// stateless multi-egress proxy: typed YAML routing with hot reload, egress
// fallback + health, per-egress concurrency limits and graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	// Routing config: OFP_CONFIG set → typed file with a hot-reload poller
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

	server := router.NewServer(cfg, store, uaCache, direct, log.Printf, time.Sleep)

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
	uaStop := uaCache.StartSync(direct.HTTP, cfg.UASyncInterval)

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
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("opencode-free-proxy %s listening on %s (upstream %s)", version, addr, cfg.UpstreamBase)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server exited: %v", err)
		}
	}()

	// Graceful shutdown: on SIGINT/SIGTERM stop accepting NEW work first (the
	// drain gate answers 503), stop the config poller and UA sync, then let
	// the HTTP server finish in-flight requests/streams — bounded by
	// OFP_SHUTDOWN_GRACE, after which remaining connections are closed.
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
		log.Printf("shutdown: forced close after grace: %v", err)
	}
	log.Printf("shutdown complete")
}

// runHealthcheck backs the Docker HEALTHCHECK: GET the server's own /healthz
// on the configured port and report success. The 5 s client timeout matches
// HEALTHCHECK --timeout=5s.
func runHealthcheck() error {
	port := os.Getenv("PORT")
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
