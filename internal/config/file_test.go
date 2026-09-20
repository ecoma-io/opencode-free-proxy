package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// validYAML is a fully-valid three-egress config exercising every section.
const validYAML = `
egress:
  - id: proxy-a
    proxy:
      type: http
      url: "${PROXY_A_URL}"
    enabled: true
    weight: 5
    max_concurrency: 20
  - id: proxy-b
    proxy:
      type: socks5
      url: "${PROXY_B_URL}"
    enabled: true
    weight: 3
    max_concurrency: 10
  - id: proxy-c
    proxy:
      type: https
      url: "https://user:pass@proxy-c.example:8443"
    enabled: true
    weight: 2
routes:
  - id: streaming
    priority: 100
    match:
      streaming: true
    egress:
      - proxy-a
      - proxy-b
    strategy: weighted_round_robin
  - id: large-request
    priority: 90
    match:
      min_body_bytes: 5000000
    egress:
      - proxy-a
      - proxy-b
    strategy: round_robin
  - id: default
    priority: 0
    egress:
      - proxy-a
      - proxy-b
      - proxy-c
    strategy: weighted_round_robin
fallback:
  enabled: true
  max_attempts: 3
health:
  enabled: true
  failure_threshold: 3
  cooldown: 30s
`

// resolveYAML runs the same load path as the store (LoadBytes: interpolate →
// parse → validate → resolve) without touching the filesystem.
func resolveYAML(t *testing.T, doc string) (*Runtime, error) {
	t.Helper()
	return LoadBytes([]byte(doc))
}

func TestValidConfigResolves(t *testing.T) {
	t.Setenv("PROXY_A_URL", "http://a.example:8080")
	t.Setenv("PROXY_B_URL", "socks5://user:secret@b.example:1080")
	rt, err := resolveYAML(t, validYAML)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if rt.Direct {
		t.Fatal("file-driven config must not be the synthetic direct runtime")
	}
	if len(rt.File.Egress) != 3 {
		t.Fatalf("egresses = %d, want 3", len(rt.File.Egress))
	}
	if e, ok := rt.Egress("proxy-a"); !ok || e.Proxy.Type != ProxyHTTP {
		t.Fatalf("proxy-a lookup failed or wrong type: %+v", e)
	}
	// Interpolation reached the URL with credentials intact at parse time.
	if e, _ := rt.Egress("proxy-b"); e.Proxy.URL != "socks5://user:secret@b.example:1080" {
		t.Fatalf("proxy-b url not interpolated: %q", e.Proxy.URL)
	}
	if e, _ := rt.Egress("proxy-c"); e.weight() != 2 || !e.enabled() {
		t.Fatalf("proxy-c wrong: %+v", e)
	}
	if rt.Fallback.MaxAttempts != 3 || !*rt.Fallback.Enabled {
		t.Fatalf("fallback wrong: %+v", rt.Fallback)
	}
	if rt.HealthThreshold() != 3 || rt.HealthCooldown() != 30*time.Second {
		t.Fatalf("health wrong: %+v", rt.Health)
	}
	// Route order is priority desc (stable sort).
	var got []string
	for _, r := range rt.Routes() {
		if r.ID == "" {
			t.Fatal("route id lost")
		}
		got = append(got, r.ID)
	}
	want := []string{"streaming", "large-request", "default"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("route order = %v, want %v", got, want)
	}
}

// TestResolveCopiesInput pins the snapshot's isolation from its input: after
// Resolve, mutating EVERY nested field reachable from the loader's File —
// pointer targets, slice elements, scalar fields, whole elements, slice
// lengths — must leave the snapshot byte-for-byte unchanged. This is the
// immutability contract's input half (the accessor half lives in the
// Test*Accessor* tests below).
func TestResolveCopiesInput(t *testing.T) {
	var f File
	if err := yaml.Unmarshal([]byte(`
egress:
  - id: a
    proxy: {type: http, url: "http://a.example:1"}
    enabled: true
    weight: 2
    streaming: true
    models: ["m-*", "x"]
routes:
  - id: r
    priority: 5
    match: {streaming: true, models: ["m-*"], min_body_bytes: 1, max_body_bytes: 9}
    egress: [a]
    strategy: weighted_round_robin
fallback: {enabled: true, max_attempts: 2}
health: {enabled: true, failure_threshold: 4, cooldown: 12s}
`), &f); err != nil {
		t.Fatal(err)
	}
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	// Mutate every input field the snapshot could possibly still share.
	f.Egress[0].ID = "hijacked"
	f.Egress[0].MaxConcurrency = 99
	f.Egress[0].MaxBodyBytes = 99
	*f.Egress[0].Enabled = false
	*f.Egress[0].Weight = 99
	*f.Egress[0].Streaming = false
	f.Egress[0].Proxy.Type = ProxySOCKS5
	f.Egress[0].Proxy.URL = "socks5://evil:1"
	f.Egress[0].Models[0] = "hijacked"
	f.Routes[0].ID = "hijacked"
	f.Routes[0].Priority = -5
	f.Routes[0].Strategy = StrategyRoundRobin
	*f.Routes[0].Match.Streaming = false
	f.Routes[0].Match.Models[0] = "hijacked"
	f.Routes[0].Match.MinBodyBytes = 99
	f.Routes[0].Match.MaxBodyBytes = -99
	f.Routes[0].Egress[0] = "hijacked"
	*f.Fallback.Enabled = false
	f.Fallback.MaxAttempts = 99
	*f.Health.Enabled = false
	*f.Health.FailureThreshold = 99
	*f.Health.Cooldown = Duration(99 * time.Hour)
	// Whole elements and slice lengths: the snapshot must not share backing
	// arrays either.
	f.Egress[0] = Egress{ID: "hijacked"}
	f.Routes[0] = Route{ID: "hijacked"}
	f.Egress = append(f.Egress, Egress{ID: "extra"})
	f.Routes = f.Routes[:0]

	// The snapshot is exactly what was resolved.
	e, ok := rt.Egress("a")
	if !ok {
		t.Fatal("snapshot lost its egress")
	}
	if e.ID != "a" || e.MaxConcurrency != 0 || e.MaxBodyBytes != 0 {
		t.Fatalf("egress scalar mutated through the input: %+v", e)
	}
	if e.Proxy == nil || e.Proxy.Type != ProxyHTTP || e.Proxy.URL != "http://a.example:1" {
		t.Fatalf("egress proxy mutated through the input: %+v", e.Proxy)
	}
	if !*e.Enabled || *e.Weight != 2 || !*e.Streaming {
		t.Fatalf("egress pointer targets mutated through the input: %+v", e)
	}
	if len(e.Models) != 2 || e.Models[0] != "m-*" || e.Models[1] != "x" {
		t.Fatalf("egress models mutated through the input: %v", e.Models)
	}
	rs := rt.Routes()
	if len(rs) != 1 {
		t.Fatalf("routes length mutated through the input: %d", len(rs))
	}
	r := rs[0]
	if r.ID != "r" || r.Priority != 5 || r.Strategy != StrategyWeightedRR {
		t.Fatalf("route mutated through the input: %+v", r)
	}
	if r.Match.Streaming == nil || !*r.Match.Streaming || len(r.Match.Models) != 1 || r.Match.Models[0] != "m-*" ||
		r.Match.MinBodyBytes != 1 || r.Match.MaxBodyBytes != 9 {
		t.Fatalf("route match mutated through the input: %+v", r.Match)
	}
	if len(r.Egress) != 1 || r.Egress[0] != "a" {
		t.Fatalf("route egress list mutated through the input: %v", r.Egress)
	}
	if m, ok := rt.MatchRoute(true, 5, "m-1"); !ok || m.ID != "r" {
		t.Fatalf("match table mutated through the input: %q/%v", m.ID, ok)
	}
	if rt.Fallback.MaxAttempts != 2 || rt.Fallback.Enabled == nil || !*rt.Fallback.Enabled {
		t.Fatalf("fallback policy mutated through the input: %+v", rt.Fallback)
	}
	if rt.Health.Enabled == nil || !*rt.Health.Enabled || rt.HealthThreshold() != 4 || rt.HealthCooldown() != 12*time.Second {
		t.Fatalf("health policy mutated through the input: %+v", rt.Health)
	}
}

func TestDefaultsWhenFieldsOmitted(t *testing.T) {
	rt, err := resolveYAML(t, `
egress:
  - id: a
routes:
  - id: default
    egress: [a]
`)
	if err != nil {
		t.Fatalf("minimal config rejected: %v", err)
	}
	e, _ := rt.Egress("a")
	if !e.enabled() || e.weight() != defaultWeight || e.MaxConcurrency != 0 {
		t.Fatalf("egress defaults wrong: %+v", e)
	}
	// Empty match is a catch-all.
	if r, ok := rt.MatchRoute(true, 1, "any-model"); !ok || r.ID != "default" {
		t.Fatalf("catch-all route did not match: %+v", r)
	}
	if rt.HealthThreshold() != defaultHealthThreshold {
		t.Fatalf("health threshold default = %d, want %d", rt.HealthThreshold(), defaultHealthThreshold)
	}
	if !*rt.Health.Enabled || !*rt.Fallback.Enabled {
		t.Fatal("fallback/health default to enabled")
	}
}

// TestExplicitZeroHealthNeverCools: an EXPLICIT failure_threshold/cooldown of
// 0 means "never cool down" / "no exclusion window" — it must survive
// Resolve, not silently become the defaults (0 is distinguishable from
// unset only through the pointer fields).
func TestExplicitZeroHealthNeverCools(t *testing.T) {
	rt, err := resolveYAML(t, `
egress:
  - id: a
routes:
  - id: default
    egress: [a]
health:
  failure_threshold: 0
  cooldown: 0s
`)
	if err != nil {
		t.Fatalf("explicit-zero health config rejected: %v", err)
	}
	if rt.HealthThreshold() != 0 {
		t.Fatalf("threshold = %d, want 0 (never cool down)", rt.HealthThreshold())
	}
	if rt.HealthCooldown() != 0 {
		t.Fatalf("cooldown = %v, want 0", rt.HealthCooldown())
	}
}

// TestExplicitNegativeHealthRejected: the pointer fields must still reject
// negative values (nil-safe validation).
func TestExplicitNegativeHealthRejected(t *testing.T) {
	if _, err := resolveYAML(t, `
egress:
  - id: a
routes:
  - id: default
    egress: [a]
health:
  failure_threshold: -1
`); err == nil || !strings.Contains(err.Error(), "failure_threshold") {
		t.Fatalf("err = %v, want failure_threshold rejection", err)
	}
}

func TestExplicitZeroWeightNeverScheduled(t *testing.T) {
	rt, err := resolveYAML(t, `
egress:
  - {id: a, weight: 0}
  - {id: b, weight: 2}
routes:
  - id: r
    egress: [a, b]
    strategy: weighted_round_robin
`)
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	e, _ := rt.Egress("a")
	if e.weight() != 0 {
		t.Fatalf("explicit weight 0 lost: %d", e.weight())
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{"malformed yaml", `egress: [`, "yaml"},
		{"no egress defined", `
routes: [{id: r, egress: [a]}]
`, "at least one egress"},
		{"no routes defined", `
egress: [{id: a}]
`, "at least one route"},
		{"duplicate egress id", `
egress:
  - {id: a}
  - {id: a}
routes: [{id: r, egress: [a]}]
`, "duplicate"},
		{"duplicate route id", `
egress: [{id: a}]
routes:
  - {id: r, egress: [a]}
  - {id: r, egress: [a]}
`, "duplicate"},
		{"missing egress id", `
egress: [{proxy: {type: http, url: "http://h:1"}}]
routes: [{id: r, egress: ["", ]}]
`, "id is required"},
		{"missing route id", `
egress: [{id: a}]
routes: [{egress: [a]}]
`, "id is required"},
		{"empty egress list in route", `
egress: [{id: a}]
routes: [{id: r, egress: []}]
`, "empty"},
		{"unknown egress reference", `
egress: [{id: a}]
routes: [{id: r, egress: [nope]}]
`, `unknown egress "nope" (configured: a)`},
		{"invalid strategy", `
egress: [{id: a}]
routes: [{id: r, egress: [a], strategy: shuffle}]
`, `unknown strategy "shuffle"`},
		{"unsupported proxy type", `
egress: [{id: a, proxy: {type: ftp, url: "ftp://h:1"}}]
routes: [{id: r, egress: [a]}]
`, "unknown proxy type"},
		{"socks5h type rejected", `
egress: [{id: a, proxy: {type: socks5h, url: "socks5h://h:1"}}]
routes: [{id: r, egress: [a]}]
`, "socks5h"},
		{"socks5h url scheme rejected", `
egress: [{id: a, proxy: {type: socks5, url: "socks5h://h:1"}}]
routes: [{id: r, egress: [a]}]
`, "socks5h"},
		{"malformed proxy url", `
egress: [{id: a, proxy: {type: http, url: "://no-host"}}]
routes: [{id: r, egress: [a]}]
`, "malformed proxy url"},
		{"scheme type mismatch", `
egress: [{id: a, proxy: {type: http, url: "socks5://h:1"}}]
routes: [{id: r, egress: [a]}]
`, "does not match type"},
		{"missing proxy host", `
egress: [{id: a, proxy: {type: http, url: "http://"}}]
routes: [{id: r, egress: [a]}]
`, "no host"},
		{"negative weight", `
egress: [{id: a, weight: -1}]
routes: [{id: r, egress: [a]}]
`, "weight"},
		{"negative max_concurrency", `
egress: [{id: a, max_concurrency: -1}]
routes: [{id: r, egress: [a]}]
`, "max_concurrency"},
		{"negative fallback attempts", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
fallback: {max_attempts: -1}
`, "max_attempts"},
		{"negative health threshold", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {failure_threshold: -1}
`, "failure_threshold"},
		{"negative cooldown", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {cooldown: -5s}
`, "cooldown"},
		{"bad cooldown duration", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {cooldown: 30}
`, "duration"},
		{"match min above max", `
egress: [{id: a}]
routes: [{id: r, egress: [a], match: {min_body_bytes: 10, max_body_bytes: 5}}]
`, "min_body_bytes"},
		{"weighted rr without positive weight", `
egress: [{id: a, weight: 0}, {id: b, weight: 0}]
routes: [{id: r, egress: [a, b], strategy: weighted_round_robin}]
`, "at least one egress with weight > 0"},
		{"invalid model pattern", `
egress: [{id: a, models: ["["]}]
routes: [{id: r, egress: [a]}]
`, "invalid model pattern"},
		{"duplicate egress reference in route", `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, a, b]}]
`, "more than once"},
		{"control character in egress id", `
egress: [{id: "a\0b"}]
routes: [{id: r, egress: [a]}]
`, "control characters"},
		{"control character in route id", `
egress: [{id: a}]
routes: [{id: "r\0x", egress: [a]}]
`, "control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveYAML(t, tc.doc)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestMatchConditions(t *testing.T) {
	rt, err := resolveYAML(t, `
egress: [{id: a}, {id: b}]
routes:
  - id: streaming
    priority: 100
    match: {streaming: true}
    egress: [a, b]
  - id: big
    priority: 90
    match: {min_body_bytes: 100}
    egress: [a]
  - id: muse
    priority: 80
    match: {models: ["muse-*"]}
    egress: [b]
  - id: default
    priority: 0
    egress: [a, b]
`)
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	cases := []struct {
		streaming bool
		size      int64
		model     string
		want      string
	}{
		{true, 10, "x", "streaming"},
		{false, 1000, "x", "big"},
		{false, 50, "muse-spark", "muse"},
		{false, 50, "other", "default"},
	}
	for _, tc := range cases {
		r, ok := rt.MatchRoute(tc.streaming, tc.size, tc.model)
		if !ok || r.ID != tc.want {
			t.Fatalf("MatchRoute(%v,%d,%q) = %q/%v, want %s", tc.streaming, tc.size, tc.model, r.ID, ok, tc.want)
		}
	}
	// Tie → file order, not priority collision randomness.
	a, aok := rt.MatchRoute(false, 10, "muse-1")
	b1, bok := rt.MatchRoute(false, 10, "muse-2")
	if !aok || !bok || a.ID != b1.ID {
		t.Fatalf("deterministic tie broken: %q != %q", a.ID, b1.ID)
	}
}

func TestDefaultRuntime(t *testing.T) {
	rt := DefaultRuntime()
	if !rt.Direct {
		t.Fatal("DefaultRuntime must be Direct")
	}
	r, ok := rt.MatchRoute(false, 0, "anything")
	if !ok || r.ID != "default" || len(r.Egress) != 1 || r.Egress[0] != "direct" {
		t.Fatalf("default route wrong: %+v", r)
	}
	if rt.Fallback.MaxAttempts != 1 {
		t.Fatalf("default fallback max_attempts = %d, want 1 (no fallback)", rt.Fallback.MaxAttempts)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, model string
		want       bool
	}{
		{"muse-spark", "muse-spark", true},
		{"muse-*", "muse-spark-1.2", true},
		{"muse-*", "other", false},
		{"exact", "exact", true},
		{"exact", "exact-2", false},
		{"[", "x", false}, // invalid pattern never matches, never errors
	}
	for _, tc := range cases {
		if got := globMatch(tc.pat, tc.model); got != tc.want {
			t.Fatalf("globMatch(%q,%q) = %v, want %v", tc.pat, tc.model, got, tc.want)
		}
	}
}

func TestInterpolate(t *testing.T) {
	t.Setenv("PROXY_A_URL", "http://a:1")
	got, err := Interpolate([]byte(`url: "${PROXY_A_URL}/path"`))
	if err != nil || string(got) != "url: \"http://a:1/path\"" {
		t.Fatalf("interpolate = %q, %v", got, err)
	}
	if _, err := Interpolate([]byte("${NOPE}")); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("missing var: want error naming NOPE, got %v", err)
	}
	if _, err := Interpolate([]byte("${NOPE")); err == nil {
		t.Fatal("unclosed reference must error")
	}
	got, err = Interpolate([]byte(`price $5`))
	if err != nil || string(got) != "price $5" {
		t.Fatalf("dollar passthrough = %q, %v", got, err)
	}
}

func TestRedactProxyURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://user:secret@h:8080", "http://h:8080"},
		{"https://user:secret@h:8443", "https://h:8443"},
		{"socks5://user:secret@h:1080", "socks5://h:1080"},
		{"http://h:8080", "http://h:8080"},
		{"not a url", "<invalid proxy url>"},
	}
	for _, tc := range cases {
		if got := RedactProxyURL(tc.in); got != tc.want {
			t.Fatalf("RedactProxyURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if HasSecret(RedactProxyURL("http://user:secret@h:8080")) {
		t.Fatal("redacted url still carries credentials")
	}
	if !HasSecret("dialing http://user:secret@h:8080 failed") {
		t.Fatal("HasSecret missed embedded credentials")
	}
}

// TestValidationErrorsNeverCarryProxyCredentials: a malformed proxy url's
// validation error is logged verbatim on a rejected reload (the config
// store) and at startup — so it must carry only url.Parse's REASON, never
// the *url.Error line, whose text embeds the raw url with the credentials
// in it (redact.go's contract, applied to the validation surface).
func TestValidationErrorsNeverCarryProxyCredentials(t *testing.T) {
	cases := []struct {
		name string
		url  string
		leak string // a substring that exists only in the raw url
	}{
		{"space in password", "http://admin:my secret@p.example:3128", "my secret"},
		{"bad percent escape", "http://admin:abc%zzsup3r@p.example:3128", "abc%zzsup3r"},
		{"control byte in url", "http://admin:s3cret\x01@p.example:3128", "s3cret"},
		{"unclosed ipv6 bracket", "http://admin:s3cret@[::1", "s3cret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := File{
				Egress: []Egress{{ID: "e", Proxy: &Proxy{Type: ProxyHTTP, URL: tc.url}}},
				Routes: []Route{{ID: "r", Egress: []string{"e"}}},
			}
			err := f.Validate()
			if err == nil {
				t.Fatalf("url %q must fail validation", tc.url)
			}
			if strings.Contains(err.Error(), tc.leak) {
				t.Fatalf("validation error leaks credential material %q: %q", tc.leak, err)
			}
			if strings.Contains(err.Error(), tc.url) {
				t.Fatalf("validation error embeds the raw url: %q", err)
			}
		})
	}
}

// TestValidationErrorsBoundEchoedValues: load errors are logged verbatim, so
// every input value they echo — ids, glob patterns, strategy names, egress
// refs, duration strings — is bounded (boundedEcho: >10 bytes render as the
// first 7 plus "...", mirroring yaml.v3's scalar echo). A hostile config line
// can name neither a log line's shape nor its size. The one deliberate
// exception is the unset-variable NAME in Interpolate, which is the error's
// actionable payload and stays verbatim (asserted by TestInterpolate).
func TestValidationErrorsBoundEchoedValues(t *testing.T) {
	long := strings.Repeat("v", 200)
	head := long[:7] + "..."
	cases := []struct {
		name string
		load func() error
	}{
		{"long egress id", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: %q, weight: -1}]
routes: [{id: r, egress: [%q]}]
`, long, long))
			return err
		}},
		{"long duplicate egress id", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: %q}, {id: %q}]
routes: [{id: r, egress: [%q]}]
`, long, long, long))
			return err
		}},
		{"long model pattern", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: a, models: ["%s"]}]
routes: [{id: r, egress: [a]}]
`, strings.Repeat("[", 200)))
			return err
		}},
		{"long unknown egress ref", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: a}]
routes: [{id: r, egress: [%q]}]
`, long))
			return err
		}},
		{"long route id with unknown egress", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: a}]
routes: [{id: %q, egress: [nope]}]
`, long))
			return err
		}},
		{"long strategy", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: a}]
routes: [{id: r, egress: [a], strategy: %q}]
`, long))
			return err
		}},
		{"long invalid duration", func() error {
			_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {cooldown: %q}
`, long))
			return err
		}},
		{"long non-string duration", func() error {
			_, err := resolveYAML(t, `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {cooldown: 30}
`)
			return err
		}},
		{"long non-string duration", func() error {
			_, err := resolveYAML(t, `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
health: {cooldown: 30}
`)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.load()
			if err == nil {
				t.Fatal("want a load error, got nil")
			}
			msg := err.Error()
			if len(msg) > 300 {
				t.Fatalf("error text unbounded (%d bytes): %q", len(msg), msg)
			}
			if strings.Contains(msg, long) {
				t.Fatalf("error echoes the full 200-byte input value: %q", msg)
			}
		})
	}
	// The bound keeps values identifiable: the truncation head carries the
	// first 7 bytes, so an operator still learns which value was rejected.
	_, err := resolveYAML(t, fmt.Sprintf(`
egress: [{id: %q, weight: -1}]
routes: [{id: r, egress: [%q]}]
`, long, long))
	if err == nil || !strings.Contains(err.Error(), head) {
		t.Fatalf("bounded echo %q missing from error %v", head, err)
	}
}

// accessorDoc builds a File exercising every pointer and slice a snapshot
// accessor can hand out.
const accessorDoc = `
egress:
  - id: a
    proxy: {type: http, url: "http://a.example:1"}
    enabled: true
    weight: 3
    streaming: true
    models: ["m-*"]
  - id: b
    proxy: {type: socks5, url: "socks5://b.example:2"}
routes:
  - id: stream
    priority: 10
    match: {streaming: true, models: ["m-*"]}
    egress: [a, b]
    strategy: weighted_round_robin
  - id: default
    priority: 0
    egress: [a, b]
`

// TestEgressAccessorPinnedSnapshotPointer: Egress(id) deliberately returns
// the snapshot's own pointer — internal/routing pins pointer identity as its
// proof that a RoutePlan resolves against ONE snapshot
// (TestPlanEgressesResolvedFromSnapshot there), so a per-call copy is a
// cross-package change, not a config-local one. This test pins the contract
// that contract relies on: stable pointer per (snapshot, id), content exactly
// the resolved egress, (nil, false) for unknown ids — and documents that
// writes through the pointer are forbidden (no in-repo caller does; see the
// accessor audit in the Egress doc comment).
func TestEgressAccessorPinnedSnapshotPointer(t *testing.T) {
	rt, err := resolveYAML(t, accessorDoc)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := rt.Egress("a")
	if !ok {
		t.Fatal("egress a missing")
	}
	e2, ok := rt.Egress("a")
	if !ok || e2 != e {
		t.Fatalf("two resolutions of the same id differ: %p vs %p — the pinning contract broke", e2, e)
	}
	if e.ID != "a" || e.Proxy == nil || e.Proxy.Type != ProxyHTTP || e.Proxy.URL != "http://a.example:1" {
		t.Fatalf("resolved content wrong: %+v", e)
	}
	if !e.IsEnabled() || !e.AcceptsStreaming() || e.EffectiveWeight() != 3 {
		t.Fatalf("resolved gates wrong: %+v", e)
	}
	if len(e.Models) != 1 || e.Models[0] != "m-*" {
		t.Fatalf("resolved models wrong: %v", e.Models)
	}
	// Pointer identity holds across the snapshot's own surfaces too: the id
	// map entry IS the File slice element (one egress, one truth).
	if &rt.File.Egress[0] != e {
		t.Fatalf("byID entry %p aliases a different egress than File %p", e, &rt.File.Egress[0])
	}
	// Unknown ids keep the documented (nil, false) shape.
	if g, ok := rt.Egress("nope"); ok || g != nil {
		t.Fatalf("unknown id = (%v, %v), want (nil, false)", g, ok)
	}
}

// TestRoutesAccessorReturnsDeepCopy: Routes() deep-copies — element order and
// the nested Match.Models/Egress backing arrays included — so the result can
// be mutated (health.ActiveKeys and the router's keep-set derivation range it)
// without reaching the snapshot or a second call's result.
func TestRoutesAccessorReturnsDeepCopy(t *testing.T) {
	rt, err := resolveYAML(t, accessorDoc)
	if err != nil {
		t.Fatal(err)
	}
	rs := rt.Routes()
	if len(rs) != 2 || rs[0].ID != "stream" || rs[1].ID != "default" {
		t.Fatalf("unexpected route order: %+v", rs)
	}
	// Mutate everything the copy exposes, including nested backing arrays.
	rs[0].ID = "hijacked"
	rs[0].Priority = -1
	rs[0].Strategy = StrategyRoundRobin
	*rs[0].Match.Streaming = false
	rs[0].Match.Models[0] = "hijacked"
	rs[0].Match.MinBodyBytes = 999
	rs[0].Egress[0] = "hijacked"
	// Length mutation too: appending to the copy must not touch the snapshot.
	rs = append(rs, Route{ID: "injected"})
	if len(rs) != 3 || rs[2].ID != "injected" {
		t.Fatalf("append to the copy misbehaved: %d", len(rs))
	}

	rs2 := rt.Routes()
	if len(rs2) != 2 {
		t.Fatalf("routes length mutated through the accessor: %d", len(rs2))
	}
	if rs2[0].ID != "stream" || rs2[0].Priority != 10 || rs2[0].Strategy != StrategyWeightedRR {
		t.Fatalf("snapshot route mutated through the accessor: %+v", rs2[0])
	}
	if rs2[0].Match.Streaming == nil || !*rs2[0].Match.Streaming || rs2[0].Match.Models[0] != "m-*" || rs2[0].Match.MinBodyBytes != 0 {
		t.Fatalf("snapshot match mutated through the accessor: %+v", rs2[0].Match)
	}
	if rs2[0].Egress[0] != "a" {
		t.Fatalf("snapshot egress list mutated through the accessor: %v", rs2[0].Egress)
	}
	if m, ok := rt.MatchRoute(true, 0, "m-1"); !ok || m.ID != "stream" {
		t.Fatalf("match table damaged: %q/%v", m.ID, ok)
	}
}

// TestMatchRouteReturnsDeepCopy: the per-request matched route is a private
// deep copy — its Match.Models and Egress slices are not snapshot backing
// arrays, so the pipeline can mutate the route it plans against freely.
func TestMatchRouteReturnsDeepCopy(t *testing.T) {
	rt, err := resolveYAML(t, accessorDoc)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rt.MatchRoute(true, 0, "m-1")
	if !ok {
		t.Fatal("stream route did not match")
	}
	r.ID = "hijacked"
	r.Priority = -1
	if r.Match.Streaming != nil {
		*r.Match.Streaming = false
	}
	r.Match.Models[0] = "hijacked"
	r.Egress[0] = "hijacked"
	r.Egress = append(r.Egress, "injected")

	// The next request matches the untouched route.
	r2, ok := rt.MatchRoute(true, 0, "m-1")
	if !ok || r2.ID != "stream" || r2.Priority != 10 {
		t.Fatalf("match table mutated through the returned route: %+v/%v", r2, ok)
	}
	if r2.Match.Models[0] != "m-*" || len(r2.Egress) != 2 || r2.Egress[0] != "a" {
		t.Fatalf("matched route slices aliased the snapshot: %v %v", r2.Match.Models, r2.Egress)
	}
	if e, _ := rt.Egress("a"); e.Proxy.URL != "http://a.example:1" {
		t.Fatalf("egress damaged through the matched route: %+v", e)
	}
	// Non-matching profiles still miss.
	if _, ok := rt.MatchRoute(false, 0, "m-1"); !ok {
		t.Fatal("catch-all route vanished")
	}
}
