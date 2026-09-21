package router

import (
	"regexp"
	"strings"
)

// Claude Code appends a bracketed context marker to the model name when the
// 1M-context beta is toggled on: `claude-opus-5` becomes `claude-opus-5[1m]`
// (open-sse/utils/modelMarkers.js:1-9, applied at src/sse/handlers/chat.js:51-55).
// The marker is a client-side annotation, not part of any model id: it never
// matches a model, an alias or a provider/model pair, so a request that carries
// it would die at resolution. The capability itself travels in the
// `anthropic-beta: context-1m-2025-08-07` header.
//
// "Forwarded untouched" is JS's own claim (modelMarkers.js:7, chat.js:53), not
// this proxy's: here the upstream headers are REBUILT by upstream.BuildHeaders,
// which forwards only a fixed allow-list (User-Agent + the captured
// x-opencode-* headers) and never copies anthropic-beta — the 9router executor
// likewise rebuilds its headers from scratch (executors/opencode.js:388-408
// returns a fixed eight-header map; base.js:46-75) instead of echoing the
// client's, so the free-tier condition, not the beta header, is what the
// upstream sees. Stripping the marker is enough to let the request route
// normally.

// contextMarkerRe is modelMarkers.js CONTEXT_MARKER (:11) — `/\[1m\]$/i`.
var contextMarkerRe = regexp.MustCompile(`(?i)\[1m\]$`)

// stripModelContextMarker ports modelMarkers.js stripModelContextMarker
// (:14-20). The marker check runs on the TRIMMED model; when matched, the
// result is the trimmed model minus the marker; when NOT matched, the original
// (untrimmed) string is kept — JS returns `modelStr` untouched (:18).
// matched reports whether a marker was found (the lowercase marker value the
// JS pair returns is unused here).
func stripModelContextMarker(model string) (stripped string, matched bool) {
	trimmed := strings.TrimSpace(model)
	loc := contextMarkerRe.FindStringIndex(trimmed)
	if loc == nil {
		return model, false // modelMarkers.js:18
	}
	return trimmed[:loc[0]], true // modelMarkers.js:19 (slice off the match)
}
