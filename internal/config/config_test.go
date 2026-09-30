package config

import (
	"fmt"
	"strings"
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
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want nil", err)
	}
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
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want nil", err)
	}
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

// Issue #86: the millisecond integer is STILL the contract, and every value
// that parsed under the old envMs must parse to the identical duration now.
// This table is the backward-compatibility guarantee.
func TestEnvDurationAcceptsMilliseconds(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"1", time.Millisecond},
		{"250", 250 * time.Millisecond},
		{"2000", 2 * time.Second},
		{"55000", 55 * time.Second},
		{"350000", 350 * time.Second},
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			t.Setenv("OCFP_SHUTDOWN_GRACE", tc.raw)
			got, err := envDuration("OCFP_SHUTDOWN_GRACE", time.Second)
			if err != nil {
				t.Fatalf("envDuration(%q) error = %v, want nil", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("envDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// Issue #86: the duration literal that the sister service
// openai-compatible-injector already accepts for its identically named
// OAICR_SHUTDOWN_GRACE. "350s" is the value observed in the production
// compose environment, parsed as 55s by every start before this fix.
func TestEnvDurationAcceptsDurationLiteral(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"350s", 350 * time.Second},
		{"55s", 55 * time.Second},
		{"1m30s", 90 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"2h", 2 * time.Hour},
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			t.Setenv("OCFP_SHUTDOWN_GRACE", tc.raw)
			got, err := envDuration("OCFP_SHUTDOWN_GRACE", time.Second)
			if err != nil {
				t.Fatalf("envDuration(%q) error = %v, want nil", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("envDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// Issue #86 replaces the silent fallback: every value the old envMs dropped
// without a word is now a hard error naming the key. "30s" moves out of this
// table — it is now VALID (TestEnvDurationAcceptsDurationLiteral covers the
// shape), which is the whole point of the issue.
func TestEnvDurationRejectsMalformed(t *testing.T) {
	cases := []string{
		"not-a-number",
		"0",     // zero is indistinguishable from unset to an operator
		"-100",  // negative is not a drain window
		"0s",    // a zero duration silently disables the drain
		"-2s",   // negative in the other syntax too
		" 3000", // whitespace is not parsed by Atoi
		"3.5",   // fractional ms is not an integer
		"55000x",
	}
	for _, raw := range cases {
		t.Run("raw="+raw, func(t *testing.T) {
			t.Setenv("OCFP_SHUTDOWN_GRACE", raw)
			got, err := envDuration("OCFP_SHUTDOWN_GRACE", time.Second)
			if err == nil {
				t.Fatalf("envDuration(%q) = %v with nil error, want a hard error", raw, got)
			}
			// The returned duration is the default, so a caller that ignores
			// the error still gets a usable value; the error is what must
			// never be ignored.
			if got != time.Second {
				t.Fatalf("envDuration(%q) = %v, want the default %v alongside the error", raw, got, time.Second)
			}
			if !strings.Contains(err.Error(), "OCFP_SHUTDOWN_GRACE") {
				t.Fatalf("error %q does not name the key", err)
			}
			// The value is never echoed: an env var can carry a secret. The
			// LENGTH is reported instead.
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error %q echoes the operator's value; only its length may be reported", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("(%d characters", len(raw))) {
				t.Fatalf("error %q does not report the value's length (%d)", err, len(raw))
			}
		})
	}
}

// A malformed value must fail the BOOT, so the error has to survive FromEnv
// to main — and both duration variables are reported in one pass rather than
// the process dying on whichever the runtime evaluated first.
func TestFromEnvReportsEveryMalformedDuration(t *testing.T) {
	t.Setenv("OCFP_SHUTDOWN_GRACE", "not-a-grace")
	t.Setenv("OCFP_CONFIG_POLL_MS", "not-a-poll")
	c, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv() error = nil, want both malformed values reported")
	}
	if !strings.Contains(err.Error(), "OCFP_SHUTDOWN_GRACE") || !strings.Contains(err.Error(), "OCFP_CONFIG_POLL_MS") {
		t.Fatalf("error %q does not name both keys", err)
	}
	// The defaults are still populated: the config is returned so the caller
	// has something concrete to log alongside the error.
	if c.ShutdownGrace != DefaultShutdownGrace || c.ConfigPoll != DefaultConfigPoll {
		t.Fatalf("defaults not populated alongside the error: grace=%v poll=%v", c.ShutdownGrace, c.ConfigPoll)
	}
}

// One malformed variable must not hide the other's valid setting: a valid
// grace next to a malformed poll keeps the grace.
func TestFromEnvKeepsValidSiblingsOnOneMalformedVariable(t *testing.T) {
	t.Setenv("OCFP_SHUTDOWN_GRACE", "350s")
	t.Setenv("OCFP_CONFIG_POLL_MS", "bogus")
	c, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv() error = nil, want the malformed poll reported")
	}
	if c.ShutdownGrace != 350*time.Second {
		t.Fatalf("ShutdownGrace = %v, want the valid 350s preserved", c.ShutdownGrace)
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
