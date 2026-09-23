package router

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/logging"
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
	Store     *config.Store
	Scheduler *routing.Scheduler
	Exec      *upstream.Executor
	Health    *health.Registry
	Slots     *upstream.Limiter
	Upstream  *upstream.Client // direct client: UA warm/sync only
	UA        *identity.UserAgentCache
	Draining  atomic.Bool

	clientMu sync.Mutex
	clients  map[string]*upstream.Client
	// prunedGen is the last generation whose transport cache was pruned
	// (onGeneration, once per generation, monotonic — a request holding a
	// STALE snapshot never prunes against its older keep-set).
	prunedGen atomic.Uint64
	log       zerolog.Logger
}

// NewServer wires the multi-egress machinery. store may be a live poller
// (OCFP_CONFIG set) or the default direct runtime. There is no sleep seam any
// more: it existed to stub the retry matrix's delays, and this proxy no
// longer waits on purpose anywhere.
func NewServer(store *config.Store, ua *identity.UserAgentCache, direct *upstream.Client, logger any) *Server {
	s := &Server{
		Store:     store,
		Scheduler: routing.NewScheduler(),
		Health:    health.New(),
		Slots:     upstream.NewLimiter(),
		Upstream:  direct,
		UA:        ua,
		clients:   map[string]*upstream.Client{},
		log:       logging.Resolve(logger),
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
		s.log.Warn().Str("egress", e.ID).Err(err).Msg("egress transport setup failed")
		return nil, false
	}
	s.clients[sig] = c
	return c, true
}

// onGeneration runs once-per-generation maintenance for all three process-wide
// state caches: when the request's snapshot is from a generation NEWER than
// the last one serviced, prune cached transports absent from that snapshot
// (a reload that swapped out a proxy URL), reclaim health identities it no
// longer names (issue #9), and drop scheduler rotation state for routes it no
// longer names (a removed route must not carry a cursor or current_weight
// into a future config that reuses the id). The guard is a monotonic CAS — a
// request holding a STALE snapshot never prunes or reclaims against its older
// keep-set (it would drop a newer generation's transports) — and the
// CAS+maintenance pair runs under clientMu so they are ONE atomic step:
// with the CAS advanced before an interleaved prune completed, a stale
// snapshot could still evict a transport the newer generation had just
// rebuilt (self-healing — the next dial rebuilds — but needless conn churn).
// In-flight requests hold their *Client by reference (the map is only a
// lookup), so pruning cannot invalidate them; evicted clients close their
// idle conns instead of waiting on GC. Health reclamation additionally
// spares every identity pinned by an in-flight request (health.Registry.Pin
// at snapshot pin time), so state an old-generation request still observes
// is never dropped under it; scheduler rotation state needs no such pin — a
// route state pruned under an in-flight request is simply re-derived from
// that request's own snapshot at its next Plan, and a mid-request reset
// degrades to a deterministic first pick, never an error. NewClientFor does
// no I/O, so holding clientMu here never blocks on a dial. Generation 0 is
// the built-in default runtime: nothing to prune.
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
			sigs, active, routes := generationKeepSets(rt)
			s.pruneClientsLocked(sigs)
			if s.Health != nil {
				s.Health.Reclaim(active)
			}
			if s.Scheduler != nil {
				s.Scheduler.PruneRoutes(routes)
			}
			return
		}
	}
}

// generationKeepSets derives, from one snapshot, the transport signatures
// the client cache keeps, the health identities the runtime can still reach,
// and the route ids the scheduler keeps rotation state for. The first two
// name the same route-referenced egresses — eligibility and dialing are only
// ever consulted for egresses a route lists — while the third names every
// route the snapshot declares, resolvable egresses or not (a route state's
// fingerprint re-validates itself at the next Plan).
func generationKeepSets(rt *config.Runtime) (sigs, active, routes map[string]struct{}) {
	sigs = make(map[string]struct{}, len(rt.File.Egress))
	active = make(map[string]struct{}, len(rt.File.Egress))
	routes = make(map[string]struct{}, len(rt.Routes()))
	for _, r := range rt.Routes() {
		routes[r.ID] = struct{}{}
		for _, id := range r.Egress {
			e, ok := rt.Egress(id)
			if !ok {
				continue
			}
			sigs[e.TransportSignature()] = struct{}{}
			active[e.HealthKey()] = struct{}{}
		}
	}
	return sigs, active, routes
}

// pruneClientsLocked drops cached per-egress transports absent from the
// keep set; clientMu must be held. Called only from onGeneration, once per
// generation.
func (s *Server) pruneClientsLocked(keep map[string]struct{}) {
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

// routeHealthKeys resolves the route's egress list to the health identities
// the request may touch (eligibility checks and observations), for
// health.Registry.Pin.
func routeHealthKeys(rt *config.Runtime, route config.Route) []string {
	keys := make([]string, 0, len(route.Egress))
	for _, id := range route.Egress {
		if e, ok := rt.Egress(id); ok {
			keys = append(keys, e.HealthKey())
		}
	}
	return keys
}

// newRequestID is the per-request correlation id in every structured log
// line (two fresh crypto/rand bytes words; unguessable by design — it is not
// the opaque session id, which never appears).
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
