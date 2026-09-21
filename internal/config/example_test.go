package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The shipped example and the parser are locked together: these tests load
// ../../config.example.yaml through the REAL load path (LoadFile: read →
// interpolate → parse → validate → resolve) instead of an in-test fixture,
// so any schema or validation change that would break an operator's copy of
// the example fails CI here first.

// examplePath is the repo-root example, relative to this package's dir.
const examplePath = "../../config.example.yaml"

// Fake credentials in the e2e-fixture style (AGENTS.md: clearly fake values,
// never real ones). Each egress gets a distinct password so a copy-pasted or
// misrouted placeholder shows up as a wrong assertion, not a silent pass.
const (
	exampleUser      = "e2e-user"
	exampleHTTPPass  = "e2e-secret"
	exampleTLSPass   = "e2e-secret-tls"
	exampleSocksPass = "e2e-secret-socks"
	exampleAPIKey    = "e2e-secret-api"
)

// setExampleEnv pins every variable the example references. Unset variables
// are load errors by design (Interpolate), so the file is loadable only with
// the full set — exactly the contract an operator faces.
func setExampleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OFP_EXAMPLE_HTTP_USER", exampleUser)
	t.Setenv("OFP_EXAMPLE_HTTP_PASS", exampleHTTPPass)
	t.Setenv("OFP_EXAMPLE_HTTPS_USER", exampleUser)
	t.Setenv("OFP_EXAMPLE_HTTPS_PASS", exampleTLSPass)
	t.Setenv("OFP_EXAMPLE_SOCKS_USER", exampleUser)
	t.Setenv("OFP_EXAMPLE_SOCKS_PASS", exampleSocksPass)
	t.Setenv("OFP_EXAMPLE_API_KEY", exampleAPIKey)
}

func loadExample(t *testing.T) *Runtime {
	t.Helper()
	setExampleEnv(t)
	rt, err := LoadFile(examplePath)
	if err != nil {
		t.Fatalf("config.example.yaml rejected by the real loader: %v", err)
	}
	return rt
}

// TestExampleConfigLoads pins the example's resolved shape: egress roster,
// per-egress gates and weights, route order, and the fallback/health policy.
func TestExampleConfigLoads(t *testing.T) {
	rt := loadExample(t)
	if rt.Direct {
		t.Fatal("file-driven config must not be the synthetic direct runtime")
	}
	if len(rt.File.Egress) != 5 {
		t.Fatalf("egresses = %d, want 5", len(rt.File.Egress))
	}

	// Every supported proxy type appears exactly once; direct is the absence
	// of a proxy key. (The socks5 row carries a socks5:// url — the
	// socks5h:// scheme is equally valid at load; only the type NAME socks5h
	// is rejected.)
	cases := []struct {
		id     string
		ptype  ProxyType
		url    string
		weight int
	}{
		{"http-proxy", ProxyHTTP, "http://" + exampleUser + ":" + exampleHTTPPass + "@http-proxy.example.net:8080", 5},
		{"https-proxy", ProxyHTTPS, "https://" + exampleUser + ":" + exampleTLSPass + "@https-proxy.example.net:8443", 3},
		{"socks5-proxy", ProxySOCKS5, "socks5://" + exampleUser + ":" + exampleSocksPass + "@socks-proxy.example.net:1080", 1},
	}
	for _, tc := range cases {
		e, ok := rt.Egress(tc.id)
		if !ok {
			t.Fatalf("egress %q missing", tc.id)
		}
		if e.Proxy == nil || e.Proxy.Type != tc.ptype {
			t.Fatalf("egress %q type = %v, want %v", tc.id, e.Proxy, tc.ptype)
		}
		// Interpolation reached the URL with credentials intact at parse time.
		if e.Proxy.URL != tc.url {
			t.Fatalf("egress %q url = %q, want %q", tc.id, e.Proxy.URL, tc.url)
		}
		if e.EffectiveWeight() != tc.weight {
			t.Fatalf("egress %q weight = %d, want %d", tc.id, e.EffectiveWeight(), tc.weight)
		}
	}

	// Per-egress gates.
	httpProxy, _ := rt.Egress("http-proxy")
	if httpProxy.MaxConcurrency != 8 || !httpProxy.IsEnabled() || !httpProxy.AcceptsStreaming() || len(httpProxy.Models) != 0 || httpProxy.MaxBodyBytes != 0 {
		t.Fatalf("http-proxy gates wrong: %+v", httpProxy)
	}
	httpsProxy, _ := rt.Egress("https-proxy")
	if httpsProxy.MaxBodyBytes != 4194304 {
		t.Fatalf("https-proxy max_body_bytes = %d, want 4194304", httpsProxy.MaxBodyBytes)
	}
	socksProxy, _ := rt.Egress("socks5-proxy")
	if socksProxy.MaxConcurrency != 4 {
		t.Fatalf("socks5-proxy max_concurrency = %d, want 4", socksProxy.MaxConcurrency)
	}
	if got := strings.Join(socksProxy.Models, ","); got != "*-free,big-pickle" {
		t.Fatalf("socks5-proxy models = %q, want *-free,big-pickle", got)
	}
	// The standby egress: explicit weight 0 survives Resolve (never heads a
	// weighted route while a positive-weight sibling is eligible) and its
	// streaming gate is off.
	lan, _ := rt.Egress("lan-proxy")
	if lan.EffectiveWeight() != 0 {
		t.Fatalf("lan-proxy weight = %d, want explicit 0", lan.EffectiveWeight())
	}
	if lan.AcceptsStreaming() {
		t.Fatal("lan-proxy must not accept streaming")
	}
	if lan.Proxy == nil || HasSecret(lan.Proxy.URL) {
		t.Fatalf("lan-proxy must be a credential-less proxy, got %v", lan.Proxy)
	}
	// Direct egress: nil proxy, "direct" transport identity.
	direct, _ := rt.Egress("direct")
	if direct.Proxy != nil || direct.TransportSignature() != "direct" {
		t.Fatalf("direct egress wrong: proxy=%v sig=%q", direct.Proxy, direct.TransportSignature())
	}
	// Health state identity: egress id + transport signature (proxy type,
	// colon, full url), NUL-separated.
	if lan.HealthKey() != "lan-proxy\x00http:http://10.10.0.9:3128" {
		t.Fatalf("lan-proxy health key = %q", lan.HealthKey())
	}

	// Route order: priority descending, file order breaking ties (the two
	// priority-80 model routes keep their file order).
	var order []string
	for _, r := range rt.Routes() {
		order = append(order, r.ID)
	}
	if want := "streaming-requests,large-bodies,free-suffix,big-pickle,default"; strings.Join(order, ",") != want {
		t.Fatalf("route order = %v, want %s", order, want)
	}
	// The two weighted rotations and a plain round-robin route, by id.
	strategies := map[string]Strategy{}
	for _, r := range rt.Routes() {
		strategies[r.ID] = r.Strategy
	}
	if strategies["streaming-requests"] != StrategyWeightedRR || strategies["default"] != StrategyWeightedRR {
		t.Fatalf("weighted routes lost their strategy: %+v", strategies)
	}
	if strategies["large-bodies"] != StrategyRoundRobin {
		t.Fatalf("large-bodies strategy = %q, want round_robin", strategies["large-bodies"])
	}

	// Global policies and their effective defaults.
	if rt.Fallback.Enabled == nil || !*rt.Fallback.Enabled || rt.Fallback.MaxAttempts != 3 {
		t.Fatalf("fallback policy wrong: %+v", rt.Fallback)
	}
	if rt.Health.Enabled == nil || !*rt.Health.Enabled {
		t.Fatal("health must default to enabled with a config file")
	}
	if rt.HealthThreshold() != 5 {
		t.Fatalf("health threshold = %d, want 5", rt.HealthThreshold())
	}
	if rt.HealthCooldown() != 45*time.Second {
		t.Fatalf("health cooldown = %v, want 45s", rt.HealthCooldown())
	}

	// Service settings: the three new OFP_CONFIG sections resolved into the
	// snapshot (upstream base, named inbound keys, UA sync cadence).
	if rt.UpstreamBase() != UpstreamBase {
		t.Fatalf("upstream base = %q, want the default %q", rt.UpstreamBase(), UpstreamBase)
	}
	if !rt.AuthEnabled() {
		t.Fatal("example config has auth.keys — auth must be enabled")
	}
	if name, ok := rt.LookupAPIKey(exampleAPIKey); !ok || name != "primary" {
		t.Fatalf("LookupAPIKey(key) = %q,%v, want primary,true", name, ok)
	}
	if _, ok := rt.LookupAPIKey("wrong-key"); ok {
		t.Fatal("LookupAPIKey must reject an unknown key")
	}
	if rt.UASyncInterval() != 3600*time.Second {
		t.Fatalf("UA sync interval = %v, want 3600s", rt.UASyncInterval())
	}
}

// TestExampleConfigRouteSelection pins the documented matching order: the
// FIRST route in priority order whose match holds serves the request, a
// route's model patterns are ANDed (so the example splits its two free-tier
// families into same-priority siblings), and body gates gate ROUTE SELECTION
// here (the server-level 8 MiB request cap is a different layer in
// internal/router/handler.go and rejects before routing).
func TestExampleConfigRouteSelection(t *testing.T) {
	rt := loadExample(t)
	cases := []struct {
		name      string
		streaming bool
		body      int64
		model     string
		want      string
	}{
		{"streaming wins over everything", true, 10, "whatever", "streaming-requests"},
		{"mid-size body", false, 2_000_000, "whatever", "large-bodies"},
		{"free-suffix glob", false, 500_000, "qwen3-coder-free", "free-suffix"},
		{"big-pickle needs its own route", false, 500_000, "big-pickle", "big-pickle"},
		{"plain model falls through", false, 500_000, "gpt-5", "default"},
		{"catch-all absorbs the oversized", false, 9_000_000, "whatever", "default"},
	}
	for _, tc := range cases {
		r, ok := rt.MatchRoute(tc.streaming, tc.body, tc.model)
		if !ok || r.ID != tc.want {
			t.Fatalf("%s: MatchRoute(%v,%d,%q) = %q/%v, want %q", tc.name, tc.streaming, tc.body, tc.model, r.ID, ok, tc.want)
		}
	}
}

// TestExampleConfigCarriesNoLiteralCredentials: the file on disk names the
// environment variables and never a secret; at runtime the credentials are
// present in the URLs (so the proxy hop authenticates) and gone from every
// redacted form (so no log path can leak them).
func TestExampleConfigCarriesNoLiteralCredentials(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(examplePath))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, fake := range []string{exampleUser, exampleHTTPPass, exampleTLSPass, exampleSocksPass, exampleAPIKey} {
		if strings.Contains(doc, fake) {
			t.Fatalf("example file embeds a literal credential value %q", fake)
		}
	}
	for _, name := range []string{
		"OFP_EXAMPLE_HTTP_USER", "OFP_EXAMPLE_HTTP_PASS",
		"OFP_EXAMPLE_HTTPS_USER", "OFP_EXAMPLE_HTTPS_PASS",
		"OFP_EXAMPLE_SOCKS_USER", "OFP_EXAMPLE_SOCKS_PASS",
		"OFP_EXAMPLE_API_KEY",
	} {
		if !strings.Contains(doc, name) {
			t.Fatalf("example file no longer references %s", name)
		}
	}

	rt := loadExample(t)
	for _, id := range []string{"http-proxy", "https-proxy", "socks5-proxy"} {
		e, _ := rt.Egress(id)
		if !HasSecret(e.Proxy.URL) {
			t.Fatalf("egress %q lost its userinfo during interpolation", id)
		}
		redacted := RedactProxyURL(e.Proxy.URL)
		if HasSecret(redacted) {
			t.Fatalf("egress %q redacted url still carries credentials: %q", id, redacted)
		}
		if strings.Contains(redacted, "@") {
			t.Fatalf("egress %q redacted url keeps userinfo: %q", id, redacted)
		}
	}
	// Inbound auth: the same contract as proxy credentials — the file names
	// the environment variable, the runtime resolves the value, and the
	// accessor hands back the NAME only (an accidental log of the lookup
	// result can never carry the secret).
	if name, ok := rt.LookupAPIKey(exampleAPIKey); !ok || name != "primary" {
		t.Fatalf("auth key not resolved from env: %q,%v", name, ok)
	}
}

// TestExampleCommentsCarryNoPlaceholder: the env interpolator scans the RAW
// file bytes — comments included (AGENTS.md) — so a ${…} reference inside a
// comment would silently demand an environment variable no operator knows to
// export, and the example would fail to load for everyone. The example keeps
// placeholder syntax out of comments entirely; this pins it. Quoted scalars
// are stripped first (stripQuoted drops every "…" span, mirroring what the
// parser reads as values): a placeholder INSIDE a quoted url value is the
// intended mechanism, not a violation.
func TestExampleCommentsCarryNoPlaceholder(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(examplePath))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(stripQuoted(line), "${") {
			t.Fatalf("example line %d carries a placeholder outside a quoted scalar: %q",
				i+1, strings.TrimSpace(line))
		}
	}
}
