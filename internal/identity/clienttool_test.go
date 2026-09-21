// DetectClientTool tests — a port check of open-sse/utils/clientDetector.js
// detectClientTool (:20-50) branch order as consumed by the opencode executor
// (executors/opencode.js:153 translateSessionId(resolved, clientTool)).
package identity

import "testing"

func TestDetectClientTool(t *testing.T) {
	cases := []struct {
		note    string
		headers map[string]string
		body    map[string]any
		want    string
	}{
		{note: "no signals → JS null (generic downstream)", want: ""},
		{
			note:    "antigravity body marker outranks every header (:28)",
			headers: map[string]string{"user-agent": "claude-cli/2.0"},
			body:    map[string]any{"userAgent": "antigravity"},
			want:    "antigravity",
		},
		{
			note: "non-string antigravity marker never matches (=== compare)",
			body: map[string]any{"userAgent": 42},
			want: "",
		},
		{
			note:    "github copilot chat UA (:31)",
			headers: map[string]string{"user-agent": "GitHubCopilotChat/1.0"},
			want:    "github-copilot",
		},
		{
			note:    "github copilot outranks claude (branch order)",
			headers: map[string]string{"user-agent": "githubcopilotchat claude-code/2.0"},
			want:    "github-copilot",
		},
		{
			note:    "claude-cli UA (:36)",
			headers: map[string]string{"user-agent": "claude-cli/2.0.14 (external, cli)"},
			want:    "claude",
		},
		{
			note:    "claude-code UA, case-insensitive value",
			headers: map[string]string{"user-agent": "Claude-Code/2.0"},
			want:    "claude",
		},
		{
			note:    "x-app=cli with a foreign UA (:36)",
			headers: map[string]string{"user-agent": "node", "x-app": "CLI"},
			want:    "claude",
		},
		{
			note:    "gemini-cli UA (:39)",
			headers: map[string]string{"user-agent": "GeminiCLI/0.8 (gemini-cli)"},
			want:    "gemini-cli",
		},
		{
			note:    "codex-tui UA (:43)",
			headers: map[string]string{"user-agent": "codex-tui 0.42.0"},
			want:    "codex",
		},
		{
			note:    "codex desktop UA, case-insensitive (:44)",
			headers: map[string]string{"user-agent": "Codex Desktop/1.2"},
			want:    "codex",
		},
		{
			note:    "deepseek-tui UA (:47)",
			headers: map[string]string{"user-agent": "deepseek-tui/0.3"},
			want:    "deepseek-tui",
		},
		{
			note:    "opencode UA is not a detected tool",
			headers: map[string]string{"user-agent": "opencode/1.19.0 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"},
			want:    "",
		},
		{
			note:    "plain curl is generic",
			headers: map[string]string{"user-agent": "curl/8.0.1"},
			want:    "",
		},
	}
	for _, tc := range cases {
		if got := DetectClientTool(tc.headers, tc.body); got != tc.want {
			t.Errorf("[%s] DetectClientTool() = %q, want %q", tc.note, got, tc.want)
		}
	}
}

// The detected tool feeds the `opencode\0<tool>\0<key>` sha
// (executors/opencode.js:89-103) — "" (JS null) and "generic" must land on the
// same id, and any detected tool on a different one.
func TestDetectClientToolChangesTranslatedSession(t *testing.T) {
	const key = "conv-42"
	generic := TranslateSessionID(key, "")
	if generic != TranslateSessionID(key, "generic") {
		t.Fatal("empty tool must equal the generic tool in the sha")
	}
	claude := TranslateSessionID(key, DetectClientTool(
		map[string]string{"user-agent": "claude-cli/2.0"}, nil))
	if claude == generic {
		t.Error("a detected claude client must derive a different session id than generic")
	}
	if claude != TranslateSessionID(key, "claude") {
		t.Error("detected tool must feed the sha verbatim")
	}
}
