package relay

// JS-semantics parity tests for the `||` / `??` / Number-coercion ports added
// across the relay package: the common.go helpers, convertUsageForFormat
// (usageTracking.js:158-190), the stream.js truthiness sites (finish_reason,
// choices iteration order, mergeTracked's estimate marker), the translate-mode
// usage seam, and the raw-value `||` chains of sseToJsonHandler.js.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// --- common.go: jsOr / jsNullish / jsNumOr0 / accumulate ---

// jsOr mirrors `a || b`: first truthy wins, else the LAST operand's raw value —
// even when falsy (`x || 0` is 0). "0" and empty arrays are TRUTHY.
func TestJsOr(t *testing.T) {
	if got := jsOr(0.0, 5.0); got != 5.0 {
		t.Errorf("jsOr(0, 5) = %v, want 5", got)
	}
	if got := jsOr(3.0, 5.0); got != 3.0 {
		t.Errorf("jsOr(3, 5) = %v, want 3", got)
	}
	if got := jsOr(0.0, ""); got != "" {
		t.Errorf(`jsOr(0, "") = %#v, want the falsy LAST operand ""`, got)
	}
	if got := jsOr(nil, nil, false); got != false {
		t.Errorf("jsOr(nil, nil, false) = %#v, want the falsy LAST operand false", got)
	}
	if got := jsOr("0", "x"); got != "0" {
		t.Errorf(`jsOr("0", "x") = %#v, want "0" ("0" is truthy in JS)`, got)
	}
	if got := jsOr([]any{}, "x"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("jsOr([], \"x\") = %#v, want the empty array (truthy in JS)", got)
	}
}

// jsNullish mirrors `a ?? b`: only an absent key (nil in Go) or a JSON null
// falls through — a present 0, "" or false wins (usageTracking.js:178-180).
func TestJsNullish(t *testing.T) {
	if got := jsNullish(0.0, 50.0); got != 0.0 {
		t.Errorf("jsNullish(0, 50) = %v, want 0 (present zero is not nullish)", got)
	}
	if got := jsNullish(nil, 50.0); got != 50.0 {
		t.Errorf("jsNullish(nil, 50) = %v, want 50", got)
	}
	if got := jsNullish(nil, nil); got != nil {
		t.Errorf("jsNullish(nil, nil) = %v, want nil", got)
	}
	if got := jsNullish("", "fallback"); got != "" {
		t.Errorf(`jsNullish("", "fallback") = %#v, want ""`, got)
	}
	if got := jsNullish(false, 1.0); got != false {
		t.Errorf("jsNullish(false, 1) = %#v, want false", got)
	}
}

// jsNumOr0 is `num(v) ?? 0` (usageTracking.js:159): Number() coercion, 0 for
// every non-finite result — including numeric strings and arrays that coerce.
func TestJsNumOr0(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{nil, 0},
		{3.5, 3.5},
		{"7", 7},
		{"0x10", 16},
		{"abc", 0},
		{[]any{"12"}, 12},
		{true, 1},
		{false, 0},
	}
	for _, c := range cases {
		if got := jsNumOr0(c.in); got != c.want {
			t.Errorf("jsNumOr0(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// accumulate counts UTF-16 CODE UNITS (stream.js:315-325 adds `content.length`)
// — a CJK rune is 1 unit and an emoji is 2, whatever their byte widths.
func TestAccumulateCountsUtf16Units(t *testing.T) {
	var totalLen int
	var content, thinking strings.Builder
	accumulate(&totalLen, &content, &thinking, map[string]any{
		"content":           "模型😀", // 2 CJK units + a 2-unit emoji
		"reasoning_content": "ab",
	})
	if totalLen != 6 {
		t.Fatalf("totalLen = %d, want 6 UTF-16 units", totalLen)
	}
	if content.String() != "模型😀" || thinking.String() != "ab" {
		t.Fatalf("builders = %q / %q", content.String(), thinking.String())
	}
}

// --- passthrough.go: convertUsageForFormat (usageTracking.js:158-190) ---

func TestConvertUsageForFormat(t *testing.T) {
	t.Run("responses nullish: a present 0 beats a populated sibling", func(t *testing.T) {
		u := map[string]any{"input_tokens": 0.0, "prompt_tokens": 50.0, "output_tokens": 7.0}
		got := convertUsageForFormat(u, FormatResponses)
		if got["input_tokens"] != 0.0 || got["output_tokens"] != 7.0 {
			t.Fatalf("got %v, want input 0 (present) / output 7", got)
		}
	})
	t.Run("numeric strings coerce through Number", func(t *testing.T) {
		u := map[string]any{"prompt_tokens": "7", "completion_tokens": "0x10"}
		got := convertUsageForFormat(u, FormatResponses)
		if got["input_tokens"] != 7.0 || got["output_tokens"] != 16.0 {
			t.Fatalf("got %v, want 7 / 16 (Number(\"7\") and Number(\"0x10\"))", got)
		}
	})
	t.Run("cached: negative kept, zero and non-numbers skipped", func(t *testing.T) {
		neg := convertUsageForFormat(map[string]any{"cached_tokens": -3.0}, FormatResponses)
		want := map[string]any{"input_tokens": 0.0, "output_tokens": 0.0,
			"input_tokens_details": map[string]any{"cached_tokens": -3.0}}
		if !reflect.DeepEqual(neg, want) {
			t.Fatalf("negative cached must survive the truthiness gate: %v", neg)
		}
		for _, v := range []any{0.0, "abc"} {
			got := convertUsageForFormat(map[string]any{"cached_tokens": v}, FormatResponses)
			if _, has := got["input_tokens_details"]; has {
				t.Fatalf("cached %#v must skip the details object: %v", v, got)
			}
		}
	})
	t.Run("reasoning falls through to thoughtsTokenCount", func(t *testing.T) {
		got := convertUsageForFormat(map[string]any{"thoughtsTokenCount": 9.0}, FormatResponses)
		details, ok := got["output_tokens_details"].(map[string]any)
		if !ok || details["reasoning_tokens"] != 9.0 {
			t.Fatalf("got %v, want output_tokens_details.reasoning_tokens 9", got)
		}
	})
	t.Run("estimated: truthiness gate, literal true written", func(t *testing.T) {
		got := convertUsageForFormat(map[string]any{"estimated": "maybe"}, FormatResponses)
		if got["estimated"] != true {
			t.Fatalf(`estimated "maybe" must mark the copy with the literal true: %v`, got["estimated"])
		}
		got = convertUsageForFormat(map[string]any{"estimated": 0.0}, FormatResponses)
		if _, has := got["estimated"]; has {
			t.Fatalf("estimated 0 is falsy and must drop: %v", got)
		}
	})
	t.Run("chat family is identity", func(t *testing.T) {
		u := map[string]any{"prompt_tokens": 5.0, "vendor_extra": "keep me"}
		got := convertUsageForFormat(u, FormatChat)
		if !reflect.DeepEqual(got, u) {
			t.Fatalf("chat conversion must be identity: %v", got)
		}
	})
}

// --- passthrough.go: stream.js truthiness sites ---

// stream.js:334 `parsed.choices?.[0]?.finish_reason` is TRUTHINESS, not a
// string check — a boolean/numeric finish_reason marks the finish chunk and
// triggers the estimate, while a falsy non-string does not.
func TestPassthroughFinishReasonTruthiness(t *testing.T) {
	t.Run("boolean finish reason injects the estimate", func(t *testing.T) {
		r, buf := newPassthrough(t, estBody, FormatChat, nil)
		processAll(t, r, contentChunk,
			`data: {"id":"chatcmpl-12345678","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":true}]}`)
		frames := dataFrames(t, buf.String())
		if len(frames) != 2 {
			t.Fatalf("want 2 frames, got %d: %q", len(frames), buf.String())
		}
		usage, has := frames[1]["usage"].(map[string]any)
		if !has || usage["prompt_tokens"] != 2004.0 || usage["estimated"] != true {
			t.Fatalf("truthy non-string finish_reason must inject the estimate: %v", frames[1])
		}
	})
	t.Run("zero finish reason is not a finish chunk", func(t *testing.T) {
		r, buf := newPassthrough(t, estBody, FormatChat, nil)
		processAll(t, r, contentChunk,
			`data: {"id":"chatcmpl-12345678","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"content":"y"},"finish_reason":0}]}`)
		frames := dataFrames(t, buf.String())
		if len(frames) != 2 {
			t.Fatalf("want 2 frames, got %d: %q", len(frames), buf.String())
		}
		if _, has := frames[1]["usage"]; has {
			t.Fatalf("falsy finish_reason must not inject usage: %v", frames[1])
		}
	})
}

// stream.js:288-309 run BEFORE the terminal probe at stream.js:332: a choices
// array holding NULL (property read throws) or a non-iterable truthy choices
// (for..of throws) drops the WHOLE chunk without setting terminalSeen — a
// hostile `choices:[null]` terminal-looking chunk must not suppress the
// failed-stream synthesis (the catch at stream.js:364-369 never reaches 332).
func TestPassthroughNullChoiceElementDroppedBeforeTerminal(t *testing.T) {
	for _, choices := range []string{`[null]`, `{}`} {
		t.Run("responses terminal-looking chunk with choices "+choices, func(t *testing.T) {
			r, buf := newPassthrough(t, estBody, FormatResponses, nil)
			processAll(t, r, `data: {"type":"response.completed","response":{"id":"resp_x","status":"completed"},"choices":`+choices+`}`)
			if buf.Len() != 0 {
				t.Fatalf("the whole chunk must drop, wrote %q", buf.String())
			}
			if r.terminalSeen {
				t.Fatal("the throw happens before the terminal probe — terminalSeen must stay false")
			}
			if err := r.Flush(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), "event: response.failed") {
				t.Fatalf("flush must still synthesize the failed frame, got %q", buf.String())
			}
		})
	}
	t.Run("chat: whole chunk dropped, no seam", func(t *testing.T) {
		r, buf := newPassthrough(t, estBody, FormatChat, nil)
		processAll(t, r, `data: {"id":"chatcmpl-12345678","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[null],"finish_reason":"stop"}`)
		if buf.Len() != 0 {
			t.Fatalf("the whole chunk must drop, wrote %q", buf.String())
		}
		if r.FinalUsage != nil {
			t.Fatalf("a dropped chunk must leave no tracked usage, got %v", r.FinalUsage)
		}
	})
}

// stream.js:168-169 `prev?.estimated ? next : mergeUsage(prev, next)` — the
// estimate marker is TRUTHINESS: any truthy marker (a string, 1, true) makes
// the next usage REPLACE the estimate; only a falsy marker max-merges.
// Both relays port the same seam independently.
func TestMergeTrackedEstimatedTruthiness(t *testing.T) {
	next := map[string]any{"prompt_tokens": 5.0}
	run := func(merge func(prev, next map[string]any) map[string]any) {
		t.Helper()
		got := merge(map[string]any{"prompt_tokens": 10.0, "estimated": "yes"}, next)
		if got["prompt_tokens"].(float64) != 5 {
			t.Fatalf("a truthy non-bool marker must REPLACE (no max-merge): %v", got)
		}
		got = merge(map[string]any{"prompt_tokens": 10.0, "estimated": 0.0}, next)
		if got["prompt_tokens"].(float64) != 10 {
			t.Fatalf("a falsy marker must max-merge: %v", got)
		}
	}
	pr, _ := newPassthrough(t, estBody, FormatChat, nil)
	t.Run("passthrough", func(t *testing.T) { run(pr.mergeTracked) })
	var buf bytes.Buffer
	tr := NewTranslateRelay(&buf, estBody, "m", FormatChat, "", nil)
	t.Run("translate", func(t *testing.T) { run(tr.mergeTracked) })
}

// --- translate.go: buildClientUsage + the seam's finish-reason truthiness ---

// buildClientUsage = rename (convertUsageForFormat) → synthesize → filter.
// With synthesis on (reasoning_effort "high"), a 100-token completion gets
// floor(100 * 0.75) = 75 reasoning tokens in the target family's details key.
func TestTranslateBuildClientUsage(t *testing.T) {
	in := map[string]any{"prompt_tokens": 10.0, "completion_tokens": 100.0, "total_tokens": 110.0}
	var buf bytes.Buffer

	t.Run("responses client", func(t *testing.T) {
		body := map[string]any{"reasoning_effort": "high"}
		r := NewTranslateRelay(&buf, body, "m", FormatResponses, "", nil)
		if !r.synthesis.Enabled {
			t.Fatal("reasoning_effort must enable synthesis")
		}
		got := r.buildClientUsage(in)
		want := map[string]any{
			"input_tokens":          10.0,
			"output_tokens":         100.0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 75.0},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
	t.Run("chat client", func(t *testing.T) {
		body := map[string]any{"reasoning_effort": "high"}
		r := NewTranslateRelay(&buf, body, "m", FormatChat, "resp_seed", nil)
		got := r.buildClientUsage(in)
		want := map[string]any{
			"prompt_tokens":             10.0,
			"completion_tokens":         100.0,
			"total_tokens":              110.0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 75.0},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
}

// stream.js:185 — `item.type === "message_delta"` is strict equality, but
// `item.choices?.[0]?.finish_reason` is TRUTHINESS: a boolean finish_reason
// marks the finish item and gets the estimate; a zero does not.
func TestTranslateSeamFinishReasonTruthiness(t *testing.T) {
	var buf bytes.Buffer
	r := NewTranslateRelay(&buf, estBody, "m", FormatChat, "", nil)
	r.totalLen = 12

	finishing := map[string]any{"choices": []any{map[string]any{
		"index": 0.0, "delta": map[string]any{}, "finish_reason": true}}}
	got := r.applyUsageSeam(finishing)
	usage, has := got["usage"].(map[string]any)
	if !has || usage["prompt_tokens"] != 2004.0 || usage["estimated"] != true {
		t.Fatalf("a truthy non-string finish_reason must inject the estimate: %#v", got)
	}

	notFinishing := map[string]any{"choices": []any{map[string]any{
		"index": 0.0, "delta": map[string]any{"content": "x"}, "finish_reason": 0.0}}}
	got2 := r.applyUsageSeam(notFinishing)
	if _, has := got2["usage"]; has {
		t.Fatalf("a falsy finish_reason must not inject usage: %#v", got2)
	}
}

// --- nonstream.go: sseToJsonHandler.js raw-value `||` chains ---

// sseToJsonHandler.js:140-176: finish_reason and usage keep RAW truthy values
// (any type), tool-call fragments join through String() coercion, and the
// envelope's id/created/model survive as their raw truthy types.
func TestParseSSEToOpenAIResponseRawTruthyValues(t *testing.T) {
	t.Run("boolean finish reason survives raw", func(t *testing.T) {
		raw := sseOf(`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":true}]}`)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok {
			t.Fatal("expected a result")
		}
		choice := result["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != true {
			t.Fatalf("finish_reason = %#v, want the raw true", choice["finish_reason"])
		}
	})
	t.Run("array usage captured verbatim", func(t *testing.T) {
		raw := sseOf(
			`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			`data: {"usage":[1,2]}`,
		)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok {
			t.Fatal("expected a result")
		}
		if want := []any{1.0, 2.0}; !reflect.DeepEqual(result["usage"], want) {
			t.Fatalf("usage = %#v, want the array verbatim (typeof [] === \"object\")", result["usage"])
		}
	})
	t.Run("numeric tool-call fragments join as JS strings", func(t *testing.T) {
		raw := sseOf(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":1,"function":{"name":5}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"x","arguments":2}}]}}]}`,
		)
		result, _, _ := ParseSSEToOpenAIResponse(raw, "fb")
		call := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		if call["id"] != 1.0 {
			t.Fatalf("id = %#v, want the raw truthy number 1", call["id"])
		}
		fn := call["function"].(map[string]any)
		if fn["name"] != "5x" {
			t.Fatalf(`name = %#v, want "5x" (String(5) + "x")`, fn["name"])
		}
		if fn["arguments"] != "2" {
			t.Fatalf(`arguments = %#v, want "2" (String(2))`, fn["arguments"])
		}
	})
	t.Run("envelope keeps raw truthy id created model", func(t *testing.T) {
		raw := sseOf(`data: {"id":42,"created":"2024-01-01","model":true,"choices":[{"index":0,"delta":{"content":"x"}}]}`)
		result, _, ok := ParseSSEToOpenAIResponse(raw, "fb")
		if !ok {
			t.Fatal("expected a result")
		}
		if result["id"] != 42.0 || result["created"] != "2024-01-01" || result["model"] != true {
			t.Fatalf("envelope = %#v, want raw 42 / \"2024-01-01\" / true", result)
		}
	})
}

func TestChatCompletionToResponsesRawValues(t *testing.T) {
	t.Run("falsy arguments stringify to the empty object", func(t *testing.T) {
		body := map[string]any{"id": "x", "choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": []any{
				map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": 0.0}},
				map[string]any{"id": "c2", "function": map[string]any{"name": "g", "arguments": false}},
			}},
		}}}
		out := ChatCompletionToResponses(body, nil)["output"].([]any)
		for i, want := range []string{"{}", "{}"} {
			if got := out[i].(map[string]any)["arguments"]; got != want {
				t.Fatalf("arguments[%d] = %#v, want %q (JSON.stringify(v || {}))", i, got, want)
			}
		}
	})
	t.Run("numeric id: call_id raw, id template stringifies", func(t *testing.T) {
		body := map[string]any{"id": "x", "choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": []any{
				map[string]any{"id": 7.0, "function": map[string]any{"name": 5.0, "arguments": "{}"}},
			}},
		}}}
		item := ChatCompletionToResponses(body, nil)["output"].([]any)[0].(map[string]any)
		if item["call_id"] != 7.0 {
			t.Fatalf("call_id = %#v, want the raw number 7", item["call_id"])
		}
		if item["id"] != "fc_7" {
			t.Fatalf("id = %#v, want the stringified fc_7", item["id"])
		}
		if item["name"] != 5.0 {
			t.Fatalf("name = %#v, want the raw number 5", item["name"])
		}
	})
}

func TestBuildChatFromResponsesRawValues(t *testing.T) {
	t.Run("numeric call_id raw; absent name key drops, null name stays", func(t *testing.T) {
		absent := map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "function_call", "call_id": 5.0},
		}, "usage": map[string]any{}}
		call := BuildChatFromResponses(absent, "m", nil)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		if call["id"] != 5.0 {
			t.Fatalf("id = %#v, want the raw numeric call_id", call["id"])
		}
		fn := call["function"].(map[string]any)
		if _, has := fn["name"]; has {
			t.Fatalf("an ABSENT name must drop the key entirely (JS object spread): %#v", fn)
		}
		if fn["arguments"] != "{}" {
			t.Fatalf("arguments = %#v, want {} (falsy stringify)", fn["arguments"])
		}

		nullName := map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "function_call", "call_id": "c", "name": nil},
		}, "usage": map[string]any{}}
		fn2 := BuildChatFromResponses(nullName, "m", nil)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
		v, has := fn2["name"]
		if !has || v != nil {
			t.Fatalf("a NULL name must keep the key with null (distinct from absent): %#v", fn2)
		}
	})
	t.Run("raw truthy status is the finish reason", func(t *testing.T) {
		resp := map[string]any{
			"status": 5.0,
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "x"}}}},
			"usage": map[string]any{},
		}
		fr := BuildChatFromResponses(resp, "m", nil)["choices"].([]any)[0].(map[string]any)["finish_reason"]
		if fr != 5.0 {
			t.Fatalf("finish_reason = %#v, want the raw status 5 (`status || \"stop\"`)", fr)
		}
	})
	t.Run("string created_at survives raw", func(t *testing.T) {
		resp := map[string]any{"id": "resp_9", "created_at": "2024-01-01", "model": "m",
			"status": "completed", "output": []any{}, "usage": map[string]any{}}
		if got := BuildChatFromResponses(resp, "m", nil)["created"]; got != "2024-01-01" {
			t.Fatalf("created = %#v, want the raw string", got)
		}
	})
}
