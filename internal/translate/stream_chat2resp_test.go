package translate

// Tests for stream_chat2resp.go (ChatToRespState) — the Chat SSE → Responses
// events state machine ported from
// 9router open-sse/translator/response/openai-responses.js
// openaiToOpenAIResponsesResponse (registered OPENAI → OPENAI_RESPONSES).

import (
	"strings"
	"testing"

	"opencode-free-proxy/internal/jsonx"
)

// newChatRespState seeds the state the way relay does: model, created (seconds,
// initState's Math.floor(Date.now()/1000)) and the seeded resp_ id.
func newChatRespState() *ChatToRespState {
	return NewChatToRespState("muse-spark-1.2", 1700000000, "resp_1700000000000")
}

// feedChat runs raw chat chunks through Convert and concatenates the events.
func feedChat(t *testing.T, st *ChatToRespState, raw ...string) []Event {
	t.Helper()
	var out []Event
	for _, r := range raw {
		out = append(out, st.Convert(jb(t, r))...)
	}
	return out
}

func names(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

func first(t *testing.T, evs []Event, name string) map[string]any {
	t.Helper()
	for _, e := range evs {
		if e.Event == name {
			return e.Data
		}
	}
	t.Fatalf("no %q event in [%s]", name, strings.Join(names(evs), ", "))
	return nil
}

func TestChatToRespFirstChunkSeedsResponseAndStreamsText(t *testing.T) {
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
	)
	// JS openai-responses.js response translator: the first chunk emits the
	// response.created + response.in_progress scaffold before any item events.
	eqNames(t, "events", names(evs),
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
	)

	// sequence_number starts at 1 and increments per event (initState seq: 0).
	for i, want := range []float64{1, 2, 3, 4} {
		eq(t, "sequence_number", dig(t, evs[i].Data, "sequence_number"), want)
	}

	eq(t, "response created", evs[0].Data, jb(t, `{
		"type":"response.created","sequence_number":1,
		"response":{"id":"resp_1700000000000","object":"response","created_at":1700000000,
			"status":"in_progress","background":false,"error":null,"output":[]}
	}`))
	added := evs[2].Data
	eq(t, "item added", added, jb(t, `{
		"type":"response.output_item.added","sequence_number":3,"output_index":0,
		"item":{"id":"msg_resp_1700000000000_0","type":"message","content":[],"role":"assistant"}
	}`))
	eq(t, "content part added", evs[3].Data, jb(t, `{
		"type":"response.content_part.added","sequence_number":4,
		"item_id":"msg_resp_1700000000000_0","output_index":0,"content_index":0,
		"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}
	}`))
	eq(t, "text delta", evs[4].Data, jb(t, `{
		"type":"response.output_text.delta","sequence_number":5,
		"item_id":"msg_resp_1700000000000_0","output_index":0,"content_index":0,
		"delta":"He","logprobs":[]
	}`))
	eq(t, "second delta", dig(t, evs[5].Data, "delta"), "llo")
}

func TestChatToRespResponseIDFromChunk(t *testing.T) {
	// JS: state.responseId = chunk.id ? `resp_${chunk.id}` : state.responseId.
	st := newChatRespState()
	evs := feedChat(t, st, `{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{"content":"x"}}]}`)
	eq(t, "response id", st.ResponseID, "resp_chatcmpl-abc")
	eq(t, "item id", dig(t, first(t, evs, "response.output_text.delta"), "item_id"), "msg_resp_chatcmpl-abc_0")

	// A chunk without an id keeps the seeded id.
	st2 := newChatRespState()
	feedChat(t, st2, `{"choices":[{"index":0,"delta":{"content":"x"}}]}`)
	eq(t, "seeded id kept", st2.ResponseID, "resp_1700000000000")
}

func TestChatToRespFinishClosesMessageAndCompletes(t *testing.T) {
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	eqNames(t, "events", names(evs),
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	)

	eq(t, "text done", evs[5].Data, jb(t, `{
		"type":"response.output_text.done","sequence_number":6,
		"item_id":"msg_resp_cmpl-1_0","output_index":0,"content_index":0,
		"text":"Hello","logprobs":[]
	}`))
	eq(t, "content part done", evs[6].Data, jb(t, `{
		"type":"response.content_part.done","sequence_number":7,
		"item_id":"msg_resp_cmpl-1_0","output_index":0,"content_index":0,
		"part":{"type":"output_text","annotations":[],"logprobs":[],"text":"Hello"}
	}`))
	eq(t, "item done", evs[7].Data, jb(t, `{
		"type":"response.output_item.done","sequence_number":8,"output_index":0,
		"item":{"id":"msg_resp_cmpl-1_0","type":"message","role":"assistant",
			"content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"Hello"}]}
	}`))
	eq(t, "completed", evs[8].Data, jb(t, `{
		"type":"response.completed","sequence_number":9,
		"response":{"id":"resp_cmpl-1","object":"response","created_at":1700000000,
			"status":"completed","background":false,"error":null}
	}`))

	// completedSent: a flush afterwards emits nothing.
	if got := st.Convert(nil); got != nil {
		t.Fatalf("flush after completed must be empty: %s", js(got))
	}
}

func TestChatToRespFinishAloneCompletes(t *testing.T) {
	// A finish chunk with no prior content just completes the response.
	st := newChatRespState()
	evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	eqNames(t, "events", names(evs), "response.created", "response.in_progress", "response.completed")
	eq(t, "completed", first(t, evs, "response.completed"), jb(t, `{
		"type":"response.completed","sequence_number":3,
		"response":{"id":"resp_1700000000000","object":"response","created_at":1700000000,
			"status":"completed","background":false,"error":null}
	}`))
}

func TestChatToRespReasoningContent(t *testing.T) {
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking hard"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	eqNames(t, "events", names(evs),
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning item
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.output_item.added", // message item
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done", // message closes BEFORE reasoning (JS loop order)
		"response.content_part.done",
		"response.output_item.done",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.completed",
	)

	eq(t, "reasoning item added", evs[2].Data, jb(t, `{
		"type":"response.output_item.added","sequence_number":3,"output_index":0,
		"item":{"id":"rs_resp_1700000000000_0","type":"reasoning","summary":[]}
	}`))
	eq(t, "reasoning delta", dig(t, evs[4].Data, "delta"), "thinking hard")
	eq(t, "reasoning text done", dig(t, evs[11].Data, "text"), "thinking hard")
	eq(t, "reasoning item done", evs[13].Data, jb(t, `{
		"type":"response.output_item.done","sequence_number":14,"output_index":0,
		"item":{"id":"rs_resp_1700000000000_0","type":"reasoning",
			"summary":[{"type":"summary_text","text":"thinking hard"}]}
	}`))
}

func TestChatToRespReasoningVendorShapes(t *testing.T) {
	t.Run("bare reasoning field", func(t *testing.T) {
		// concerns/reasoning.js extractReasoningText.
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{"reasoning":"alt"}}]}`)
		eq(t, "delta", dig(t, first(t, evs, "response.reasoning_summary_text.delta"), "delta"), "alt")
	})
	t.Run("reasoning_details array", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{"reasoning_details":[{"text":"a"},{"content":"b"}]}}]}`)
		eq(t, "delta", dig(t, first(t, evs, "response.reasoning_summary_text.delta"), "delta"), "ab")
	})
}

func TestChatToRespThinkBlockSplitAndJoin(t *testing.T) {
	t.Run("open, stream, close, resume text", func(t *testing.T) {
		// JS: content.replace("<think>","") opens; split("</think>") makes the
		// first part reasoning and the re-joined rest content again.
		st := newChatRespState()
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"content":"<think>plan:"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":" more"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"</think>final answer"}}]}`,
		)
		eqNames(t, "events", names(evs),
			"response.created",
			"response.in_progress",
			"response.output_item.added",
			"response.reasoning_summary_part.added",
			"response.reasoning_summary_text.delta", // "plan:"
			"response.reasoning_summary_text.delta", // " more"
			"response.reasoning_summary_text.done",
			"response.reasoning_summary_part.done",
			"response.output_item.done",
			"response.output_item.added",
			"response.content_part.added",
			"response.output_text.delta", // "final answer"
		)
		eq(t, "reasoning buf", dig(t, evs[6].Data, "text"), "plan: more")
		eq(t, "text resumes", dig(t, evs[11].Data, "delta"), "final answer")
	})

	t.Run("open and close in one delta", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{"content":"<think>abc</think>def"}}]}`)
		eqNames(t, "events", names(evs),
			"response.created",
			"response.in_progress",
			"response.output_item.added",
			"response.reasoning_summary_part.added",
			"response.reasoning_summary_text.delta", // abc
			"response.reasoning_summary_text.done",
			"response.reasoning_summary_part.done",
			"response.output_item.done",
			"response.output_item.added",
			"response.content_part.added",
			"response.output_text.delta", // def
		)
		eq(t, "reasoning delta", dig(t, evs[4].Data, "delta"), "abc")
		eq(t, "text delta", dig(t, evs[10].Data, "delta"), "def")
	})

	t.Run("text inside an unclosed think block never becomes content", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"content":"<think>hidden"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":" still hidden"}}]}`,
		)
		for _, e := range evs {
			if e.Event == "response.output_text.delta" {
				t.Fatalf("no text may leak while inThinking: %s", js(e.Data))
			}
		}
	})
}

func TestChatToRespToolCalls(t *testing.T) {
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"content":"calling"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[
			{"index":0,"id":"call_z","type":"function","function":{"name":"get_weather","arguments":""}}
		]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[
			{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}
		]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	eqNames(t, "events", names(evs),
		"response.created",
		"response.in_progress",
		"response.output_item.added", // message for "calling"
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done", // the message closes when tool_calls start
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	)

	eq(t, "function_call added", evs[8].Data, jb(t, `{
		"type":"response.output_item.added","sequence_number":9,"output_index":0,
		"item":{"id":"fc_call_z","type":"function_call","call_id":"call_z",
			"name":"get_weather","arguments":""}
	}`))
	eq(t, "args delta", evs[9].Data, jb(t, `{
		"type":"response.function_call_arguments.delta","sequence_number":10,
		"item_id":"fc_call_z","output_index":0,"delta":"{\"city\":\"SF\"}"
	}`))
	eq(t, "args done", evs[10].Data, jb(t, `{
		"type":"response.function_call_arguments.done","sequence_number":11,
		"item_id":"fc_call_z","output_index":0,"arguments":"{\"city\":\"SF\"}"
	}`))
	eq(t, "item done", evs[11].Data, jb(t, `{
		"type":"response.output_item.done","sequence_number":12,"output_index":0,
		"item":{"id":"fc_call_z","type":"function_call","call_id":"call_z",
			"name":"get_weather","arguments":"{\"city\":\"SF\"}"}
	}`))
}

func TestChatToRespToolCallWithoutArgsClosesAsEmptyObject(t *testing.T) {
	// JS closeToolCall: `state.funcArgsBuf[idx] || "{}"`.
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"f"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	eq(t, "arguments", dig(t, first(t, evs, "response.function_call_arguments.done"), "arguments"), "{}")
}

func TestChatToRespCustomToolCall(t *testing.T) {
	t.Run("registered custom tool becomes a custom_tool_call item with unwrapped input", func(t *testing.T) {
		st := newChatRespState()
		st.CustomToolNames["edit"] = true // the request translator's set
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":0,"id":"call_c","function":{"name":"edit","arguments":""}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":0,"function":{"arguments":"{\"input\":\"hello world\"}"}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		eqNames(t, "events", names(evs),
			"response.created",
			"response.in_progress",
			"response.output_item.added", // custom_tool_call, announced with input ""
			"response.custom_tool_call_input.delta",
			"response.custom_tool_call_input.done",
			"response.output_item.done",
			"response.completed",
		)
		eq(t, "item added", evs[2].Data, jb(t, `{
			"type":"response.output_item.added","sequence_number":3,"output_index":0,
			"item":{"id":"ctc_call_c","type":"custom_tool_call","call_id":"call_c",
				"name":"edit","input":""}
		}`))
		// The {"input": ...} wrapper never streams: no
		// response.function_call_arguments.delta may appear.
		eq(t, "input delta", first(t, evs, "response.custom_tool_call_input.delta"), jb(t, `{
			"type":"response.custom_tool_call_input.delta","sequence_number":4,
			"item_id":"ctc_call_c","output_index":0,"delta":"hello world"
		}`))
		eq(t, "input done", dig(t, evs[4].Data, "input"), "hello world")
		eq(t, "item done", evs[5].Data, jb(t, `{
			"type":"response.output_item.done","sequence_number":6,"output_index":0,
			"item":{"id":"ctc_call_c","type":"custom_tool_call","call_id":"call_c",
				"name":"edit","input":"hello world"}
		}`))
	})

	t.Run("unparsable fragments pass through raw", func(t *testing.T) {
		// extractCustomToolInput: JSON parse failure returns the raw text.
		st := newChatRespState()
		st.CustomToolNames["edit"] = true
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c","function":{"name":"edit"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"print('hi"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		eq(t, "raw input", dig(t, first(t, evs, "response.custom_tool_call_input.done"), "input"), "print('hi")
	})

	t.Run("json without an input key passes through raw", func(t *testing.T) {
		st := newChatRespState()
		st.CustomToolNames["edit"] = true
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c","function":{"name":"edit"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		eq(t, "raw input", dig(t, first(t, evs, "response.custom_tool_call_input.done"), "input"), `{"x":1}`)
	})
}

func TestChatToRespToolCallAnnounceWaitsForIDAndName(t *testing.T) {
	// JS: some providers split id and name across chunks — announce only when
	// both are known, or a custom tool is mislabeled as function_call forever.
	st := newChatRespState()
	st.CustomToolNames["edit"] = true
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"edit","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c"}]}}]}`,
	)
	// The first chunk must NOT announce; the second one does (as custom).
	eqNames(t, "events", names(evs), "response.created", "response.in_progress", "response.output_item.added")
	eq(t, "custom item", dig(t, evs[2].Data, "item", "type"), "custom_tool_call")
}

func TestChatToRespFlushClosesOpenItems(t *testing.T) {
	t.Run("text stream without finish", func(t *testing.T) {
		st := newChatRespState()
		feedChat(t, st, `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)
		evs := st.Convert(nil)
		eqNames(t, "events", names(evs),
			"response.output_text.done",
			"response.content_part.done",
			"response.output_item.done",
			"response.completed",
		)
		// A second flush is a no-op once completed was sent.
		if again := st.Convert(nil); again != nil {
			t.Fatalf("second flush must be empty: %s", js(again))
		}
	})

	t.Run("parallel tool calls close in ascending index order", func(t *testing.T) {
		// JS `for (const i in state.funcCallIds)` walks integer keys ascending;
		// Go replicates it with sorted keys in flushEvents.
		st := newChatRespState()
		feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":1,"id":"call_b","function":{"name":"g"}},
				{"index":0,"id":"call_a","function":{"name":"f"}}
			]}}]}`,
		)
		evs := st.Convert(nil)
		eqNames(t, "events", names(evs),
			"response.function_call_arguments.done", // idx 0
			"response.output_item.done",
			"response.function_call_arguments.done", // idx 1
			"response.output_item.done",
			"response.completed",
		)
		eq(t, "first closed", dig(t, evs[0].Data, "item_id"), "fc_call_a")
		eq(t, "second closed", dig(t, evs[2].Data, "item_id"), "fc_call_b")
	})

	t.Run("open reasoning closes on flush", func(t *testing.T) {
		st := newChatRespState()
		feedChat(t, st, `{"choices":[{"index":0,"delta":{"reasoning_content":"hm"}}]}`)
		evs := st.Convert(nil)
		eqNames(t, "events", names(evs),
			"response.reasoning_summary_text.done",
			"response.reasoning_summary_part.done",
			"response.output_item.done",
			"response.completed",
		)
	})
}

func TestChatToRespChunkWithoutChoicesIgnored(t *testing.T) {
	// JS: `if (!chunk.choices?.length) return []` — and state stays unstarted,
	// so a usage-only trailing chunk emits nothing.
	st := newChatRespState()
	if evs := st.Convert(jb(t, `{"id":"x","usage":{"total_tokens":5}}`)); len(evs) != 0 {
		t.Fatalf("usage-only chunk must emit nothing: %s", js(evs))
	}
	if st.Started {
		t.Fatal("state must stay unstarted")
	}
}

func TestChatToRespParallelToolCallsFinishOrderIsAscending(t *testing.T) {
	// openai-responses.js:111-114 — the finish path closes items with
	// `for (const i in state.msgItemAdded)` / `for (const i in
	// state.funcCallIds)`, which walks integer property keys ASCENDING. Go
	// ranges over maps in random order, so the loops must sort (the former
	// known parity bug #7). Announced call_b (index 1) before call_a (index 0)
	// — the finish path still closes call_a first.
	st := newChatRespState()
	evs := feedChat(t, st,
		`{"choices":[{"index":0,"delta":{"tool_calls":[
			{"index":1,"id":"call_b","function":{"name":"g"}},
			{"index":0,"id":"call_a","function":{"name":"f"}}
		]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	var closedInOrder []string
	for _, e := range evs {
		if e.Event == "response.output_item.done" {
			if item := jsonx.AsObj(e.Data["item"]); item != nil && jsonx.AsStr(item["type"]) == "function_call" {
				closedInOrder = append(closedInOrder, jsonx.AsStr(item["call_id"]))
			}
		}
	}
	if len(closedInOrder) != 2 || closedInOrder[0] != "call_a" || closedInOrder[1] != "call_b" {
		t.Fatalf("calls must close ascending (call_a then call_b), got %v (%s)",
			closedInOrder, strings.Join(names(evs), ","))
	}
	if last := evs[len(evs)-1]; last.Event != "response.completed" {
		t.Fatalf("stream must end with response.completed, got %s", last.Event)
	}
}

func TestChatToRespFinishReasonTruthiness(t *testing.T) {
	// JS `if (choice.finish_reason)` (openai-responses.js:111) — truthiness,
	// not stringiness: any truthy value completes; ""/0/false/null do not.
	t.Run("truthy non-string finish_reason completes", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{},"finish_reason":true}]}`)
		if names := names(evs); names[len(names)-1] != "response.completed" {
			t.Fatalf("must complete, got %s", strings.Join(names, ", "))
		}
	})

	t.Run("falsy finish_reason does not complete", func(t *testing.T) {
		for _, raw := range []string{
			`{"choices":[{"index":0,"delta":{},"finish_reason":""}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":0}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{}}]}`,
		} {
			st := newChatRespState()
			for _, e := range feedChat(t, st, raw) {
				if e.Event == "response.completed" {
					t.Fatalf("%s must not complete the response", raw)
				}
			}
		}
	})
}

func TestChatToRespNumericStringIndices(t *testing.T) {
	// JS plain-object property keys coerce: index "5" and 5 share one map slot
	// (initState's msg*/func* objects, index.js:252-263), while `output_index:
	// idx` emits the RAW value and `msg_${id}_${idx}` stringifies it.
	t.Run("choice index \"2\" announces raw and closes coerced", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st,
			`{"choices":[{"index":"2","delta":{"content":"hi"}}]}`,
			`{"choices":[{"index":"2","delta":{},"finish_reason":"stop"}]}`,
		)
		added := first(t, evs, "response.output_item.added")
		eq(t, "added output_index raw", dig(t, added, "output_index"), "2")
		eq(t, "item id coerced", dig(t, added, "item", "id"), "msg_resp_1700000000000_2")
		done := first(t, evs, "response.output_item.done")
		eq(t, "done output_index coerced", dig(t, done, "output_index"), float64(2))
	})

	t.Run("tool_call index \"5\" routes deltas by the coerced slot", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":"5","id":"call_s","function":{"name":"f"}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":"5","function":{"arguments":"{}"}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		added := first(t, evs, "response.output_item.added")
		eq(t, "added output_index raw", dig(t, added, "output_index"), "5")
		eq(t, "delta routes to the same slot",
			dig(t, first(t, evs, "response.function_call_arguments.delta"), "output_index"), "5")
		eq(t, "close coerces",
			dig(t, first(t, evs, "response.function_call_arguments.done"), "output_index"), float64(5))
	})

	t.Run("tool_call index null is nullish-coerced to 0", func(t *testing.T) {
		// `tc.index ?? 0` (openai-responses.js:275) — null/undefined only.
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{"tool_calls":[
			{"index":null,"id":"call_n","function":{"name":"f"}}
		]}}]}`)
		eq(t, "output_index", dig(t, first(t, evs, "response.output_item.added"), "output_index"), float64(0))
	})
}

func TestChatToRespArgumentsTruthiness(t *testing.T) {
	// JS `if (tc.function?.arguments)` (openai-responses.js:305) — truthiness:
	// the raw value rides the delta and the buffer accumulates String(v)
	// (`state.funcArgsBuf[tcIdx] += tc.function.arguments`, :318).
	t.Run("numeric arguments delta raw, close coerced", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":0,"id":"call_n","function":{"name":"f"}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":0,"function":{"arguments":5}}
			]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		eq(t, "delta raw", dig(t, first(t, evs, "response.function_call_arguments.delta"), "delta"), float64(5))
		eq(t, "close coerces via +=", dig(t, first(t, evs, "response.function_call_arguments.done"), "arguments"), "5")
	})

	t.Run("empty-string arguments emit no delta", func(t *testing.T) {
		st := newChatRespState()
		evs := feedChat(t, st, `{"choices":[{"index":0,"delta":{"tool_calls":[
			{"index":0,"id":"call_n","function":{"name":"f","arguments":""}}
		]}}]}`)
		for _, e := range evs {
			if e.Event == "response.function_call_arguments.delta" {
				t.Fatalf("falsy arguments must not emit a delta: %s", js(e.Data))
			}
		}
	})
}
