// Package config holds all runtime constants. Mirrors open-sse/config —
// values are never hardcoded outside this package.
//
// The multi-egress layer (Egress/Route/Fallback/Health below) is a deliberate
// Go-side addition with no 9router counterpart: the JS router runs behind a
// single egress. Routing, fallback, and health are kept as three separate
// concerns — routing chooses where to start, fallback chooses what to try
// after a retryable failure, health decides whether an egress is temporarily
// eligible. They never collapse into one policy.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// ProxyType is the outbound proxy scheme an egress dials through. Exactly
// these three are supported; socks5h is deliberately NOT (remote-DNS
// semantics would silently change which resolver sees upstream hostnames —
// validation rejects it by name).
type ProxyType string

const (
	ProxyHTTP   ProxyType = "http"
	ProxyHTTPS  ProxyType = "https"
	ProxySOCKS5 ProxyType = "socks5"
)

// Strategy selects how the scheduler rotates a route's eligible egresses.
type Strategy string

const (
	StrategyRoundRobin     Strategy = "round_robin"
	StrategyWeightedRR     Strategy = "weighted_round_robin"
	defaultStrategy                 = StrategyRoundRobin
	defaultWeight                   = 1
	defaultMaxAttempts              = 3
	defaultHealthThreshold          = 3
	defaultHealthCooldown           = 30 * time.Second
)

// Proxy is one outbound proxy endpoint. URL carries optional userinfo
// (user:password@host:port); it is env-interpolated (${VAR}) at load and its
// credentials are redacted by RedactProxyURL everywhere it can reach a log.
type Proxy struct {
	Type ProxyType `yaml:"type"`
	URL  string    `yaml:"url"`
}

// Egress is one outbound path. A nil Proxy means the host's own network
// (the pre-routing behavior — the implicit default egress).
type Egress struct {
	ID    string `yaml:"id"`
	Proxy *Proxy `yaml:"proxy,omitempty"`
	// Enabled gates scheduling; nil = true.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Weight feeds weighted_round_robin only. Unset = default 1. A weight of
	// 0 means "configured but never scheduled as a route head" — it is NOT
	// an eligibility mechanism (eligibility is hard; weight only distributes).
	Weight *int `yaml:"weight,omitempty"`
	// MaxConcurrency caps in-flight requests on this egress; 0 = unlimited.
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
	// Models is an optional glob allow-list (path.Match syntax, matched
	// against the suffix-stripped model id). Empty = every model.
	Models []string `yaml:"models,omitempty"`
	// Streaming declares whether the egress accepts streaming requests;
	// nil = yes. A false here makes streaming requests ineligible.
	Streaming *bool `yaml:"streaming,omitempty"`
	// MaxBodyBytes caps the inbound request body this egress accepts;
	// 0 = unlimited.
	MaxBodyBytes int64 `yaml:"max_body_bytes,omitempty"`
}

func (e *Egress) enabled() bool { return e.Enabled == nil || *e.Enabled }
func (e *Egress) streaming() bool {
	return e.Streaming == nil || *e.Streaming
}
func (e *Egress) weight() int {
	if e.Weight == nil {
		return defaultWeight
	}
	return *e.Weight
}

// TransportSignature names the PHYSICAL transport an egress dials through:
// proxy type + URL, or "direct" for the host's own network. The per-egress
// client cache is keyed by it (a reload that keeps a URL reuses the same
// immutable client) and the health registry derives its state identity from
// it (issue #6): a policy-only reload keeps the signature — history
// continues — while swapping the proxy URL changes it, so a fresh physical
// transport never inherits the old transport's failure streak or cooldown.
// The signature can carry proxy credentials: it is a map key ONLY and must
// never be logged (log lines name the egress id, never this string).
func (e *Egress) TransportSignature() string {
	if e.Proxy == nil {
		return "direct"
	}
	return string(e.Proxy.Type) + ":" + e.Proxy.URL
}

// HealthKey is the health registry's state identity: logical egress id +
// physical transport signature, joined as id + NUL + signature. The registry
// consumes the key opaquely (a map key — nothing ever splits it back apart),
// so what matters is injectivity, and that holds by construction on the
// FIRST NUL: Validate rejects control characters in egress ids
// (hasControlByte), so the id half cannot contain the separator and the first
// NUL always terminates the id — whatever follows it, NULs included, belongs
// to the signature half. In practice the signature half cannot carry a NUL
// either (Validate gates the type to the three enum constants, and url.Parse
// rejects ASCII control bytes outright — net/url stringContainsCTLByte), so a
// live key holds exactly one NUL. Signatures are never empty ("direct" or
// type:url).
func (e *Egress) HealthKey() string {
	return e.ID + "\x00" + e.TransportSignature()
}

// hasControlByte reports ASCII control characters (C0 + DEL). Ids reach log
// lines and the X-OFP-Egress response header verbatim — Go's header writer
// strips only \r and \n, so a NUL would go over the wire — and control-free
// ids are what makes the NUL-separated HealthKey injective by construction.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// IsEnabled reports whether the egress participates in scheduling (nil = yes).
func (e *Egress) IsEnabled() bool { return e.enabled() }

// AcceptsStreaming reports whether the egress accepts streaming requests.
func (e *Egress) AcceptsStreaming() bool { return e.streaming() }

// EffectiveWeight returns the scheduling weight: omitted = 1, explicit 0 =
// configured but never scheduled as a route head (not eligibility).
func (e *Egress) EffectiveWeight() int { return e.weight() }

// Match is the typed route condition set. Every set field must hold (AND);
// an empty Match matches everything (the default-route shape).
type Match struct {
	Streaming    *bool    `yaml:"streaming,omitempty"`
	MinBodyBytes int64    `yaml:"min_body_bytes,omitempty"`
	MaxBodyBytes int64    `yaml:"max_body_bytes,omitempty"`
	Models       []string `yaml:"models,omitempty"`
}

// Route maps a request class to an ordered egress list. Overlapping routes
// are allowed; the highest Priority wins and file order breaks ties (the
// resolved list is a stable sort — never Go map order).
type Route struct {
	ID       string   `yaml:"id"`
	Priority int      `yaml:"priority,omitempty"`
	Match    Match    `yaml:"match,omitempty"`
	Egress   []string `yaml:"egress"`
	Strategy Strategy `yaml:"strategy,omitempty"`
}

// FallbackPolicy governs the executor's cross-egress retry loop.
type FallbackPolicy struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// MaxAttempts bounds the number of DISTINCT egresses one request may
	// try (including the first). Default 3; the per-attempt upstream retry
	// matrix (RetryRules) is unchanged and sits inside each attempt.
	MaxAttempts int `yaml:"max_attempts,omitempty"`
}

// HealthPolicy governs the runtime health registry (never part of the
// immutable config snapshot's decisions — it is runtime state keyed by
// egress id, tuned by these fields). Threshold/cooldown are pointers so an
// explicit 0 is distinguishable from "unset": 0 means "never cool down"
// (respectively "no exclusion window"), whereas an omitted field takes
// defaultHealthThreshold/defaultHealthCooldown at Resolve.
type HealthPolicy struct {
	Enabled          *bool     `yaml:"enabled,omitempty"`
	FailureThreshold *int      `yaml:"failure_threshold,omitempty"`
	Cooldown         *Duration `yaml:"cooldown,omitempty"`
}

// File is the OFP_CONFIG YAML document.
type File struct {
	Egress   []Egress       `yaml:"egress"`
	Routes   []Route        `yaml:"routes"`
	Fallback FallbackPolicy `yaml:"fallback,omitempty"`
	Health   HealthPolicy   `yaml:"health,omitempty"`
}

// Duration accepts Go duration strings ("30s", "1m30s"). yaml.v3 would
// otherwise decode into time.Duration as a bare nanosecond count — a silent
// unit bug; strings only, with an explicit error.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("duration must be a string like \"30s\", got %s", boundedEcho(fmt.Sprintf("%v", raw)))
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		// time.ParseDuration's error text echoes the raw input back inside
		// quotes, unbounded — stripQuoted keeps the reason class while the
		// value reaches the log only through the bounded echo.
		return fmt.Errorf("invalid duration %q: %s", boundedEcho(s), stripQuoted(err.Error()))
	}
	*d = Duration(parsed)
	return nil
}

// Runtime is the immutable post-validation snapshot a request holds for its
// whole lifetime. Hot reload swaps the pointer atomically; an in-flight
// request never observes a mutation.
//
// Immutability contract: nothing outside Resolve may write a snapshot.
// Routes/MatchRoute deep-copy what they return and HealthThreshold/
// HealthCooldown return values. Two surfaces still expose snapshot memory and
// stay for cause, each documented at its site: the exported FIELDS (File,
// Fallback, Health) — read directly by the router and health packages, so
// they cannot become methods or unexport without touching out-of-package
// callers — and Egress(id), whose pointer identity internal/routing pins as
// the proof that a RoutePlan resolves against a single snapshot. Both are
// READ-ONLY by contract; no in-repo code writes through them. What Resolve
// guarantees regardless is that a snapshot never aliases its loader's input:
// every pointer/slice field is deep-copied there, so input mutation and
// snapshot state can never meet.
type Runtime struct {
	File     File
	byID     map[string]*Egress
	routes   []Route // Priority desc, file order tiebreak (stable sort)
	Fallback FallbackPolicy
	Health   HealthPolicy
	// Direct marks the synthetic single-egress runtime used when no config
	// file is set — byte-identical behavior to the pre-routing proxy.
	Direct bool
	// Generation is the store's monotonic config version: first file load 1,
	// each successful swap +1, the built-in default runtime 0. It names the
	// snapshot in logs/tests and drives once-per-generation health tuning.
	// Hot reload stamps only the NEW snapshot; an in-flight request keeps
	// the generation it started with.
	Generation uint64
}

// Egress resolves an egress id from this snapshot. The returned pointer
// ALIASES the snapshot's own egress: it is READ-ONLY by contract, because the
// scheduler pins exactly these pointers into a request's RoutePlan (one
// request = one snapshot) and the executor dials from that pinned list for
// the request's whole lifetime.
//
// Accessor audit (snapshot hardening): every in-repo caller only reads
// through it (routing's resolveEgresses/weightOf, the router's routeHeads/
// routeHealthKeys/generationKeepSets/clientForEgress, health.ActiveKeys, the
// upstream budget/fallback tests). The strict fix — a private per-call copy —
// was built and then rejected here: internal/routing's
// TestPlanEgressesResolvedFromSnapshot asserts pointer identity with
// rt.Egress as its proof that a plan resolves against a single snapshot, and
// that package is outside this hardening's scope. Until a coordinated change
// lands there, this is the ONE accessor that hands out snapshot memory —
// deliberately, documented, and unused for writes anywhere in the repo.
func (rt *Runtime) Egress(id string) (*Egress, bool) {
	e, ok := rt.byID[id]
	return e, ok
}

// Routes returns the snapshot's routes in deterministic match order as a deep
// copy — mutating the result, including nested Match.Models and Egress
// backing arrays, never reaches the live snapshot. It runs once per
// generation (keep-set derivation, tests), never per request; MatchRoute is
// the per-request accessor.
func (rt *Runtime) Routes() []Route {
	out := make([]Route, len(rt.routes))
	for i := range rt.routes {
		out[i] = cloneRoute(&rt.routes[i])
	}
	return out
}

// MatchRoute returns the first route whose conditions hold for the profile,
// as a deep copy (the returned Match.Models and Egress slices are private).
// Order is priority desc then file order — never random.
func (rt *Runtime) MatchRoute(streaming bool, bodyBytes int64, model string) (Route, bool) {
	for i := range rt.routes {
		if matchHolds(rt.routes[i].Match, streaming, bodyBytes, model) {
			return cloneRoute(&rt.routes[i]), true
		}
	}
	return Route{}, false
}

func matchHolds(m Match, streaming bool, bodyBytes int64, model string) bool {
	if m.Streaming != nil && *m.Streaming != streaming {
		return false
	}
	if m.MinBodyBytes > 0 && bodyBytes < m.MinBodyBytes {
		return false
	}
	if m.MaxBodyBytes > 0 && bodyBytes > m.MaxBodyBytes {
		return false
	}
	for _, pat := range m.Models {
		if !globMatch(pat, model) {
			return false
		}
	}
	return true
}

// globMatch reports whether the model id matches one glob pattern. Exact ids
// match themselves; "*" is the only wildcard (path.Match semantics, no
// expression language).
func globMatch(pattern, model string) bool {
	if pattern == model {
		return true
	}
	ok, err := path.Match(pattern, model)
	return err == nil && ok
}

// boundedEcho bounds an input-derived value echoed into a load error. Those
// errors are logged verbatim — at startup and on a rejected reload — so a
// hostile config line must not be able to bloat a log or smuggle unbounded
// bytes into an error surface. The convention mirrors yaml.v3's scalar echo
// (decode.go, decoder.terror): a value over 10 bytes renders as its first 7
// plus "...". The one deliberate exception is Interpolate's unset-variable
// name, documented at its site.
func boundedEcho(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:7] + "..."
}

// stripQuoted removes every "…" span from a text. Parse errors
// (time.ParseDuration among them) echo the offending input back inside
// quotes, which would defeat boundedEcho on the wrapping site — the useful
// reason survives, the unbounded original does not. The example test reuses
// it for the same shape of problem: stripping quoted YAML scalars from a line
// so a placeholder check can look at the comment text alone.
func stripQuoted(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i+1:]
		j := strings.IndexByte(s, '"')
		if j < 0 {
			return b.String()
		}
		s = s[j+1:]
	}
}

// Validate applies the full structural + semantic check set in a fixed
// order, returning the first failure with an actionable path ("egress
// proxy-b: …", "route streaming: …"). Every echoed input value goes through
// boundedEcho — see its comment for why.
func (f *File) Validate() error {
	seen := map[string]bool{}
	if len(f.Egress) == 0 {
		return errors.New("egress: at least one egress is required")
	}
	if len(f.Routes) == 0 {
		return errors.New("routes: at least one route is required")
	}
	for i := range f.Egress {
		e := &f.Egress[i]
		if strings.TrimSpace(e.ID) == "" {
			return fmt.Errorf("egress[%d]: id is required", i)
		}
		if hasControlByte(e.ID) {
			// The id is not echoed: it IS the control-byte payload. Ids reach
			// log lines and the X-OFP-Egress response header verbatim.
			return fmt.Errorf("egress[%d]: id must not contain control characters", i)
		}
		eid := boundedEcho(e.ID)
		if seen[e.ID] {
			return fmt.Errorf("egress %q: duplicate id", eid)
		}
		seen[e.ID] = true
		if e.Weight != nil && *e.Weight < 0 {
			return fmt.Errorf("egress %q: weight must be >= 0 (0 = configured but never scheduled)", eid)
		}
		if e.MaxConcurrency < 0 {
			return fmt.Errorf("egress %q: max_concurrency must be >= 0 (0 = unlimited)", eid)
		}
		if e.MaxBodyBytes < 0 {
			return fmt.Errorf("egress %q: max_body_bytes must be >= 0 (0 = unlimited)", eid)
		}
		for _, pat := range e.Models {
			if _, err := path.Match(pat, ""); err != nil {
				// path.ErrBadPattern is a constant sentence — it never echoes
				// the pattern back, so only the bounded echo carries input.
				return fmt.Errorf("egress %q: invalid model pattern %q: %w", eid, boundedEcho(pat), err)
			}
		}
		if e.Proxy == nil {
			continue
		}
		if err := validateProxy(eid, e.Proxy); err != nil {
			return err
		}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, boundedEcho(id))
	}
	sort.Strings(ids)
	routeSeen := map[string]bool{}
	for i := range f.Routes {
		r := &f.Routes[i]
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("route[%d]: id is required", i)
		}
		if hasControlByte(r.ID) {
			return fmt.Errorf("route[%d]: id must not contain control characters", i)
		}
		rid := boundedEcho(r.ID)
		if routeSeen[r.ID] {
			return fmt.Errorf("route %q: duplicate id", rid)
		}
		routeSeen[r.ID] = true
		switch r.Strategy {
		case "", StrategyRoundRobin, StrategyWeightedRR:
		default:
			return fmt.Errorf("route %q: unknown strategy %q (want %q or %q)", rid, boundedEcho(string(r.Strategy)), StrategyRoundRobin, StrategyWeightedRR)
		}
		if len(r.Egress) == 0 {
			return fmt.Errorf("route %q: egress list is empty", rid)
		}
		if r.Match.MinBodyBytes < 0 {
			return fmt.Errorf("route %q: match.min_body_bytes must be >= 0", rid)
		}
		if r.Match.MaxBodyBytes < 0 {
			return fmt.Errorf("route %q: match.max_body_bytes must be >= 0", rid)
		}
		if r.Match.MinBodyBytes > 0 && r.Match.MaxBodyBytes > 0 && r.Match.MinBodyBytes > r.Match.MaxBodyBytes {
			return fmt.Errorf("route %q: match.min_body_bytes (%d) > match.max_body_bytes (%d)", rid, r.Match.MinBodyBytes, r.Match.MaxBodyBytes)
		}
		for _, pat := range r.Match.Models {
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("route %q: invalid match model pattern %q: %w", rid, boundedEcho(pat), err)
			}
		}
		refSeen := map[string]bool{}
		for _, ref := range r.Egress {
			if !seen[ref] {
				return fmt.Errorf("route %q: unknown egress %q (configured: %s)", rid, boundedEcho(ref), strings.Join(ids, ", "))
			}
			// A repeated ref would let ONE request dial the same egress twice —
			// two health strikes on one identity and two draws against the
			// max_attempts contract ("DISTINCT egresses one request may try").
			if refSeen[ref] {
				return fmt.Errorf("route %q: egress %q is listed more than once (each egress is tried at most once per request)", rid, boundedEcho(ref))
			}
			refSeen[ref] = true
		}
		if r.Strategy == StrategyWeightedRR {
			usable := false
			for _, ref := range r.Egress {
				if w := weightOf(&f.Egress[indexByID(f.Egress, ref)]); w > 0 {
					usable = true
					break
				}
			}
			if !usable {
				return fmt.Errorf("route %q: weighted_round_robin needs at least one egress with weight > 0", rid)
			}
		}
	}

	if f.Fallback.MaxAttempts < 0 {
		return fmt.Errorf("fallback: max_attempts must be >= 0 (0 = default 3)")
	}
	if f.Health.FailureThreshold != nil && *f.Health.FailureThreshold < 0 {
		return fmt.Errorf("health: failure_threshold must be >= 0 (0 = never cool down)")
	}
	if f.Health.Cooldown != nil && *f.Health.Cooldown < 0 {
		return fmt.Errorf("health: cooldown must be >= 0")
	}
	return nil
}

// HealthThreshold returns the effective failure threshold (default applied
// when the field is unset; explicit 0 survives as "never cool down").
func (rt *Runtime) HealthThreshold() int {
	if rt.Health.FailureThreshold == nil {
		return defaultHealthThreshold
	}
	return *rt.Health.FailureThreshold
}

// HealthCooldown is the effective cooldown, mirroring HealthThreshold.
func (rt *Runtime) HealthCooldown() time.Duration {
	if rt.Health.Cooldown == nil {
		return defaultHealthCooldown
	}
	return time.Duration(*rt.Health.Cooldown)
}

// validateProxy checks the proxy type/scheme pair. socks5h is rejected BY
// NAME — silently normalizing it into socks5 would flip DNS resolution to
// the proxy side without the operator noticing. id arrives already bounded
// (Validate only calls this with boundedEcho(e.ID)); the type and url echoes
// below are bounded here — the url only after RedactProxyURL, so the bound
// can never reintroduce what redaction removed.
func validateProxy(id string, p *Proxy) error {
	switch p.Type {
	case ProxyHTTP, ProxyHTTPS, ProxySOCKS5:
	default:
		if p.Type == "socks5h" {
			return fmt.Errorf("egress %q: proxy type \"socks5h\" is not supported (remote-DNS semantics are deliberately out of scope; use \"socks5\", which resolves hostnames locally)", id)
		}
		return fmt.Errorf("egress %q: unknown proxy type %q (want http, https, or socks5)", id, boundedEcho(string(p.Type)))
	}
	if p.URL == "" {
		return fmt.Errorf("egress %q: proxy url is required", id)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		// *url.Error's own text embeds the RAW url — credentials included —
		// and this error is logged verbatim on a rejected reload (the config
		// store) and at startup. Surface only the underlying reason:
		// credentials never reach a surfaced string (redact.go's contract).
		reason := err
		if inner := errors.Unwrap(err); inner != nil {
			reason = inner
		}
		return fmt.Errorf("egress %q: malformed proxy url: %v", id, reason)
	}
	switch {
	case u.Scheme == string(ProxyType(p.Type)):
	case p.Type == ProxySOCKS5 && u.Scheme == "socks5h":
		return fmt.Errorf("egress %q: proxy url scheme \"socks5h://\" is not supported (use \"socks5://\" — hostnames resolve locally)", id)
	default:
		return fmt.Errorf("egress %q: proxy url scheme %q does not match type %q", id, boundedEcho(RedactProxyURL(p.URL)), p.Type)
	}
	if u.Host == "" {
		return fmt.Errorf("egress %q: proxy url %q has no host", id, boundedEcho(RedactProxyURL(p.URL)))
	}
	return nil
}

func indexByID(egs []Egress, id string) int {
	for i := range egs {
		if egs[i].ID == id {
			return i
		}
	}
	return -1
}

func weightOf(e *Egress) int { return e.weight() }

// cloneEgress deep-copies every pointer and slice so the snapshot shares no
// memory with the loader's parse result.
func cloneEgress(e *Egress) Egress {
	c := *e
	if e.Proxy != nil {
		p := *e.Proxy
		c.Proxy = &p
	}
	c.Enabled = cloneBoolPtr(e.Enabled)
	c.Weight = cloneIntPtr(e.Weight)
	c.Streaming = cloneBoolPtr(e.Streaming)
	c.Models = append([]string(nil), e.Models...)
	return c
}

func cloneRoute(r *Route) Route {
	c := *r
	c.Match.Streaming = cloneBoolPtr(r.Match.Streaming)
	c.Match.Models = append([]string(nil), r.Match.Models...)
	c.Egress = append([]string(nil), r.Egress...)
	return c
}

func cloneBoolPtr(b *bool) *bool {
	if b == nil {
		return nil
	}
	return new(*b)
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	return new(*v)
}

func cloneDurationPtr(d *Duration) *Duration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

// Resolve validates the file and builds the immutable Runtime snapshot with
// defaults applied. EVERY pointer- and slice-typed field is deep-copied —
// elements, pointees, and backing arrays — so the snapshot shares no memory
// with the loader's parse result: a caller mutating anything reachable from
// its input after Resolve can never observe (or cause) a snapshot change.
func (f *File) Resolve() (*Runtime, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	cp := *f
	cp.Egress = make([]Egress, len(f.Egress))
	for i := range f.Egress {
		cp.Egress[i] = cloneEgress(&f.Egress[i])
	}
	cp.Routes = make([]Route, len(f.Routes))
	for i := range f.Routes {
		cp.Routes[i] = cloneRoute(&f.Routes[i])
	}
	// Fallback/Health are value structs carrying pointers (*bool/*int/
	// *Duration): the struct copy above shares the POINTERS with the input, so
	// they are re-cloned here — otherwise *f.Fallback.Enabled = false after
	// Resolve would flip the snapshot's fallback switch.
	cp.Fallback = FallbackPolicy{Enabled: cloneBoolPtr(f.Fallback.Enabled)}
	cp.Health = HealthPolicy{
		Enabled:          cloneBoolPtr(f.Health.Enabled),
		FailureThreshold: cloneIntPtr(f.Health.FailureThreshold),
		Cooldown:         cloneDurationPtr(f.Health.Cooldown),
	}
	rt := &Runtime{File: cp, byID: make(map[string]*Egress, len(cp.Egress))}
	for i := range cp.Egress {
		rt.byID[cp.Egress[i].ID] = &rt.File.Egress[i]
	}
	// The match path reads rt.routes per request, so it gets its own deep copy
	// rather than a struct-copy of cp.Routes (which would share the nested
	// Match.Models/Egress backing arrays with the exported File field for no
	// benefit — every consumer path clones on the way out anyway).
	rt.routes = make([]Route, len(cp.Routes))
	for i := range cp.Routes {
		rt.routes[i] = cloneRoute(&cp.Routes[i])
	}
	sort.SliceStable(rt.routes, func(i, j int) bool {
		return rt.routes[i].Priority > rt.routes[j].Priority
	})
	rt.Fallback = FallbackPolicy{Enabled: new(f.Fallback.Enabled == nil || *f.Fallback.Enabled), MaxAttempts: defaultMaxAttempts}
	if f.Fallback.MaxAttempts > 0 {
		rt.Fallback.MaxAttempts = f.Fallback.MaxAttempts
	}
	rt.Health = HealthPolicy{
		Enabled: new(f.Health.Enabled == nil || *f.Health.Enabled),
	}
	if f.Health.FailureThreshold != nil {
		v := *f.Health.FailureThreshold
		rt.Health.FailureThreshold = &v
	}
	if f.Health.Cooldown != nil {
		c := *f.Health.Cooldown
		rt.Health.Cooldown = &c
	}
	// rt.Fallback/rt.Health above are built fresh (new pointers, never the
	// input's): the request path reads them via value-returning accessors, and
	// the deep copy of cp.Fallback/cp.Health keeps the exported File half
	// independent of the input too.
	return rt, nil
}

// DefaultRuntime is the no-config snapshot: one implicit direct egress, a
// catch-all route, fallback capped at the single attempt. Requests behave
// byte-identically to the pre-routing proxy — in particular health gating
// is DISABLED here: the old proxy had no failure-threshold outage, and a
// config-less deployment must not acquire one after 3 connection errors.
// File-driven configs opt into health by default (Resolve), which is the
// multi-egress feature's intent.
func DefaultRuntime() *Runtime {
	return &Runtime{
		File: File{
			Egress:   []Egress{{ID: "direct", Weight: new(defaultWeight)}},
			Routes:   []Route{{ID: "default", Egress: []string{"direct"}, Strategy: defaultStrategy}},
			Fallback: FallbackPolicy{Enabled: new(true), MaxAttempts: 1},
			Health:   HealthPolicy{Enabled: new(false)},
		},
		byID: map[string]*Egress{"direct": {ID: "direct", Weight: new(defaultWeight)}},
		routes: []Route{
			{ID: "default", Egress: []string{"direct"}, Strategy: defaultStrategy},
		},
		Fallback: FallbackPolicy{Enabled: new(true), MaxAttempts: 1},
		Health:   HealthPolicy{Enabled: new(false)},
		Direct:   true,
	}
}

// Interpolate expands ${VAR} references from the environment. An unset
// variable is a load error naming the variable — an empty expansion would
// surface later as a baffling "malformed proxy url".
func Interpolate(raw []byte) ([]byte, error) {
	var out strings.Builder
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			end := strings.IndexByte(string(raw[i+2:]), '}')
			if end < 0 {
				return nil, errors.New("unclosed ${…} reference")
			}
			name := string(raw[i+2 : i+2+end])
			if name == "" {
				return nil, errors.New("empty ${…} reference")
			}
			v, ok := lookupEnv(name)
			if !ok {
				// The name is deliberately echoed verbatim, not through
				// boundedEcho: this error exists to NAME the variable the
				// operator must export (AGENTS.md: "an unset ${VAR} is a load
				// error naming the variable") — the example's own 21-character
				// names would be mangled beyond use.
				return nil, fmt.Errorf("environment variable %s is not set (referenced in config)", name)
			}
			out.WriteString(v)
			i += 2 + end + 1
			continue
		}
		out.WriteByte(c)
		i++
	}
	return []byte(out.String()), nil
}
