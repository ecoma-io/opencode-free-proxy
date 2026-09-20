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
// lazily by clientForEgress and cached by proxy signature.
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

	sleep    func(time.Duration)
	clientMu sync.Mutex
	clients  map[string]*upstream.Client
	// healthMu serializes health-threshold application: Load → Configure →
	// Store must be atomic so a request holding a STALE snapshot can never
	// overwrite a newer generation's tuning (see applyHealth).
	healthMu sync.Mutex
	// healthGen is the last generation whose health thresholds were applied
	// (applyHealth, once per generation — never per request).
	healthGen atomic.Uint64
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
		logf:      logf,
	}
	s.Exec = upstream.NewExecutor(s.clientForEgress, s.Health, s.Slots)
	// Seed health thresholds from the CURRENT snapshot; relay() re-applies
	// only when the generation advances past healthGen. healthGen starts at
	// the never-applied sentinel so the seed always runs.
	s.healthGen.Store(^uint64(0))
	s.applyHealth(s.runtime())
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

// clientForEgress resolves a snapshot-resolved egress to its transport,
// cached by proxy signature ("type:url" — never logged). Because the cache
// key is the transport itself, a config reload that keeps a proxy URL reuses
// the SAME immutable client — generations sharing a transport share a
// transport — and a reload that swaps URLs simply adds a new entry: the old
// client stays alive for in-flight requests that still reference it. No
// runtime lookup happens here; the egress comes from the request's snapshot
// (routing.Plan), so this method cannot see a reload under an active
// request. Returns false only when the transport cannot be built.
func (s *Server) clientForEgress(e *config.Egress) (*upstream.Client, bool) {
	sig := proxySignature(e)
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if c, ok := s.clients[sig]; ok {
		return c, true
	}
	c, err := upstream.NewClientFor(e.Proxy)
	if err != nil {
		s.logf("egress %q: %v", e.ID, err)
		return nil, false
	}
	c.Sleep = s.sleep
	s.clients[sig] = c
	return c, true
}

// applyHealth applies the snapshot's health thresholds when its generation
// is not already applied. The registry stays global mutable state keyed by
// egress id (deliberate — see internal/health); this bounds how often config
// tuning touches it: once per generation, never per request. The fast path
// is lock-free (the common case: this or a NEWER generation already applied
// — a request holding an old snapshot must never regress the tuning); the
// slow path takes healthMu so Load → Configure → Store is atomic and a
// stale-generation racer can never interleave Configure with a newer one.
func (s *Server) applyHealth(rt *config.Runtime) {
	if cur := s.healthGen.Load(); cur == rt.Generation || (cur != ^uint64(0) && cur > rt.Generation) {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if cur := s.healthGen.Load(); cur == rt.Generation || (cur != ^uint64(0) && cur > rt.Generation) {
		return // reconfirm under the lock: a concurrent apply won the race
	}
	enabled := rt.Health.Enabled == nil || *rt.Health.Enabled
	s.Health.Configure(enabled, rt.HealthThreshold(), rt.HealthCooldown())
	s.healthGen.Store(rt.Generation)
	// A swap happened: prune cached transports whose signature no longer
	// exists in this snapshot. In-flight requests hold their *Client by
	// reference (the map is only a lookup), so pruning cannot invalidate
	// them — the old client lives until the request's last reference and GC
	// drops it.
	s.pruneClients(rt)
}

// pruneClients drops cached per-egress transports absent from the snapshot's
// routes (a reload that swapped out a proxy URL). Called only from
// applyHealth, once per generation, already holding healthMu.
func (s *Server) pruneClients(rt *config.Runtime) {
	keep := make(map[string]struct{}, 8)
	for _, r := range rt.Routes() {
		for _, id := range r.Egress {
			if e, ok := rt.Egress(id); ok {
				keep[proxySignature(e)] = struct{}{}
			}
		}
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	for sig := range s.clients {
		if _, ok := keep[sig]; !ok {
			delete(s.clients, sig)
		}
	}
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
