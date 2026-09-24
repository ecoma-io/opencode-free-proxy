package config

import (
	"testing"
	"time"
)

// FromEnv is the process-bootstrap entry point cmd/server/main.go:49 uses.
// It was exercised only through the store/loader until now; these tests pin
// the defaulting and the envMs contract directly.

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("OCFP_PORT", "")
	t.Setenv("OCFP_CONFIG", "")
	t.Setenv("OCFP_SHUTDOWN_GRACE", "")
	t.Setenv("OCFP_CONFIG_POLL_MS", "")
	c := FromEnv()
	if c.Port != DefaultPort {
		t.Fatalf("Port = %q, want default %q", c.Port, DefaultPort)
	}
	if c.ConfigPath != "" {
		t.Fatalf("ConfigPath = %q, want empty (built-in runtime)", c.ConfigPath)
	}
	if c.ShutdownGrace != DefaultShutdownGrace {
		t.Fatalf("ShutdownGrace = %v, want default %v", c.ShutdownGrace, DefaultShutdownGrace)
	}
	if c.ConfigPoll != DefaultConfigPoll {
		t.Fatalf("ConfigPoll = %v, want default %v", c.ConfigPoll, DefaultConfigPoll)
	}
}

func TestFromEnvReadsAll(t *testing.T) {
	t.Setenv("OCFP_PORT", "9123")
	t.Setenv("OCFP_CONFIG", "/tmp/op.cfg.yaml")
	t.Setenv("OCFP_SHUTDOWN_GRACE", "7000")
	t.Setenv("OCFP_CONFIG_POLL_MS", "250")
	c := FromEnv()
	if c.Port != "9123" {
		t.Fatalf("Port = %q, want 9123", c.Port)
	}
	if c.ConfigPath != "/tmp/op.cfg.yaml" {
		t.Fatalf("ConfigPath = %q, want /tmp/op.cfg.yaml", c.ConfigPath)
	}
	if c.ShutdownGrace != 7*time.Second {
		t.Fatalf("ShutdownGrace = %v, want 7s", c.ShutdownGrace)
	}
	if c.ConfigPoll != 250*time.Millisecond {
		t.Fatalf("ConfigPoll = %v, want 250ms", c.ConfigPoll)
	}
}

func TestEnvMsInvalidFallsBackToDefault(t *testing.T) {
	cases := []string{
		"not-a-number",
		"0",     // zero is indistinguishable from unset
		"-100",  // negative is not a drain window
		"30s",   // a duration literal is not an ms integer
		" 3000", // whitespace is not parsed by Atoi
		"3.5",   // fractional ms is not an integer
	}
	for _, raw := range cases {
		t.Run("raw="+raw, func(t *testing.T) {
			t.Setenv("OCFP_SHUTDOWN_GRACE", raw)
			if got := envMs("OCFP_SHUTDOWN_GRACE", time.Second); got != time.Second {
				t.Fatalf("envMs(%q) = %v, want fallback %v", raw, got, time.Second)
			}
		})
	}
}

func TestEnvMsValidParity(t *testing.T) {
	t.Setenv("OCFP_CONFIG_POLL_MS", "1")
	if got := envMs("OCFP_CONFIG_POLL_MS", time.Second); got != time.Millisecond {
		t.Fatalf("envMs('1') = %v, want 1ms", got)
	}
	t.Setenv("OCFP_CONFIG_POLL_MS", "2000")
	if got := envMs("OCFP_CONFIG_POLL_MS", time.Second); got != 2*time.Second {
		t.Fatalf("envMs('2000') = %v, want 2s", got)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("OCFP_PORT", "")
	if got := envOr("OCFP_PORT", "def"); got != "def" {
		t.Fatalf("envOr(unset/empty) = %q, want def", got)
	}
	t.Setenv("OCFP_PORT", "8443")
	if got := envOr("OCFP_PORT", "def"); got != "8443" {
		t.Fatalf("envOr(set) = %q, want 8443", got)
	}
}
