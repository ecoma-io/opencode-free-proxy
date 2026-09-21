package translate

// Tests for chunk.go — the shared chunk/usage builders ported from
// 9router open-sse/translator/concerns/chunk.js (buildChunk),
// concerns/usage.js (buildUsage) and concerns/toolCall.js (fallbackToolCallId).

import (
	"regexp"
	"testing"
)

func TestBuildChunk(t *testing.T) {
	// concerns/chunk.js buildChunk: caller supplies id/created/model; choices
	// always carries exactly one entry with index 0.
	got := BuildChunk("chatcmpl-1", 1700000000, "m",
		map[string]any{"content": "hi"}, nil)
	eq(t, "chunk", got, jb(t, `{
		"id":"chatcmpl-1",
		"object":"chat.completion.chunk",
		"created":1700000000,
		"model":"m",
		"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]
	}`))

	got2 := BuildChunk("chatcmpl-1", 1700000000, "m", map[string]any{}, "tool_calls")
	eq(t, "finish reason", dig(t, got2, "choices", 0, "finish_reason"), "tool_calls")
}

func TestBuildUsage(t *testing.T) {
	// Counters arrive as decoded JSON numbers (float64) or the JS `|| 0`
	// fallback; BuildUsage embeds them verbatim and only the detail gates
	// coerce numerically.
	f := func(n int) any { return float64(n) }

	t.Run("bare counters only when every detail is zero", func(t *testing.T) {
		eq(t, "usage", BuildUsage(f(10), f(5), f(15), f(0), f(0), f(0)), jb(t, `{
			"prompt_tokens":10,"completion_tokens":5,"total_tokens":15
		}`))
	})

	t.Run("cached tokens produce prompt_tokens_details.cached_tokens", func(t *testing.T) {
		eq(t, "usage", BuildUsage(f(120), f(5), f(125), f(80), f(0), f(0)), jb(t, `{
			"prompt_tokens":120,"completion_tokens":5,"total_tokens":125,
			"prompt_tokens_details":{"cached_tokens":80}
		}`))
	})

	t.Run("cache creation tokens share the prompt details block", func(t *testing.T) {
		eq(t, "usage", BuildUsage(f(120), f(5), f(125), f(0), f(7), f(0)), jb(t, `{
			"prompt_tokens":120,"completion_tokens":5,"total_tokens":125,
			"prompt_tokens_details":{"cache_creation_tokens":7}
		}`))
	})

	t.Run("cached and cache creation combine", func(t *testing.T) {
		eq(t, "usage", BuildUsage(f(120), f(5), f(125), f(80), f(7), f(0)), jb(t, `{
			"prompt_tokens":120,"completion_tokens":5,"total_tokens":125,
			"prompt_tokens_details":{"cached_tokens":80,"cache_creation_tokens":7}
		}`))
	})

	t.Run("reasoning tokens produce completion_tokens_details", func(t *testing.T) {
		eq(t, "usage", BuildUsage(f(10), f(30), f(40), f(0), f(0), f(9)), jb(t, `{
			"prompt_tokens":10,"completion_tokens":30,"total_tokens":40,
			"completion_tokens_details":{"reasoning_tokens":9}
		}`))
	})

	t.Run("zero reasoning adds no details block", func(t *testing.T) {
		// JS: `if (reasoningTokens > 0)` — 0 stays absent.
		got := BuildUsage(f(10), f(30), f(40), f(0), f(0), f(0))
		if _, has := got["completion_tokens_details"]; has {
			t.Fatalf("no details block expected: %s", js(got))
		}
	})

	t.Run("numeric-string counters keep their type and pass the > 0 gate", func(t *testing.T) {
		// JS buildUsage embeds the raw value (usage.js:4) and only the
		// detail gates coerce: "80" > 0 is numerically true.
		eq(t, "usage", BuildUsage("120", f(5), "125", "80", f(0), f(0)), jb(t, `{
			"prompt_tokens":"120","completion_tokens":5,"total_tokens":"125",
			"prompt_tokens_details":{"cached_tokens":"80"}
		}`))
	})

	t.Run("non-numeric strings never open a details block", func(t *testing.T) {
		// JS `"abc" > 0` is NaN > 0 → false.
		got := BuildUsage("abc", f(5), "abc5", "abc", f(0), f(0))
		if _, has := got["prompt_tokens_details"]; has {
			t.Fatalf("no details block expected: %s", js(got))
		}
	})
}

func TestFallbackToolCallID(t *testing.T) {
	// concerns/toolCall.js fallbackToolCallId(): `call_${Date.now()}` (millis).
	re := regexp.MustCompile(`^call_[0-9]{10,}$`)
	if !re.MatchString(FallbackToolCallID()) {
		t.Fatalf("FallbackToolCallID() = %q, want call_<unix-millis>", FallbackToolCallID())
	}
}

func TestJSONStringifyStr(t *testing.T) {
	// Strings pass through; everything else is JSON.stringify'd.
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passthrough", "already", "already"},
		{"empty string", "", ""},
		{"object", map[string]any{"a": float64(1)}, `{"a":1}`},
		{"array", []any{"x"}, `["x"]`},
		{"number", float64(5), "5"},
		{"bool", true, "true"},
		{"nil", nil, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := JSONStringifyStr(tc.in); got != tc.want {
				t.Fatalf("JSONStringifyStr(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReasoningDeltaAndExtract(t *testing.T) {
	// concerns/reasoning.js reasoningDelta / extractReasoningText.
	eq(t, "delta shape", ReasoningDelta("why"), map[string]any{"reasoning_content": "why"})

	t.Run("extraction across vendor shapes", func(t *testing.T) {
		cases := []struct {
			name  string
			delta string
			want  string
		}{
			{"reasoning_content", `{"reasoning_content":"a"}`, "a"},
			{"reasoning fallback", `{"reasoning":"b"}`, "b"},
			{"reasoning_details strings", `{"reasoning_details":["x","y"]}`, "xy"},
			{"reasoning_details objects", `{"reasoning_details":[{"text":"a"},{"content":"b"},{}]}`, "ab"},
			{"empty reasoning_content falls through", `{"reasoning_content":"","reasoning":"z"}`, "z"},
			{"nothing", `{}`, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := ExtractReasoningText(jb(t, tc.delta)); got != tc.want {
					t.Fatalf("ExtractReasoningText(%s) = %q, want %q", tc.delta, got, tc.want)
				}
			})
		}
	})

	t.Run("nil delta", func(t *testing.T) {
		if got := ExtractReasoningText(nil); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}
