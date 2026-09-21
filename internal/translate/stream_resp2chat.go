package translate

import (
	"fmt"
	"time"

	"opencode-free-proxy/internal/jsonx"
)

// RespToChatState carries openaiResponsesToOpenAIResponse's accumulator across
// SSE events (Responses upstream → Chat client).
type RespToChatState struct {
	Started       bool
	ChatID        string
	Created       int64
	Model         string
	ToolCallIndex int
	// CurrentToolCall is sticky: the RAW truthy call id (:481
	// `item.call_id || fallbackToolCallId()`), or nil.
	CurrentToolCall any
	// item_id → chat tool_calls index; deltas key on item_id so parallel calls
	// stay separate when upstream emits all addeds before dones. JS keys a
	// `new Map()` (openai-responses.js:453) — SameValueZero, no string
	// coercion, hence mapKeyOf's type-tagged keys.
	RespToolChatIndex   map[string]int
	RespToolArgsEmitted map[string]bool
	FinishReasonSent    bool
	FinishReason        string
	Usage               map[string]any
	// Error mirrors JS state.error — whatever truthy payload the event
	// carried, any shape (:584 `state.error = error`).
	Error any
	now   func() time.Time
}

// NewRespToChatState seeds a translator state (stream.js createStreamState).
func NewRespToChatState(model string) *RespToChatState {
	return &RespToChatState{Model: model, now: time.Now}
}

func (s *RespToChatState) timestamp() int64 { return s.now().UnixMilli() }

func (s *RespToChatState) chunk(delta map[string]any, finish any) map[string]any {
	return BuildChunk(s.ChatID, s.Created, s.modelOrFallback(), delta, finish)
}

func (s *RespToChatState) modelOrFallback() string {
	if s.Model != "" {
		return s.Model
	}
	return ModelFallback
}

// computeFinishReason: tool_calls when any call was announced, else stop.
// CurrentToolCallId stays sticky so a flush can still finalize as tool_calls.
func (s *RespToChatState) computeFinishReason() string {
	if s.ToolCallIndex > 0 || s.CurrentToolCall != nil {
		return "tool_calls"
	}
	return "stop"
}

// finalChunk is the flush/completed chunk carrying finish_reason and usage.
func (s *RespToChatState) finalChunk() map[string]any {
	finish := s.computeFinishReason()
	s.FinishReasonSent = true
	s.FinishReason = finish
	id := s.ChatID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", s.timestamp())
	}
	created := s.Created
	if created == 0 {
		created = s.timestamp() / 1000
	}
	final := BuildChunk(id, created, s.modelOrFallback(), map[string]any{}, finish)
	if s.Usage != nil {
		final["usage"] = s.Usage
	}
	return final
}

// Convert translates one parsed Responses SSE event into zero or one Chat
// chunk; nil means "ignore this event". A nil chunk input flushes.
func (s *RespToChatState) Convert(chunk map[string]any) map[string]any {
	if chunk == nil {
		if s.FinishReasonSent || !s.Started {
			return nil
		}
		return s.finalChunk()
	}

	eventType := jsonx.AsStr(chunk["type"])
	if eventType == "" {
		eventType = jsonx.AsStr(chunk["event"])
	}
	data := chunk
	if d, is := chunk["data"].(map[string]any); is {
		data = d
	}

	if !s.Started {
		s.Started = true
		s.ChatID = fmt.Sprintf("chatcmpl-%d", s.timestamp())
		s.Created = s.timestamp() / 1000
		s.ToolCallIndex = 0
		s.CurrentToolCall = nil
	}

	switch eventType {
	case "response.output_text.delta":
		// JS `const delta = data.delta || ""; if (!delta) return null`
		// (openai-responses.js:460-461) — truthiness, not stringiness: a
		// truthy non-string delta (true, 5, {}) streams through as the
		// content value; falsy ones are dropped.
		delta := data["delta"]
		if !jsonx.Truthy(delta) {
			return nil
		}
		return s.chunk(map[string]any{"content": delta}, nil)

	case "response.output_text.done":
		return nil

	case "response.output_item.added":
		item := jsonx.AsObj(data["item"])
		itemType := jsonx.AsStr(jsonx.Get(item, "type"))
		if itemType != ItemFunctionCall && itemType != "custom_tool_call" {
			return nil
		}
		// JS `item.call_id || fallbackToolCallId()` (:481) — truthiness; the
		// raw truthy value becomes the chunk's tool_call id.
		callID := jsonx.Get(item, "call_id")
		if jsonx.Truthy(callID) {
			s.CurrentToolCall = callID
		} else {
			s.CurrentToolCall = FallbackToolCallID()
		}
		// JS `item.id || data.item_id || state.currentToolCallId` (:483) —
		// raw values through truthy || chains; the Map key keeps type
		// identity (mapKeyOf).
		key := jsonx.Get(item, "id")
		if !jsonx.Truthy(key) {
			key = data["item_id"]
		}
		if !jsonx.Truthy(key) {
			key = s.CurrentToolCall
		}
		if s.RespToolChatIndex == nil {
			s.RespToolChatIndex = map[string]int{}
		}
		var idx int
		if prev, seen := s.RespToolChatIndex[mapKeyOf(key)]; seen {
			idx = prev // duplicate added (retry) — reuse
		} else {
			idx = s.ToolCallIndex
			s.ToolCallIndex++
			s.RespToolChatIndex[mapKeyOf(key)] = idx
		}
		name := jsonx.AsStr(jsonx.Get(item, "name"))
		return s.chunk(map[string]any{
			"tool_calls": jsonx.ArrOf(jsonx.ObjOf(
				"index", idx,
				"id", s.CurrentToolCall,
				"type", BlockFunction,
				"function", jsonx.ObjOf("name", name, "arguments", ""))),
		}, nil)

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		// Same truthiness gate as the text delta (:508-509) — the raw value
		// rides the chunk's function.arguments.
		argsDelta := data["delta"]
		if !jsonx.Truthy(argsDelta) {
			return nil
		}
		// JS `data.item_id ? map.get(item_id) : undefined` + `known ??
		// fallback` (:511-512) — truthiness gate on the raw key, nullish
		// rescue only for a miss.
		idx := maxInt(0, s.ToolCallIndex-1)
		if itemID := data["item_id"]; jsonx.Truthy(itemID) {
			if known, ok := s.RespToolChatIndex[mapKeyOf(itemID)]; ok {
				idx = known
			}
		}
		if s.RespToolArgsEmitted == nil {
			s.RespToolArgsEmitted = map[string]bool{}
		}
		s.RespToolArgsEmitted[mapKeyOf(idx)] = true
		return s.chunk(map[string]any{
			"tool_calls": jsonx.ArrOf(jsonx.ObjOf(
				"index", idx,
				"function", jsonx.ObjOf("arguments", argsDelta))),
		}, nil)

	case "response.output_item.done":
		item := jsonx.AsObj(data["item"])
		itemType := jsonx.AsStr(jsonx.Get(item, "type"))
		if itemType != ItemFunctionCall && itemType != "custom_tool_call" {
			return nil
		}
		// JS `const key = data.item?.id || data.item_id` (:525) — truthy
		// chain keeping the raw value; `const idx = (key && map.get(key))
		// ?? fallback` (:526): nullish coalescing rescues only
		// undefined/null, so a falsy-but-present key ("" | 0 | false)
		// survives as the chunk index itself.
		key := jsonx.Get(item, "id")
		if !jsonx.Truthy(key) {
			key = data["item_id"]
		}
		var idx any
		switch {
		case key == nil:
			idx = maxInt(0, s.ToolCallIndex-1) // undefined/null ?? fallback
		case jsonx.Truthy(key):
			if known, ok := s.RespToolChatIndex[mapKeyOf(key)]; ok {
				idx = known
			} else {
				idx = maxInt(0, s.ToolCallIndex-1) // get() miss ?? fallback
			}
		default:
			idx = key // ""/0/false are non-nullish — they stay the index
		}
		fullArgs, isStr := jsonx.Get(item, "arguments").(string)
		if isStr && fullArgs != "" {
			if s.RespToolArgsEmitted == nil {
				s.RespToolArgsEmitted = map[string]bool{}
			}
			if !s.RespToolArgsEmitted[mapKeyOf(idx)] {
				s.RespToolArgsEmitted[mapKeyOf(idx)] = true
				return s.chunk(map[string]any{
					"tool_calls": jsonx.ArrOf(jsonx.ObjOf(
						"index", idx,
						"function", jsonx.ObjOf("arguments", fullArgs))),
				}, nil)
			}
		}
		return nil

	case "response.completed", "response.done":
		resp := jsonx.AsObj(data["response"])
		// JS gate `responseUsage && typeof responseUsage === "object"`
		// (:545) — typeof [] is "object" too, so arrays pass; strings,
		// numbers and bools do not. Field reads off an array yield undefined
		// (jsonx.Get returns nil for non-objects), matching JS.
		var respUsage any
		switch v := jsonx.Get(resp, "usage").(type) {
		case map[string]any, []any:
			respUsage = v
		}
		if respUsage != nil {
			// `a || b || 0` chains (:546-550) keep the RAW truthy value — a
			// numeric-string token count flows through as a string and only
			// the buildUsage gates coerce numerically.
			inputTokens := jsonx.Get(respUsage, "input_tokens")
			if !jsonx.Truthy(inputTokens) {
				inputTokens = jsonx.Get(respUsage, "prompt_tokens")
			}
			if !jsonx.Truthy(inputTokens) {
				inputTokens = float64(0)
			}
			outputTokens := jsonx.Get(respUsage, "output_tokens")
			if !jsonx.Truthy(outputTokens) {
				outputTokens = jsonx.Get(respUsage, "completion_tokens")
			}
			if !jsonx.Truthy(outputTokens) {
				outputTokens = float64(0)
			}
			cacheRead := jsonx.Get(jsonx.Get(respUsage, "input_tokens_details"), "cached_tokens")
			if !jsonx.Truthy(cacheRead) {
				cacheRead = jsonx.Get(respUsage, "cache_read_input_tokens")
			}
			if !jsonx.Truthy(cacheRead) {
				cacheRead = float64(0)
			}
			s.Usage = BuildUsage(inputTokens, outputTokens, jsAdd(inputTokens, outputTokens), cacheRead, 0, 0)
		}
		if !s.FinishReasonSent {
			return s.finalChunk()
		}
		return nil

	case "error", "response.failed":
		if s.FinishReasonSent {
			return nil
		}
		// JS `const error = data.error || data.response?.error; if (error)`
		// (openai-responses.js:582-584) — truthiness, not shape: string,
		// number and bool payloads surface too; only falsy ones are dropped.
		errVal := data["error"]
		if !jsonx.Truthy(errVal) {
			errVal = jsonx.Get(data["response"], "error")
		}
		if !jsonx.Truthy(errVal) {
			return nil
		}
		s.Error = errVal
		s.FinishReasonSent = true
		// `[Error] ${error.message || JSON.stringify(error)}` (:590) — the
		// `||` keeps the RAW truthy message (a numeric 429 renders "429" via
		// template-literal ToString, not the whole-error stringify); only a
		// falsy/missing message falls back to JSON.stringify of the payload,
		// so a string error renders quoted ("\"rate limited\"").
		msg := ""
		if m := jsonx.Get(errVal, "message"); jsonx.Truthy(m) {
			msg = jsStringOf(m)
		} else {
			msg = jsonStringifyOf(errVal)
		}
		id := s.ChatID
		if id == "" {
			id = fmt.Sprintf("chatcmpl-%d", s.timestamp())
		}
		created := s.Created
		if created == 0 {
			created = s.timestamp() / 1000
		}
		return BuildChunk(id, created, s.modelOrFallback(),
			map[string]any{"content": "[Error] " + msg}, "stop")

	case "response.reasoning_summary_text.delta":
		// Same `data.delta || ""` + `!delta` gate (:599-600); reasoningDelta
		// embeds the raw value (concerns/reasoning.js:4-8).
		delta := data["delta"]
		if !jsonx.Truthy(delta) {
			return nil
		}
		return s.chunk(ReasoningDelta(delta), nil)
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
