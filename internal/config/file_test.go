package config

import (
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

// resolveYAML runs the same load path as store.LoadFile (interpolate →
// parse → resolve) without touching the filesystem.
func resolveYAML(t *testing.T, doc string) (*Runtime, error) {
	t.Helper()
	raw, err := Interpolate([]byte(doc))
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return f.Resolve()
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

func TestResolveCopiesInput(t *testing.T) {
	t.Setenv("PROXY_A_URL", "http://a:1")
	t.Setenv("PROXY_B_URL", "http://b:1")
	var f File
	if err := yaml.Unmarshal([]byte(`
egress: [{id: a, weight: 1}]
routes: [{id: r, egress: [a]}]
`), &f); err != nil {
		t.Fatal(err)
	}
	rt, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the loader's slice must not reach the snapshot.
	f.Egress[0].ID = "mutated"
	if e, _ := rt.Egress("a"); e == nil {
		t.Fatal("snapshot egress mutated by loader slice")
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
