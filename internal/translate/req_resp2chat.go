package translate

import (
	"encoding/json"
	"strings"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
)

// jsonStringify is the JS JSON.stringify stand-in for coercion points.
func jsonStringify(v any) string {
	switch v.(type) {
	case string:
		return jsonx.AsStr(v)
	case nil:
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// ResponsesToChatRequest ports openaiResponsesToOpenAIRequest: OpenAI
// Responses request → Chat Completions request. Bodies without a truthy
// `input` pass through untouched; invalid input shapes return the original
// body (the JS translator bails after spreading, leaving the Responses
// fields in place).
func ResponsesToChatRequest(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	// JS `if (!body.input) return body` (openai-responses.js:23) — truthiness:
	// ""/0/false/null leave the body untranslated (key presence would
	// wrongly translate an empty-string input into placeholder messages).
	if !jsonx.Truthy(body["input"]) {
		return body
	}

	result := jsonx.Clone(body).(map[string]any)
	result["messages"] = []any{}

	// JS `if (body.instructions)` (:29) — truthiness; the raw value becomes
	// the system message content.
	if jsonx.Truthy(body["instructions"]) {
		result["messages"] = append(result["messages"].([]any),
			jsonx.ObjOf("role", RoleSystem, "content", body["instructions"]))
	}

	inputItems := cloak.NormalizeResponsesInput(body["input"])
	if inputItems == nil {
		return body
	}

	var currentAssistantMsg map[string]any
	var pendingToolResults []any
	pendingReasoning := ""
	pendingReasoningEncrypted := ""
	var additionalTools []any
	customToolNames := map[string]bool{}

	flushAssistant := func() {
		if currentAssistantMsg != nil {
			result["messages"] = append(result["messages"].([]any), currentAssistantMsg)
			currentAssistantMsg = nil
		}
	}
	flushToolResults := func() {
		if len(pendingToolResults) > 0 {
			msgs := result["messages"].([]any)
			msgs = append(msgs, pendingToolResults...)
			result["messages"] = msgs
			pendingToolResults = nil
		}
	}
	attachPendingReasoning := func(msg map[string]any) {
		if pendingReasoning != "" {
			msg["reasoning_content"] = pendingReasoning
		}
		if pendingReasoningEncrypted != "" {
			msg["encrypted_content"] = pendingReasoningEncrypted
		}
		pendingReasoning = ""
		pendingReasoningEncrypted = ""
	}

	for _, raw := range inputItems {
		item := jsonx.AsObj(raw)
		if item == nil {
			continue
		}
		// JS `item.type || (item.role ? MESSAGE : null)` (:67) — truthiness
		// on both: a truthy non-string type matches no branch, and an empty
		// role does NOT promote a typeless item to a message.
		itemType := jsonx.AsStr(item["type"])
		if !jsonx.Truthy(item["type"]) {
			if jsonx.Truthy(item["role"]) {
				itemType = ItemMessage
			}
		}

		switch itemType {
		case ItemMessage:
			flushAssistant()
			flushToolResults()

			var content any
			if parts, isArr := item["content"].([]any); isArr {
				mapped := make([]any, 0, len(parts))
				for _, pc := range parts {
					c := jsonx.AsObj(pc)
					if c == nil {
						mapped = append(mapped, pc)
						continue
					}
					switch jsonx.AsStr(c["type"]) {
					case ItemInputText, ItemOutputText:
						mapped = append(mapped, jsonx.ObjOf("type", BlockText, "text", c["text"]))
					case ItemInputImage:
						// JS `c.image_url || c.file_id || ""` and
						// `c.detail || "auto"` (:89-90) — truthiness chains
						// keeping the raw value.
						url := c["image_url"]
						if !jsonx.Truthy(url) {
							url = c["file_id"]
						}
						if !jsonx.Truthy(url) {
							url = ""
						}
						detail := c["detail"]
						if !jsonx.Truthy(detail) {
							detail = "auto"
						}
						mapped = append(mapped, jsonx.ObjOf(
							"type", BlockImageURL,
							"image_url", jsonx.ObjOf("url", url, "detail", detail)))
					default:
						mapped = append(mapped, c)
					}
				}
				content = mapped
			} else {
				content = item["content"]
			}
			msg := jsonx.ObjOf("role", item["role"], "content", content)
			if jsonx.AsStr(item["role"]) == RoleAssistant {
				attachPendingReasoning(msg)
			} else {
				pendingReasoning = ""
				pendingReasoningEncrypted = ""
			}
			result["messages"] = append(result["messages"].([]any), msg)

		case ItemFunctionCall, ItemCustomToolCall:
			if currentAssistantMsg == nil {
				currentAssistantMsg = jsonx.ObjOf(
					"role", RoleAssistant,
					"content", nil,
					"tool_calls", []any{})
				attachPendingReasoning(currentAssistantMsg)
			}
			// Nameless calls are skipped AFTER the shell message is created —
			// an empty tool_calls array can survive to flush (JS #444 parity).
			// JS gates on the trimmed name (:115) but pushes the RAW one
			// (:116, :124) — trimming would desynchronize the call from its
			// tool declaration.
			name := jsonx.AsStr(item["name"])
			if strings.TrimSpace(name) == "" {
				continue
			}
			if itemType == ItemCustomToolCall {
				customToolNames[name] = true
			}
			var toolInput any
			if itemType == ItemCustomToolCall {
				// JS wraps freeform input as {"input": <string-or-json>} in
				// the chat tool_calls.arguments (openai-responses.js:117-118)
				// — the response side unwraps it via extractCustomToolInput.
				// `JSON.stringify(item.input ?? "")` turns a missing/null
				// input into the two-character string `""` (not "").
				in := item["input"]
				var wrapped string
				if s, isStr := in.(string); isStr {
					wrapped = s
				} else if in == nil {
					wrapped = `""`
				} else {
					wrapped = jsonStringify(in)
				}
				toolInput = jsonx.ObjOf("input", wrapped)
			} else {
				toolInput = item["arguments"]
			}
			// JS `typeof toolInput === "string" ? toolInput :
			// JSON.stringify(toolInput ?? {})` (:125) — null/absent
			// arguments become "{}" (null ?? {}), never "null".
			var args string
			if s, isStr := toolInput.(string); isStr {
				args = s
			} else if toolInput == nil {
				args = "{}"
			} else {
				args = jsonStringify(toolInput)
			}
			calls := currentAssistantMsg["tool_calls"].([]any)
			calls = append(calls, jsonx.ObjOf(
				"id", item["call_id"],
				"type", BlockFunction,
				"function", jsonx.ObjOf("name", name, "arguments", args)))
			currentAssistantMsg["tool_calls"] = calls

		case ItemFunctionCallOutput, ItemCustomToolCallOutput:
			flushAssistant()
			flushToolResults()
			// JS `typeof item.output === "string" ? item.output :
			// JSON.stringify(item.output)` (:146): an ABSENT output is
			// JSON.stringify(undefined) = undefined — the content key
			// vanishes on the wire — while an explicit null keeps "null".
			toolMsg := jsonx.ObjOf("role", RoleTool, "tool_call_id", item["call_id"])
			if out, has := item["output"]; has {
				toolMsg["content"] = jsonStringify(out)
			}
			result["messages"] = append(result["messages"].([]any), toolMsg)

		case ItemAdditionalTools:
			if tools, isArr := item["tools"].([]any); isArr {
				additionalTools = append(additionalTools, tools...)
			}

		case ItemReasoning:
			if txt := extractReasoningText(item); txt != "" {
				if pendingReasoning != "" {
					pendingReasoning += "\n" + txt
				} else {
					pendingReasoning = txt
				}
			}
			if enc := jsonx.AsStr(item["encrypted_content"]); enc != "" {
				pendingReasoningEncrypted = enc
			}
		}
	}

	flushAssistant()
	flushToolResults()

	var responseTools []any
	if tools, isArr := body["tools"].([]any); isArr {
		responseTools = append(responseTools, tools...)
	}
	responseTools = append(responseTools, additionalTools...)
	if len(responseTools) > 0 {
		out := make([]any, 0, len(responseTools))
		for _, rt := range responseTools {
			tool := jsonx.AsObj(rt)
			if tool == nil {
				continue
			}
			// JS `if (tool.function) return tool` (openai-responses.js:189)
			// — truthiness: a null function is NOT already-chat shape and
			// falls through to the Responses-tool conversion below.
			if jsonx.Truthy(tool["function"]) {
				out = append(out, tool)
				continue
			}
			name := jsonx.AsStr(tool["name"])
			if strings.TrimSpace(name) == "" {
				continue
			}
			if jsonx.AsStr(tool["type"]) == "custom" {
				customToolNames[name] = true
				var hint []string
				if fmt2 := jsonx.AsObj(tool["format"]); fmt2 != nil {
					// JS [syntax, definition].filter(Boolean).join("\n")
					// (:198) — truthy non-strings stringify through join.
					for _, k := range []string{"syntax", "definition"} {
						if v := fmt2[k]; jsonx.Truthy(v) {
							hint = append(hint, jsStringOf(v))
						}
					}
				}
				// JS: String(tool.description || "") (:203), then
				// [description, formatHint].join("\n\n") — two different joins.
				joined := joinNonEmpty([]string{jsStringOrEmpty(tool["description"]), joinNonEmpty(hint, "\n")}, "\n\n")
				out = append(out, jsonx.ObjOf(
					"type", BlockFunction,
					"function", jsonx.ObjOf(
						"name", name,
						"description", joined,
						"parameters", jsonx.ObjOf(
							"type", "object",
							"properties", jsonx.ObjOf(
								"input", jsonx.ObjOf(
									"type", "string",
									"description", "Raw freeform input for this custom tool")),
							"required", jsonx.ArrOf("input"),
							"additionalProperties", false))))
				continue
			}
			fn := jsonx.ObjOf(
				"name", name,
				// JS String(tool.description || "") (:224) — truthy
				// non-string descriptions stringify, falsy become "".
				"description", jsStringOrEmpty(tool["description"]),
				"parameters", normalizeToolParameters(tool["parameters"]))
			if strict, has := tool["strict"]; has {
				fn["strict"] = strict
			}
			out = append(out, jsonx.ObjOf("type", BlockFunction, "function", fn))
		}
		result["tools"] = out
	}
	if len(customToolNames) > 0 {
		names := make([]any, 0, len(customToolNames))
		for n := range customToolNames {
			names = append(names, n)
		}
		result["_customToolNames"] = names
	}

	if _, has := result["max_output_tokens"]; has {
		if _, hasMax := result["max_tokens"]; !hasMax {
			result["max_tokens"] = result["max_output_tokens"]
		}
		delete(result, "max_output_tokens")
	}

	// JS: if (typeof result.reasoning?.effort === "string") map it to
	// reasoning_effort before deleting reasoning (openai-responses.js cleanup).
	if reasoning := jsonx.AsObj(result["reasoning"]); reasoning != nil {
		if eff, isStr := reasoning["effort"].(string); isStr {
			result["reasoning_effort"] = eff // JS typeof-check only — "" maps too
		}
	}
	jsonx.Delete(result, "input", "instructions", "include", "prompt_cache_key", "store", "reasoning", "client_metadata")
	return result
}

// extractReasoningText pulls reasoning text from summary[].text, falling back
// to content[].text (encrypted_content is continuity-only).
func extractReasoningText(item map[string]any) string {
	for _, key := range []string{"summary", "content"} {
		arr, isArr := item[key].([]any)
		if !isArr {
			continue
		}
		var parts []string
		for _, p := range arr {
			o := jsonx.AsObj(p)
			if o == nil {
				continue
			}
			if s := jsonx.AsStr(o["text"]); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// jsStringOrEmpty is JS `String(v || "")` (:203, :224) over decoded JSON:
// falsy values coerce to ""; truthy non-strings stringify (templated/joined).
func jsStringOrEmpty(v any) string {
	if !jsonx.Truthy(v) {
		return ""
	}
	return jsStringOf(v)
}

// normalizeToolParameters ports openai-responses.js:275-278.
//   - `if (!params) return {...}` — falsy parameters (null/0/false/"") get
//     the default object schema. A TRUTHY array (e.g. `parameters: []`)
//     passes through as-is: JS truthiness is blind to its non-object shape.
//   - `params.type === "object" && !params.properties` replaces the
//     properties field whenever it is falsy (null/0/false/""), a keyed
//     presence check would wrongly keep them.
func normalizeToolParameters(params any) any {
	o := jsonx.AsObj(params)
	if o == nil {
		if !jsonx.Truthy(params) {
			return jsonx.ObjOf("type", "object", "properties", jsonx.ObjOf())
		}
		return params // truthy but not an object (array) — passthrough
	}
	if jsonx.AsStr(o["type"]) == "object" && !jsonx.Truthy(o["properties"]) {
		clone := jsonx.Clone(o).(map[string]any)
		clone["properties"] = jsonx.ObjOf()
		return clone
	}
	return params
}

// ChatRequestToResponsesRequest ports openaiToOpenAIResponsesRequest: Chat
// Completions request → Responses request. Bodies already carrying a truthy
// `input` pass through with max_tokens remapped.
func ChatRequestToResponsesRequest(model string, body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	// JS `if (body.input)` (openai-responses.js:323) — truthiness: a falsy
	// input (""/0/false/null) is NOT an already-Responses body; the full
	// chat translation runs instead.
	if jsonx.Truthy(body["input"]) {
		out := jsonx.Clone(body).(map[string]any)
		out["model"] = model
		out["stream"] = true
		if _, has := out["max_output_tokens"]; !has {
			if v, has := out["max_completion_tokens"]; has {
				out["max_output_tokens"] = v
			} else if v, has := out["max_tokens"]; has {
				out["max_output_tokens"] = v
			}
		}
		jsonx.Delete(out, "max_tokens", "max_completion_tokens")
		return out
	}

	result := jsonx.ObjOf(
		"model", model,
		"input", []any{},
		"stream", true,
		"store", false,
	)
	input := result["input"].([]any)

	instructions := ""
	hasSystemMessage := false
	messages, _ := body["messages"].([]any)

	for _, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			continue
		}
		role := jsonx.AsStr(msg["role"])
		if role == RoleSystem || role == RoleDeveloper {
			if !hasSystemMessage {
				instructions = extractInstructionsText(msg["content"])
				hasSystemMessage = true
			}
			continue
		}

		if role == RoleUser || role == RoleAssistant {
			if role == RoleAssistant {
				if reasoningItem := buildReasoningInputItem(msg); reasoningItem != nil {
					input = append(input, reasoningItem)
				}
			}
			contentType := ItemInputText
			if role == RoleAssistant {
				contentType = ItemOutputText
			}
			var content []any
			switch c := msg["content"].(type) {
			case string:
				content = []any{jsonx.ObjOf("type", contentType, "text", c)}
			case []any:
				for _, pc := range c {
					part := jsonx.AsObj(pc)
					if part == nil {
						continue
					}
					switch jsonx.AsStr(part["type"]) {
					case BlockText:
						content = append(content, jsonx.ObjOf("type", contentType, "text", part["text"]))
					case BlockImageURL:
						var url string
						switch iu := part["image_url"].(type) {
						case string:
							url = iu
						case map[string]any:
							url = jsonx.AsStr(iu["url"])
						}
						detail := "auto"
						if iu, isObj := part["image_url"].(map[string]any); isObj {
							if d := jsonx.AsStr(iu["detail"]); d != "" {
								detail = d
							}
						}
						content = append(content, jsonx.ObjOf(
							"type", ItemInputImage, "image_url", url, "detail", detail))
					case ItemInputImage:
						content = append(content, part)
					default:
						// JS `const text = c.text || c.content ||
						// JSON.stringify(c)` then `typeof text === "string" ?
						// text : JSON.stringify(text)` (openai-responses.js:381-382)
						// — truthy chains keeping raw values; a truthy
						// non-string text (number, object) stringifies alone,
						// not the whole block.
						text := part["text"]
						if !jsonx.Truthy(text) {
							text = part["content"]
						}
						if !jsonx.Truthy(text) {
							text = jsonStringify(part)
						}
						if _, isStr := text.(string); !isStr {
							text = jsonStringify(text)
						}
						content = append(content, jsonx.ObjOf("type", contentType, "text", text))
					}
				}
			}
			if len(content) > 0 {
				input = append(input, jsonx.ObjOf(
					"type", ItemMessage, "role", role, "content", content))
			}
		}

		if role == RoleAssistant {
			if calls, isArr := msg["tool_calls"].([]any); isArr {
				for _, tcRaw := range calls {
					tc := jsonx.AsObj(tcRaw)
					if tc == nil {
						continue
					}
					fn := jsonx.AsObj(tc["function"])
					name := strings.TrimSpace(jsonx.AsStr(jsonx.Get(fn, "name")))
					if name == "" {
						continue
					}
					if len(name) > config.MaxToolNameLen {
						name = name[:config.MaxToolNameLen]
					}
					input = append(input, jsonx.ObjOf(
						"type", ItemFunctionCall,
						"call_id", cloak.ClampResponsesCallID(tc["id"]),
						"name", name,
						"arguments", cloak.CoerceResponsesArguments(jsonx.Get(fn, "arguments"))))
				}
			}
		}

		if role == RoleTool {
			input = append(input, jsonx.ObjOf(
				"type", ItemFunctionCallOutput,
				"call_id", cloak.ClampResponsesCallID(msg["tool_call_id"]),
				"output", cloak.CoerceResponsesOutput(msg["content"])))
		}
	}
	result["input"] = input
	result["instructions"] = instructions

	if tools, isArr := body["tools"].([]any); isArr {
		out := make([]any, 0, len(tools))
		for _, tRaw := range tools {
			tool := jsonx.AsObj(tRaw)
			if tool == nil {
				continue
			}
			if jsonx.AsStr(tool["type"]) == BlockFunction {
				fn := jsonx.AsObj(tool["function"])
				name := strings.TrimSpace(jsonx.AsStr(jsonx.Get(fn, "name")))
				if name == "" {
					continue
				}
				if len(name) > config.MaxToolNameLen {
					name = name[:config.MaxToolNameLen]
				}
				flat := jsonx.ObjOf(
					"type", BlockFunction,
					"name", name,
					"description", jsonx.AsStr(jsonx.Get(fn, "description")),
					"parameters", normalizeToolParameters(jsonx.Get(fn, "parameters")))
				if strict, has := fn["strict"]; has {
					flat["strict"] = strict
				}
				out = append(out, flat)
				continue
			}
			out = append(out, tool)
		}
		result["tools"] = out
	}

	if v, has := body["temperature"]; has {
		result["temperature"] = v
	}
	if v, has := body["max_output_tokens"]; has {
		result["max_output_tokens"] = v
	} else if v, has := body["max_completion_tokens"]; has {
		result["max_output_tokens"] = v
	} else if v, has := body["max_tokens"]; has {
		result["max_output_tokens"] = v
	}
	if v, has := body["top_p"]; has {
		result["top_p"] = v
	}
	if v, has := body["reasoning"]; has {
		result["reasoning"] = v
	}
	if v, has := body["reasoning_effort"]; has {
		result["reasoning"] = jsonx.ObjOf("effort", v, "summary", "auto")
	}
	if v, has := body["service_tier"]; has {
		result["service_tier"] = v
	}
	if v, has := body["prompt_cache_key"]; has {
		result["prompt_cache_key"] = v
	}

	return result
}

// extractInstructionsText pulls plain text from a system/developer message
// (openai-responses.js:260-270); array content parts join with "\n" after
// JS `.filter(Boolean)` drops the empty ones, everything else degrades to "".
func extractInstructionsText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, pc := range c {
			o := jsonx.AsObj(pc)
			if o == nil {
				continue
			}
			if s, isStr := o["text"].(string); isStr {
				if s != "" { // filter(Boolean)
					parts = append(parts, s)
				}
				continue
			}
			if s, isStr := o["content"].(string); isStr && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// buildReasoningInputItem rebuilds a Responses reasoning item from chat
// assistant fields (store=false multi-turn continuity). Nil when there is
// nothing to re-send.
func buildReasoningInputItem(msg map[string]any) map[string]any {
	encrypted := jsonx.AsStr(msg["encrypted_content"])
	if encrypted == "" {
		encrypted = jsonx.AsStr(msg["reasoning_encrypted_content"])
	}
	if encrypted == "" {
		encrypted = jsonx.AsStr(jsonx.Get(msg["reasoning"], "encrypted_content"))
	}

	summaryText := ""
	if s := jsonx.AsStr(msg["reasoning_content"]); strings.TrimSpace(s) != "" {
		summaryText = s
	} else if s, isStr := msg["reasoning"].(string); isStr && strings.TrimSpace(s) != "" {
		summaryText = s
	} else if details, isArr := msg["reasoning_details"].([]any); isArr {
		var parts []string
		for _, d := range details {
			o := jsonx.AsObj(d)
			if o == nil {
				continue
			}
			if s, isStr := o["text"].(string); isStr {
				parts = append(parts, s)
				continue
			}
			if s, isStr := o["content"].(string); isStr {
				parts = append(parts, s)
			}
		}
		summaryText = strings.Join(parts, "\n")
	}

	if encrypted == "" && summaryText == "" {
		return nil
	}
	item := jsonx.ObjOf("type", ItemReasoning)
	if summaryText != "" {
		item["summary"] = []any{jsonx.ObjOf("type", ItemSummaryText, "text", summaryText)}
	}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}
