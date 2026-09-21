// Package logging centralizes zerolog construction so every logger in the
// process — production and tests — renders identical JSON field names. The
// runtime log level lives in zerolog's package-global atomic level
// (SetGlobalLevel): only the config-reload goroutine writes it, and every
// event checks it at emit time, which is what makes log-level hot reload work
// without touching handler state.
package logging

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/rs/zerolog"
)

func init() {
	zerolog.MessageFieldName = "msg"
	zerolog.LevelFieldName = "level"
	zerolog.TimestampFieldName = "time"
}

// New returns a JSON logger writing one object per line to w. What it emits
// is gated by the process-global level.
func New(w io.Writer) zerolog.Logger {
	return zerolog.New(w).With().Timestamp().Logger()
}

// Nop returns a logger that discards every event.
func Nop() zerolog.Logger {
	return zerolog.Nop()
}

// Resolve normalizes a production zerolog logger or the legacy callback shape
// used by focused tests. Store and Server retain only the returned zerolog
// logger; the compatibility adapter exists solely at their constructor edges.
func Resolve(v any) zerolog.Logger {
	switch log := v.(type) {
	case zerolog.Logger:
		return log
	case *zerolog.Logger:
		if log != nil {
			return *log
		}
	case func(string, ...any):
		return FromLegacy(log)
	}
	return Nop()
}

// FromLegacy adapts existing test callbacks to zerolog's JSON event stream.
func FromLegacy(logf func(string, ...any)) zerolog.Logger {
	if logf == nil {
		return Nop()
	}
	return New(legacyWriter{logf: logf})
}

type legacyWriter struct {
	logf func(string, ...any)
}

func (w legacyWriter) Write(p []byte) (int, error) {
	var event map[string]any
	if err := json.Unmarshal(p, &event); err != nil {
		w.logf(strings.TrimSpace(string(p)))
		return len(p), nil
	}
	if msg, ok := event[zerolog.MessageFieldName].(string); ok {
		// The rendered message travels as the format string with no args:
		// legacy capturers either keep format verbatim or Sprintf it, and
		// both shapes recover the message (the legacy logf contract logged
		// preformatted lines the same way).
		w.logf(msg)
	}
	return len(p), nil
}
