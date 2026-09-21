// Pre-translation request normalizations, ported from 9router
// open-sse/translator/index.js translateRequest — these run for EVERY request
// before the same-format skip, so a chat→chat passthrough still goes through
// them. Order matters and mirrors translateRequest exactly:
//
//	NormalizeThinkingConfig → EnsureToolCallIDs → FixMissingToolResponses
//	(capture intent) → [format translation] → ApplyThinking →
//	FilterToOpenAIFormat (only when the upstream format is chat)
package translate

import (
	"encoding/json"
	"regexp"
	"strings"

	"opencode-free-proxy/internal/jsonx"
)

// toolIDRe is the Anthropic-compatible id pattern (concerns/toolCall.js
// TOOL_ID_PATTERN).
var toolIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var toolIDInvalidRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// NormalizeThinkingConfig deletes the Claude-style `thinking` block when the
// last message is not from the user (services/provider.js). reasoning_effort
// is request-level and deliberately survives tool-result turns.
func NormalizeThinkingConfig(body map[string]any) {
	if body == nil {
		return
	}
	messages := jsonx.AsArr(body["messages"])
	if messages == nil {
		messages = jsonx.AsArr(body["contents"])
	}
	if len(messages) == 0 {
		return
	}
	last := jsonx.AsObj(messages[len(messages)-1])
	if last == nil || jsonx.AsStr(last["role"]) != "user" {
		delete(body, "thinking")
	}
}

// sanitizeToolID strips invalid characters; empty result → "" (JS null).
func sanitizeToolID(id any) string {
	s := jsonx.AsStr(id)
	if s == "" {
		return ""
	}
	cleaned := toolIDInvalidRe.ReplaceAllString(s, "")
	return cleaned
}

// GenerateToolCallID builds a deterministic cache-friendly id from position +
// tool name (concerns/toolCall.js generateToolCallId).
func GenerateToolCallID(msgIndex, tcIndex int, toolName string) string {
	suffix := ""
	if toolName != "" {
		if cleaned := toolIDInvalidRe.ReplaceAllString(toolName, ""); cleaned != "" {
			suffix = "_" + cleaned
		}
	}
	return "call_msg" + itoa(msgIndex) + "_tc" + itoa(tcIndex) + suffix
}

// EnsureToolCallIDs gives every chat tool_call a valid id, a type, and string
// arguments; tool messages get a valid tool_call_id (concerns/toolCall.js).
func EnsureToolCallIDs(body map[string]any) {
	if body == nil {
		return
	}
	messages := jsonx.AsArr(body["messages"])
	if messages == nil {
		return
	}
	for i, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			continue
		}
		if jsonx.AsStr(msg["role"]) == "assistant" {
			if toolCalls := jsonx.AsArr(msg["tool_calls"]); toolCalls != nil {
				for j, tcRaw := range toolCalls {
					tc := jsonx.AsObj(tcRaw)
					if tc == nil {
						continue
					}
					id := jsonx.AsStr(tc["id"])
					if !toolIDRe.MatchString(id) {
						fallback := sanitizeToolID(tc["id"])
						if fallback == "" {
							name := jsonx.AsStr(jsonx.Get(tc["function"], "name"))
							fallback = GenerateToolCallID(i, j, name)
						}
						tc["id"] = fallback
					}
					if jsonx.AsStr(tc["type"]) == "" {
						tc["type"] = "function"
					}
					fn := jsonx.AsObj(tc["function"])
					if fn != nil {
						// JS `tc.function?.arguments && typeof ... !== "string"`
						// (concerns/toolCall.js:44) — truthiness: falsy
						// non-string arguments (0, false, null) stay as-is;
						// only truthy non-strings are stringified.
						if args, has := fn["arguments"]; has && jsonx.Truthy(args) {
							if _, isStr := args.(string); !isStr {
								b, err := json.Marshal(args)
								if err == nil {
									fn["arguments"] = string(b)
								}
							}
						}
					}
				}
			}
		}
		if jsonx.AsStr(msg["role"]) == "tool" {
			if tcID := jsonx.AsStr(msg["tool_call_id"]); tcID != "" && !toolIDRe.MatchString(tcID) {
				fallback := sanitizeToolID(msg["tool_call_id"])
				if fallback == "" {
					fallback = GenerateToolCallID(i, 0, "")
				}
				msg["tool_call_id"] = fallback
			}
		}
		// Claude-format tool_use/tool_result blocks inside array content.
		if content := jsonx.AsArr(msg["content"]); content != nil {
			for k, blockRaw := range content {
				block := jsonx.AsObj(blockRaw)
				if block == nil {
					continue
				}
				switch jsonx.AsStr(block["type"]) {
				case "tool_use":
					if id := jsonx.AsStr(block["id"]); id != "" && !toolIDRe.MatchString(id) {
						fallback := sanitizeToolID(block["id"])
						if fallback == "" {
							fallback = GenerateToolCallID(i, k, jsonx.AsStr(block["name"]))
						}
						block["id"] = fallback
					}
				case "tool_result":
					if id := jsonx.AsStr(block["tool_use_id"]); id != "" && !toolIDRe.MatchString(id) {
						fallback := sanitizeToolID(block["tool_use_id"])
						if fallback == "" {
							fallback = GenerateToolCallID(i, k, "")
						}
						block["tool_use_id"] = fallback
					}
				}
			}
		}
	}
}

// assistantToolCallIDs collects tool_call/tool_use ids of one message.
func assistantToolCallIDs(msg map[string]any) []string {
	if jsonx.AsStr(msg["role"]) != "assistant" {
		return nil
	}
	var ids []string
	for _, tcRaw := range jsonx.AsArr(msg["tool_calls"]) {
		tc := jsonx.AsObj(tcRaw)
		if tc == nil {
			continue
		}
		if id := jsonx.AsStr(tc["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	for _, blockRaw := range jsonx.AsArr(msg["content"]) {
		block := jsonx.AsObj(blockRaw)
		if block == nil || jsonx.AsStr(block["type"]) != "tool_use" {
			continue
		}
		if id := jsonx.AsStr(block["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// messageHasToolResults reports whether msg answers any of ids.
func messageHasToolResults(msg map[string]any, ids []string) bool {
	if msg == nil || len(ids) == 0 {
		return false
	}
	contains := func(id string) bool {
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}
	if jsonx.AsStr(msg["role"]) == "tool" {
		if tcID := jsonx.AsStr(msg["tool_call_id"]); tcID != "" && contains(tcID) {
			return true
		}
	}
	if jsonx.AsStr(msg["role"]) == "user" {
		for _, blockRaw := range jsonx.AsArr(msg["content"]) {
			block := jsonx.AsObj(blockRaw)
			if block == nil || jsonx.AsStr(block["type"]) != "tool_result" {
				continue
			}
			if tuID := jsonx.AsStr(block["tool_use_id"]); tuID != "" && contains(tuID) {
				return true
			}
		}
	}
	return false
}

// FixMissingToolResponses inserts empty tool messages after an assistant
// tool-call turn the client left unanswered — upstreams 400 on the dangling
// tool_calls (concerns/toolCall.js fixMissingToolResponses).
func FixMissingToolResponses(body map[string]any) {
	if body == nil {
		return
	}
	messages := jsonx.AsArr(body["messages"])
	if messages == nil {
		return
	}
	out := make([]any, 0, len(messages)+4)
	for i, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			out = append(out, raw)
			continue
		}
		out = append(out, raw)
		ids := assistantToolCallIDs(msg)
		if len(ids) == 0 {
			continue
		}
		var next map[string]any
		if i+1 < len(messages) {
			next = jsonx.AsObj(messages[i+1])
		}
		// JS: if (nextMsg && !hasToolResults(nextMsg, toolCallIds)) — no next
		// message at all means nothing to repair; never fabricate a dangling
		// tool response at the end of the conversation.
		if next == nil || messageHasToolResults(next, ids) {
			continue
		}
		for _, id := range ids {
			out = append(out, jsonx.ObjOf(
				"role", "tool",
				"tool_call_id", id,
				"content", ""))
		}
	}
	body["messages"] = out
}

// StripContinuityFields removes the encrypted reasoning blobs the
// Responses→Chat request translator stashes on assistant messages
// (chatCore.js stripContinuityFields) — never forward them upstream.
func StripContinuityFields(body map[string]any) {
	if body == nil {
		return
	}
	for _, raw := range jsonx.AsArr(body["messages"]) {
		if msg := jsonx.AsObj(raw); msg != nil {
			delete(msg, "encrypted_content")
			delete(msg, "reasoning_encrypted_content")
		}
	}
}

// validOpenAIContentTypes are the content block types filterToOpenAIFormat
// keeps (schema/blocks.js VALID_OPENAI_CONTENT_TYPES).
var validOpenAIContentTypes = map[string]bool{
	"text": true, "image_url": true, "image": true,
	"input_audio": true, "audio_url": true, "file": true,
}

// cloneMsg shallow-copies one message. The size hint is exactly len(msg) —
// no +1 reserve: the hint is not a bound, growth is the map's job, and the
// arithmetic is what code scanning's allocation-size-overflow flags
// (issue #17).
func cloneMsg(msg map[string]any) map[string]any {
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	return out
}

// FilterToOpenAIFormat normalizes a body to clean Chat Completions
// (formats/openai.js) — runs only when the upstream format is chat:
//   - developer role → system
//   - Claude thinking/redacted_thinking blocks and signature/cache_control
//     stripped; invalid block types dropped; emptied content becomes one
//     empty text block
//   - messages whose content is only whitespace dropped (tool messages and
//     assistant tool_calls turns always kept)
//   - empty tools array deleted; Claude/Gemini tool shapes converted
//   - Claude tool_choice objects converted
func FilterToOpenAIFormat(body map[string]any) {
	if body == nil {
		return
	}
	messages := jsonx.AsArr(body["messages"])
	if messages == nil {
		return
	}
	mapped := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			mapped = append(mapped, raw)
			continue
		}
		if jsonx.AsStr(msg["role"]) == "developer" {
			msg = cloneMsg(msg)
			msg["role"] = "system"
		}
		switch jsonx.AsStr(msg["role"]) {
		case "tool":
			mapped = append(mapped, msg)
			continue
		case "assistant":
			// JS `msg.role === ROLE.ASSISTANT && msg.tool_calls`
			// (formats/openai.js:27) — truthiness: `tool_calls: null` is NOT
			// a tool-call turn and falls through to the content filter
			// (possibly dropping the message); `[]` IS truthy and stays
			// verbatim.
			if jsonx.Truthy(msg["tool_calls"]) {
				mapped = append(mapped, msg)
				continue
			}
		}
		if _, isStr := msg["content"].(string); isStr {
			mapped = append(mapped, msg)
			continue
		}
		content := jsonx.AsArr(msg["content"])
		if content == nil {
			mapped = append(mapped, msg)
			continue
		}
		filtered := make([]any, 0, len(content))
		for _, blockRaw := range content {
			block := jsonx.AsObj(blockRaw)
			if block == nil {
				continue
			}
			switch jsonx.AsStr(block["type"]) {
			case "thinking", "redacted_thinking":
				continue
			case "tool_use":
				continue
			case "tool_result":
				cleaned := cloneMsg(block)
				delete(cleaned, "signature")
				delete(cleaned, "cache_control")
				filtered = append(filtered, cleaned)
			default:
				if validOpenAIContentTypes[jsonx.AsStr(block["type"])] {
					cleaned := cloneMsg(block)
					delete(cleaned, "signature")
					delete(cleaned, "cache_control")
					filtered = append(filtered, cleaned)
				}
			}
		}
		if len(filtered) == 0 {
			filtered = append(filtered, jsonx.ObjOf("type", "text", "text", ""))
		}
		msg = cloneMsg(msg)
		msg["content"] = filtered
		mapped = append(mapped, msg)
	}

	kept := make([]any, 0, len(mapped))
	for _, raw := range mapped {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			kept = append(kept, raw)
			continue
		}
		switch jsonx.AsStr(msg["role"]) {
		case "tool":
			kept = append(kept, msg)
			continue
		case "assistant":
			// Same truthiness gate as the map pass (formats/openai.js:68).
			if jsonx.Truthy(msg["tool_calls"]) {
				kept = append(kept, msg)
				continue
			}
		}
		if s, isStr := msg["content"].(string); isStr {
			if strings.TrimSpace(s) != "" {
				kept = append(kept, msg)
			}
			continue
		}
		if content := jsonx.AsArr(msg["content"]); content != nil {
			keep := false
			for _, blockRaw := range content {
				block := jsonx.AsObj(blockRaw)
				if block == nil {
					continue
				}
				if jsonx.AsStr(block["type"]) == "text" {
					if strings.TrimSpace(jsonx.AsStr(block["text"])) != "" {
						keep = true
						break
					}
				} else {
					keep = true
					break
				}
			}
			if keep {
				kept = append(kept, msg)
			}
			continue
		}
		kept = append(kept, msg)
	}
	body["messages"] = kept

	if tools := jsonx.AsArr(body["tools"]); tools != nil && len(tools) == 0 {
		delete(body, "tools")
	}
	if tools := jsonx.AsArr(body["tools"]); len(tools) > 0 {
		norm := make([]any, 0, len(tools))
		for _, toolRaw := range tools {
			tool := jsonx.AsObj(toolRaw)
			if tool == nil {
				norm = append(norm, toolRaw)
				continue
			}
			if jsonx.AsStr(tool["type"]) == "function" {
				// JS `tool.type === OPENAI_BLOCK.FUNCTION && tool.function`
				// (formats/openai.js:89) — truthiness: `function: null` is
				// not already-OpenAI shape and falls through to the Claude
				// conversion below.
				if jsonx.Truthy(tool["function"]) {
					norm = append(norm, tool)
					continue
				}
			}
			// Claude: {name, description, input_schema}
			if name := jsonx.AsStr(tool["name"]); name != "" {
				_, hasSchema := tool["input_schema"]
				_, hasDesc := tool["description"]
				if hasSchema || hasDesc {
					params, hasParams := tool["input_schema"].(map[string]any)
					if !hasParams {
						params = jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf())
					}
					norm = append(norm, jsonx.ObjOf(
						"type", "function",
						"function", jsonx.ObjOf(
							"name", name,
							"description", jsonx.AsStr(tool["description"]),
							"parameters", params)))
					continue
				}
			}
			// Gemini: {functionDeclarations: [...]}
			if decls := jsonx.AsArr(tool["functionDeclarations"]); decls != nil {
				for _, declRaw := range decls {
					fn := jsonx.AsObj(declRaw)
					if fn == nil {
						continue
					}
					params, hasParams := fn["parameters"].(map[string]any)
					if !hasParams {
						params = jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf())
					}
					norm = append(norm, jsonx.ObjOf(
						"type", "function",
						"function", jsonx.ObjOf(
							"name", jsonx.AsStr(fn["name"]),
							"description", jsonx.AsStr(fn["description"]),
							"parameters", params)))
				}
				continue
			}
			norm = append(norm, tool)
		}
		body["tools"] = norm
	}

	if choice, isObj := body["tool_choice"].(map[string]any); isObj {
		switch jsonx.AsStr(choice["type"]) {
		case "auto":
			body["tool_choice"] = "auto"
		case "any":
			body["tool_choice"] = "required"
		case "tool":
			if name := jsonx.AsStr(choice["name"]); name != "" {
				body["tool_choice"] = jsonx.ObjOf(
					"type", "function",
					"function", jsonx.ObjOf("name", name))
			}
		}
	}
}
