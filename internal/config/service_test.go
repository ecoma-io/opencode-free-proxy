package config

// Tests for the runtime's service fields — upstream.base and
// user_agent.sync_interval — which replaced the removed OFP_UPSTREAM_BASE /
// OFP_UA_SYNC_INTERVAL env vars: they live in the OCFP_CONFIG document and
// land in the immutable per-generation Runtime snapshot exactly like
// egresses and routes.

import (
	"os"
	"strings"
	"testing"
	"time"
)

const serviceYAML = `
upstream:
  base: "https://zen.example"
user_agent:
  sync_interval: 120
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`

func TestRuntimeServiceFieldsResolve(t *testing.T) {
	rt, err := resolveYAML(t, serviceYAML)
	if err != nil {
		t.Fatalf("valid service config rejected: %v", err)
	}
	if got := rt.UpstreamBase(); got != "https://zen.example" {
		t.Fatalf("UpstreamBase() = %q, want https://zen.example", got)
	}
	if got := rt.UASyncInterval(); got != 120*time.Second {
		t.Fatalf("UASyncInterval() = %v, want 120s", got)
	}
}

func TestUpstreamBaseDefaultsTrailingSlashTrimmed(t *testing.T) {
	// Omitted upstream.base → the compiled-in default.
	rt, err := resolveYAML(t, "egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n")
	if err != nil {
		t.Fatalf("base-less config rejected: %v", err)
	}
	if got := rt.UpstreamBase(); got != UpstreamBase {
		t.Fatalf("UpstreamBase() = %q, want default %q", got, UpstreamBase)
	}
	// Trailing slash is normalized at resolve (never in the request path:
	// raw-string concat would produce //zen/... → 404).
	rt, err = resolveYAML(t, `upstream: {base: "https://zen.example/"}
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`)
	if err != nil {
		t.Fatalf("trailing-slash config rejected: %v", err)
	}
	if got := rt.UpstreamBase(); got != "https://zen.example" {
		t.Fatalf("UpstreamBase() = %q, want trailing slash trimmed", got)
	}
}

func TestUpstreamBaseValidation(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string // substring of the error, "" = valid
	}{
		{name: "no scheme", base: "zen.example", want: "scheme"},
		{name: "ftp scheme", base: "ftp://zen.example", want: "http"},
		{name: "userinfo rejected", base: "https://user:pass@zen.example", want: "userinfo"},
		{name: "query rejected", base: "https://zen.example?x=1", want: "query"},
		{name: "fragment rejected", base: "https://zen.example#frag", want: "fragment"},
		{name: "no host", base: "https://", want: "has no host"},
		{name: "valid https", base: "https://zen.example"},
		{name: "valid http", base: "http://zen.example:8080"},
		{name: "valid ip", base: "http://127.0.0.1:9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "upstream:\n  base: \"" + tc.base + "\"\n" +
				"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n"
			rt, err := resolveYAML(t, doc)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid base %q rejected: %v", tc.base, err)
				}
				if rt.UpstreamBase() != strings.TrimRight(tc.base, "/") {
					t.Fatalf("UpstreamBase() = %q, want %q", rt.UpstreamBase(), tc.base)
				}
				return
			}
			if err == nil {
				t.Fatalf("base %q accepted, want rejection (%s)", tc.base, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestUASyncIntervalResolution(t *testing.T) {
	base := "egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n"
	// Omitted → the 1 h default.
	rt, err := resolveYAML(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.UASyncInterval(); got != UASyncInterval {
		t.Fatalf("UASyncInterval() = %v, want default %v", got, UASyncInterval)
	}
	// Explicit 0 = disabled (a <= 0 sync_interval is a valid CONFIG; the
	// identity loop treats it as "idle, never fetch"). Negative is a load
	// error — a bad value must be rejected at the boundary, not guessed at.
	rt, err = resolveYAML(t, "user_agent:\n  sync_interval: 0\n"+base)
	if err != nil {
		t.Fatalf("sync_interval: 0 rejected: %v", err)
	}
	if rt.UASyncInterval() != 0 {
		t.Fatalf("UASyncInterval() = %v after explicit 0, want 0", rt.UASyncInterval())
	}
	if _, err := resolveYAML(t, "user_agent:\n  sync_interval: -5\n"+base); err == nil {
		t.Fatal("negative sync_interval accepted, want rejection")
	}
}

func TestStoreReloadSwapsServiceFields(t *testing.T) {
	// The service fields ride the same immutable per-generation snapshot as
	// egresses and routes: the live store swaps to the new values, and a
	// snapshot captured BEFORE the reload keeps the old ones (a request never
	// observes a mixed generation).
	dir := t.TempDir()
	gen1 := "upstream:\n  base: \"https://gen1.example\"\n" +
		"user_agent:\n  sync_interval: 120\n" +
		"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n"
	gen2 := "upstream:\n  base: \"https://gen2.example\"\n" +
		"user_agent:\n  sync_interval: 0\n" +
		"egress:\n  - {id: direct}\nroutes:\n  - {id: default, egress: [direct]}\n"
	p := writeConfig(t, dir, "cfg.yaml", gen1)
	store, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)

	before := store.Get()
	if got := before.UpstreamBase(); got != "https://gen1.example" {
		t.Fatalf("gen1 UpstreamBase() = %q", got)
	}
	if got := before.UASyncInterval(); got != 120*time.Second {
		t.Fatalf("gen1 UASyncInterval() = %v, want 120s", got)
	}

	if err := os.WriteFile(p, []byte(gen2), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for store.Get().Generation < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	after := store.Get()
	if after.Generation != 2 {
		t.Fatalf("generation = %d, want 2", after.Generation)
	}
	if got := after.UpstreamBase(); got != "https://gen2.example" {
		t.Fatalf("gen2 UpstreamBase() = %q, want https://gen2.example", got)
	}
	if got := after.UASyncInterval(); got != 0 {
		t.Fatalf("gen2 UASyncInterval() = %v, want 0 (disabled)", got)
	}
	// The pre-reload capture is untouched — the snapshot immutability
	// contract covers the service fields too.
	if got := before.UpstreamBase(); got != "https://gen1.example" {
		t.Fatalf("pre-reload snapshot mutated: UpstreamBase() = %q", got)
	}
	if got := before.UASyncInterval(); got != 120*time.Second {
		t.Fatalf("pre-reload snapshot mutated: UASyncInterval() = %v", got)
	}
}
