// Package cloak implements the upstream free-tier gate evasion surface —
// everything that makes a request look like the official OpenCode agentic
// client: the tool fingerprint quartet, Responses-API normalization/sanitization,
// reasoning-effort mapping, and the model→endpoint routing rule.
//
// Ported from 9router open-sse/executors/opencode.js +
// open-sse/translator/formats/responsesApi.js. The gate rules are
// reverse-engineered from live upstream behavior (verify dates in comments)
// and can change at any time — keep every rule in this package, config-driven.
package cloak

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
)

// ResponsesModelRe matches Muse Spark models — the only free models served by
// /zen/v1/responses. The thinking suffix "model(level)" is stripped first and
// the last "/" segment wins (mirrors isMuseSparkModel).
var ResponsesModelRe = regexp.MustCompile(config.ResponsesURLModelsPattern)

var suffixRe = regexp.MustCompile(`\([^()]+\)\s*$`)

// BaseModelID strips the thinking suffix "model(level)" so registry-style
// lookups hit the base id.
func BaseModelID(model string) string {
	return strings.TrimSpace(suffixRe.ReplaceAllString(model, ""))
}

// UpstreamModelID strips a trailing parenthesised suffix AND any leading
// "oc/" style prefix, yielding the id sent to upstream.
func UpstreamModelID(model string) string {
	base := BaseModelID(model)
	if i := strings.Index(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// IsResponsesModel reports whether the model must be routed to
// /zen/v1/responses instead of /chat/completions. The executor also keeps an
// explicit set (muse-spark-1.2/1.3-contributor-free) — both ids match the
// regex, so the pattern alone is equivalent.
func IsResponsesModel(model string) bool {
	base := BaseModelID(model)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return ResponsesModelRe.MatchString(base)
}

// URL returns the upstream endpoint for the model (config.ZenResponsesPath /
// config.ZenChatPath — no other literals — joined onto the snapshot base at
// executor.BuildURL; blockcatPrepareModel.js prepares the model, the base
// plus path shape mirrors open-sse's request construction).
func URL(upstreamBase, model string) string {
	if IsResponsesModel(model) {
		return upstreamBase + config.ZenResponsesPath
	}
	return upstreamBase + config.ZenChatPath
}

// toolNameOf reads a tool declaration's name from either wire shape
// (chat: tool.function.name, responses: tool.name).
func toolNameOf(tool any) string {
	o := jsonx.AsObj(tool)
	if o == nil {
		return ""
	}
	if s := jsonx.AsStr(o["name"]); s != "" {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(jsonx.AsStr(jsonx.Get(o["function"], "name")))
}

func fingerprintToolChat(name string) map[string]any {
	return jsonx.ObjOf(
		"type", "function",
		"function", jsonx.ObjOf(
			"name", name,
			"description", "OpenCode built-in "+name+" tool",
			"parameters", jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf()),
		),
	)
}

func fingerprintToolResponses(name string) map[string]any {
	return jsonx.ObjOf(
		"type", "function",
		"name", name,
		"description", "OpenCode built-in "+name+" tool",
		"parameters", jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf()),
	)
}

// EnsureChatFingerprintTools merges the upstream-mandated file-search quartet
// into a Chat Completions body. Caller tools are preserved verbatim (extras
// are allowed upstream); only the missing fingerprint names are appended as
// no-op declarations the model may ignore. Without this, plain chat callers
// that send no tools get 403 FreeTierError on every request.
//
// Cloak contract (mirrors the upstream executor): a caller that sent no tools
// gets tool_choice "none" so the model never calls the injected declarations
// on plain chat.
func EnsureChatFingerprintTools(body map[string]any) {
	if body == nil {
		return
	}
	callerToolCount := 0
	present := map[string]bool{}
	if tools, ok := body["tools"].([]any); ok {
		callerToolCount = len(tools)
		for _, t := range tools {
			if name := toolNameOf(t); name != "" {
				present[name] = true
			}
		}
	} else {
		body["tools"] = []any{}
	}
	tools := body["tools"].([]any)
	for _, name := range config.FingerprintTools {
		if present[name] {
			continue
		}
		tools = append(tools, fingerprintToolChat(name))
		present[name] = true
	}
	body["tools"] = tools
	// JS truthiness (!body.tool_choice): absent/null/""/false all count as unset.
	if callerToolCount == 0 && !truthy(body["tool_choice"]) {
		body["tool_choice"] = "none"
	}
}

// truthy mirrors JS boolean coercion for the values tool_choice can hold.
func truthy(v any) bool {
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

// EnsureResponsesFingerprintTools is the same quartet for the Responses flat
// tool shape. Runs before normalizeResponsesTools so the injected
// declarations get the same coercion as caller tools. A missing choice
// defaults to "auto" so the free gate never sees tools without tool_choice.
func EnsureResponsesFingerprintTools(body map[string]any) {
	if body == nil {
		return
	}
	present := map[string]bool{}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			if name := toolNameOf(t); name != "" {
				present[name] = true
			}
		}
	} else {
		body["tools"] = []any{}
	}
	tools := body["tools"].([]any)
	for _, name := range config.FingerprintTools {
		if present[name] {
			continue
		}
		tools = append(tools, fingerprintToolResponses(name))
		present[name] = true
	}
	body["tools"] = tools
	if !truthy(body["tool_choice"]) {
		body["tool_choice"] = "auto"
	}
}

// NormalizeResponsesTools flattens Chat Completions tool declarations into the
// Responses flat shape and drops hosted/nameless tools the /responses endpoint
// rejects. Mirrors openai-responses.js + executors/opencode.js:
//   - name required (trimmed, capped at 128 — upstream InputValidationError)
//   - parameters: tool.parameters, else function.parameters, else the empty
//     object schema; {type:"object"} without properties gets an empty
//     properties map (strict backends reject its absence)
//   - tool_choice objects naming an unknown/absent function are deleted
func NormalizeResponsesTools(body map[string]any) {
	if body == nil {
		return
	}
	rawTools, ok := body["tools"].([]any)
	if !ok {
		return
	}
	validNames := map[string]bool{}
	out := make([]any, 0, len(rawTools))
	for _, rt := range rawTools {
		tool := jsonx.AsObj(rt)
		if tool == nil {
			continue
		}
		fn := jsonx.AsObj(tool["function"])
		name := strings.TrimSpace(jsonx.AsStr(tool["name"]))
		if name == "" && fn != nil {
			name = strings.TrimSpace(jsonx.AsStr(fn["name"]))
		}
		if name == "" {
			continue
		}
		description := jsonx.AsStr(tool["description"])
		if description == "" && fn != nil {
			description = jsonx.AsStr(fn["description"])
		}
		parameters, hasParams := tool["parameters"].(map[string]any)
		if !hasParams && fn != nil {
			parameters, hasParams = fn["parameters"].(map[string]any)
		}
		if !hasParams {
			parameters = jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf())
		}
		if t := jsonx.AsStr(parameters["type"]); t == "object" {
			// JS !parameters.properties is truthiness — null/false/"" are all
			// replaced (the upstream strict schema rejects a null properties).
			if !truthy(parameters["properties"]) {
				// Clone round-trips through JSON; for the decoded-JSON values
				// that reach here it always yields a map, but the assertion is
				// guarded anyway (a Clone marshal failure returns nil) rather
				// than risking a panic on the defense-in-depth path.
				clone, ok := jsonx.Clone(parameters).(map[string]any)
				if !ok {
					clone = jsonx.ObjOf()
				}
				clone["properties"] = jsonx.ObjOf()
				parameters = clone
			}
		}
		flat := jsonx.ObjOf("type", "function", "name", name[:min(len(name), config.MaxToolNameLen)])
		if description != "" {
			flat["description"] = description
		}
		flat["parameters"] = parameters
		out = append(out, flat)
		validNames[jsonx.AsStr(flat["name"])] = true
	}
	body["tools"] = out

	if choice, ok := body["tool_choice"].(map[string]any); ok {
		if jsonx.AsStr(choice["type"]) == "function" {
			n := strings.TrimSpace(jsonx.AsStr(choice["name"]))
			if n == "" || !validNames[n] {
				delete(body, "tool_choice")
			}
		}
	}
}

var callIDSeq atomic.Int64

// ClampResponsesCallID caps call ids at 64 chars (strict upstreams reject
// overlong ids with InputValidationError) and synthesizes a unique fallback
// for missing ones. The per-process sequence keeps same-millisecond ids
// unique so function_call ↔ function_call_output correlation never collides.
func ClampResponsesCallID(id any) string {
	s := jsonx.AsStr(id)
	if s == "" {
		return fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), callIDSeq.Add(1))
	}
	if len(s) > config.MaxResponsesCallID {
		return s[:config.MaxResponsesCallID]
	}
	return s
}

// CoerceResponsesArguments makes tool-call arguments a valid JSON string in a
// single stringify: objects → JSON once; valid JSON strings pass through
// untouched; anything else (partial fragments, empty) falls back to "{}"
// instead of double-encoding and tripping upstream InputValidationError.
func CoerceResponsesArguments(value any) string {
	switch v := value.(type) {
	case nil:
		return "{}"
	case string:
		if v == "" {
			return "{}"
		}
		if json.Valid([]byte(v)) {
			return v
		}
		return "{}"
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

// CoerceResponsesOutput makes function_call_output.output a string — never
// null/object. Array items use their text field when present (JS ?? — any
// type), JSON otherwise, joined with "".
func CoerceResponsesOutput(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, c := range v {
			if o := jsonx.AsObj(c); o != nil {
				if text, has := o["text"]; has && text != nil {
					if s, isStr := text.(string); isStr {
						parts = append(parts, s)
					} else {
						b, _ := json.Marshal(text)
						parts = append(parts, string(b))
					}
					continue
				}
			}
			b, err := json.Marshal(c)
			if err != nil {
				parts = append(parts, fmt.Sprint(c))
			} else {
				parts = append(parts, string(b))
			}
		}
		return strings.Join(parts, "")
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(b)
	}
}

// NormalizeResponsesInput normalizes a Responses `input` to array form.
// Accepts string or array; an empty array (or whitespace-only string) becomes
// a placeholder user message — providers require at least one user message
// and messages:[] is rejected upstream. The original string text is kept
// untrimmed (JS `input.trim() === "" ? "..." : input`). Returns nil for
// invalid shapes.
func NormalizeResponsesInput(input any) []any {
	switch v := input.(type) {
	case string:
		text := v
		if strings.TrimSpace(text) == "" {
			text = "..."
		}
		return []any{userMessage(text)}
	case []any:
		if len(v) == 0 {
			return []any{userMessage("...")}
		}
		return v
	default:
		return nil
	}
}

func userMessage(text string) map[string]any {
	return jsonx.ObjOf(
		"type", "message",
		"role", "user",
		"content", jsonx.ArrOf(jsonx.ObjOf("type", "input_text", "text", text)),
	)
}

// SanitizeResponsesItems is the last line of defense on a Responses body.
// Ported from executors/opencode.js sanitizeResponsesItems:
//   - prior-turn reasoning items are STRIPPED: the free tier uses
//     public/pooled credentials routing to an upstream OpenAI account pool;
//     reasoning `encrypted_content` can only be decrypted by the exact caller
//     that issued it, so replaying it across pooled/rotated accounts 400s
//     ("reasoning encrypted_content was not issued to this caller"). Under
//     store=false, omitting the blob instead gets "not found or was deleted".
//     Dropping prior reasoning items lets multi-turn and tool loops succeed.
//   - function_call/function_call_output items are coerced in place so
//     malformed tool payloads fail here with a clear shape instead of
//     upstream as InputValidationError.
func SanitizeResponsesItems(body map[string]any) {
	if body == nil {
		return
	}
	input, ok := body["input"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		item := jsonx.AsObj(raw)
		if item == nil {
			out = append(out, raw)
			continue
		}
		if jsonx.AsStr(item["type"]) == "reasoning" {
			continue
		}
		delete(item, "encrypted_content")
		delete(item, "reasoning_encrypted_content")
		switch jsonx.AsStr(item["type"]) {
		case "function_call":
			name := strings.TrimSpace(jsonx.AsStr(item["name"]))
			if name == "" {
				continue
			}
			if len(name) > config.MaxToolNameLen {
				name = name[:config.MaxToolNameLen]
			}
			item["name"] = name
			item["call_id"] = ClampResponsesCallID(item["call_id"])
			item["arguments"] = CoerceResponsesArguments(item["arguments"])
		case "function_call_output":
			item["call_id"] = ClampResponsesCallID(item["call_id"])
			item["output"] = CoerceResponsesOutput(item["output"])
		}
		out = append(out, item)
	}
	body["input"] = out
}

// DemoteToolChoice demotes explicit non-auto tool_choice to "auto" on the
// Muse Spark free models — upstream rejects named-object/required/none with
// 400 (auto-only, verified live 2026-09-19, decolua/9router#4165). Absent
// choices are defaulted to auto later by EnsureResponsesFingerprintTools.
func DemoteToolChoice(body map[string]any, model string) {
	base := BaseModelID(model)
	if !config.ForceAutoToolChoiceModels[base] {
		return
	}
	if current, ok := body["tool_choice"]; ok {
		if s, isStr := current.(string); !isStr || s != "auto" {
			body["tool_choice"] = "auto"
		}
	}
}

// InjectReasoningContent ports utils/reasoningContentInjector.js for this
// router's scope. Some thinking-mode providers require reasoning_content to
// be echoed back on assistant messages; OpenAI-format clients never send it,
// so a non-empty placeholder satisfies upstream validation. Provider-level
// rules don't exist for opencode; only the model rule (/deepseek/i, scope
// all) applies — it hits free model deepseek-v4-flash-free.
func InjectReasoningContent(model string, body map[string]any) {
	if body == nil {
		return
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return
	}
	if !deepseekRe.MatchString(model) {
		return
	}
	for _, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil || jsonx.AsStr(msg["role"]) != "assistant" {
			continue
		}
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			continue
		}
		msg["reasoning_content"] = " "
	}
}

var deepseekRe = regexp.MustCompile(`(?i)deepseek`)
