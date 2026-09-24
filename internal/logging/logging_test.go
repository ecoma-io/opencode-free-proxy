package logging

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

// Package logging is the only package in the repo without a test file; it
// centralizes zerolog construction and the legacy-callback compatibility
// adapter every constructor edge depends on (AGENTS.md layout note).

// isLive reports whether a resolved logger would emit: a Nop logger reports
// the Disabled level, every real logger a usable threshold.
func isLive(l zerolog.Logger) bool {
	return l.GetLevel() != zerolog.Disabled
}

func TestNewWritesJSONFieldNames(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Info().Msg("hello")
	// init() pins the JSON field names (time/level/msg) that every logger in
	// the process — production and tests — must render identically.
	var ev map[string]any
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("emit is not one JSON object: %v\nraw: %s", err, buf.String())
	}
	if ev["msg"] != "hello" {
		t.Fatalf("msg = %v, want hello", ev["msg"])
	}
	if ev["level"] != "info" {
		t.Fatalf("level = %v, want info", ev["level"])
	}
	if _, ok := ev["time"]; !ok {
		t.Fatal("time field missing")
	}
}

func TestResolve(t *testing.T) {
	t.Run("zerolog.Logger passes through", func(t *testing.T) {
		l := New(&bytes.Buffer{})
		if got := Resolve(l); !isLive(got) {
			t.Fatal("a live logger must resolve to itself")
		}
	})
	t.Run("nil *zerolog.Logger resolves to Nop", func(t *testing.T) {
		log := Resolve((*zerolog.Logger)(nil))
		if isLive(log) {
			t.Fatal("nil pointer must not enable anything")
		}
	})
	t.Run("non-nil *zerolog.Logger dereferences", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(&buf)
		if got := Resolve(&l); !isLive(got) {
			t.Fatal("a live logger pointer must resolve to the logger")
		}
	})
	t.Run("legacy func adapter resolves", func(t *testing.T) {
		l := Resolve(func(string, ...any) {})
		// FromLegacy wraps a real writer; the level default is informational.
		if !isLive(l) {
			t.Fatal("legacy callback must resolve to a live logger")
		}
	})
	t.Run("nil legacy func resolves to Nop", func(t *testing.T) {
		log := Resolve((func(string, ...any))(nil))
		if isLive(log) {
			t.Fatal("nil legacy func must not enable anything")
		}
	})
	t.Run("unknown shape resolves to Nop", func(t *testing.T) {
		if log := Resolve("not a logger"); isLive(log) {
			t.Fatal("unknown shape must resolve to Nop")
		}
	})
}

func TestFromLegacyNilIsNop(t *testing.T) {
	if log := FromLegacy(nil); isLive(log) {
		t.Fatal("FromLegacy(nil) must be Nop")
	}
}

func TestLegacyWriter(t *testing.T) {
	t.Run("JSON event extracts msg", func(t *testing.T) {
		var got string
		logf := func(format string, _ ...any) { got = format }
		l := New(legacyWriter{logf: logf})
		// New().Info().Msgf renders one JSON object per line; the adapter
		// should recover the rendered message for legacy capturers.
		l.Info().Msg("the message")
		if got != "the message" {
			t.Fatalf("logf got %q, want the message", got)
		}
	})
	t.Run("non-JSON line falls back to raw", func(t *testing.T) {
		w := legacyWriter{logf: func(format string, _ ...any) {}}
		n, err := w.Write([]byte("not json\n"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len("not json\n") {
			t.Fatalf("Write returned %d, want %d", n, len("not json\n"))
		}
	})
	t.Run("msg missing drops event", func(t *testing.T) {
		var called bool
		w := legacyWriter{logf: func(format string, _ ...any) { called = true }}
		if _, err := w.Write([]byte(`{"level":"info"}`)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if called {
			t.Fatal("logf must not be called when msg is absent")
		}
	})
}
