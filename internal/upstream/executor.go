// Package upstream ports the OpenCode executor boundary: request transform
// chain (executors/opencode.js transformRequest), upstream headers
// (buildHeaders), session resolution, and the retrying HTTP call.
package upstream

import (
	"math"
	"strings"
	"time"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/translate"
)

// Downstream captures the client's original headers needed upstream.
type Downstream struct {
	// Raw header values keyed by lowercase name (only the ones forwarded).
	Headers map[string]string
}

// SessionHeader is the inbound session header name (executors/opencode.js
// SESSION_HEADER).
const SessionHeader = "x-opencode-session"

var sessionRe = identity.SessionRE

// normalizeSession trims and length-caps a session id; empty result for
// invalid input (executors/opencode.js normalizeSession).
func normalizeSession(value string) string {
	normalized := strings.TrimSpace(value)
	if normalized == "" || len(normalized) > config.MaxSessionLength {
		return ""
	}
	return normalized
}

// nativeSession returns the downstream session header when it already looks
// like a genuine opencode session id (nativeSession).
func nativeSession(d Downstream) string {
	if v, has := d.Headers[SessionHeader]; has {
		if normalized := normalizeSession(v); normalized != "" && sessionRe.MatchString(normalized) {
			return normalized
		}
	}
	return ""
}

// ResolveSession ports resolveOpencodeSession: a native downstream session
// passes through verbatim; otherwise the incoming (or synthesized) session id
// is translated into an opencode-scoped sha; nothing at all → a fresh
// generated id.
func ResolveSession(body map[string]any, d Downstream) string {
	if native := nativeSession(d); native != "" {
		return native
	}
	incoming := ""
	if v, has := d.Headers[SessionHeader]; has {
		incoming = normalizeSession(v)
	}
	resolved := incoming
	if resolved == "" {
		resolved = identity.ResolveSessionID(body, d.Headers)
	}
	if resolved == "" {
		return identity.GenerateSessionID(time.Now())
	}
	return identity.TranslateSessionID(resolved, "generic")
}

// TransformRequest ports OpenCodeExecutor.transformRequest. model is the
// upstream model id WITH its thinking suffix; body is the already-translated
// wire body (chat or responses shape). Mutates body in place.
func TransformRequest(model string, body map[string]any) {
	if body == nil {
		return
	}
	// JS `model && !body.model`: falsy model fields (absent/null/""/0) refill.
	if model != "" && !jsonTruthy(body["model"]) {
		body["model"] = model
	}
	// The free-tier gate rejects stream:false with 403 FreeTierError — always
	// stream upstream, aggregate for non-streaming clients.
	body["stream"] = true

	if cloak.IsResponsesModel(model) {
		// Muse Spark free models are auto-only upstream; demote explicit
		// non-auto tool_choice (verified live 2026-09-19).
		if _, has := body["tool_choice"]; has {
			if s, isStr := body["tool_choice"].(string); !isStr || s != "auto" {
				if config.ForceAutoToolChoiceModels[cloak.BaseModelID(model)] {
					body["tool_choice"] = "auto"
				}
			}
		}
		if normalized := cloak.NormalizeResponsesInput(body["input"]); normalized != nil {
			body["input"] = normalized
		}
		if input := jsonx.AsArr(body["input"]); len(input) == 0 {
			body["input"] = []any{jsonx.ObjOf(
				"type", "message",
				"role", "user",
				"content", []any{jsonx.ObjOf("type", "input_text", "text", "...")},
			)}
		}
		// Responses names the output cap max_output_tokens and takes thinking
		// as reasoning:{effort,summary} — normalize the Chat fields here.
		if _, has := body["max_output_tokens"]; !has {
			if v, hasMC := body["max_completion_tokens"]; hasMC {
				body["max_output_tokens"] = v
			} else if v, hasMT := body["max_tokens"]; hasMT {
				body["max_output_tokens"] = v
			}
		}
		delete(body, "max_tokens")
		delete(body, "max_completion_tokens")
		if _, has := body["max_output_tokens"]; has {
			body["max_output_tokens"] = clampMaxOutput(body["max_output_tokens"])
		}
		cloak.NormalizeOpencodeReasoning(body)
		body["store"] = false
		cloak.EnsureResponsesFingerprintTools(body)
		cloak.NormalizeResponsesTools(body)
		cloak.SanitizeResponsesItems(body)
	} else {
		cloak.EnsureChatFingerprintTools(body)
	}
	// deepseek-family free models demand reasoning_content on every assistant
	// message (utils/reasoningContentInjector.js, model rule only).
	cloak.InjectReasoningContent(model, body)
}

// clampMaxOutput mirrors Math.max(16, Number(x) || 0) (executors/opencode.js:362).
// NaN collapses to 0 inside NumCoerce, and -Infinity falls to the floor
// (Math.max(16, -Infinity) = 16), so those need no special case. The one
// JS-reachable result Go cannot hold is +Infinity: Number("Infinity")||0
// stays Infinity, JS keeps it in the body, and JSON.stringify serializes
// non-finite numbers as null (executors/base.js:141) — so the upstream wire
// payload is max_output_tokens:null. Go's json.Marshal ERRORS on +Inf (the
// whole request would die), while nil marshals to the very same null — so
// the non-finite result is returned as nil to keep the wire identical.
func clampMaxOutput(v any) any {
	n := jsonx.NumCoerce(v)
	if math.IsInf(n, 1) {
		return nil
	}
	if n < config.MinMaxOutputTokens {
		// float64 keeps the decoded-JSON number convention for map values.
		return float64(config.MinMaxOutputTokens)
	}
	return n
}

// jsonTruthy mirrors JS truthiness for body["model"] and friends.
func jsonTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0
	default:
		return true
	}
}

// BuildURL picks the upstream endpoint for the model.
func BuildURL(upstreamBase, model string) string {
	return cloak.URL(upstreamBase, model)
}

// BuildHeaders ports OpenCodeExecutor.buildHeaders: the downstream UA passes
// through only when it already looks like opencode >= 1.17; everything else
// is forged. stream is always true upstream.
func BuildHeaders(d Downstream, session string, cachedUA string) map[string]string {
	h := map[string]string{
		"Content-Type":       "application/json",
		"Authorization":      "Bearer " + config.PublicBearer,
		"x-opencode-client":  "desktop",
		"x-opencode-session": session,
		"x-opencode-request": identity.GenerateRequestID(time.Now()),
		"x-opencode-project": "global",
		"Accept":             "text/event-stream",
	}
	downstreamUA := d.Headers["user-agent"]
	if identity.HasValidVersion(downstreamUA) {
		h["User-Agent"] = downstreamUA
	} else {
		h["User-Agent"] = cachedUA
	}
	// Client passthroughs (lowercased header keys).
	if v, has := d.Headers["x-opencode-client"]; has && v != "" {
		h["x-opencode-client"] = v
	}
	if v, has := d.Headers["x-opencode-request"]; has && v != "" {
		h["x-opencode-request"] = v
	}
	if v, has := d.Headers["x-opencode-project"]; has && v != "" {
		h["x-opencode-project"] = v
	}
	return h
}

// PrepareRequest is the chatCore→executor preamble: continuity-strip (after
// translation, before transform — chatCore.js order) + session resolve +
// transform. Returns the resolved session id for header building.
func PrepareRequest(model string, body map[string]any, d Downstream) string {
	translate.StripContinuityFields(body)
	session := ResolveSession(body, d)
	TransformRequest(model, body)
	return session
}
