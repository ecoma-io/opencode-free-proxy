package router

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/routing"
	"opencode-free-proxy/internal/upstream"
)

// Server carries the process-wide dependencies. Upstream is the DIRECT
// client, retained for the UA warm/sync path only (GitHub is reached
// directly, never through an egress proxy); per-egress clients are built
// lazily by clientFor and cached by proxy signature.
type Server struct {
	Cfg       *config.Config
	Store     *config.Store
	Scheduler *routing.Scheduler
	Exec      *upstream.Executor
	Health    *health.Registry
	Slots     *upstream.Limiter
	Upstream  *upstream.Client // direct client: UA warm/sync only
	UA        *identity.UserAgentCache
	Draining  atomic.Bool

	sleep     func(time.Duration)
	clientMu  sync.Mutex
	clients   map[string]*upstream.Client
	clientGen map[string]string // egress id → proxy signature the cached client was built for
	logf      func(format string, args ...any)
}

// NewServer wires the multi-egress machinery. store may be a live poller
// (OFP_CONFIG set) or the default direct runtime; logf nil → log.Printf;
// sleep nil → time.Sleep (tests pass a no-op to keep retry matrices fast).
func NewServer(cfg *config.Config, store *config.Store, ua *identity.UserAgentCache, direct *upstream.Client, logf func(string, ...any), sleep func(time.Duration)) *Server {
	if logf == nil {
		logf = log.Printf
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	s := &Server{
		Cfg:       cfg,
		Store:     store,
		Scheduler: routing.NewScheduler(),
		Health:    health.New(),
		Slots:     upstream.NewLimiter(),
		Upstream:  direct,
		UA:        ua,
		sleep:     sleep,
		clients:   map[string]*upstream.Client{},
		clientGen: map[string]string{},
		logf:      logf,
	}
	s.Exec = upstream.NewExecutor(s.clientFor, s.Health, s.Slots)
	return s
}

// runtime returns the current immutable snapshot. A nil Store (tests building
// the Server struct literal directly) falls back to the default direct
// runtime — the historical single-egress behavior.
func (s *Server) runtime() *config.Runtime {
	if s.Store != nil {
		return s.Store.Get()
	}
	return config.DefaultRuntime()
}

// Drain marks the server shutting down: relay() answers 503 to new requests
// from this point on; in-flight requests/streams finish or are cut by the
// shutdown grace after the listener stops.
func (s *Server) Drain() { s.Draining.Store(true) }

// clientFor resolves an egress id to its transport, cached per proxy
// signature ("type:url" — never logged). A signature change (config reload)
// rebuilds the client; the old one is left to GC once in-flight requests
// drop it. Returns false when the egress left the snapshot.
func (s *Server) clientFor(id string) (*upstream.Client, bool) {
	rt := s.runtime()
	e, ok := rt.Egress(id)
	if !ok {
		return nil, false
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	sig := proxySignature(e)
	if c, ok := s.clients[id]; ok && s.clientGen[id] == sig {
		return c, true
	}
	c, err := upstream.NewClientFor(e.Proxy)
	if err != nil {
		s.logf("egress %q: %v", id, err)
		return nil, false
	}
	c.Sleep = s.sleep
	s.clients[id] = c
	s.clientGen[id] = sig
	return c, true
}

func proxySignature(e *config.Egress) string {
	if e.Proxy == nil {
		return "direct:"
	}
	return string(e.Proxy.Type) + ":" + e.Proxy.URL
}

// routeHeads filters the route's egress list to the CURRENTLY eligible head
// set: enabled, streaming-compatible, model allow-list, body-size bound,
// health-eligible, and under its concurrency cap (a soft Peek — the executor
// re-checks with Acquire at dial time). Order follows route.Egress.
func (s *Server) routeHeads(rt *config.Runtime, route config.Route, p routing.Profile) []string {
	heads := make([]string, 0, len(route.Egress))
	for _, id := range route.Egress {
		e, ok := rt.Egress(id)
		if !ok || !e.IsEnabled() {
			continue
		}
		if !e.AcceptsStreaming() && p.Streaming {
			continue
		}
		if len(e.Models) > 0 && !routing.ModelAllowed(e.Models, p.Model) {
			continue
		}
		if e.MaxBodyBytes > 0 && p.BodyBytes > e.MaxBodyBytes {
			continue
		}
		if s.Health != nil && !s.Health.Healthy(id) {
			continue
		}
		if s.Slots != nil && !s.Slots.Peek(id, e.MaxConcurrency) {
			continue
		}
		heads = append(heads, id)
	}
	return heads
}

// newRequestID is the per-request correlation id in every structured log
// line (two fresh crypto/rand bytes words; unguessable by design — it is not
// the opaque session id, which never appears).
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
