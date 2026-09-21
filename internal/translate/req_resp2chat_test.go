package translate

// Tests for req_resp2chat.go — both directions of the OpenAI Responses ↔ Chat
// request translators ported from
// 9router open-sse/translator/request/openai-responses.js:
//   - ResponsesToChatRequest      (openaiResponsesToOpenAIRequest)
//   - ChatRequestToResponsesRequest (openaiToOpenAIResponsesRequest)

import (
	"strings"
	"testing"

	"opencode-free-proxy/internal/jsonx"
)

func TestResponsesToChatRequestBasics(t *testing.T) {
	t.Run("nil body returns nil", func(t *testing.T) {
		if got := ResponsesToChatRequest(nil); got != nil {
			t.Fatalf("want nil, got %s", js(got))
		}
	})

	t.Run("body without input passes through untouched", func(t *testing.T) {
		// JS `if (!body.input) return body`.
		body := jb(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if got := ResponsesToChatRequest(body); got == nil {
			t.Fatal("want the same body back")
		}
		eq(t, "body", body, jb(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	})

	t.Run("instructions become a system message, string input becomes a user message", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"model":"m","stream":true,
			"instructions":"be terse",
			"input":"hi"
		}`))
		eq(t, "result", got, jb(t, `{
			"model":"m","stream":true,
			"messages":[
				{"role":"system","content":"be terse"},
				{"role":"user","content":[{"type":"text","text":"hi"}]}
			]
		}`))
	})

	t.Run("empty input array becomes the placeholder user message", func(t *testing.T) {
		// formats/responsesApi.js normalizeResponsesInput: providers reject
		// messages:[] so an empty input[] injects "...".
		got := ResponsesToChatRequest(jb(t, `{"input":[]}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":[{"type":"text","text":"..."}]}
		]`))
	})

	t.Run("invalid input shape returns the original body", func(t *testing.T) {
		// JS: `if (!inputItems) return body` — the original body escapes.
		body := jb(t, `{"input":42,"model":"m"}`)
		got := ResponsesToChatRequest(body)
		if got == nil {
			t.Fatal("want the body back")
		}
		if _, has := got["messages"]; has {
			t.Fatalf("messages must not be added: %s", js(got))
		}
		eq(t, "input", got["input"], float64(42))
	})

	t.Run("role-only items are treated as messages (Droid CLI parity)", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[{"role":"user","content":"plain"}]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":"plain"}
		]`))
	})
}

func TestResponsesToChatRequestItems(t *testing.T) {
	t.Run("message content parts mapped to chat blocks", func(t *testing.T) {
		// input_text/output_text → text; input_image → image_url with url/detail;
		// anything else passes through untouched.
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"message","role":"user","content":[
					{"type":"input_text","text":"look"},
					{"type":"output_text","text":"prev"},
					{"type":"input_image","image_url":"http://p","detail":"low"},
					{"type":"input_image","file_id":"file_1"},
					{"type":"audio","id":"a1"}
				]}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"text","text":"prev"},
			{"type":"image_url","image_url":{"url":"http://p","detail":"low"}},
			{"type":"image_url","image_url":{"url":"file_1","detail":"auto"}},
			{"type":"audio","id":"a1"}
		]}]`))
	})

	t.Run("function_call items group into one assistant tool_calls message with the tool result after it", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
				{"type":"reasoning","summary":[{"type":"summary_text","text":"need weather"}],"encrypted_content":"ENC"},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":{"city":"SF"}},
				{"type":"function_call","call_id":"call_2","name":"other","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"sunny"}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":[{"type":"text","text":"weather?"}]},
			{"role":"assistant","content":null,
			 "reasoning_content":"need weather","encrypted_content":"ENC",
			 "tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}},
				{"id":"call_2","type":"function","function":{"name":"other","arguments":"{}"}}
			 ]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"}
		]`))
	})

	t.Run("reasoning before a user message is dropped, not attached", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"stray"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":[{"type":"text","text":"q"}]}
		]`))
	})

	t.Run("multiple reasoning items join with newline inside and across items", func(t *testing.T) {
		// extractReasoningText joins a single item's summary parts with "\n";
		// consecutive reasoning items are buffered with "\n" as well.
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]},
				{"type":"reasoning","summary":[{"type":"summary_text","text":"c"}]},
				{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
			]
		}`))
		eq(t, "reasoning_content", dig(t, got, "messages", 0, "reasoning_content"), "a\nb\nc")
	})

	t.Run("reasoning content falls back from summary to content array", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"reasoning","content":[{"type":"summary_text","text":"deep thought"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ans"}]}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"assistant","content":[{"type":"text","text":"ans"}],"reasoning_content":"deep thought"}
		]`))
	})

	t.Run("nameless function_call still leaves the empty assistant shell (JS #444)", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[{"type":"function_call","call_id":"call_x","name":"   ","arguments":"{}"}]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"assistant","content":null,"tool_calls":[]}
		]`))
	})

	t.Run("custom_tool_call registers its name and converts to a function tool_call", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[{"type":"custom_tool_call","call_id":"call_c","name":"edit","input":"print('hi')"}]
		}`))
		// The tool_call id/type/name mapping is shared with function_call.
		eq(t, "tool_call id", dig(t, got, "messages", 0, "tool_calls", 0, "id"), "call_c")
		eq(t, "tool_call type", dig(t, got, "messages", 0, "tool_calls", 0, "type"), "function")
		eq(t, "tool_call name", dig(t, got, "messages", 0, "tool_calls", 0, "function", "name"), "edit")
		// NOTE: the arguments payload diverges from JS — see
		// parity_known_bugs_test.go (known parity bug #3): JS wraps the freeform
		// input as {"input": ...}.
		setOf(t, "_customToolNames", jsonx.AsArr(got["_customToolNames"]), "edit")
	})

	t.Run("custom_tool_call_output becomes a tool message with stringified output", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"custom_tool_call_output","call_id":"call_c","output":{"ok":true}},
				{"type":"function_call_output","call_id":"call_s","output":"plain"}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"tool","tool_call_id":"call_c","content":"{\"ok\":true}"},
			{"role":"tool","tool_call_id":"call_s","content":"plain"}
		]`))
	})

	t.Run("additional_tools items merge into tools", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"tools":[{"type":"function","name":"flat1","description":"d1","parameters":{"type":"object","properties":{}}}],
			"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"additional_tools","tools":[{"type":"function","name":"hosted_fn","description":"h"}]}
			]
		}`))
		eq(t, "tools", got["tools"], ja(t, `[
			{"type":"function","function":{"name":"flat1","description":"d1","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"hosted_fn","description":"h","parameters":{"type":"object","properties":{}}}}
		]`))
	})

	t.Run("unknown item types are ignored", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"item_reference","id":"x"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}
			]
		}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":[{"type":"text","text":"q"}]}
		]`))
	})
}

func TestResponsesToChatRequestTools(t *testing.T) {
	t.Run("flat function tools become chat function tools with strict preserved", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[
				{"type":"function","name":"f","description":"d","parameters":{"type":"object"},"strict":true},
				{"type":"function","name":"g","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}
			]
		}`))
		eq(t, "tools", got["tools"], ja(t, `[
			{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}},"strict":true}},
			{"type":"function","function":{"name":"g","description":"","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}
		]`))
	})

	t.Run("hosted tools without a name are dropped", func(t *testing.T) {
		// openai-responses.js: nameless hosted tools cannot be represented as
		// chat function declarations (Gemini strictly validates names).
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[{"type":"web_search"},{"type":"function","name":"keep","description":""}]
		}`))
		eq(t, "tools", got["tools"], ja(t, `[
			{"type":"function","function":{"name":"keep","description":"","parameters":{"type":"object","properties":{}}}}
		]`))
	})

	t.Run("custom tools become input-parameter functions and are registered", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[{"type":"custom","name":"edit","description":"d","format":{"syntax":"regex","definition":"\\d+"}}]
		}`))
		// The description text is asserted in parity_known_bugs_test.go
		// (known parity bug #5: the format-hint join separator diverges).
		fn := dig(t, got, "tools", 0, "function").(map[string]any)
		eq(t, "name", fn["name"], "edit")
		eq(t, "parameters", fn["parameters"], jb(t, `{
			"type":"object",
			"properties":{"input":{"type":"string","description":"Raw freeform input for this custom tool"}},
			"required":["input"],
			"additionalProperties":false
		}`))
		setOf(t, "_customToolNames", jsonx.AsArr(got["_customToolNames"]), "edit")
	})

	t.Run("already-chat tools pass through verbatim", func(t *testing.T) {
		tool := map[string]any{"type": "function", "function": map[string]any{"name": "f"}}
		got := ResponsesToChatRequest(map[string]any{"input": "hi", "tools": []any{tool}})
		eq(t, "tools", got["tools"], []any{tool})
	})
}

func TestResponsesToChatRequestFieldCleanup(t *testing.T) {
	t.Run("max_output_tokens maps to max_tokens", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{"input":"hi","max_output_tokens":500}`))
		eq(t, "max_tokens", got["max_tokens"], float64(500))
		if key(got, "max_output_tokens") {
			t.Fatalf("max_output_tokens must be deleted: %s", js(got))
		}
	})

	t.Run("existing max_tokens wins over max_output_tokens", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{"input":"hi","max_tokens":100,"max_output_tokens":500}`))
		eq(t, "max_tokens", got["max_tokens"], float64(100))
	})

	t.Run("Responses-only fields are cleaned up, other fields preserved", func(t *testing.T) {
		// openai-responses.js cleanup list: input, instructions, include,
		// prompt_cache_key, store, reasoning, client_metadata.
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"instructions":"sys",
			"include":["reasoning.encrypted_content"],
			"prompt_cache_key":"k",
			"store":false,
			"reasoning":{"effort":"high","summary":"auto"},
			"client_metadata":{"a":1},
			"temperature":0.5,
			"stream":true
		}`))
		for _, k := range []string{"input", "instructions", "include", "prompt_cache_key", "store", "reasoning", "client_metadata"} {
			if key(got, k) {
				t.Fatalf("%s must be deleted: %s", k, js(got))
			}
		}
		eq(t, "temperature", got["temperature"], 0.5)
		eq(t, "stream", got["stream"], true)
		// KNOWN PARITY BUG #2: JS also maps reasoning.effort → reasoning_effort
		// before deleting reasoning; Go drops it. Repro in
		// parity_known_bugs_test.go — not asserted here.
	})

	t.Run("empty instructions adds no system message", func(t *testing.T) {
		// JS `if (body.instructions)` — falsy instructions are skipped.
		got := ResponsesToChatRequest(jb(t, `{"input":"hi","instructions":""}`))
		eq(t, "messages", got["messages"], ja(t, `[
			{"role":"user","content":[{"type":"text","text":"hi"}]}
		]`))
	})
}

func TestChatRequestToResponsesRequestBasics(t *testing.T) {
	t.Run("nil body returns nil", func(t *testing.T) {
		if got := ChatRequestToResponsesRequest("m", nil); got != nil {
			t.Fatalf("want nil, got %s", js(got))
		}
	})

	t.Run("chat messages become input items, system becomes instructions", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("upstream-model", jb(t, `{
			"model":"client-model",
			"messages":[
				{"role":"system","content":"be terse"},
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"hello"}
			]
		}`))
		eq(t, "result", got, jb(t, `{
			"model":"upstream-model",
			"input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
			],
			"stream":true,
			"store":false,
			"instructions":"be terse"
		}`))
	})

	t.Run("only the first system message becomes instructions", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[
				{"role":"system","content":"one"},
				{"role":"system","content":"two"},
				{"role":"developer","content":"dev"},
				{"role":"user","content":"q"}
			]
		}`))
		eq(t, "instructions", got["instructions"], "one")
		eq(t, "input", got["input"], ja(t, `[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}
		]`))
	})

	t.Run("developer role is instruction-bearing too", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"developer","content":"dev prompt"},{"role":"user","content":"q"}]
		}`))
		eq(t, "instructions", got["instructions"], "dev prompt")
	})

	t.Run("system array content parts join with newline", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"system","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]
		}`))
		eq(t, "instructions", got["instructions"], "a\nb")
	})

	t.Run("no system message leaves instructions empty", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{"messages":[{"role":"user","content":"q"}]}`))
		eq(t, "instructions", got["instructions"], "")
	})

	t.Run("image_url blocks become input_image items", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"http://p","detail":"low"}},
				{"type":"image_url","image_url":"http://q"},
				{"type":"input_image","image_url":"http://kept","detail":"high"}
			]}]
		}`))
		eq(t, "content", dig(t, got, "input", 0, "content"), ja(t, `[
			{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":"http://p","detail":"low"},
			{"type":"input_image","image_url":"http://q","detail":"auto"},
			{"type":"input_image","image_url":"http://kept","detail":"high"}
		]`))
	})

	t.Run("unknown content blocks serialize as text", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":[
				{"type":"tool_use","id":"tu","name":"f"}
			]}]
		}`))
		eq(t, "content", dig(t, got, "input", 0, "content"), ja(t, `[
			{"type":"input_text","text":"{\"id\":\"tu\",\"name\":\"f\",\"type\":\"tool_use\"}"}
		]`))
	})

	t.Run("assistant content null with tool_calls emits no message item", func(t *testing.T) {
		// openai-responses.js: only push the message block when content is
		// non-empty; tool_calls become function_call items either way.
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}
			]}]
		}`))
		eq(t, "input", got["input"], ja(t, `[
			{"type":"function_call","call_id":"call_1","name":"f1","arguments":"{\"a\":1}"}
		]`))
	})

	t.Run("tool_calls names are trimmed, capped and coerced", func(t *testing.T) {
		long := strings.Repeat("x", 130)
		body := map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "function": map[string]any{"name": "  f2  ", "arguments": "not json"}},
				map[string]any{"id": strings.Repeat("c", 70), "function": map[string]any{"name": long}},
				map[string]any{"id": "call_3", "function": map[string]any{"name": "f3"}}, // no arguments key
			}},
		}}
		got := ChatRequestToResponsesRequest("m", body)
		eq(t, "trimmed name", dig(t, got, "input", 0, "name"), "f2")
		eq(t, "invalid arguments coerced", dig(t, got, "input", 0, "arguments"), "{}")
		eq(t, "name capped", dig(t, got, "input", 1, "name"), long[:128])
		eq(t, "call id clamped", dig(t, got, "input", 1, "call_id"), strings.Repeat("c", 64))
		eq(t, "missing arguments coerced", dig(t, got, "input", 2, "arguments"), "{}")
	})

	t.Run("tool messages become function_call_output items", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[
				{"role":"tool","tool_call_id":"call_1","content":"res"},
				{"role":"tool","tool_call_id":"call_2","content":{"a":1}},
				{"role":"tool","tool_call_id":"call_3","content":null}
			]
		}`))
		eq(t, "input", got["input"], ja(t, `[
			{"type":"function_call_output","call_id":"call_1","output":"res"},
			{"type":"function_call_output","call_id":"call_2","output":"{\"a\":1}"},
			{"type":"function_call_output","call_id":"call_3","output":""}
		]`))
	})

	t.Run("assistant reasoning fields rebuild a reasoning input item", func(t *testing.T) {
		// buildReasoningInputItem: store=false multi-turn continuity.
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"assistant","content":"ans","reasoning_content":"because","encrypted_content":"ENC"}]
		}`))
		eq(t, "input", got["input"], ja(t, `[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"because"}],"encrypted_content":"ENC"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ans"}]}
		]`))
	})

	t.Run("reasoning item variants", func(t *testing.T) {
		t.Run("reasoning string becomes the summary", func(t *testing.T) {
			got := ChatRequestToResponsesRequest("m", jb(t, `{
				"messages":[{"role":"assistant","reasoning":"why"}]
			}`))
			eq(t, "reasoning item", dig(t, got, "input", 0), jb(t, `{
				"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]
			}`))
		})
		t.Run("reasoning_details join text and content", func(t *testing.T) {
			got := ChatRequestToResponsesRequest("m", jb(t, `{
				"messages":[{"role":"assistant","reasoning_details":[{"text":"a"},{"content":"b"}]}]
			}`))
			eq(t, "summary text", dig(t, got, "input", 0, "summary", 0, "text"), "a\nb")
		})
		t.Run("encrypted_content alone is preserved", func(t *testing.T) {
			got := ChatRequestToResponsesRequest("m", jb(t, `{
				"messages":[{"role":"assistant","reasoning_encrypted_content":"ENC"}]
			}`))
			eq(t, "reasoning item", dig(t, got, "input", 0), jb(t, `{
				"type":"reasoning","encrypted_content":"ENC"
			}`))
		})
		t.Run("plain assistant message emits no reasoning item", func(t *testing.T) {
			got := ChatRequestToResponsesRequest("m", jb(t, `{
				"messages":[{"role":"assistant","content":"hi"}]
			}`))
			eq(t, "input", got["input"], ja(t, `[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}
			]`))
		})
	})
}

func TestChatRequestToResponsesRequestTools(t *testing.T) {
	t.Run("chat tools flatten to the Responses shape", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],
			"tools":[
				{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"},"strict":true}},
				{"type":"function","function":{}},
				{"type":"web_search"}
			]
		}`))
		// Nameless function tools are dropped; non-function tools pass through
		// untouched (JS `return tool`).
		eq(t, "tools", got["tools"], ja(t, `[
			{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{}},"strict":true},
			{"type":"web_search"}
		]`))
	})

	t.Run("custom-type tools pass through untouched (responses-side only concept)", func(t *testing.T) {
		// openaiToOpenAIResponsesRequest has no custom-tool branch and never
		// sets _customToolNames — verified against openai-responses.js.
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"custom","name":"edit","description":"d"}]
		}`))
		eq(t, "tools", got["tools"], ja(t, `[{"type":"custom","name":"edit","description":"d"}]`))
		if key(got, "_customToolNames") {
			t.Fatalf("chat→responses must not set _customToolNames: %s", js(got))
		}
	})
}

func TestChatRequestToResponsesRequestParams(t *testing.T) {
	t.Run("sampling and cache params pass through", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],
			"temperature":0.5,"top_p":0.9,"service_tier":"default","prompt_cache_key":"k"
		}`))
		eq(t, "temperature", got["temperature"], 0.5)
		eq(t, "top_p", got["top_p"], 0.9)
		eq(t, "service_tier", got["service_tier"], "default")
		eq(t, "prompt_cache_key", got["prompt_cache_key"], "k")
	})

	t.Run("max_tokens family maps to max_output_tokens", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],"max_tokens":100
		}`))
		eq(t, "max_output_tokens", got["max_output_tokens"], float64(100))
		if key(got, "max_tokens") {
			t.Fatalf("max_tokens must not leak: %s", js(got))
		}

		got2 := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],"max_completion_tokens":77
		}`))
		eq(t, "max_output_tokens", got2["max_output_tokens"], float64(77))
	})

	t.Run("reasoning_effort becomes the reasoning object", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],"reasoning_effort":"high"
		}`))
		eq(t, "reasoning", got["reasoning"], map[string]any{"effort": "high", "summary": "auto"})
	})

	t.Run("reasoning object passes through and reasoning_effort wins", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],
			"reasoning":{"effort":"low","summary":"detailed"},
			"reasoning_effort":"medium"
		}`))
		eq(t, "reasoning", got["reasoning"], map[string]any{"effort": "medium", "summary": "auto"})
	})

	t.Run("bodies already carrying input pass through with model/stream/max remapped", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("upstream", jb(t, `{
			"model":"client-model","stream":false,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
			"max_tokens":99
		}`))
		eq(t, "result", got, jb(t, `{
			"model":"upstream","stream":true,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
			"max_output_tokens":99
		}`))
	})
}

func TestResponsesToChatRequestInputGateTruthiness(t *testing.T) {
	// JS `if (!body.input) return body` (openai-responses.js:23) — truthiness,
	// not key presence: every falsy input value leaves the body untranslated.
	for _, in := range []string{`""`, `0`, `false`, `null`} {
		t.Run("falsy input "+in+" passes through", func(t *testing.T) {
			body := jb(t, `{"model":"m","input":`+in+`}`)
			got := ResponsesToChatRequest(body)
			if got == nil {
				t.Fatal("want the body back")
			}
			if key(got, "messages") {
				t.Fatalf("no translation may run: %s", js(got))
			}
			eq(t, "input kept", got["input"], jb(t, `{"v":`+in+`}`)["v"])
		})
	}

	t.Run("truthy non-string input still attempts normalization", func(t *testing.T) {
		// `{"input":true}` is truthy → translation starts, the input shape is
		// invalid → the original body escapes (JS `if (!inputItems) return
		// body`).
		body := jb(t, `{"input":true,"model":"m"}`)
		got := ResponsesToChatRequest(body)
		if got == nil {
			t.Fatal("want the body back")
		}
		if key(got, "messages") {
			t.Fatalf("invalid input shape must not add messages: %s", js(got))
		}
	})
}

func TestChatRequestToResponsesInputGateTruthiness(t *testing.T) {
	// JS `if (body.input)` (openai-responses.js:323) — truthiness: only a
	// TRUTHY input marks an already-Responses body. `input: ""` alongside
	// chat messages runs the full translation.
	got := ChatRequestToResponsesRequest("m", jb(t, `{
		"input":"",
		"messages":[{"role":"user","content":"q"}]
	}`))
	eq(t, "input rebuilt from messages", got["input"], ja(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}
	]`))
}

func TestResponsesToChatRequestTruthyRawValues(t *testing.T) {
	t.Run("truthy non-string instructions become the system content", func(t *testing.T) {
		// JS `if (body.instructions)` (:29) — the raw value is the content.
		got := ResponsesToChatRequest(jb(t, `{"input":"hi","instructions":5}`))
		eq(t, "system content", dig(t, got, "messages", 0, "role"), "system")
		eq(t, "content", dig(t, got, "messages", 0, "content"), float64(5))
	})

	t.Run("truthy non-string type and empty role match no branch", func(t *testing.T) {
		// JS `item.type || (item.role ? MESSAGE : null)` (:67) — type 5 is
		// truthy so the role fallback never runs, and 5 matches no item
		// branch; an empty role is falsy and promotes nothing.
		got := ResponsesToChatRequest(jb(t, `{"input":[{"role":""},{"type":5,"role":"user"}]}`))
		eq(t, "messages", got["messages"], ja(t, `[]`))
	})

	t.Run("input_image url/detail keep raw truthy values", func(t *testing.T) {
		// JS `c.image_url || c.file_id || ""` / `c.detail || "auto"` (:89-90).
		got := ResponsesToChatRequest(jb(t, `{"input":[{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":5,"detail":7},
			{"type":"input_image","image_url":0,"file_id":"file_9"},
			{"type":"input_image"}
		]}]}`))
		eq(t, "parts", dig(t, got, "messages", 0, "content"), ja(t, `[
			{"type":"image_url","image_url":{"url":5,"detail":7}},
			{"type":"image_url","image_url":{"url":"file_9","detail":"auto"}},
			{"type":"image_url","image_url":{"url":"","detail":"auto"}}
		]`))
	})
}

func TestResponsesToChatRequestToolCallPayloads(t *testing.T) {
	t.Run("custom_tool_call input null wraps as the two-character empty string", func(t *testing.T) {
		// JS `{ input: typeof item.input === "string" ? item.input :
		// JSON.stringify(item.input ?? "") }` (:117-118) — null ?? "" is "",
		// and JSON.stringify("") is the two-char string `""`.
		got := ResponsesToChatRequest(jb(t, `{
			"input":[{"type":"custom_tool_call","call_id":"call_c","name":"edit","input":null}]
		}`))
		eq(t, "arguments", dig(t, got, "messages", 0, "tool_calls", 0, "function", "arguments"),
			`{"input":"\"\""}`)
	})

	t.Run("custom_tool_call non-string input stringifies as JSON", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":[{"type":"custom_tool_call","call_id":"call_c","name":"edit","input":{"k":1}}]
		}`))
		eq(t, "arguments", dig(t, got, "messages", 0, "tool_calls", 0, "function", "arguments"),
			`{"input":"{\"k\":1}"}`)
	})

	t.Run("function_call null and absent arguments coerce to {}", func(t *testing.T) {
		// JS `JSON.stringify(toolInput ?? {})` (:125) — null becomes {},
		// never "null".
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"function_call","call_id":"c1","name":"f","arguments":null},
				{"type":"function_call","call_id":"c2","name":"g"}
			]
		}`))
		eq(t, "null arguments", dig(t, got, "messages", 0, "tool_calls", 0, "function", "arguments"), "{}")
		eq(t, "absent arguments", dig(t, got, "messages", 0, "tool_calls", 1, "function", "arguments"), "{}")
	})

	t.Run("padded names are gated on trim but pushed raw and registered raw", func(t *testing.T) {
		// JS skips on the trimmed name (:115) but pushes `item.name` raw
		// (:124) and registers it raw — trimming would desynchronize the call
		// from its tool declaration.
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"function_call","call_id":"c1","name":" f ","arguments":"{}"},
				{"type":"custom_tool_call","call_id":"c2","name":" padded ","input":"x"}
			]
		}`))
		eq(t, "function_call name raw", dig(t, got, "messages", 0, "tool_calls", 0, "function", "name"), " f ")
		eq(t, "custom name raw", dig(t, got, "messages", 0, "tool_calls", 1, "function", "name"), " padded ")
		setOf(t, "_customToolNames", jsonx.AsArr(got["_customToolNames"]), " padded ")
	})

	t.Run("absent output drops the content key, explicit null keeps \"null\"", func(t *testing.T) {
		// JS `typeof item.output === "string" ? item.output :
		// JSON.stringify(item.output)` (:146) — an absent output is
		// JSON.stringify(undefined) = undefined (key vanishes on the wire);
		// null keeps "null".
		got := ResponsesToChatRequest(jb(t, `{
			"input":[
				{"type":"function_call_output","call_id":"c1"},
				{"type":"function_call_output","call_id":"c2","output":null}
			]
		}`))
		first := dig(t, got, "messages", 0).(map[string]any)
		if _, has := first["content"]; has {
			t.Fatalf("absent output must not create a content key: %s", js(first))
		}
		eq(t, "null output", dig(t, got, "messages", 1, "content"), "null")
	})
}

func TestNormalizeToolParametersTruthiness(t *testing.T) {
	// JS (openai-responses.js:275-278):
	//   - falsy parameters → default object schema (null/0/false/"");
	//   - a TRUTHY array passes through verbatim (truthiness is blind to shape);
	//   - type "object" with falsy properties → properties injected as {}.
	defaults := jb(t, `{"type":"object","properties":{}}`)

	t.Run("responses→chat tools", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[
				{"type":"function","name":"a","parameters":{"type":"object","properties":null}},
				{"type":"function","name":"b","parameters":[]},
				{"type":"function","name":"c","parameters":0},
				{"type":"function","name":"d","parameters":""}
			]
		}`))
		eq(t, "falsy properties injected", dig(t, got, "tools", 0, "function", "parameters"), defaults)
		eq(t, "truthy array verbatim", dig(t, got, "tools", 1, "function", "parameters"), ja(t, `[]`))
		eq(t, "falsy 0 default", dig(t, got, "tools", 2, "function", "parameters"), defaults)
		eq(t, "falsy empty string default", dig(t, got, "tools", 3, "function", "parameters"), defaults)
	})

	t.Run("chat→responses tools", func(t *testing.T) {
		got := ChatRequestToResponsesRequest("m", jb(t, `{
			"messages":[{"role":"user","content":"q"}],
			"tools":[
				{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":null}}},
				{"type":"function","function":{"name":"b","parameters":[]}},
				{"type":"function","function":{"name":"c","parameters":false}}
			]
		}`))
		eq(t, "falsy properties injected", dig(t, got, "tools", 0, "parameters"), defaults)
		eq(t, "truthy array verbatim", dig(t, got, "tools", 1, "parameters"), ja(t, `[]`))
		eq(t, "falsy false default", dig(t, got, "tools", 2, "parameters"), defaults)
	})
}

func TestResponsesToChatRequestToolFunctionTruthiness(t *testing.T) {
	// JS `if (tool.function) return tool` (openai-responses.js:189) —
	// truthiness: null falls through to the Responses-tool conversion, while
	// a truthy array (also not chat shape) passes verbatim.
	t.Run("null function converts as a flat Responses tool", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[{"type":"function","function":null,"name":"flat","description":"d"}]
		}`))
		eq(t, "tool", dig(t, got, "tools", 0), jb(t, `{
			"type":"function",
			"function":{"name":"flat","description":"d","parameters":{"type":"object","properties":{}}}
		}`))
	})

	t.Run("truthy array function passes through verbatim", func(t *testing.T) {
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[{"type":"function","function":[],"name":"flat"}]
		}`))
		eq(t, "tool", dig(t, got, "tools", 0), jb(t, `{
			"type":"function","function":[],"name":"flat"
		}`))
	})

	t.Run("custom-tool format hint and description coerce like String()", func(t *testing.T) {
		// JS `[syntax, definition].filter(Boolean).join("\n")` (:198) —
		// truthy non-strings stringify through join; `String(tool.description
		// || "")` (:203) coerces numerics.
		got := ResponsesToChatRequest(jb(t, `{
			"input":"hi",
			"tools":[{"type":"custom","name":"edit","description":7,
				"format":{"syntax":5,"definition":null}}]
		}`))
		eq(t, "description", dig(t, got, "tools", 0, "function", "description"), "7\n\n5")
	})
}

func TestChatRequestToResponsesContentPartTruthiness(t *testing.T) {
	// JS `const text = c.text || c.content || JSON.stringify(c)` then
	// `typeof text === "string" ? text : JSON.stringify(text)`
	// (openai-responses.js:381-382) — truthy chains keeping raw values.
	got := ChatRequestToResponsesRequest("m", jb(t, `{
		"messages":[{"role":"user","content":[
			{"type":"weird","text":5},
			{"type":"weird","text":"","content":true},
			{"type":"weird"},
			{"type":"weird","text":false,"content":0}
		]}]
	}`))
	parts := dig(t, got, "input", 0, "content").([]any)
	eq(t, "numeric text", dig(t, parts[0], "text"), "5")
	eq(t, "truthy bool content", dig(t, parts[1], "text"), "true")
	eq(t, "all falsy stringify whole part", dig(t, parts[2], "text"), `{"type":"weird"}`)
	eq(t, "explicit falsy values stringify whole part", dig(t, parts[3], "text"), `{"content":0,"text":false,"type":"weird"}`)
}

func TestChatRequestToResponsesInstructionsFilterBoolean(t *testing.T) {
	// JS extractInstructionsText: `.filter(Boolean).join("\n")`
	// (openai-responses.js:260-270) — empty texts drop out instead of
	// contributing blank lines.
	got := ChatRequestToResponsesRequest("m", jb(t, `{
		"messages":[
			{"role":"system","content":[{"type":"text","text":""},{"type":"text","text":"a"},{"type":"text","text":""}]},
			{"role":"user","content":"q"}
		]
	}`))
	eq(t, "instructions", got["instructions"], "a")
}
