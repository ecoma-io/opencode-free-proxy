package translate

// Tests for prenorm.go — the translateRequest pre-normalizations ported from
// 9router open-sse/translator/index.js:
//   - NormalizeThinkingConfig  (open-sse/services/provider.js normalizeThinkingConfig)
//   - EnsureToolCallIDs        (open-sse/translator/concerns/toolCall.js ensureToolCallIds)
//   - FixMissingToolResponses  (concerns/toolCall.js fixMissingToolResponses)
//   - FilterToOpenAIFormat     (open-sse/translator/formats/openai.js filterToOpenAIFormat)
//   - StripContinuityFields    (open-sse/handlers/chatCore.js stripContinuityFields)

import (
	"regexp"
	"testing"

	"opencode-free-proxy/internal/jsonx"
)

func TestNormalizeThinkingConfig(t *testing.T) {
	t.Run("last message from user keeps thinking and reasoning_effort", func(t *testing.T) {
		// provider.js: isLastMessageFromUser → true → thinking untouched.
		body := jb(t, `{
			"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"enabled","budget_tokens":2000},
			"reasoning_effort":"high"
		}`)
		NormalizeThinkingConfig(body)
		eq(t, "thinking", body["thinking"], map[string]any{"type": "enabled", "budget_tokens": float64(2000)})
		if s := jsonx.AsStr(body["reasoning_effort"]); s != "high" {
			t.Fatalf("reasoning_effort must survive: %s", js(body["reasoning_effort"]))
		}
	})

	t.Run("last message assistant deletes thinking, keeps reasoning_effort", func(t *testing.T) {
		// provider.js normalizeThinkingConfig: `delete body.thinking` — and the
		// comment there is explicit that reasoning_effort is request-level and
		// survives tool-result turns.
		body := jb(t, `{
			"messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}
			],
			"thinking":{"type":"enabled"},
			"reasoning_effort":"medium"
		}`)
		NormalizeThinkingConfig(body)
		if key(body, "thinking") {
			t.Fatalf("thinking must be deleted: %s", js(body))
		}
		if s := jsonx.AsStr(body["reasoning_effort"]); s != "medium" {
			t.Fatalf("reasoning_effort must never be deleted: %s", js(body["reasoning_effort"]))
		}
	})

	t.Run("last message tool deletes thinking", func(t *testing.T) {
		body := jb(t, `{
			"messages":[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"call_1","content":"r"}],
			"thinking":{"type":"enabled"}
		}`)
		NormalizeThinkingConfig(body)
		if key(body, "thinking") {
			t.Fatalf("thinking must be deleted: %s", js(body))
		}
	})

	t.Run("no messages leaves the body untouched", func(t *testing.T) {
		// JS isLastMessageFromUser: `if (!messages?.length) return true`.
		for _, body := range []map[string]any{
			jb(t, `{"thinking":{"type":"enabled"}}`),
			jb(t, `{"messages":[],"thinking":{"type":"enabled"}}`),
			jb(t, `{"messages":"garbage","thinking":{"type":"enabled"}}`),
		} {
			NormalizeThinkingConfig(body)
			if !key(body, "thinking") {
				t.Fatalf("thinking must be untouched: %s", js(body))
			}
		}
	})

	t.Run("Gemini contents array is honored", func(t *testing.T) {
		kept := jb(t, `{
			"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"b"}]}],
			"thinking":{"type":"enabled"}
		}`)
		NormalizeThinkingConfig(kept)
		if !key(kept, "thinking") {
			t.Fatalf("last content user → thinking kept: %s", js(kept))
		}

		dropped := jb(t, `{
			"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]}],
			"thinking":{"type":"enabled"}
		}`)
		NormalizeThinkingConfig(dropped)
		if key(dropped, "thinking") {
			t.Fatalf("last content model → thinking deleted: %s", js(dropped))
		}
	})

	t.Run("nil body is a no-op", func(t *testing.T) {
		NormalizeThinkingConfig(nil)
	})
}

func TestEnsureToolCallIDs(t *testing.T) {
	t.Run("invalid id chars sanitized, type defaulted, arguments stringified", func(t *testing.T) {
		// concerns/toolCall.js ensureToolCallIds: sanitizeToolId strips
		// everything outside [a-zA-Z0-9_-].
		body := jb(t, `{
			"messages":[{"role":"assistant","tool_calls":[
				{"id":"call##ab 1","function":{"name":"f","arguments":{"a":1}}}
			]}]
		}`)
		EnsureToolCallIDs(body)
		tc := dig(t, body, "messages", 0, "tool_calls", 0).(map[string]any)
		eq(t, "id", tc["id"], "callab1")
		eq(t, "type", tc["type"], "function")
		eq(t, "arguments", dig(t, tc, "function", "arguments"), `{"a":1}`)
	})

	t.Run("missing id generated from position and tool name", func(t *testing.T) {
		// generateToolCallId(msgIndex, tcIndex, toolName).
		body := jb(t, `{
			"messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","tool_calls":[{"function":{"name":"get weather!"}}]}
			]
		}`)
		EnsureToolCallIDs(body)
		tc := dig(t, body, "messages", 1, "tool_calls", 0).(map[string]any)
		eq(t, "generated id", tc["id"], "call_msg1_tc0_getweather")
		eq(t, "type", tc["type"], "function")
	})

	t.Run("missing id without function name falls back to position only", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"assistant","tool_calls":[{}]}]}`)
		EnsureToolCallIDs(body)
		tc := dig(t, body, "messages", 0, "tool_calls", 0).(map[string]any)
		eq(t, "generated id", tc["id"], "call_msg0_tc0")
	})

	t.Run("arguments empty string stays empty string", func(t *testing.T) {
		// JS guard `tc.function?.arguments &&` — a falsy arguments is left as-is.
		body := jb(t, `{"messages":[{"role":"assistant","tool_calls":[
			{"id":"ok","type":"function","function":{"name":"f","arguments":""}}
		]}]}`)
		EnsureToolCallIDs(body)
		eq(t, "arguments", dig(t, body, "messages", 0, "tool_calls", 0, "function", "arguments"), "")
	})

	t.Run("falsy non-string arguments stay as-is, truthy ones stringify", func(t *testing.T) {
		// JS `tc.function?.arguments && typeof tc.function.arguments !==
		// "string"` (concerns/toolCall.js:44) — truthiness: 0/false/null are
		// falsy and pass through untouched ({} and [] are TRUTHY objects and
		// stringify).
		body := jb(t, `{"messages":[{"role":"assistant","tool_calls":[
			{"id":"a1","function":{"name":"f","arguments":0}},
			{"id":"b2","function":{"name":"f","arguments":false}},
			{"id":"c3","function":{"name":"f","arguments":null}},
			{"id":"d4","function":{"name":"f","arguments":{}}},
			{"id":"e5","function":{"name":"f","arguments":[]}}
		]}]}`)
		EnsureToolCallIDs(body)
		calls := dig(t, body, "messages", 0, "tool_calls").([]any)
		eq(t, "0 stays", dig(t, calls[0], "function", "arguments"), float64(0))
		eq(t, "false stays", dig(t, calls[1], "function", "arguments"), false)
		eq(t, "null stays", dig(t, calls[2], "function", "arguments"), nil)
		eq(t, "{} stringifies", dig(t, calls[3], "function", "arguments"), "{}")
		eq(t, "[] stringifies", dig(t, calls[4], "function", "arguments"), "[]")
	})

	t.Run("valid id and type untouched", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"assistant","tool_calls":[
			{"id":"call_123-abc_X","type":"function","function":{"name":"f","arguments":"{}"}}
		]}]}`)
		EnsureToolCallIDs(body)
		eq(t, "id", dig(t, body, "messages", 0, "tool_calls", 0, "id"), "call_123-abc_X")
		eq(t, "type", dig(t, body, "messages", 0, "tool_calls", 0, "type"), "function")
	})

	t.Run("tool message tool_call_id sanitized", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"tool","tool_call_id":"tool##9","content":"r"}]}`)
		EnsureToolCallIDs(body)
		eq(t, "tool_call_id", dig(t, body, "messages", 0, "tool_call_id"), "tool9")
	})

	t.Run("Claude tool_use and tool_result blocks sanitized", func(t *testing.T) {
		body := jb(t, `{
			"messages":[
				{"role":"assistant","content":[
					{"type":"text","text":"using tool"},
					{"type":"tool_use","id":"tu##1","name":"fn"}
				]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"tr##1"}
				]}
			]
		}`)
		EnsureToolCallIDs(body)
		eq(t, "tool_use id", dig(t, body, "messages", 0, "content", 1, "id"), "tu1")
		eq(t, "tool_result id", dig(t, body, "messages", 1, "content", 0, "tool_use_id"), "tr1")
	})

	t.Run("fully invalid content-block id regenerated with name suffix", func(t *testing.T) {
		// sanitizeToolId("##") → null → generateToolCallId(0, 1, "fn").
		body := jb(t, `{"messages":[{"role":"assistant","content":[
			{"type":"text","text":"x"},
			{"type":"tool_use","id":"##","name":"fn"}
		]}]}`)
		EnsureToolCallIDs(body)
		eq(t, "regenerated id", dig(t, body, "messages", 0, "content", 1, "id"), "call_msg0_tc1_fn")
	})

	t.Run("missing messages is a no-op", func(t *testing.T) {
		body := jb(t, `{"input":"x"}`)
		EnsureToolCallIDs(body)
		eq(t, "body", body, map[string]any{"input": "x"})
	})
}

func TestFixMissingToolResponses(t *testing.T) {
	t.Run("assistant tool_calls before unrelated user message inserts empty tool result", func(t *testing.T) {
		// concerns/toolCall.js fixMissingToolResponses: one {role:"tool",
		// tool_call_id, content:""} per dangling id, inserted right after the
		// assistant turn.
		body := jb(t, `{
			"messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}
				]},
				{"role":"user","content":"next"}
			]
		}`)
		FixMissingToolResponses(body)
		eq(t, "inserted message", msgAt(t, body, 2), map[string]any{
			"role": "tool", "tool_call_id": "call_1", "content": "",
		})
		if n := len(msgs(t, body)); n != 4 {
			t.Fatalf("want 4 messages, got %d: %s", n, js(body["messages"]))
		}
	})

	t.Run("multiple tool_calls insert one result per id in order", func(t *testing.T) {
		body := jb(t, `{
			"messages":[
				{"role":"assistant","tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}
				]},
				{"role":"user","content":"next"}
			]
		}`)
		FixMissingToolResponses(body)
		eq(t, "first insert", dig(t, body, "messages", 1, "tool_call_id"), "call_a")
		eq(t, "second insert", dig(t, body, "messages", 2, "tool_call_id"), "call_b")
	})

	t.Run("already answered tool turn is left alone", func(t *testing.T) {
		body := jb(t, `{
			"messages":[
				{"role":"assistant","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"42"}
			]
		}`)
		want := js(body["messages"])
		FixMissingToolResponses(body)
		eq(t, "messages unchanged", body["messages"], ja(t, want))
	})

	t.Run("Claude tool_use answered by tool_result in next user message", func(t *testing.T) {
		body := jb(t, `{
			"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"f"}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}
				]}
			]
		}`)
		want := js(body["messages"])
		FixMissingToolResponses(body)
		eq(t, "messages unchanged", body["messages"], ja(t, want))
	})

	t.Run("partial answer still counts as answered (JS includes())", func(t *testing.T) {
		// JS hasToolResults checks toolCallIds.includes(...) for ANY id, so one
		// answered id suppresses insertion for the whole turn.
		body := jb(t, `{
			"messages":[
				{"role":"assistant","tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_a","content":"42"}
			]
		}`)
		want := js(body["messages"])
		FixMissingToolResponses(body)
		eq(t, "messages unchanged", body["messages"], ja(t, want))
	})

	t.Run("assistant without tool_calls is untouched", func(t *testing.T) {
		body := jb(t, `{"messages":[
			{"role":"assistant","content":"plain"},
			{"role":"user","content":"next"}
		]}`)
		want := js(body["messages"])
		FixMissingToolResponses(body)
		eq(t, "messages unchanged", body["messages"], ja(t, want))
	})

	t.Run("dangling tool_calls at end of conversation inserts nothing", func(t *testing.T) {
		// JS guards with `nextMsg && !hasToolResults(nextMsg, toolCallIds)` —
		// no next message means nothing to repair; never fabricate a dangling
		// tool response at the end of the conversation. (Formerly known parity
		// bug #1; the repro also lives in parity_known_bugs_test.go.)
		body := jb(t, `{
			"messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}
				]}
			]
		}`)
		FixMissingToolResponses(body)
		if got := len(msgs(t, body)); got != 2 {
			t.Fatalf("JS inserts nothing for a dangling end-of-conversation tool call: got %d messages\nmessages: %s",
				got, js(body["messages"]))
		}
	})

	t.Run("missing messages and nil body are no-ops", func(t *testing.T) {
		body := jb(t, `{"input":"x"}`)
		FixMissingToolResponses(body)
		eq(t, "body", body, map[string]any{"input": "x"})
		FixMissingToolResponses(nil)
	})
}

func TestFilterToOpenAIFormat(t *testing.T) {
	t.Run("developer role renamed to system", func(t *testing.T) {
		// formats/openai.js: `msg.role === ROLE.DEVELOPER → {...msg, role: SYSTEM}`.
		body := jb(t, `{"messages":[{"role":"developer","content":"be terse"}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"system","content":"be terse"}]`))
	})

	t.Run("thinking and redacted_thinking blocks dropped", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":[
			{"type":"thinking","thinking":"m","signature":"sig"},
			{"type":"redacted_thinking","data":"xx"},
			{"type":"text","text":"hi"}
		]}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "content", dig(t, body, "messages", 0, "content"),
			ja(t, `[{"type":"text","text":"hi"}]`))
	})

	t.Run("invalid block types dropped, valid types kept with signature/cache_control stripped", func(t *testing.T) {
		// VALID_OPENAI_CONTENT_TYPES = text,image_url,image,input_audio,audio_url,file.
		body := jb(t, `{"messages":[{"role":"user","content":[
			{"type":"document","source":{"x":1}},
			{"type":"tool_use","id":"tu","name":"f"},
			{"type":"text","text":"keep","signature":"s","cache_control":{"type":"ephemeral"}},
			{"type":"image_url","image_url":{"url":"http://x"}}
		]}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "content", dig(t, body, "messages", 0, "content"), ja(t, `[
			{"type":"text","text":"keep"},
			{"type":"image_url","image_url":{"url":"http://x"}}
		]`))
	})

	t.Run("tool_result blocks kept but cleaned", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":"ok","signature":"s"}
		]}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "content", dig(t, body, "messages", 0, "content"), ja(t, `[
			{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}
		]`))
	})

	t.Run("fully emptied content becomes one empty text block and the message is dropped", func(t *testing.T) {
		// Stage 1 rebuilds content as [{type:text,text:""}]; stage 2 then drops
		// a message whose only block is blank text.
		body := jb(t, `{"messages":[
			{"role":"user","content":[{"type":"thinking","thinking":"m"}]},
			{"role":"user","content":"still here"}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"user","content":"still here"}]`))
	})

	t.Run("assistant tool_calls turn always kept verbatim", func(t *testing.T) {
		// Stage 1 returns assistant+tool_calls messages untouched — even a
		// thinking block inside array content survives.
		body := jb(t, `{"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"m"}
		],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"assistant","content":[
			{"type":"thinking","thinking":"m"}
		],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]`))
	})

	t.Run("tool_calls null falls through to the content filter and may drop the message", func(t *testing.T) {
		// JS `msg.role === ROLE.ASSISTANT && msg.tool_calls`
		// (formats/openai.js:27/:68) — truthiness: null is not a tool-call
		// turn, so the whitespace-content drop applies to it.
		body := jb(t, `{"messages":[
			{"role":"assistant","tool_calls":null,"content":"   "},
			{"role":"user","content":"kept"}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"user","content":"kept"}]`))
	})

	t.Run("tool_calls empty array is truthy and kept verbatim", func(t *testing.T) {
		body := jb(t, `{"messages":[
			{"role":"assistant","tool_calls":[],"content":"   "}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"assistant","tool_calls":[],"content":"   "}]`))
	})

	t.Run("function null tool falls through to the Claude conversion", func(t *testing.T) {
		// JS `tool.type === OPENAI_BLOCK.FUNCTION && tool.function`
		// (formats/openai.js:89) — truthiness: a null function is not
		// already-OpenAI shape.
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[
			{"type":"function","function":null,"name":"flat","description":"d"}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], ja(t, `[{"type":"function","function":{
			"name":"flat","description":"d","parameters":{"type":"object","properties":{}}
		}}]`))
	})

	t.Run("tool messages always kept", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"tool","tool_call_id":"call_1","content":"   "}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"],
			ja(t, `[{"role":"tool","tool_call_id":"call_1","content":"   "}]`))
	})

	t.Run("whitespace-only string content dropped", func(t *testing.T) {
		body := jb(t, `{"messages":[
			{"role":"user","content":"  \n\t "},
			{"role":"user","content":"real"},
			{"role":"assistant","content":"ok"}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[
			{"role":"user","content":"real"},
			{"role":"assistant","content":"ok"}
		]`))
	})

	t.Run("array content with only blank text dropped", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":[
			{"type":"text","text":"   "}
		]}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[]`))
	})

	t.Run("non-array non-string content kept as-is", func(t *testing.T) {
		// JS falls through both branches and keeps the message.
		body := jb(t, `{"messages":[{"role":"assistant","content":null}]}`)
		FilterToOpenAIFormat(body)
		eq(t, "messages", body["messages"], ja(t, `[{"role":"assistant","content":null}]`))
	})

	t.Run("empty tools array deleted", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[]}`)
		FilterToOpenAIFormat(body)
		if key(body, "tools") {
			t.Fatalf("empty tools must be deleted: %s", js(body))
		}
	})

	t.Run("Claude tool shape converted to function shape", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[
			{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], ja(t, `[{"type":"function","function":{
			"name":"get_weather","description":"w",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}
		}}]`))
	})

	t.Run("Claude tool with description only gets the empty object schema", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[
			{"name":"f","description":"d"}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], ja(t, `[{"type":"function","function":{
			"name":"f","description":"d","parameters":{"type":"object","properties":{}}
		}}]`))
	})

	t.Run("tool with only a name passes through untouched", func(t *testing.T) {
		// JS: `tool.name && (tool.input_schema || tool.description)` — neither
		// schema nor description means neither the Claude nor the Gemini branch
		// applies.
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[
			{"name":"bare","custom":true}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], ja(t, `[{"name":"bare","custom":true}]`))
	})

	t.Run("Gemini functionDeclarations flattened", func(t *testing.T) {
		body := jb(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[
			{"functionDeclarations":[
				{"name":"a","description":"da","parameters":{"type":"object","properties":{}}},
				{"name":"b"}
			]}
		]}`)
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], ja(t, `[
			{"type":"function","function":{"name":"a","description":"da","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"b","description":"","parameters":{"type":"object","properties":{}}}}
		]`))
	})

	t.Run("already OpenAI tools untouched", func(t *testing.T) {
		tool := map[string]any{"type": "function", "function": map[string]any{
			"name": "f", "description": "d", "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}
		body := map[string]any{"messages": ja(t, `[{"role":"user","content":"hi"}]`), "tools": []any{tool}}
		FilterToOpenAIFormat(body)
		eq(t, "tools", body["tools"], []any{tool})
	})

	t.Run("Claude tool_choice objects converted", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want any
		}{
			{"auto", `{"type":"auto"}`, "auto"},
			{"any", `{"type":"any"}`, "required"},
			{"tool", `{"type":"tool","name":"f"}`, map[string]any{
				"type": "function", "function": map[string]any{"name": "f"},
			}},
			{"already openai", `{"type":"function","function":{"name":"f"}}`, map[string]any{
				"type": "function", "function": map[string]any{"name": "f"},
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := map[string]any{
					"messages":    ja(t, `[{"role":"user","content":"hi"}]`),
					"tool_choice": jb(t, tc.in),
				}
				FilterToOpenAIFormat(body)
				eq(t, "tool_choice", body["tool_choice"], tc.want)
			})
		}
	})

	t.Run("string tool_choice untouched", func(t *testing.T) {
		body := map[string]any{
			"messages":    ja(t, `[{"role":"user","content":"hi"}]`),
			"tool_choice": "auto",
		}
		FilterToOpenAIFormat(body)
		eq(t, "tool_choice", body["tool_choice"], "auto")
	})

	t.Run("missing messages array skips everything (parity with JS early return)", func(t *testing.T) {
		// filterToOpenAIFormat returns before touching tools/tool_choice.
		body := jb(t, `{"tools":[{"name":"f","input_schema":{}}],"tool_choice":{"type":"auto"}}`)
		FilterToOpenAIFormat(body)
		eq(t, "body", body, jb(t, `{"tools":[{"name":"f","input_schema":{}}],"tool_choice":{"type":"auto"}}`))
	})

	t.Run("nil body is a no-op", func(t *testing.T) {
		FilterToOpenAIFormat(nil)
	})
}

func TestStripContinuityFields(t *testing.T) {
	// chatCore.js stripContinuityFields: the Responses→Chat translator stashes
	// encrypted reasoning blobs on assistant messages; never forward them.
	body := jb(t, `{"messages":[
		{"role":"assistant","content":"a","encrypted_content":"ENC","reasoning_encrypted_content":"ENC2","reasoning_content":"why"},
		{"role":"user","content":"b"}
	]}`)
	StripContinuityFields(body)
	eq(t, "messages", body["messages"], ja(t, `[
		{"role":"assistant","content":"a","reasoning_content":"why"},
		{"role":"user","content":"b"}
	]`))

	StripContinuityFields(nil) // must not panic
	StripContinuityFields(jb(t, `{"input":"x"}`))
}

func TestGenerateToolCallID(t *testing.T) {
	// concerns/toolCall.js generateToolCallId.
	eq(t, "positional", GenerateToolCallID(2, 3, ""), "call_msg2_tc3")
	eq(t, "name suffix sanitized", GenerateToolCallID(0, 1, "get weather!"), "call_msg0_tc1_getweather")
}

func TestFallbackToolCallIDShape(t *testing.T) {
	// concerns/toolCall.js fallbackToolCallId(): `call_${Date.now()}`.
	re := regexp.MustCompile(`^call_[0-9]+$`)
	if !re.MatchString(FallbackToolCallID()) {
		t.Fatalf("FallbackToolCallID = %q, want call_<unixmilli>", FallbackToolCallID())
	}
}

func TestCloneMsgShallowCopy(t *testing.T) {
	// Go-side hygiene (issue #17): the size hint carries no arithmetic, but
	// the semantics callers rely on are pinned — an equal copy that does not
	// alias the source, with room for the key FilterToOpenAIFormat adds.
	msg := map[string]any{"role": "assistant", "content": "x"}
	out := cloneMsg(msg)
	if len(out) != 2 || out["role"] != "assistant" || out["content"] != "x" {
		t.Fatalf("cloneMsg = %v, want an equal copy of %v", out, msg)
	}
	out["tool_calls"] = []any{}
	if _, ok := msg["tool_calls"]; ok {
		t.Fatal("cloneMsg must not alias the source map")
	}
}
