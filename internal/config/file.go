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
// egress id, tuned by these fields).
type HealthPolicy struct {
	Enabled          *bool    `yaml:"enabled,omitempty"`
	FailureThreshold int      `yaml:"failure_threshold,omitempty"`
	Cooldown         Duration `yaml:"cooldown,omitempty"`
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
		return fmt.Errorf("duration must be a string like \"30s\", got %v", raw)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Runtime is the immutable post-validation snapshot a request holds for its
// whole lifetime. Hot reload swaps the pointer atomically; an in-flight
// request never observes a mutation.
type Runtime struct {
	File     File
	byID     map[string]*Egress
	routes   []Route // Priority desc, file order tiebreak (stable sort)
	Fallback FallbackPolicy
	Health   HealthPolicy
	// Direct marks the synthetic single-egress runtime used when no config
	// file is set — byte-identical behavior to the pre-routing proxy.
	Direct bool
}

// Egress resolves an egress id from this snapshot.
func (rt *Runtime) Egress(id string) (*Egress, bool) {
	e, ok := rt.byID[id]
	return e, ok
}

// Routes returns the snapshot's routes in deterministic match order.
func (rt *Runtime) Routes() []Route { return rt.routes }

// MatchRoute returns the first route whose conditions hold for the profile.
// Order is priority desc then file order — never random.
func (rt *Runtime) MatchRoute(streaming bool, bodyBytes int64, model string) (Route, bool) {
	for _, r := range rt.routes {
		if matchHolds(r.Match, streaming, bodyBytes, model) {
			return r, true
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

// Validate applies the full structural + semantic check set in a fixed
// order, returning the first failure with an actionable path ("egress
// proxy-b: …", "route streaming: …").
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
		if seen[e.ID] {
			return fmt.Errorf("egress %q: duplicate id", e.ID)
		}
		seen[e.ID] = true
		if e.Weight != nil && *e.Weight < 0 {
			return fmt.Errorf("egress %q: weight must be >= 0 (0 = configured but never scheduled)", e.ID)
		}
		if e.MaxConcurrency < 0 {
			return fmt.Errorf("egress %q: max_concurrency must be >= 0 (0 = unlimited)", e.ID)
		}
		if e.MaxBodyBytes < 0 {
			return fmt.Errorf("egress %q: max_body_bytes must be >= 0 (0 = unlimited)", e.ID)
		}
		for _, pat := range e.Models {
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("egress %q: invalid model pattern %q: %w", e.ID, pat, err)
			}
		}
		if e.Proxy == nil {
			continue
		}
		if err := validateProxy(e.ID, e.Proxy); err != nil {
			return err
		}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	routeSeen := map[string]bool{}
	for i := range f.Routes {
		r := &f.Routes[i]
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("route[%d]: id is required", i)
		}
		if routeSeen[r.ID] {
			return fmt.Errorf("route %q: duplicate id", r.ID)
		}
		routeSeen[r.ID] = true
		switch r.Strategy {
		case "", StrategyRoundRobin, StrategyWeightedRR:
		default:
			return fmt.Errorf("route %q: unknown strategy %q (want %q or %q)", r.ID, r.Strategy, StrategyRoundRobin, StrategyWeightedRR)
		}
		if len(r.Egress) == 0 {
			return fmt.Errorf("route %q: egress list is empty", r.ID)
		}
		if r.Match.MinBodyBytes < 0 {
			return fmt.Errorf("route %q: match.min_body_bytes must be >= 0", r.ID)
		}
		if r.Match.MaxBodyBytes < 0 {
			return fmt.Errorf("route %q: match.max_body_bytes must be >= 0", r.ID)
		}
		if r.Match.MinBodyBytes > 0 && r.Match.MaxBodyBytes > 0 && r.Match.MinBodyBytes > r.Match.MaxBodyBytes {
			return fmt.Errorf("route %q: match.min_body_bytes (%d) > match.max_body_bytes (%d)", r.ID, r.Match.MinBodyBytes, r.Match.MaxBodyBytes)
		}
		for _, pat := range r.Match.Models {
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("route %q: invalid match model pattern %q: %w", r.ID, pat, err)
			}
		}
		for _, ref := range r.Egress {
			if !seen[ref] {
				return fmt.Errorf("route %q: unknown egress %q (configured: %s)", r.ID, ref, strings.Join(ids, ", "))
			}
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
				return fmt.Errorf("route %q: weighted_round_robin needs at least one egress with weight > 0", r.ID)
			}
		}
	}

	if f.Fallback.MaxAttempts < 0 {
		return fmt.Errorf("fallback: max_attempts must be >= 0 (0 = only the scheduled egress)")
	}
	if f.Health.FailureThreshold < 0 {
		return fmt.Errorf("health: failure_threshold must be >= 0 (0 = never cool down)")
	}
	if f.Health.Cooldown < 0 {
		return fmt.Errorf("health: cooldown must be >= 0")
	}
	return nil
}

// validateProxy checks the proxy type/scheme pair. socks5h is rejected BY
// NAME — silently normalizing it into socks5 would flip DNS resolution to
// the proxy side without the operator noticing.
func validateProxy(id string, p *Proxy) error {
	switch p.Type {
	case ProxyHTTP, ProxyHTTPS, ProxySOCKS5:
	default:
		if p.Type == "socks5h" {
			return fmt.Errorf("egress %q: proxy type \"socks5h\" is not supported (remote-DNS semantics are deliberately out of scope; use \"socks5\", which resolves hostnames locally)", id)
		}
		return fmt.Errorf("egress %q: unknown proxy type %q (want http, https, or socks5)", id, p.Type)
	}
	if p.URL == "" {
		return fmt.Errorf("egress %q: proxy url is required", id)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return fmt.Errorf("egress %q: malformed proxy url: %v", id, err)
	}
	switch {
	case u.Scheme == string(ProxyType(p.Type)):
	case p.Type == ProxySOCKS5 && u.Scheme == "socks5h":
		return fmt.Errorf("egress %q: proxy url scheme \"socks5h://\" is not supported (use \"socks5://\" — hostnames resolve locally)", id)
	default:
		return fmt.Errorf("egress %q: proxy url scheme %q does not match type %q", id, RedactProxyURL(p.URL), p.Type)
	}
	if u.Host == "" {
		return fmt.Errorf("egress %q: proxy url %q has no host", id, RedactProxyURL(p.URL))
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

// Resolve validates the file and builds the immutable Runtime snapshot with
// defaults applied. The slice contents are copied — the snapshot shares no
// backing array with the loader, so callers can never observe a mutation.
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
	rt := &Runtime{File: cp, byID: make(map[string]*Egress, len(cp.Egress))}
	for i := range cp.Egress {
		rt.byID[cp.Egress[i].ID] = &rt.File.Egress[i]
	}
	rt.routes = append([]Route(nil), cp.Routes...)
	sort.SliceStable(rt.routes, func(i, j int) bool {
		return rt.routes[i].Priority > rt.routes[j].Priority
	})
	rt.Fallback = FallbackPolicy{Enabled: new(f.Fallback.Enabled == nil || *f.Fallback.Enabled), MaxAttempts: defaultMaxAttempts}
	if f.Fallback.MaxAttempts > 0 {
		rt.Fallback.MaxAttempts = f.Fallback.MaxAttempts
	}
	rt.Health = HealthPolicy{
		Enabled:          new(f.Health.Enabled == nil || *f.Health.Enabled),
		FailureThreshold: defaultHealthThreshold,
		Cooldown:         Duration(defaultHealthCooldown),
	}
	if f.Health.FailureThreshold > 0 {
		rt.Health.FailureThreshold = f.Health.FailureThreshold
	}
	if f.Health.Cooldown > 0 {
		rt.Health.Cooldown = f.Health.Cooldown
	}
	return rt, nil
}

// DefaultRuntime is the no-config snapshot: one implicit direct egress, a
// catch-all route, fallback capped at the single attempt. Requests behave
// byte-identically to the pre-routing proxy.
func DefaultRuntime() *Runtime {
	return &Runtime{
		File: File{
			Egress:   []Egress{{ID: "direct", Weight: new(defaultWeight)}},
			Routes:   []Route{{ID: "default", Egress: []string{"direct"}, Strategy: defaultStrategy}},
			Fallback: FallbackPolicy{Enabled: new(true), MaxAttempts: 1},
			Health:   HealthPolicy{Enabled: new(true), FailureThreshold: defaultHealthThreshold, Cooldown: Duration(defaultHealthCooldown)},
		},
		byID: map[string]*Egress{"direct": {ID: "direct", Weight: new(defaultWeight)}},
		routes: []Route{
			{ID: "default", Egress: []string{"direct"}, Strategy: defaultStrategy},
		},
		Fallback: FallbackPolicy{Enabled: new(true), MaxAttempts: 1},
		Health:   HealthPolicy{Enabled: new(true), FailureThreshold: defaultHealthThreshold, Cooldown: Duration(defaultHealthCooldown)},
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
