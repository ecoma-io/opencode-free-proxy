package relay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/translate"
	"opencode-free-proxy/internal/usage"
)

// ParseSSEToOpenAIResponse folds a raw Chat Completions SSE body into one
// chat.completion JSON (sseToJsonHandler.js parseSSEToOpenAIResponse).
// Returns (nil, false) when no chunks parsed; errBody non-nil when the stream
// carried an error chunk (a map error keeps its fields — the caller reads
// .message; a scalar/empty-container error carries none, so the caller falls
// back to its generic message exactly like sseToJsonHandler.js:309-313).
func ParseSSEToOpenAIResponse(rawSSE string, fallbackModel string) (result map[string]any, errBody map[string]any, ok bool) {
	// sseToJsonHandler.js:115-127 pushes ANY successfully parsed chunk —
	// numbers, strings, arrays and even JSON null are all valid JSON.parse
	// results and all count toward chunks.length; only a truthy .error diverts.
	var chunks []any
	for _, line := range strings.Split(rawSSE, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		// sseToJsonHandler.js:125 `if (chunk?.error)` — JS TRUTHINESS: a
		// scalar or empty-container error ("quota exceeded", 1, true, [], {})
		// is an error chunk too; only null/false/0/"" fall through as data.
		// The last error chunk wins (streamError is overwritten).
		if obj, isObj := chunk.(map[string]any); isObj && jsTruthy(obj["error"]) {
			if e := jsonx.AsObj(obj["error"]); e != nil {
				errBody = e
			} else {
				// Scalar error: JS reads parsed.error.message → undefined →
				// the caller's fallback text (sseToJsonHandler.js:312).
				errBody = map[string]any{}
			}
			continue
		}
		chunks = append(chunks, chunk)
	}
	if errBody != nil {
		return nil, errBody, true
	}
	if len(chunks) == 0 {
		return nil, nil, false
	}

	// sseToJsonHandler.js:133: `first = chunks[0]` — whatever it is. A scalar
	// first chunk reads as undefined fields, so id/created/model all fall back
	// (sseToJsonHandler.js:170-173) even when a later object carries them. (A
	// JSON-null first chunk would THROW at `first.id` in JS and surface as the
	// caller's 502 envelope — Go has no exception to propagate, so it degrades
	// to the same undefined-field fallbacks as a scalar; documented
	// divergence.)
	first, _ := chunks[0].(map[string]any)
	var contentParts, reasoningParts []string
	toolCallMap := map[int]map[string]any{}
	var toolIndexes []int
	// sseToJsonHandler.js:140/141: finish_reason keeps its RAW value (any
	// truthy type) and usage may be ANY typeof-"object" — arrays included.
	finishReason := any("stop")
	var usageObj any

	for _, raw := range chunks {
		chunk, isObj := raw.(map[string]any)
		if !isObj {
			// A non-object chunk contributes nothing: every read below is
			// optional-chained in JS (sseToJsonHandler.js:141-160).
			continue
		}
		choice := jsonx.AsObj(firstOf(chunk["choices"]))
		delta := jsonx.AsObj(jsonx.Get(choice, "delta"))
		if c, is := jsonx.Get(delta, "content").(string); is && c != "" {
			contentParts = append(contentParts, c)
		}
		if r, is := jsonx.Get(delta, "reasoning_content").(string); is && r != "" {
			reasoningParts = append(reasoningParts, r)
		}
		// sseToJsonHandler.js:141 `if (choice?.finish_reason)` — truthiness,
		// and the RAW value is assigned (a boolean/numeric finish survives).
		if fr := jsonx.Get(choice, "finish_reason"); jsTruthy(fr) {
			finishReason = fr
		}
		// sseToJsonHandler.js:142 `chunk?.usage && typeof chunk.usage ===
		// "object"` — typeof [] is "object", so an ARRAY usage is captured
		// too (and forwarded verbatim at 176).
		if u := chunk["usage"]; jsTruthy(u) {
			switch u.(type) {
			case map[string]any, []any:
				usageObj = u
			}
		}
		for _, tcRaw := range jsonx.AsArr(jsonx.Get(delta, "tool_calls")) {
			tc := jsonx.AsObj(tcRaw)
			if tc == nil {
				continue
			}
			idx := 0
			if v, is := tc["index"].(float64); is {
				idx = int(v)
			}
			existing, seen := toolCallMap[idx]
			if !seen {
				existing = jsonx.ObjOf(
					"id", "",
					"type", "function",
					"function", jsonx.ObjOf("name", "", "arguments", ""))
				toolCallMap[idx] = existing
				toolIndexes = append(toolIndexes, idx)
			}
			fn := jsonx.AsObj(existing["function"])
			// sseToJsonHandler.js:151-156 — all three accumulation gates are
			// TRUTHINESS over the RAW value (`if (tc.id)`, `if (tc.function?.
			// name)`), and `+=` concatenates with String() coercion, so a
			// numeric name/arguments fragment joins as its JS string
			// rendering.
			if id := tc["id"]; jsTruthy(id) {
				existing["id"] = id
			}
			if n := jsonx.Get(tc["function"], "name"); jsTruthy(n) {
				fn["name"] = usage.JSStr(fn["name"]) + usage.JSStr(n)
			}
			if a := jsonx.Get(tc["function"], "arguments"); jsTruthy(a) {
				fn["arguments"] = usage.JSStr(fn["arguments"]) + usage.JSStr(a)
			}
		}
	}

	content := strings.Join(contentParts, "")
	message := jsonx.ObjOf("role", "assistant")
	if content != "" {
		message["content"] = content
	} else if len(toolCallMap) > 0 {
		message["content"] = nil
	} else {
		message["content"] = ""
	}
	if len(reasoningParts) > 0 {
		message["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if len(toolCallMap) > 0 {
		sort.Ints(toolIndexes)
		calls := make([]any, 0, len(toolIndexes))
		for _, idx := range toolIndexes {
			calls = append(calls, toolCallMap[idx])
		}
		message["tool_calls"] = calls
	}

	// sseToJsonHandler.js:170-173 — `first.id || …` / `first.created || …` /
	// `first.model || fallbackModel || "unknown"` are TRUTHINESS checks over
	// the RAW values: a numeric id or a string created survives as-is, and
	// only falsy values fall through (a created of 0 IS falsy and hits the
	// now() fallback).
	id := first["id"]
	if !jsTruthy(id) {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	}
	created := first["created"]
	if !jsTruthy(created) {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	model := first["model"]
	if !jsTruthy(model) {
		model = fallbackModel
	}
	if !jsTruthy(model) {
		model = "unknown"
	}
	result = jsonx.ObjOf(
		"id", id,
		"object", "chat.completion",
		"created", created,
		"model", model,
		"choices", jsonx.ArrOf(jsonx.ObjOf(
			"index", 0,
			"message", message,
			"finish_reason", finishReason)),
	)
	if usageObj != nil {
		result["usage"] = usageObj
	}
	return result, nil, true
}

func firstOf(v any) any {
	if arr, is := v.([]any); is && len(arr) > 0 {
		return arr[0]
	}
	return nil
}

// responsesAggState accumulates convertResponsesStreamToJson. responseID and
// created are `any`: streamToJsonConverter.js:26-27 assigns them through
// TRUTHY `||` fallbacks that keep the RAW value (a numeric response id or a
// string created_at survives as-is).
type responsesAggState struct {
	responseID any
	created    any
	status     string
	usage      map[string]any
	items      map[int]map[string]any
}

// ConvertResponsesStreamToJson folds a raw Responses SSE body into one
// response JSON (transformer/streamToJsonConverter.js).
func ConvertResponsesStreamToJson(rawSSE string) map[string]any {
	now := time.Now()
	st := &responsesAggState{
		created: float64(now.UnixMilli() / 1000), // initState: Math.floor(Date.now()/1000)
		status:  "in_progress",
		usage:   map[string]any{"input_tokens": 0.0, "output_tokens": 0.0, "total_tokens": 0.0},
		items:   map[int]map[string]any{},
	}

	messages := strings.Split(rawSSE, "\n\n")
	for _, msg := range messages {
		processAggMessage(msg, st)
	}

	// Dense placeholder fill (streamToJsonConverter.js:89-93 — sane interior
	// gaps between present items keep their exact slots). Bounded by
	// construction: every key in st.items passed the
	// config.MaxResponsesOutputIndex gate at insert, so maxIndex can never
	// make this loop allocate super-linearly.
	output := make([]any, 0)
	maxIndex := -1
	for k := range st.items {
		if k > maxIndex {
			maxIndex = k
		}
	}
	for i := 0; i <= maxIndex; i++ {
		if item, has := st.items[i]; has {
			output = append(output, item)
		} else {
			output = append(output, jsonx.ObjOf(
				"type", "message", "content", jsonx.ArrOf(), "role", "assistant"))
		}
	}

	// streamToJsonConverter.js:95 `state.responseId || resp_…` — TRUTHINESS:
	// a truthy non-string id (set raw at line 26) forwards as-is, and only
	// the never-set/empty case synthesizes.
	id := st.responseID
	if !jsTruthy(id) {
		// JS: `resp_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
		// (streamToJsonConverter.js:96) — always a 6-char base36 fragment.
		// Left-pad the nanotime fragment so short values still fill 6 chars
		// (n % 36^6 never exceeds 6 base36 digits, so the frame is exact).
		id = fmt.Sprintf("resp_%d_%06s", now.UnixMilli(), formatInt36(now.UnixNano()%2176782336))
	}
	status := st.status
	if !jsTruthy(status) {
		status = "completed"
	}
	return jsonx.ObjOf(
		"id", id,
		"object", "response",
		"created_at", st.created,
		"status", status,
		"output", output,
		"usage", st.usage,
	)
}

// processAggMessage handles one SSE event block.
func processAggMessage(msg string, st *responsesAggState) {
	if strings.TrimSpace(msg) == "" {
		return
	}
	eventType := matchSSEField(msg, "event")
	dataStr := matchSSEField(msg, "data")
	if eventType == "" || dataStr == "" {
		return
	}
	if dataStr == "[DONE]" {
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(dataStr), &parsed); err != nil {
		return
	}

	switch eventType {
	case "response.created":
		// streamToJsonConverter.js:26-27 — `parsed.response?.id ||
		// state.responseId`: TRUTHINESS over the RAW value (only a falsy read
		// keeps the previous state), so a numeric id or string created_at
		// survives as-is.
		if id := jsonx.Get(parsed["response"], "id"); jsTruthy(id) {
			st.responseID = id
		}
		if created := jsonx.Get(parsed["response"], "created_at"); jsTruthy(created) {
			st.created = created
		}
	case "response.output_item.done":
		// streamToJsonConverter.js:29 `state.items.set(parsed.output_index ?? 0,
		// parsed.item)` — the Map insert is O(1) whatever the key, but the
		// ASSEMBLY is dense (streamToJsonConverter.js:89-93: `for
		// (let i = 0; i <= maxIndex; i++)` fills a placeholder for every hole),
		// so an upstream-controlled output_index of 1e18 would make the Go
		// port allocate 1e18 placeholder maps and OOM the process in seconds.
		// DIVERGENCE (deliberate, documented): JS has no guard here — V8 only
		// stops the loop at the 2^32-1 array-length limit, and the escaping
		// RangeError turns into the 502 envelope (sseToJsonHandler.js:298-301),
		// i.e. JS also dies on a hostile index, just more slowly and with the
		// whole response lost. Go drops the offending EVENT instead: indexes
		// beyond config.MaxResponsesOutputIndex (a bound no real stream
		// approaches) never reach the items map, the dense fill stays bounded
		// by that constant, and an otherwise-complete response still serves.
		// Negative indexes are dropped too — unobservable either way, since the
		// 0..maxIndex fill never reads them.
		idx := 0
		if v, is := parsed["output_index"].(float64); is {
			if v < 0 || v > float64(config.MaxResponsesOutputIndex) {
				return
			}
			idx = int(v)
		}
		if item := jsonx.AsObj(parsed["item"]); item != nil {
			st.items[idx] = item
		}
	case "response.completed", "response.done":
		st.status = "completed"
		// streamToJsonConverter.js:29-36 — `if (parsed.response?.usage)` is
		// TRUTHINESS (an array passes typeof-free truthiness too, and its
		// element reads are undefined → 0), and each counter keeps its RAW
		// truthy value through `parsed.response.usage.X || 0`.
		if respUsage := jsonx.Get(parsed["response"], "usage"); jsTruthy(respUsage) {
			st.usage["input_tokens"] = jsOr(jsonx.Get(respUsage, "input_tokens"), 0.0)
			st.usage["output_tokens"] = jsOr(jsonx.Get(respUsage, "output_tokens"), 0.0)
			st.usage["total_tokens"] = jsOr(jsonx.Get(respUsage, "total_tokens"), 0.0)
		}
	case "response.failed":
		st.status = "failed"
	}
}

// matchSSEField extracts `field: value` from an SSE block (first match, ^ at
// line start — mirrors the JS /^field:\s*(.+)$/m regexes).
func matchSSEField(msg, field string) string {
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, field+":") {
			return strings.TrimSpace(line[len(field)+1:])
		}
	}
	return ""
}

// ChatCompletionToResponses converts a chat.completion body into the Responses
// output shape (sseToJsonHandler.js chatCompletionToResponses) so tool_calls
// survive the non-streaming return path for responses clients.
func ChatCompletionToResponses(responseBody map[string]any, customToolNames map[string]bool) map[string]any {
	choice := jsonx.AsObj(firstOf(responseBody["choices"]))
	if choice == nil {
		return responseBody
	}
	message := jsonx.AsObj(choice["message"])
	if message == nil {
		message = jsonx.ObjOf()
	}
	output := make([]any, 0)

	reasoning := ""
	if r, is := message["reasoning_content"].(string); is && r != "" {
		reasoning = r
	} else if r, is := message["reasoning"].(string); is && r != "" {
		reasoning = r
	}
	if reasoning != "" {
		output = append(output, jsonx.ObjOf(
			"type", translate.ItemReasoning,
			"summary", jsonx.ArrOf(jsonx.ObjOf("type", translate.ItemSummaryText, "text", reasoning))))
	}

	if text, is := message["content"].(string); is && text != "" {
		output = append(output, jsonx.ObjOf(
			"type", translate.ItemMessage,
			"role", "assistant",
			"content", jsonx.ArrOf(jsonx.ObjOf(
				"type", translate.ItemOutputText, "text", text, "annotations", jsonx.ArrOf()))))
	}

	for _, tcRaw := range jsonx.AsArr(message["tool_calls"]) {
		tc := jsonx.AsObj(tcRaw)
		if tc == nil {
			continue
		}
		fn := jsonx.AsObj(tc["function"])
		if fn == nil {
			fn = jsonx.ObjOf()
		}
		// sseToJsonHandler.js:78-90 — the id/call_id/name fields go through
		// `|| ""` chains that keep the RAW truthy value (a numeric call_id
		// survives as a number); only `id` is stringified, by the template
		// literal. The custom-tool lookup (`customToolNames?.has(fn.name)`)
		// compares the raw name against a set that only ever holds non-empty
		// strings, so the string view is equivalent there.
		custom := customToolNames != nil && customToolNames[jsonx.AsStr(fn["name"])]
		prefix := "fc"
		if custom {
			prefix = "ctc"
		}
		idSuffix := ""
		if jsTruthy(tc["id"]) {
			idSuffix = usage.JSStr(tc["id"])
		}
		var callID any = ""
		if jsTruthy(tc["id"]) {
			callID = tc["id"]
		}
		var name any = ""
		if jsTruthy(fn["name"]) {
			name = fn["name"]
		}
		item := jsonx.ObjOf(
			"type", translate.ItemFunctionCall,
			"id", prefix+"_"+idSuffix,
			"call_id", callID,
			"name", name,
		)
		if custom {
			item["type"] = translate.ItemCustomToolCall
			item["id"] = "ctc_" + idSuffix
			item["input"] = extractCustomToolInput(fn["arguments"])
		} else {
			item["arguments"] = jsStringifyArguments(fn["arguments"])
		}
		output = append(output, item)
	}

	usageObj := jsonx.AsObj(responseBody["usage"])
	if usageObj == nil {
		usageObj = jsonx.ObjOf()
	}
	// sseToJsonHandler.js:94-97 — the id template stringifies the truthy raw
	// id, and the ^-anchored `resp_chatcmpl-` collapse replaces only at the
	// start of the concatenated string. created/model keep their RAW truthy
	// values through `|| fallback`.
	rawID := ""
	if jsTruthy(responseBody["id"]) {
		rawID = usage.JSStr(responseBody["id"])
	}
	id := "resp_" + rawID
	if strings.HasPrefix(id, "resp_chatcmpl-") {
		id = "resp_" + strings.TrimPrefix(id, "resp_chatcmpl-")
	}
	created := responseBody["created"]
	if !jsTruthy(created) {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	model := responseBody["model"]
	if !jsTruthy(model) {
		model = "unknown"
	}
	return jsonx.ObjOf(
		"id", id,
		"object", "response",
		"created_at", created,
		"model", model,
		"status", "completed",
		"background", false,
		"error", nil,
		"output", output,
		"usage", jsonx.ObjOf(
			"input_tokens", numOr(usageObj["prompt_tokens"], usageObj["input_tokens"]),
			"output_tokens", numOr(usageObj["completion_tokens"], usageObj["output_tokens"]),
			"total_tokens", numOr(usageObj["total_tokens"],
				float64(numOr(usageObj["prompt_tokens"], 0)+numOr(usageObj["completion_tokens"], 0))),
		),
	)
}

// jsStringifyArguments is sseToJsonHandler.js:88's
// `typeof v === "string" ? v : JSON.stringify(v || {})` (and the identical
// responses-branch site at 268): a string passes through, every FALSY value
// (0, false, null, "") stringifies the empty object, and any other truthy
// value JSON-serializes.
func jsStringifyArguments(v any) string {
	if s, is := v.(string); is {
		return s
	}
	if !jsTruthy(v) {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// jsKeyStr renders `m[key]` as it would appear inside a JS template literal:
// an ABSENT key stringifies to "undefined", a JSON null to "null" (both
// distinct from ""), everything else through String() coercion
// (sseToJsonHandler.js:263's `call_${item.name}_…`).
func jsKeyStr(m map[string]any, key string) string {
	v, has := m[key]
	if !has {
		return "undefined"
	}
	return usage.JSStr(v)
}

// extractCustomToolInput unwraps {"input":"..."} freeform payloads
// (sseToJsonHandler.js:45-52 — the arguments text resolves through the same
// `typeof === "string" ? … : JSON.stringify(v || {})` rule).
func extractCustomToolInput(argumentsValue any) string {
	argumentsText := jsStringifyArguments(argumentsValue)
	var parsed any
	if json.Unmarshal([]byte(argumentsText), &parsed) == nil {
		if o, isObj := parsed.(map[string]any); isObj {
			if in, isStr := o["input"].(string); isStr {
				return in
			}
		}
	}
	return argumentsText
}

// textFromResponsesMessageItem pulls display text out of a message item
// (sseToJsonHandler.js textFromResponsesMessageItem).
func textFromResponsesMessageItem(item map[string]any) string {
	content := jsonx.AsArr(item["content"])
	if content == nil {
		return ""
	}
	for _, cRaw := range content {
		c := jsonx.AsObj(cRaw)
		if c == nil || jsonx.AsStr(c["type"]) != "output_text" {
			continue
		}
		if s, is := c["text"].(string); is {
			return s
		}
	}
	for _, cRaw := range content {
		c := jsonx.AsObj(cRaw)
		if c == nil {
			continue
		}
		if s, is := c["text"].(string); is {
			return s
		}
	}
	return ""
}

// pickAssistantMessage finds the user-visible answer in a Responses output:
// the LAST message item carrying non-empty text, else the last message item
// (sseToJsonHandler.js pickAssistantMessageForChatCompletion).
func pickAssistantMessage(output []any) (msgItem map[string]any, textContent string) {
	var messages []map[string]any
	for _, item := range output {
		if o := jsonx.AsObj(item); o != nil && jsonx.AsStr(o["type"]) == "message" {
			messages = append(messages, o)
		}
	}
	if len(messages) == 0 {
		return nil, ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if text := textFromResponsesMessageItem(messages[i]); text != "" {
			return messages[i], text
		}
	}
	last := messages[len(messages)-1]
	return last, textFromResponsesMessageItem(last)
}

// BuildChatFromResponses converts the aggregated Responses JSON into a Chat
// Completions completion for a chat client whose muse-spark request was
// force-streamed upstream (sseToJsonHandler.js responses branch, chat side).
// synthesize applies the hidden-thinking seam (nil → identity).
func BuildChatFromResponses(jsonResponse map[string]any, model string, synthesize func(map[string]any) map[string]any) map[string]any {
	if synthesize == nil {
		synthesize = func(u map[string]any) map[string]any { return u }
	}
	output := jsonx.AsArr(jsonResponse["output"])
	_, textContent := pickAssistantMessage(output)

	var toolCalls []any
	for _, item := range output {
		o := jsonx.AsObj(item)
		if o == nil || jsonx.AsStr(o["type"]) != "function_call" {
			continue
		}
		// sseToJsonHandler.js:263-270 — `item.call_id || call_…` keeps the
		// RAW truthy value (a numeric call id survives); the synthesized id
		// template stringifies item.name, where an ABSENT name renders as
		// "undefined" (jsKeyStr). The function name forwards RAW — an absent
		// name key drops in JS, a null stays null — and arguments follow the
		// shared stringify rule (0/false → "{}").
		idx := len(toolCalls)
		var id any
		if v := o["call_id"]; jsTruthy(v) {
			id = v
		} else {
			id = fmt.Sprintf("call_%s_%d_%d", jsKeyStr(o, "name"), time.Now().UnixMilli(), idx)
		}
		fnObj := jsonx.ObjOf("arguments", jsStringifyArguments(o["arguments"]))
		if v, has := o["name"]; has {
			fnObj["name"] = v
		}
		toolCalls = append(toolCalls, jsonx.ObjOf(
			"id", id,
			"type", "function",
			"function", fnObj))
	}
	hasToolCalls := len(toolCalls) > 0

	message := jsonx.ObjOf("role", "assistant")
	if textContent != "" {
		message["content"] = textContent
	} else if hasToolCalls {
		message["content"] = nil
	} else {
		message["content"] = ""
	}
	if hasToolCalls {
		message["tool_calls"] = toolCalls
	}

	// sseToJsonHandler.js:283-288 — the status compares are STRICT equality
	// (strings), but the finish reason keeps the RAW truthy status through
	// `jsonResponse.status || "stop"`, and id/created/model keep their RAW
	// truthy values through the same `||` chains.
	status := jsonx.AsStr(jsonResponse["status"])
	responseDone := status == "completed" || status == "done"
	finishReason := any("stop")
	if hasToolCalls {
		finishReason = "tool_calls"
	} else if !responseDone {
		if s := jsonResponse["status"]; jsTruthy(s) {
			finishReason = s
		}
	}

	id := jsonResponse["id"]
	if !jsTruthy(id) {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	}
	created := jsonResponse["created_at"]
	if !jsTruthy(created) {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	respModel := jsonResponse["model"]
	if !jsTruthy(respModel) {
		respModel = model
	}

	// input_tokens EXCLUDES cached tokens on cache-capable upstreams — fold
	// the cache counters into prompt_tokens and surface the split.
	usage := jsonx.AsObj(jsonResponse["usage"])
	if usage == nil {
		usage = jsonx.ObjOf()
	}
	cacheRead := numOr(usage["cache_read_input_tokens"], usage["cached_tokens"])
	cacheCreate := numOr(usage["cache_creation_input_tokens"])
	inTokens := numOr(usage["input_tokens"]) + cacheRead + cacheCreate
	outTokens := numOr(usage["output_tokens"])
	clientUsage := jsonx.ObjOf(
		"prompt_tokens", inTokens,
		"completion_tokens", outTokens,
		"total_tokens", inTokens+outTokens)
	if cacheRead > 0 || cacheCreate > 0 {
		details := jsonx.ObjOf()
		if cacheRead > 0 {
			details["cached_tokens"] = cacheRead
		}
		if cacheCreate > 0 {
			details["cache_creation_tokens"] = cacheCreate
		}
		clientUsage["prompt_tokens_details"] = details
	}

	return jsonx.ObjOf(
		"id", id,
		"object", "chat.completion",
		"created", created,
		"model", respModel,
		"choices", jsonx.ArrOf(jsonx.ObjOf(
			"index", 0,
			"message", message,
			"finish_reason", finishReason)),
		"usage", synthesize(clientUsage),
	)
}

// StripRedundantReasoning drops message.reasoning_content whenever content is
// non-empty — reasoning is the only useful output when content is empty, so
// it is preserved there (sseToJsonHandler.js conditional strip).
func StripRedundantReasoning(body map[string]any) {
	for _, choiceRaw := range jsonx.AsArr(body["choices"]) {
		choice := jsonx.AsObj(choiceRaw)
		if choice == nil {
			continue
		}
		message := jsonx.AsObj(choice["message"])
		if message == nil {
			continue
		}
		if rc, is := message["reasoning_content"].(string); is && rc != "" {
			if c, is := message["content"].(string); is && c != "" {
				delete(message, "reasoning_content")
			}
		}
	}
}
