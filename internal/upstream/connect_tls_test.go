package upstream

// The TLS floor contract for both TLS-config builders (code scanning
// go/missing-ssl-minversion, issue #18): the proxy hop (connect.go method)
// and the origin hop (package-level originTLSConfig, hello.go) each state
// TLS 1.2 explicitly on their literals, an injected config's unset floor is
// raised to it, and an injected config's explicit higher floor is never
// lowered.

import (
	"crypto/tls"
	"net/url"
	"testing"
)

func httpsProxy(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://proxy.internal:8443")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestProxyHopTLSFloor(t *testing.T) {
	u := httpsProxy(t)

	t.Run("literal states the floor", func(t *testing.T) {
		cfg := (&connectDialer{proxy: u}).proxyTLSConfig()
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("proxy-hop MinVersion = %x, want TLS 1.2 stated explicitly", cfg.MinVersion)
		}
		if cfg.ServerName != "proxy.internal" {
			t.Fatalf("proxy-hop ServerName = %q, want proxy.internal", cfg.ServerName)
		}
	})

	t.Run("injected config inherits the floor", func(t *testing.T) {
		d := &connectDialer{proxy: u, proxyTLS: &tls.Config{InsecureSkipVerify: true}} // fixture: self-signed proxy cert
		cfg := d.proxyTLSConfig()
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("floored MinVersion = %x, want TLS 1.2 (an unset floor must be raised)", cfg.MinVersion)
		}
		if !cfg.InsecureSkipVerify {
			t.Fatal("clone must preserve the injected InsecureSkipVerify")
		}
		if cfg.ServerName != "proxy.internal" {
			t.Fatalf("clone ServerName = %q, want the proxy host backfilled", cfg.ServerName)
		}
	})

	t.Run("higher floor is never lowered", func(t *testing.T) {
		d := &connectDialer{proxy: u, proxyTLS: &tls.Config{MinVersion: tls.VersionTLS13}}
		if cfg := d.proxyTLSConfig(); cfg.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %x, want the injected TLS 1.3 preserved", cfg.MinVersion)
		}
	})
}

// TestOriginHopTLSFloor pins the package-level originTLSConfig (hello.go):
// same floor contract as the proxy hop, plus the ALPN-parity contract from
// issue #48 — the origin hop ALWAYS offers exactly ["http/1.1"], matching the
// official opencode client's ClientHello (the pre-parity code offered
// ["h2", "http/1.1"]; an origin that could be talked to over h2 was a
// distinguisher, and h2 would also have needed x/net plumbing the official
// client doesn't have).
func TestOriginHopTLSFloor(t *testing.T) {
	t.Run("literal states the floor", func(t *testing.T) {
		cfg := originTLSConfig(nil, "origin.example:443")
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("origin-hop MinVersion = %x, want TLS 1.2 stated explicitly", cfg.MinVersion)
		}
		if cfg.ServerName != "origin.example" {
			t.Fatalf("origin-hop ServerName = %q, want origin.example", cfg.ServerName)
		}
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
			t.Fatalf("origin-hop NextProtos = %v, want [http/1.1] (official-client ALPN parity)", cfg.NextProtos)
		}
	})

	t.Run("injected config inherits the floor", func(t *testing.T) {
		cfg := originTLSConfig(&tls.Config{}, "origin.example:443")
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Fatalf("floored MinVersion = %x, want TLS 1.2 (an unset floor must be raised)", cfg.MinVersion)
		}
		if cfg.ServerName != "origin.example" {
			t.Fatalf("clone ServerName = %q, want the dialed host backfilled", cfg.ServerName)
		}
	})

	t.Run("injected ALPN is overwritten, not honored", func(t *testing.T) {
		cfg := originTLSConfig(&tls.Config{NextProtos: []string{"h2", "http/1.1"}}, "origin.example:443")
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
			t.Fatalf("clone NextProtos = %v, want [http/1.1] — ALPN is identity, not a knob", cfg.NextProtos)
		}
	})

	t.Run("higher floor is never lowered", func(t *testing.T) {
		if cfg := originTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13}, "origin.example:443"); cfg.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %x, want the injected TLS 1.3 preserved", cfg.MinVersion)
		}
	})
}
