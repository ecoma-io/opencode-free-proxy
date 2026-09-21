package relay

// Tests for the non-streaming aggregation paths ported in
// internal/relay/nonstream.go: sseToJsonHandler.js
// (parseSSEToOpenAIResponse, chatCompletionToResponses, the responses→chat
// branch) and streamToJsonConverter.js (convertResponsesStreamToJson).

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// sseOf joins whole SSE lines into a body with blank-line separators.
func sseOf(lines ...string) string {
	return strings.Join(lines, "\n\n") + "\n\n"
}

func TestParseSSEToOpenAIResponse(t *testing.T) {
	t.Run("folds content reasoning tool calls usage and finish", func(t *testing.T) {
		raw := sseOf(
			`data: {"id":"chatcmpl-1","created":1700000000,"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"llo","reasoning_content":"th"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"ink"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		)
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fallback")
		if !ok || errBody != nil || result == nil {
			t.Fatalf("ok=%v errBody=%v result=%v", ok, errBody, result)
		}
		if result["id"] != "chatcmpl-1" || result["object"] != "chat.completion" ||
			result["created"] != 1700000000.0 || result["model"] != "gpt" {
			t.Fatalf("envelope = %v", result)
		}
		choice := result["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "tool_calls" {
			t.Fatalf("finish_reason = %v", choice["finish_reason"])
		}
		msg := choice["message"].(map[string]any)
		if msg["role"] != "assistant" || msg["content"] != "Hello" {
			t.Fatalf("message = %v", msg)
		}
		if msg["reasoning_content"] != "think" {
			t.Fatalf("reasoning_content = %v, want the joined deltas", msg["reasoning_content"])
		}
		wantCall := map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
		}
		calls := msg["tool_calls"].([]any)
		if len(calls) != 1 || !reflect.DeepEqual(calls[0], wantCall) {
			t.Fatalf("tool_calls = %v, want %v", calls, wantCall)
		}
		wantUsage := map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0, "total_tokens": 15.0}
		if !reflect.DeepEqual(result["usage"], wantUsage) {
			t.Fatalf("usage = %v, want the chunk usage verbatim %v", result["usage"], wantUsage)
		}
	})

	t.Run("tool calls sort by index", func(t *testing.T) {
		raw := sseOf(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"b"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"a"}}]}}]}`,
		)
		result, _, _ := ParseSSEToOpenAIResponse(raw, "fb")
		calls := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
		if len(calls) != 2 || calls[0].(map[string]any)["id"] != "call_a" {
			t.Fatalf("tool_calls = %v, want call_a first (index order)", calls)
		}
	})

	t.Run("last finish reason and usage win", func(t *testing.T) {
		raw := sseOf(
			`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"y"},"finish_reason":"length"}]}`,
			`data: {"usage":{"prompt_tokens":1}}`,
			`data: {"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok {
			t.Fatal("expected a result")
		}
		choice := result["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "length" {
			t.Fatalf("finish_reason = %v, want length (last wins)", choice["finish_reason"])
		}
		if got := result["usage"].(map[string]any)["prompt_tokens"]; got != 2.0 {
			t.Fatalf("usage = %v, want the last chunk's", result["usage"])
		}
	})

	t.Run("malformed lines and DONE are skipped", func(t *testing.T) {
		raw := sseOf(
			`data: not-json`,
			`data: [DONE]`,
			`event: ping`,
			`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			`data: [DONE]`,
		)
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok || errBody != nil || result == nil {
			t.Fatalf("ok=%v errBody=%v", ok, errBody)
		}
		msg := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "ok" {
			t.Fatalf("content = %v", msg["content"])
		}
	})

	t.Run("error chunk short-circuits", func(t *testing.T) {
		raw := sseOf(
			`data: {"error":{"message":"boom","type":"server_error","code":500}}`,
			`data: {"choices":[{"index":0,"delta":{"content":"ignored"}}]}`,
		)
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok || result != nil {
			t.Fatalf("ok=%v result=%v", ok, result)
		}
		if errBody["message"] != "boom" || errBody["type"] != "server_error" || errBody["code"] != 500.0 {
			t.Fatalf("errBody = %v", errBody)
		}
	})

	t.Run("no parsable chunks", func(t *testing.T) {
		for _, raw := range []string{"", "event: only\n\n", "data: [DONE]\n\n", "data: {broken\n\n"} {
			result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
			if ok || result != nil || errBody != nil {
				t.Fatalf("ParseSSEToOpenAIResponse(%q) = %v, %v, %v", raw, result, errBody, ok)
			}
		}
	})

	t.Run("id created model fallbacks", func(t *testing.T) {
		raw := sseOf(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "fb-model")
		if !ok {
			t.Fatal("expected a result")
		}
		// Parity note: JS falls back identically — `first.id || chatcmpl-…`
		// and `first.created || Math.floor(Date.now()/1000)`
		// (sseToJsonHandler.js:170-173); a falsy created substitutes now().
		if id, _ := result["id"].(string); !regexp.MustCompile(`^chatcmpl-\d+$`).MatchString(id) {
			t.Fatalf("id = %v, want chatcmpl-<millis>", result["id"])
		}
		if created := result["created"].(float64); created < 1600000000 {
			t.Fatalf("created = %v, want now-in-seconds", result["created"])
		}
		if result["model"] != "fb-model" {
			t.Fatalf("model = %v, want the fallback", result["model"])
		}
	})

	// sseToJsonHandler.js:173: `first.model || fallbackModel || "unknown"`.
	t.Run("model falls back to unknown when the fallback is empty too", func(t *testing.T) {
		raw := sseOf(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "")
		if !ok {
			t.Fatal("expected a result")
		}
		if result["model"] != "unknown" {
			t.Fatalf("model = %v, want the unknown fallback", result["model"])
		}
	})

	t.Run("content nil with tool calls empty without", func(t *testing.T) {
		rawTools := sseOf(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		result, _, _ := ParseSSEToOpenAIResponse(rawTools, "fb")
		msg := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != nil {
			t.Fatalf("content = %v, want nil for a tool-only stream", msg["content"])
		}
		rawEmpty := sseOf(`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
		result2, _, _ := ParseSSEToOpenAIResponse(rawEmpty, "fb")
		msg2 := result2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg2["content"] != "" {
			t.Fatalf("content = %v, want the empty string", msg2["content"])
		}
	})
}

func TestConvertResponsesStreamToJson(t *testing.T) {
	t.Run("folds items usage and status", func(t *testing.T) {
		// Deliberately no trailing blank line: the final block must still fold.
		raw := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_abc","created_at":1700000000}}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`
		got := ConvertResponsesStreamToJson(raw)
		if got["id"] != "resp_abc" || got["created_at"] != 1700000000.0 || got["status"] != "completed" {
			t.Fatalf("envelope = %v", got)
		}
		if got["object"] != "response" {
			t.Fatalf("object = %v", got["object"])
		}
		wantUsage := map[string]any{"input_tokens": 10.0, "output_tokens": 4.0, "total_tokens": 14.0}
		if !reflect.DeepEqual(got["usage"], wantUsage) {
			t.Fatalf("usage = %v, want %v", got["usage"], wantUsage)
		}
		out := got["output"].([]any)
		if len(out) != 2 {
			t.Fatalf("output = %v, want 2 slots (index-0 hole + index-1 item)", out)
		}
		wantHole := map[string]any{"type": "message", "content": []any{}, "role": "assistant"}
		if !reflect.DeepEqual(out[0], wantHole) {
			t.Fatalf("hole filler = %v, want %v", out[0], wantHole)
		}
		wantItem := map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "world"}},
		}
		if !reflect.DeepEqual(out[1], wantItem) {
			t.Fatalf("item = %v, want %v", out[1], wantItem)
		}
	})

	t.Run("response.failed marks the status", func(t *testing.T) {
		raw := "event: response.failed\n" + `data: {"type":"response.failed","response":{"error":{"message":"x"}}}`
		if got := ConvertResponsesStreamToJson(raw); got["status"] != "failed" {
			t.Fatalf("status = %v, want failed", got["status"])
		}
	})

	t.Run("DONE and malformed data ignored", func(t *testing.T) {
		raw := "event: response.completed\n" + "data: [DONE]\n\n" +
			"event: response.output_item.done\n" + "data: not-json\n\n" +
			"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[],"role":"assistant"}}`
		got := ConvertResponsesStreamToJson(raw)
		if got["status"] != "in_progress" {
			t.Fatalf("a [DONE] data line must not complete the response: %v", got["status"])
		}
		if len(got["output"].([]any)) != 1 {
			t.Fatalf("output = %v, want just the one stored item", got["output"])
		}
	})

	t.Run("id created and status fallbacks", func(t *testing.T) {
		raw := "event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[],"role":"assistant"}}`
		got := ConvertResponsesStreamToJson(raw)
		if id, _ := got["id"].(string); !regexp.MustCompile(`^resp_\d+_[0-9a-z]{6}$`).MatchString(id) {
			t.Fatalf("id = %v, want resp_<millis>_<base36>", got["id"])
		}
		if got["created_at"].(float64) < 1600000000 {
			t.Fatalf("created_at = %v, want now", got["created_at"])
		}
		if got["status"] != "in_progress" {
			t.Fatalf("status = %v, want the in_progress default", got["status"])
		}
		wantUsage := map[string]any{"input_tokens": 0.0, "output_tokens": 0.0, "total_tokens": 0.0}
		if !reflect.DeepEqual(got["usage"], wantUsage) {
			t.Fatalf("usage = %v, want zeros", got["usage"])
		}
	})

	// Regression (parity bug, fixed): the fallback id sliced [:6] off the
	// base36 form of UnixNano()%36^6, which is shorter than 6 chars whenever
	// the value is below 36^5 (~2.8% of nanotime values) and panicked the
	// conversion. JS (streamToJsonConverter.js:96) uses
	// Math.random().toString(36).slice(2, 8) — never shorter than 6; the port
	// left-pads the fragment to 6 instead.
	t.Run("fallback id fragment never panics and stays 6 chars", func(t *testing.T) {
		wantID := regexp.MustCompile(`^resp_\d+_[0-9a-z]{6}$`)
		for i := 0; i < 5000; i++ {
			got := ConvertResponsesStreamToJson("event: response.failed\n" + `data: {"type":"response.failed"}`)
			if id, _ := got["id"].(string); !wantID.MatchString(id) {
				t.Fatalf("iteration %d: id = %q, want resp_<millis>_<6 base36 chars>", i, id)
			}
		}
	})
}

func TestChatCompletionToResponses(t *testing.T) {
	t.Run("reasoning message and tool items", func(t *testing.T) {
		body := map[string]any{
			"id": "chatcmpl-abc", "created": 1700000000.0, "model": "gpt",
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role":              "assistant",
					"reasoning_content": "pondering",
					"content":           "answer text",
					"tool_calls": []any{
						map[string]any{"id": "call_1", "type": "function",
							"function": map[string]any{"name": "f", "arguments": `{"x":1}`}},
						map[string]any{"id": "call_2", "type": "function",
							"function": map[string]any{"name": "codex", "arguments": `{"input":"raw cmd"}`}},
					},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0},
		}
		got := ChatCompletionToResponses(body, map[string]bool{"codex": true})
		// resp_chatcmpl-… collapses to resp_… (JS replace("resp_chatcmpl-", "resp_")).
		if got["id"] != "resp_abc" {
			t.Fatalf("id = %v, want resp_abc", got["id"])
		}
		if got["object"] != "response" || got["status"] != "completed" ||
			got["background"] != false || got["error"] != nil {
			t.Fatalf("envelope = %v", got)
		}
		if got["model"] != "gpt" || got["created_at"] != 1700000000.0 {
			t.Fatalf("envelope = %v", got)
		}
		out := got["output"].([]any)
		if len(out) != 4 {
			t.Fatalf("output = %v, want reasoning + message + function_call + custom_tool_call", out)
		}
		wantReasoning := map[string]any{
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": "pondering"}},
		}
		if !reflect.DeepEqual(out[0], wantReasoning) {
			t.Fatalf("reasoning item = %v, want %v", out[0], wantReasoning)
		}
		wantMessage := map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "answer text", "annotations": []any{}}},
		}
		if !reflect.DeepEqual(out[1], wantMessage) {
			t.Fatalf("message item = %v, want %v", out[1], wantMessage)
		}
		wantFC := map[string]any{
			"type": "function_call", "id": "fc_call_1", "call_id": "call_1",
			"name": "f", "arguments": `{"x":1}`,
		}
		if !reflect.DeepEqual(out[2], wantFC) {
			t.Fatalf("function_call item = %v, want %v", out[2], wantFC)
		}
		// Custom tools unwrap the {"input": …} freeform wrapper.
		wantCTC := map[string]any{
			"type": "custom_tool_call", "id": "ctc_call_2", "call_id": "call_2",
			"name": "codex", "input": "raw cmd",
		}
		if !reflect.DeepEqual(out[3], wantCTC) {
			t.Fatalf("custom_tool_call item = %v, want %v", out[3], wantCTC)
		}
		// total_tokens falls back to prompt + completion when missing.
		wantUsage := map[string]any{"input_tokens": 10.0, "output_tokens": 5.0, "total_tokens": 15.0}
		if !reflect.DeepEqual(got["usage"], wantUsage) {
			t.Fatalf("usage = %v, want %v", got["usage"], wantUsage)
		}
	})

	t.Run("arguments object is marshaled missing becomes empty object", func(t *testing.T) {
		body := map[string]any{
			"id": "x",
			"choices": []any{map[string]any{
				"message": map[string]any{
					"tool_calls": []any{
						map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": map[string]any{"a": 1.0}}},
						map[string]any{"id": "c2", "function": map[string]any{"name": "g"}},
					},
				},
			}},
		}
		got := ChatCompletionToResponses(body, nil)
		out := got["output"].([]any)
		if args := out[0].(map[string]any)["arguments"]; args != `{"a":1}` {
			t.Fatalf("object arguments = %v, want the JSON encoding", args)
		}
		if args := out[1].(map[string]any)["arguments"]; args != "{}" {
			t.Fatalf("missing arguments = %v, want {}", args)
		}
	})

	t.Run("missing usage and model fall back", func(t *testing.T) {
		body := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}
		got := ChatCompletionToResponses(body, nil)
		wantUsage := map[string]any{"input_tokens": 0.0, "output_tokens": 0.0, "total_tokens": 0.0}
		if !reflect.DeepEqual(got["usage"], wantUsage) {
			t.Fatalf("usage = %v, want %v", got["usage"], wantUsage)
		}
		if got["model"] != "unknown" {
			t.Fatalf("model = %v, want the unknown fallback", got["model"])
		}
		if got["created_at"].(float64) < 1600000000 {
			t.Fatalf("created_at = %v, want now", got["created_at"])
		}
	})

	t.Run("no choices returns the body untouched", func(t *testing.T) {
		body := map[string]any{"error": map[string]any{"message": "boom"}}
		if got := ChatCompletionToResponses(body, nil); !reflect.DeepEqual(got, body) {
			t.Fatalf("got %v, want the body back as-is", got)
		}
	})
}

func TestBuildChatFromResponses(t *testing.T) {
	t.Run("picks the last non-empty message text", func(t *testing.T) {
		resp := map[string]any{
			"id": "resp_1", "created_at": 1700000000.0, "model": "gpt", "status": "completed",
			"output": []any{
				map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "draft"}}},
				map[string]any{"type": "reasoning", "summary": []any{}},
				map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "final"}}},
			},
			"usage": map[string]any{"input_tokens": 7.0, "output_tokens": 2.0},
		}
		got := BuildChatFromResponses(resp, "fallback-m", nil)
		if got["id"] != "resp_1" || got["object"] != "chat.completion" ||
			got["model"] != "gpt" || got["created"] != 1700000000.0 {
			t.Fatalf("envelope = %v", got)
		}
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason = %v, want stop (completed, no tools)", choice["finish_reason"])
		}
		msg := choice["message"].(map[string]any)
		if msg["content"] != "final" {
			t.Fatalf("content = %v, want the LAST non-empty message text", msg["content"])
		}
		wantUsage := map[string]any{"prompt_tokens": 7.0, "completion_tokens": 2.0, "total_tokens": 9.0}
		if !reflect.DeepEqual(got["usage"], wantUsage) {
			t.Fatalf("usage = %v, want %v", got["usage"], wantUsage)
		}
	})

	t.Run("tool calls with call ids", func(t *testing.T) {
		resp := map[string]any{
			"id": "resp_2", "status": "completed",
			"output": []any{
				map[string]any{"type": "function_call", "call_id": "call_9", "name": "f", "arguments": `{"x":1}`},
			},
			"usage": map[string]any{},
		}
		got := BuildChatFromResponses(resp, "m", nil)
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "tool_calls" {
			t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
		}
		msg := choice["message"].(map[string]any)
		if msg["content"] != nil {
			t.Fatalf("content = %v, want nil for a tool-only response", msg["content"])
		}
		wantCall := map[string]any{"id": "call_9", "type": "function",
			"function": map[string]any{"name": "f", "arguments": `{"x":1}`}}
		if calls := msg["tool_calls"].([]any); len(calls) != 1 || !reflect.DeepEqual(calls[0], wantCall) {
			t.Fatalf("tool_calls = %v, want %v", msg["tool_calls"], wantCall)
		}
	})

	t.Run("missing call id falls back to call_name_millis_index", func(t *testing.T) {
		resp := map[string]any{
			"status": "completed",
			"output": []any{map[string]any{"type": "function_call", "name": "f"}},
			"usage":  map[string]any{},
		}
		got := BuildChatFromResponses(resp, "m", nil)
		call := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		if !regexp.MustCompile(`^call_f_\d+_0$`).MatchString(call["id"].(string)) {
			t.Fatalf("id = %v, want call_<name>_<millis>_<index>", call["id"])
		}
	})

	t.Run("in progress status is the finish reason", func(t *testing.T) {
		resp := map[string]any{
			"status": "in_progress",
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "partial"}}}},
			"usage": map[string]any{},
		}
		got := BuildChatFromResponses(resp, "m", nil)
		if fr := got["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "in_progress" {
			t.Fatalf("finish_reason = %v, want in_progress", fr)
		}
		// An empty status falls back to stop.
		delete(resp, "status")
		got = BuildChatFromResponses(resp, "m", nil)
		if fr := got["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
			t.Fatalf("finish_reason = %v, want stop", fr)
		}
	})

	t.Run("cache counters fold into prompt with details split", func(t *testing.T) {
		resp := map[string]any{
			"status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "hi"}}}},
			"usage": map[string]any{
				"input_tokens": 100.0, "output_tokens": 20.0,
				"cache_read_input_tokens":     50.0,
				"cache_creation_input_tokens": 20.0,
			},
		}
		got := BuildChatFromResponses(resp, "m", nil)
		want := map[string]any{
			"prompt_tokens": 170.0, "completion_tokens": 20.0, "total_tokens": 190.0,
			"prompt_tokens_details": map[string]any{"cached_tokens": 50.0, "cache_creation_tokens": 20.0},
		}
		if !reflect.DeepEqual(got["usage"], want) {
			t.Fatalf("usage = %v, want %v", got["usage"], want)
		}
		// OpenAI-style cached_tokens variant (already inside the prompt).
		resp["usage"] = map[string]any{"input_tokens": 100.0, "output_tokens": 20.0, "cached_tokens": 30.0}
		got = BuildChatFromResponses(resp, "m", nil)
		want = map[string]any{
			"prompt_tokens": 130.0, "completion_tokens": 20.0, "total_tokens": 150.0,
			"prompt_tokens_details": map[string]any{"cached_tokens": 30.0},
		}
		if !reflect.DeepEqual(got["usage"], want) {
			t.Fatalf("usage = %v, want %v", got["usage"], want)
		}
	})

	t.Run("id created model and content fallbacks", func(t *testing.T) {
		resp := map[string]any{"status": "completed", "output": []any{}, "usage": map[string]any{}}
		got := BuildChatFromResponses(resp, "fallback-m", nil)
		if id, _ := got["id"].(string); !regexp.MustCompile(`^chatcmpl-\d+$`).MatchString(id) {
			t.Fatalf("id = %v, want chatcmpl-<millis>", got["id"])
		}
		if got["model"] != "fallback-m" {
			t.Fatalf("model = %v, want the passed model", got["model"])
		}
		if got["created"].(float64) < 1600000000 {
			t.Fatalf("created = %v, want now", got["created"])
		}
		msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "" {
			t.Fatalf("content = %v, want the empty string with no message items", msg["content"])
		}
	})

	t.Run("synthesize callback rewrites the usage", func(t *testing.T) {
		resp := map[string]any{
			"status": "completed", "output": []any{},
			"usage": map[string]any{"input_tokens": 4.0, "output_tokens": 100.0},
		}
		got := BuildChatFromResponses(resp, "m", func(u map[string]any) map[string]any {
			u["completion_tokens_details"] = map[string]any{"reasoning_tokens": 75.0}
			return u
		})
		usage := got["usage"].(map[string]any)
		details := usage["completion_tokens_details"].(map[string]any)
		if details["reasoning_tokens"] != 75.0 || usage["prompt_tokens"] != 4.0 {
			t.Fatalf("usage = %v, want the synthesized details over the folded counters", usage)
		}
	})
}

func TestStripRedundantReasoning(t *testing.T) {
	build := func(content any, reasoning string) map[string]any {
		msg := map[string]any{"role": "assistant", "content": content}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		return map[string]any{"choices": []any{map[string]any{"message": msg}}}
	}

	t.Run("both present strips reasoning", func(t *testing.T) {
		body := build("answer", "thinking")
		StripRedundantReasoning(body)
		msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if _, has := msg["reasoning_content"]; has {
			t.Fatalf("reasoning_content must be dropped: %v", msg)
		}
		if msg["content"] != "answer" {
			t.Fatalf("content must survive: %v", msg)
		}
	})

	t.Run("empty or nil content keeps reasoning", func(t *testing.T) {
		for _, content := range []any{"", nil} {
			body := build(content, "thinking")
			StripRedundantReasoning(body)
			msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["reasoning_content"] != "thinking" {
				t.Fatalf("content %#v: reasoning must be kept, got %v", content, msg)
			}
		}
	})

	t.Run("each choice is handled independently", func(t *testing.T) {
		body := map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": "a", "reasoning_content": "r1"}},
			map[string]any{"message": map[string]any{"content": "", "reasoning_content": "r2"}},
		}}
		StripRedundantReasoning(body)
		choices := body["choices"].([]any)
		if _, has := choices[0].(map[string]any)["message"].(map[string]any)["reasoning_content"]; has {
			t.Fatal("choice 0 must lose its reasoning")
		}
		if choices[1].(map[string]any)["message"].(map[string]any)["reasoning_content"] != "r2" {
			t.Fatal("choice 1 must keep its reasoning")
		}
	})
}

// Parity (fixed): sseToJsonHandler.js:118-127 pushes ANY successfully parsed
// chunk — scalars included (JSON.parse accepts 42 / "x" / [1,2] / null) — so
// the chunks.length === 0 check (sseToJsonHandler.js:131) only yields the 502
// when NOTHING parsed. A stream of only `data: 42` produces the skeleton
// chat.completion, not the 502.
func TestParseSSEToOpenAIResponseNonObjectChunksCount(t *testing.T) {
	raw := sseOf(`data: 42`, `data: "x"`, `data: [1,2]`, `data: null`, `data: true`, `data: [DONE]`)
	result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
	if !ok || errBody != nil || result == nil {
		t.Fatalf("ok=%v errBody=%v result=%v, want the skeleton", ok, errBody, result)
	}
	if result["object"] != "chat.completion" {
		t.Fatalf("object = %v, want the skeleton chat.completion", result["object"])
	}
	choice := result["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want the stop default", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "" {
		t.Fatalf("content = %v, want empty (scalars contribute nothing)", msg["content"])
	}
	if _, has := result["usage"]; has {
		t.Fatalf("scalars carry no usage, got %v", result["usage"])
	}
	if id, _ := result["id"].(string); !regexp.MustCompile(`^chatcmpl-\d+$`).MatchString(id) {
		t.Fatalf("id = %v, want the chatcmpl-<millis> fallback", result["id"])
	}
}

// sseToJsonHandler.js:133: `first = chunks[0]` — whatever it is. When the
// first parsed chunk is a scalar, its id/created/model reads are undefined and
// the envelope falls back (sseToJsonHandler.js:170-173) even though a later
// object chunk carries them.
func TestParseSSEToOpenAIResponseScalarFirstChunkWinsEnvelope(t *testing.T) {
	raw := sseOf(
		`data: 42`,
		`data: {"id":"chatcmpl-upstreamid","created":1700000000,"model":"gpt","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
	)
	result, _, ok := ParseSSEToOpenAIResponse(raw, "fb")
	if !ok {
		t.Fatal("expected a result")
	}
	if id, _ := result["id"].(string); !regexp.MustCompile(`^chatcmpl-\d+$`).MatchString(id) {
		t.Fatalf("id = %v, want the fallback (the first chunk is the scalar 42)", result["id"])
	}
	if result["created"] == 1700000000.0 {
		t.Fatalf("created = %v, want the now fallback", result["created"])
	}
	if result["model"] != "fb" {
		t.Fatalf("model = %v, want the fallbackModel", result["model"])
	}
}

// Parity (fixed): sseToJsonHandler.js:125 `if (chunk?.error)` is a JS
// TRUTHINESS check — scalar and empty-container errors are error chunks too
// (the 502), and only null/false/0/"" fall through as data.
func TestParseSSEToOpenAIResponseErrorTruthiness(t *testing.T) {
	for _, payload := range []string{`"quota exceeded"`, `1`, `true`, `[]`, `{}`, `0.5`} {
		raw := sseOf(`data: {"error":`+payload+`}`, `data: {"choices":[{"index":0,"delta":{"content":"ignored"}}]}`)
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok || result != nil || errBody == nil {
			t.Fatalf("error %s: ok=%v result=%v errBody=%v, want the error body", payload, ok, result, errBody)
		}
		// A scalar error has no .message in JS either — the caller
		// (sseToJsonHandler.js:312) falls back to its generic text.
		if _, has := errBody["message"]; has {
			t.Fatalf("error %s: scalar errors carry no message, got %v", payload, errBody["message"])
		}
	}
	for _, payload := range []string{`null`, `0`, `false`, `""`} {
		raw := sseOf(`data: {"error":`+payload+`}`, `data: {"choices":[{"index":0,"delta":{"content":"kept"}}]}`)
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok || errBody != nil || result == nil {
			t.Fatalf("falsy error %s: ok=%v errBody=%v result=%v, want data", payload, ok, errBody, result)
		}
		msg := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "kept" {
			t.Fatalf("falsy error %s must stay data, content = %v", payload, msg["content"])
		}
	}
}
