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
// lazily by clientForEgress and cached by transport signature. Health owns
// process-wide STATE only — the POLICY travels with each request's snapshot
// (health.PolicyFromSnapshot at relay time), so no global tuning is ever
// applied or stored here.
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
	// prunedGen is the last generation whose transport cache was pruned
	// (onGeneration, once per generation, monotonic — a request holding a
	// STALE snapshot never prunes against its older keep-set).
	prunedGen atomic.Uint64
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
// cached by transport signature (Egress.TransportSignature — type:url, never
// logged). Because the cache key is the transport itself, a config reload
// that keeps a proxy URL reuses the SAME immutable client — generations
// sharing a transport share a transport — and a reload that swaps URLs
// simply adds a new entry: the old client stays alive for in-flight requests
// that still reference it. No runtime lookup happens here; the egress comes
// from the request's snapshot (routing.Plan), so this method cannot see a
// reload under an active request. Returns false only when the transport
// cannot be built.
func (s *Server) clientForEgress(e *config.Egress) (*upstream.Client, bool) {
	sig := e.TransportSignature()
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

// onGeneration runs once-per-generation transport-cache maintenance: when
// the request's snapshot is from a generation NEWER than the last one
// serviced, prune cached transports absent from that snapshot (a reload that
// swapped out a proxy URL). The guard is a monotonic CAS — a request holding
// a STALE snapshot never prunes against its older keep-set (it would drop a
// newer generation's transports) — and the CAS+prune pair runs under
// clientMu so they are ONE atomic step: with the CAS advanced before an
// interleaved prune completed, a stale snapshot could still evict a transport
// the newer generation had just rebuilt (self-healing — the next dial
// rebuilds — but needless conn churn). In-flight requests hold their *Client
// by reference (the map is only a lookup), so pruning cannot invalidate
// them; evicted clients close their idle conns instead of waiting on GC.
// NewClientFor does no I/O, so holding clientMu here never blocks on a dial.
// Generation 0 is the built-in default runtime: nothing to prune.
func (s *Server) onGeneration(rt *config.Runtime) {
	if rt.Generation == 0 {
		return
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	for {
		cur := s.prunedGen.Load()
		if cur >= rt.Generation {
			return
		}
		if s.prunedGen.CompareAndSwap(cur, rt.Generation) {
			s.pruneClientsLocked(rt)
			return
		}
	}
}

// pruneClientsLocked drops cached per-egress transports absent from the
// snapshot's routes; clientMu must be held. Called only from onGeneration,
// once per generation.
func (s *Server) pruneClientsLocked(rt *config.Runtime) {
	keep := make(map[string]struct{}, 8)
	for _, r := range rt.Routes() {
		for _, id := range r.Egress {
			if e, ok := rt.Egress(id); ok {
				keep[e.TransportSignature()] = struct{}{}
			}
		}
	}
	for sig, c := range s.clients {
		if _, ok := keep[sig]; !ok {
			c.CloseIdleConnections()
			delete(s.clients, sig)
		}
	}
}

// routeHeads filters the route's egress list to the CURRENTLY eligible head
// set: enabled, streaming-compatible, model allow-list, body-size bound,
// health-eligible, and under its concurrency cap (a soft Peek — the executor
// re-checks with Acquire at dial time). Order follows route.Egress. Health
// is judged under the REQUEST's snapshot policy (hp) against the egress's
// id+transport identity — never against a global knob.
func (s *Server) routeHeads(rt *config.Runtime, route config.Route, p routing.Profile, hp health.Policy) []string {
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
		if s.Health != nil && !s.Health.Healthy(e.HealthKey(), hp) {
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
