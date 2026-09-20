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
	// (sseToJsonHandler.js:170-173) even when a later object carries them.
	first, _ := chunks[0].(map[string]any)
	var contentParts, reasoningParts []string
	toolCallMap := map[int]map[string]any{}
	var toolIndexes []int
	finishReason := "stop"
	var usageObj map[string]any

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
		if fr := jsonx.AsStr(jsonx.Get(choice, "finish_reason")); fr != "" {
			finishReason = fr
		}
		if u, is := chunk["usage"].(map[string]any); is {
			usageObj = u
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
			if id := jsonx.AsStr(tc["id"]); id != "" {
				existing["id"] = id
			}
			if n := jsonx.AsStr(jsonx.Get(tc["function"], "name")); n != "" {
				fn["name"] = jsonx.AsStr(fn["name"]) + n
			}
			if a := jsonx.AsStr(jsonx.Get(tc["function"], "arguments")); a != "" {
				fn["arguments"] = jsonx.AsStr(fn["arguments"]) + a
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

	id := jsonx.AsStr(first["id"])
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	}
	created := jsonx.AsF64(first["created"])
	if created == 0 {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	model := jsonx.AsStr(first["model"])
	if model == "" {
		model = fallbackModel
	}
	if model == "" {
		// JS: `first.model || fallbackModel || "unknown"`
		// (sseToJsonHandler.js:173).
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

// responsesAggState accumulates convertResponsesStreamToJson.
type responsesAggState struct {
	responseID string
	created    float64
	status     string
	usage      map[string]any
	items      map[int]map[string]any
}

// ConvertResponsesStreamToJson folds a raw Responses SSE body into one
// response JSON (transformer/streamToJsonConverter.js).
func ConvertResponsesStreamToJson(rawSSE string) map[string]any {
	now := time.Now()
	st := &responsesAggState{
		created: float64(now.UnixMilli() / 1000),
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

	id := st.responseID
	if id == "" {
		// JS: `resp_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
		// (streamToJsonConverter.js:96) — always a 6-char base36 fragment.
		// Left-pad the nanotime fragment so short values still fill 6 chars
		// (n % 36^6 never exceeds 6 base36 digits, so the frame is exact).
		id = fmt.Sprintf("resp_%d_%06s", now.UnixMilli(), formatInt36(now.UnixNano()%2176782336))
	}
	status := st.status
	if status == "" {
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
		if id := jsonx.AsStr(jsonx.Get(parsed["response"], "id")); id != "" {
			st.responseID = id
		}
		if created := jsonx.AsF64(jsonx.Get(parsed["response"], "created_at")); created != 0 {
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
		respUsage := jsonx.AsObj(jsonx.Get(parsed["response"], "usage"))
		if respUsage != nil {
			st.usage["input_tokens"] = jsonx.AsF64(respUsage["input_tokens"])
			st.usage["output_tokens"] = jsonx.AsF64(respUsage["output_tokens"])
			st.usage["total_tokens"] = jsonx.AsF64(respUsage["total_tokens"])
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
		name := jsonx.AsStr(fn["name"])
		custom := customToolNames != nil && customToolNames[name]
		prefix := "fc"
		item := jsonx.ObjOf(
			"type", translate.ItemFunctionCall,
			"id", prefix+"_"+jsonx.AsStr(tc["id"]),
			"call_id", jsonx.AsStr(tc["id"]),
			"name", name,
		)
		args, argsIsStr := fn["arguments"].(string)
		if custom {
			item["type"] = translate.ItemCustomToolCall
			item["id"] = "ctc_" + jsonx.AsStr(tc["id"])
			item["input"] = extractCustomToolInput(args)
		} else {
			// JS: typeof fn.arguments === "string" ? fn.arguments : stringify(fn.arguments || {})
			if !argsIsStr {
				if fn["arguments"] == nil {
					args = "{}"
				} else {
					b, _ := json.Marshal(fn["arguments"])
					args = string(b)
				}
			}
			item["arguments"] = args
		}
		output = append(output, item)
	}

	usageObj := jsonx.AsObj(responseBody["usage"])
	if usageObj == nil {
		usageObj = jsonx.ObjOf()
	}
	id := "resp_" + jsonx.AsStr(responseBody["id"])
	id = strings.Replace(id, "resp_chatcmpl-", "resp_", 1)
	created := jsonx.AsF64(responseBody["created"])
	if created == 0 {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	model := jsonx.AsStr(responseBody["model"])
	if model == "" {
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

// extractCustomToolInput unwraps {"input":"..."} freeform payloads.
func extractCustomToolInput(argumentsValue any) string {
	argumentsText, isStr := argumentsValue.(string)
	if !isStr {
		if argumentsValue == nil {
			argumentsText = "{}"
		} else {
			b, _ := json.Marshal(argumentsValue)
			argumentsText = string(b)
		}
	}
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
		idx := len(toolCalls)
		id := jsonx.AsStr(o["call_id"])
		if id == "" {
			id = fmt.Sprintf("call_%s_%d_%d", jsonx.AsStr(o["name"]), time.Now().UnixMilli(), idx)
		}
		// JS: typeof arguments === "string" ? arguments : stringify(arguments || {})
		var args string
		if s, is := o["arguments"].(string); is {
			args = s
		} else if o["arguments"] == nil {
			args = "{}"
		} else {
			b, _ := json.Marshal(o["arguments"])
			args = string(b)
		}
		toolCalls = append(toolCalls, jsonx.ObjOf(
			"id", id,
			"type", "function",
			"function", jsonx.ObjOf(
				"name", jsonx.AsStr(o["name"]),
				"arguments", args)))
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

	status := jsonx.AsStr(jsonResponse["status"])
	responseDone := status == "completed" || status == "done"
	finishReason := status
	if hasToolCalls {
		finishReason = "tool_calls"
	} else if responseDone {
		finishReason = "stop"
	} else if finishReason == "" {
		finishReason = "stop"
	}

	id := jsonx.AsStr(jsonResponse["id"])
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	}
	created := jsonx.AsF64(jsonResponse["created_at"])
	if created == 0 {
		created = float64(time.Now().UnixMilli() / 1000)
	}
	respModel := jsonx.AsStr(jsonResponse["model"])
	if respModel == "" {
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
