// Client-tool detection feeding the session-id translation sha: a port of
// open-sse/utils/clientDetector.js detectClientTool (:20-50) as the opencode
// executor consumes it. chatCore detects the tool once per request
// (chatCore.js:163) and threads it into executor.execute (chatCore.js:345),
// where resolveOpencodeSession passes it to translateSessionId's
// `opencode\0<tool>\0<key>` sha (executors/opencode.js:153, :89-103). The
// tool string changes the derived session id, so the branch ORDER below is
// load-bearing parity, not style.
package identity

import (
	"strings"

	"opencode-free-proxy/internal/jsonx"
)

// DetectClientTool mirrors detectClientTool (clientDetector.js:20-50) over the
// downstream headers (lowercase keys — the upstream.Downstream.Headers
// contract) and the request body. Returns "" where JS returns null; the
// executor maps falsy to "generic" inside the sha (executors/opencode.js:95).
//
// Deliberate divergence, by input availability: this runs at the executor
// boundary, which sees only the FORWARDED header subset (the router forwards
// user-agent and x-app, not openai-intent / x-initiator / originator) and the
// post-translation body, so the github-copilot header pair
// (clientDetector.js:23,31), the codex originator sub-signal (:44) and the
// pre-translation antigravity body marker (:28) never reach it. Requests
// identifying solely through those signals resolve to a later branch or ""
// here — a boundary gap, not a behavioral choice; every listed tool is still
// detected through its user-agent sub-signal.
func DetectClientTool(headers map[string]string, body map[string]any) string {
	ua := strings.ToLower(headers["user-agent"])
	xApp := strings.ToLower(headers["x-app"])

	// Antigravity identifies via a body field, not a header (:28).
	if jsonx.AsStr(body["userAgent"]) == "antigravity" {
		return "antigravity"
	}
	// GitHub Copilot chat panel (:31) — UA sub-signal only at this boundary.
	if strings.Contains(ua, "githubcopilotchat") {
		return "github-copilot"
	}
	// Claude Code / Claude CLI (:36).
	if strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code") || xApp == "cli" {
		return "claude"
	}
	// Gemini CLI (:39).
	if strings.Contains(ua, "gemini-cli") {
		return "gemini-cli"
	}
	// Codex CLI/Desktop (:43-44) — UA sub-signals only at this boundary
	// (originator is not forwarded).
	if strings.Contains(ua, "codex-tui") || strings.Contains(ua, "codex-cli") ||
		strings.Contains(ua, "codex_cli_rs") || strings.Contains(ua, "codex desktop") {
		return "codex"
	}
	// DeepSeek TUI (:47).
	if strings.Contains(ua, "deepseek-tui") {
		return "deepseek-tui"
	}
	return ""
}
