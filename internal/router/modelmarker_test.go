package router

// Tests for the [1m] context-marker strip (src/sse/handlers/chat.js:51-55 +
// open-sse/utils/modelMarkers.js) — unit level plus the end-to-end effect on
// the upstream body.

import (
	"testing"
)

func TestStripModelContextMarker(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		wantStripped string
		wantFound    bool
	}{
		// modelMarkers.js:17-19 — match on the trimmed value, strip the marker.
		{"marker stripped", "qwen3-coder-free[1m]", "qwen3-coder-free", true},
		// The regex is case-insensitive (:11 /\[1m\]$/i).
		{"uppercase marker", "qwen3-coder-free[1M]", "qwen3-coder-free", true},
		{"mixed case marker", "oc/muse-spark-1.2-contributor-free[1m]", "oc/muse-spark-1.2-contributor-free", true},
		// No match → the ORIGINAL, untrimmed string is returned (:18).
		{"no marker stays untrimmed", "  qwen3-coder-free  ", "  qwen3-coder-free  ", false},
		{"plain model", "qwen3-coder-free", "qwen3-coder-free", false},
		// Marker must be at the END (:11 $ anchor).
		{"marker not at end", "qwen3-coder-free[1m]x", "qwen3-coder-free[1m]x", false},
		// Trim happens before the match: surrounding spaces are dropped
		// together with the marker (trimmed.slice(:19)).
		{"trim then strip", "  qwen3-coder-free[1m] ", "qwen3-coder-free", true},
		{"marker only", "[1m]", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := stripModelContextMarker(tc.model)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if got != tc.wantStripped {
				t.Fatalf("stripped = %q, want %q", got, tc.wantStripped)
			}
		})
	}
}

func TestMarkerStripReachesUpstreamBody(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"oc/qwen3-coder-free[1m]","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	body := mustJSON(t, rec.snapshot()[0].Body)
	if got := jstr(t, body["model"], "model"); got != "qwen3-coder-free" {
		t.Fatalf("upstream model = %q, want the marker stripped before resolution", got)
	}
}
