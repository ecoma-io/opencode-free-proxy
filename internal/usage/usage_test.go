package usage

// Tests for the port of open-sse/utils/usageTracking.js. Numbers are float64
// (encoding/json's decode type), matching the JS engine's Number.

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// estimateBody stays HTML-escape-free for byte-clarity ({"model":"m"} -> 13
// units); since EstimateInput now serializes with SetEscapeHTML(false) and
// counts UTF-16 units exactly like JSON.stringify(body).length, HTML-bearing
// bodies are comparable too (see TestEstimateInputUtf16UnitsAndHTMLEscapes).
var estimateBody = map[string]any{"model": "m"} // {"model":"m"} -> 13 units

func bodyBytes(t *testing.T, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return len(b)
}

func TestNormalizeKeepsKnownFieldsOnly(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"unknown fields dropped", map[string]any{"prompt_tokens": 10.0, "provider": "x", "estimated": true},
			map[string]any{"prompt_tokens": 10.0}},
		{"numeric strings coerced (JS Number)", map[string]any{"completion_tokens": "7"},
			map[string]any{"completion_tokens": 7.0}},
		{"nil fields skipped (JS null guard)", map[string]any{"prompt_tokens": nil, "total_tokens": 3.0},
			map[string]any{"total_tokens": 3.0}},
		// assignNumber only assigns when Number(value) is finite — garbage
		// never becomes 0, the key just drops.
		{"non-numeric string dropped (Number('abc') is NaN)", map[string]any{"prompt_tokens": "abc"}, nil},
		{"objects are not finite (Number({}) is NaN)", map[string]any{"prompt_tokens": map[string]any{}}, nil},
		// ...while these coerce to finite values.
		{"empty string is Number('') === 0", map[string]any{"prompt_tokens": ""},
			map[string]any{"prompt_tokens": 0.0}},
		{"booleans coerce (Number(true) === 1)", map[string]any{"prompt_tokens": true},
			map[string]any{"prompt_tokens": 1.0}},
		{"details objects preserved as-is", map[string]any{
			"prompt_tokens":         5.0,
			"prompt_tokens_details": map[string]any{"cached_tokens": 2.0, "vendor_key": "x"}},
			map[string]any{
				"prompt_tokens":         5.0,
				"prompt_tokens_details": map[string]any{"cached_tokens": 2.0, "vendor_key": "x"}}},
		{"nothing numeric means nil", map[string]any{"estimated": true}, nil},
		{"nil input means nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Normalize(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalFoldsCacheIntoPromptOnce(t *testing.T) {
	// canonicalizeUsage: Claude reports a cache-EXCLUSIVE prompt, so
	// cache_read + cache_creation fold into prompt_tokens.
	claude := map[string]any{
		"prompt_tokens":               100.0,
		"cache_read_input_tokens":     50.0,
		"cache_creation_input_tokens": 20.0,
		"completion_tokens":           10.0,
	}
	want := map[string]any{
		"prompt_tokens":               170.0,
		"completion_tokens":           10.0,
		"total_tokens":                180.0,
		"cached_tokens":               50.0,
		"cache_creation_input_tokens": 20.0,
	}
	got := Canonical(claude)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Canonical(claude) = %v, want %v", got, want)
	}
	// Idempotent: the folded output carries cached_tokens, so re-running takes
	// the passthrough branch and must not double-add.
	if again := Canonical(got); !reflect.DeepEqual(again, want) {
		t.Fatalf("Canonical is not idempotent: %v", again)
	}

	// OpenAI-style: cached_tokens is already inside the prompt — pass through.
	// canonicalizeUsage ALWAYS emits the five canonical counts (explicit 0s
	// included); only reasoning_tokens is conditional.
	openai := map[string]any{"prompt_tokens": 100.0, "cached_tokens": 30.0, "completion_tokens": 10.0}
	wantOpenAI := map[string]any{
		"prompt_tokens": 100.0, "completion_tokens": 10.0, "total_tokens": 110.0,
		"cached_tokens": 30.0, "cache_creation_input_tokens": 0.0,
	}
	if got := Canonical(openai); !reflect.DeepEqual(got, wantOpenAI) {
		t.Fatalf("Canonical(openai) = %v, want %v", got, wantOpenAI)
	}

	// Nested prompt_tokens_details fallbacks (buildUsage's forwarding shape).
	nested := Canonical(map[string]any{
		"prompt_tokens":         100.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 30.0, "cache_creation_tokens": 9.0},
	})
	if nested["cached_tokens"] != 30.0 || nested["cache_creation_input_tokens"] != 9.0 || nested["prompt_tokens"] != 100.0 {
		t.Fatalf("nested details fallback broken: %v", nested)
	}

	// output_tokens fallback for completion; reasoning only when > 0.
	fallback := Canonical(map[string]any{"output_tokens": 12.0, "reasoning_tokens": 4.0})
	if fallback["completion_tokens"] != 12.0 || fallback["total_tokens"] != 12.0 || fallback["reasoning_tokens"] != 4.0 {
		t.Fatalf("completion/output_tokens fallback broken: %v", fallback)
	}
	if _, has := Canonical(map[string]any{"completion_tokens": 5.0})["reasoning_tokens"]; has {
		t.Fatal("reasoning_tokens must be absent when 0")
	}

	// The fallback chains are nullish (??), not zeroish: a PRESENT 0 beats an
	// absent sibling, and the nested details shapes are only consulted when
	// the top-level key is absent/null.
	presentZero := Canonical(map[string]any{"completion_tokens": 0.0, "output_tokens": 100.0})
	if presentZero["completion_tokens"] != 0.0 || presentZero["total_tokens"] != 0.0 {
		t.Fatalf("present 0 must win the ?? chain: %v", presentZero)
	}
	zeroCachedNested := Canonical(map[string]any{
		"prompt_tokens": 100.0, "completion_tokens": 10.0, "cached_tokens": 0.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 30.0}})
	if zeroCachedNested["cached_tokens"] != 0.0 {
		t.Fatalf("present cached_tokens 0 must shadow the nested shape: %v", zeroCachedNested)
	}
	zeroCreationNested := Canonical(map[string]any{
		"cache_creation_input_tokens": 0.0,
		"prompt_tokens_details":       map[string]any{"cache_creation_tokens": 9.0}})
	if zeroCreationNested["cache_creation_input_tokens"] != 0.0 {
		t.Fatalf("present cache_creation 0 must shadow the nested shape: %v", zeroCreationNested)
	}
}

func TestMergeFieldWiseMax(t *testing.T) {
	prev := map[string]any{
		"prompt_tokens":         10.0,
		"completion_tokens":     1.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 4.0},
	}
	next := map[string]any{
		"prompt_tokens":         25.0,
		"completion_tokens":     9.0,
		"total_tokens":          30.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 8.0},
		"note":                  "text",
	}
	got := Merge(prev, next)
	want := map[string]any{
		"prompt_tokens":         25.0,
		"completion_tokens":     9.0,
		"total_tokens":          30.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 8.0}, // object: latest wins
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}

	// Non-finite next values must not poison the accumulation
	// (mergeUsage's Number.isFinite guard; Math.max(x, NaN) is NaN).
	poisoned := Merge(map[string]any{"prompt_tokens": 10.0},
		map[string]any{"prompt_tokens": math.NaN(), "completion_tokens": math.Inf(1)})
	if poisoned["prompt_tokens"] != 10.0 {
		t.Fatalf("NaN next must keep prev: %v", poisoned)
	}
	if _, has := poisoned["completion_tokens"]; has {
		t.Fatalf("Inf next must be skipped: %v", poisoned)
	}

	// Math.max bases at 0 when the previous value is not itself a number
	// (`typeof merged[k] === "number" ? merged[k] : 0`) — no string coercion.
	strPrev := Merge(map[string]any{"prompt_tokens": "9"}, map[string]any{"prompt_tokens": 5.0})
	if strPrev["prompt_tokens"] != 5.0 {
		t.Fatalf("string prev must base at 0: %v", strPrev)
	}
	// Math.max(NaN, x) is NaN on the JS side too — Go's math.Max propagates it
	// the same way (only the isFinite guard protects the accumulation, and it
	// guards next, not prev).
	nanPrev := Merge(map[string]any{"prompt_tokens": math.NaN()}, map[string]any{"prompt_tokens": 5.0})
	if !math.IsNaN(nanPrev["prompt_tokens"].(float64)) {
		t.Fatalf("NaN prev must propagate like Math.max: %v", nanPrev)
	}

	if Merge(nil, next)["prompt_tokens"] != 25.0 {
		t.Fatal("nil prev returns next")
	}
	if Merge(prev, nil)["prompt_tokens"] != 10.0 {
		t.Fatal("nil next returns prev")
	}
}

func TestHasValid(t *testing.T) {
	if HasValid(nil) {
		t.Fatal("nil usage must be invalid")
	}
	if HasValid(map[string]any{}) {
		t.Fatal("empty usage must be invalid")
	}
	if HasValid(map[string]any{"prompt_tokens": 0.0, "completion_tokens": 0.0, "total_tokens": 0.0}) {
		t.Fatal("zero-only usage must be invalid")
	}
	for _, field := range []string{
		"prompt_tokens", "completion_tokens", "total_tokens",
		"input_tokens", "output_tokens", "promptTokenCount", "candidatesTokenCount",
	} {
		if !HasValid(map[string]any{field: 1.0}) {
			t.Fatalf("field %q = 1 must be valid", field)
		}
	}
	// hasValidUsage requires `typeof usage[field] === "number"` — a numeric
	// string, however parseable, never validates.
	if HasValid(map[string]any{"prompt_tokens": "5"}) {
		t.Fatal("numeric strings must not validate (JS typeof number check)")
	}
	if HasValid(map[string]any{"prompt_tokens": nil}) {
		t.Fatal("nil field must not validate")
	}
	if HasValid(map[string]any{"prompt_tokens": "0"}) {
		t.Fatal("zero-valued field must not validate")
	}
}

func TestExtractFromChat(t *testing.T) {
	full := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": 10.0, "completion_tokens": 4.0, "total_tokens": 14.0,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 3.0},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 2.0},
	}})
	want := map[string]any{
		"prompt_tokens":             10.0,
		"completion_tokens":         4.0,
		"cached_tokens":             3.0,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 3.0},
		"reasoning_tokens":          2.0,
		"completion_tokens_details": map[string]any{"reasoning_tokens": 2.0},
	}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("ExtractFromChat full = %v, want %v", full, want)
	}

	// extractUsage forwards prompt_tokens_details / completion_tokens_details
	// RAW — vendor keys beside the known ones survive untouched.
	vendor := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": 10.0, "completion_tokens": 4.0,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 3.0, "vendor_key": "x"},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 2.0, "accepted_prediction_tokens": 7.0},
	}})
	if !reflect.DeepEqual(vendor["prompt_tokens_details"], map[string]any{"cached_tokens": 3.0, "vendor_key": "x"}) {
		t.Fatalf("prompt_tokens_details must forward raw: %v", vendor)
	}
	if !reflect.DeepEqual(vendor["completion_tokens_details"], map[string]any{"reasoning_tokens": 2.0, "accepted_prediction_tokens": 7.0}) {
		t.Fatalf("completion_tokens_details must forward raw: %v", vendor)
	}

	// DeepSeek: prompt_cache_hit_tokens stands in for details.cached_tokens
	// through the `details?.cached_tokens || prompt_cache_hit_tokens` chain —
	// but JS emits NO prompt_tokens_details for a hit-only stream (it never
	// synthesizes detail objects) and no reasoning_tokens either.
	deep := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": 9.0, "completion_tokens": 3.0, "prompt_cache_hit_tokens": 6.0}})
	wantDeep := map[string]any{
		"prompt_tokens":     9.0,
		"completion_tokens": 3.0,
		"cached_tokens":     6.0,
	}
	if !reflect.DeepEqual(deep, wantDeep) {
		t.Fatalf("ExtractFromChat deepseek = %v, want %v", deep, wantDeep)
	}

	// An explicit cached_tokens 0 is falsy, so it falls through the || chain:
	// cached_tokens drops, while the raw details object still forwards.
	zeroCached := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": 8.0, "completion_tokens": 2.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 0.0}}})
	wantZero := map[string]any{
		"prompt_tokens":         8.0,
		"completion_tokens":     2.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 0.0},
	}
	if !reflect.DeepEqual(zeroCached, wantZero) {
		t.Fatalf("ExtractFromChat zero-cached = %v, want %v", zeroCached, wantZero)
	}

	// ...but `a || b` yields b even when b is falsy: a hit count of 0 SURVIVES
	// as cached_tokens 0 (`undefined || 0` is 0, and 0 is finite).
	zeroHit := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": 8.0, "completion_tokens": 2.0, "prompt_cache_hit_tokens": 0.0}})
	wantHit := map[string]any{"prompt_tokens": 8.0, "completion_tokens": 2.0, "cached_tokens": 0.0}
	if !reflect.DeepEqual(zeroHit, wantHit) {
		t.Fatalf("ExtractFromChat zero-hit = %v, want %v", zeroHit, wantHit)
	}

	// The gate is `usage.prompt_tokens !== undefined`: a JSON null enters, and
	// normalizeUsage then drops the null field (key absent, not 0).
	nullPrompt := ExtractFromChat(map[string]any{"usage": map[string]any{
		"prompt_tokens": nil, "completion_tokens": 5.0}})
	if !reflect.DeepEqual(nullPrompt, map[string]any{"completion_tokens": 5.0}) {
		t.Fatalf("null prompt_tokens must drop: %v", nullPrompt)
	}

	if ExtractFromChat(map[string]any{}) != nil {
		t.Fatal("chunk without usage returns nil")
	}
	if ExtractFromChat(map[string]any{"usage": map[string]any{"completion_tokens": 1.0}}) != nil {
		t.Fatal("usage without prompt_tokens returns nil (extractUsage requires prompt_tokens)")
	}
}

func TestExtractFromResponses(t *testing.T) {
	mk := func(typ string) map[string]any {
		return map[string]any{"type": typ, "response": map[string]any{
			"id": "resp_1",
			"usage": map[string]any{
				"input_tokens": 10.0, "output_tokens": 4.0, "total_tokens": 14.0,
				"input_tokens_details":  map[string]any{"cached_tokens": 3.0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 2.0},
			},
		}}
	}
	want := map[string]any{
		"prompt_tokens":         10.0,
		"completion_tokens":     4.0,
		"cached_tokens":         3.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 3.0},
		"reasoning_tokens":      2.0,
	}
	for _, typ := range []string{"response.completed", "response.done"} {
		if got := ExtractFromResponses(mk(typ)); !reflect.DeepEqual(got, want) {
			t.Fatalf("ExtractFromResponses(%s) = %v, want %v", typ, got, want)
		}
	}
	if ExtractFromResponses(mk("response.output_text.delta")) != nil {
		t.Fatal("non-terminal event type returns nil")
	}
	if ExtractFromResponses(map[string]any{"type": "response.completed"}) != nil {
		t.Fatal("event without response.usage returns nil")
	}

	// `usage.input_tokens || usage.prompt_tokens || 0`: the chat-shaped names
	// are the || fallbacks inside a Responses event.
	chatShaped := ExtractFromResponses(map[string]any{"type": "response.done", "response": map[string]any{
		"usage": map[string]any{"prompt_tokens": 9.0, "completion_tokens": 3.0}}})
	wantChat := map[string]any{"prompt_tokens": 9.0, "completion_tokens": 3.0}
	if !reflect.DeepEqual(chatShaped, wantChat) {
		t.Fatalf("ExtractFromResponses chat-shaped = %v, want %v", chatShaped, wantChat)
	}
	// A present-but-zero Responses name is falsy and still falls through (||).
	zeroInput := ExtractFromResponses(map[string]any{"type": "response.done", "response": map[string]any{
		"usage": map[string]any{"input_tokens": 0.0, "prompt_tokens": 9.0, "output_tokens": 1.0}}})
	if zeroInput["prompt_tokens"] != 9.0 {
		t.Fatalf("zero input_tokens must fall back to prompt_tokens: %v", zeroInput)
	}

	// `cached_tokens: usage.input_tokens_details?.cached_tokens` carries no ||
	// guard: an explicit 0 survives normalizeUsage (finite after Number), while
	// the synthesized prompt_tokens_details only appears when cached_tokens is
	// truthy (`cachedTokens ? {...} : undefined`).
	zeroCached := ExtractFromResponses(map[string]any{"type": "response.completed", "response": map[string]any{
		"usage": map[string]any{"input_tokens": 10.0, "output_tokens": 4.0,
			"input_tokens_details": map[string]any{"cached_tokens": 0.0}}}})
	wantZero := map[string]any{"prompt_tokens": 10.0, "completion_tokens": 4.0, "cached_tokens": 0.0}
	if !reflect.DeepEqual(zeroCached, wantZero) {
		t.Fatalf("ExtractFromResponses zero-cached = %v, want %v", zeroCached, wantZero)
	}

	// reasoning_tokens likewise keeps an explicit 0 — finite after Number.
	zeroReasoning := ExtractFromResponses(map[string]any{"type": "response.completed", "response": map[string]any{
		"usage": map[string]any{"input_tokens": 10.0, "output_tokens": 4.0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0.0}}}})
	wantZR := map[string]any{"prompt_tokens": 10.0, "completion_tokens": 4.0, "reasoning_tokens": 0.0}
	if !reflect.DeepEqual(zeroReasoning, wantZR) {
		t.Fatalf("ExtractFromResponses zero-reasoning = %v, want %v", zeroReasoning, wantZR)
	}

	// `input_tokens || prompt_tokens || 0` ends in the literal 0, so the
	// canonical keys are ALWAYS present — an empty usage still extracts
	// {prompt_tokens: 0, completion_tokens: 0}, never nil.
	empty := ExtractFromResponses(map[string]any{"type": "response.completed", "response": map[string]any{
		"usage": map[string]any{}}})
	if !reflect.DeepEqual(empty, map[string]any{"prompt_tokens": 0.0, "completion_tokens": 0.0}) {
		t.Fatalf("ExtractFromResponses empty usage = %v, want zeroed counts", empty)
	}
}

func TestEstimate(t *testing.T) {
	if n := bodyBytes(t, estimateBody); n != 13 {
		t.Fatalf("test assumption broken: body encodes to %d bytes, want 13", n)
	}
	// 13 body bytes -> ceil(13/4) = 4 input tokens; 12 content chars ->
	// max(1, floor(12/4)) = 3 output tokens; +2000 buffer on prompt and total
	// (estimateUsage -> formatUsage -> addBufferToUsage).
	got := Estimate(estimateBody, 12)
	want := map[string]any{
		"prompt_tokens": 2004.0, "completion_tokens": 3.0, "total_tokens": 2007.0, "estimated": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Estimate = %v, want %v", got, want)
	}
	zero := Estimate(estimateBody, 0)
	if zero["completion_tokens"] != 0.0 || zero["total_tokens"] != 2004.0 {
		t.Fatalf("Estimate with zero content = %v", zero)
	}
	if zero["estimated"] != true {
		t.Fatal("estimate must carry the estimated:true marker")
	}
}

// estimateOutputTokens returns Math.max(1, Math.floor(contentLength / 4)) for
// any positive length, so a 1-3 char completion still counts 1 output token;
// only `!contentLength || contentLength <= 0` yields 0.
func TestEstimateOutput(t *testing.T) {
	cases := []struct {
		in   int
		want float64
	}{
		{0, 0}, {-3, 0},
		{1, 1}, {2, 1}, {3, 1}, // floor(0) lifted to the 1-token minimum
		{4, 1}, {7, 1}, {8, 2}, {12, 3},
	}
	for _, c := range cases {
		if got := EstimateOutput(c.in); got != c.want {
			t.Fatalf("EstimateOutput(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAddBuffer(t *testing.T) {
	// OpenAI shape: prompt and total buffered, completion untouched.
	got := AddBuffer(map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0, "total_tokens": 15.0})
	want := map[string]any{"prompt_tokens": 2010.0, "completion_tokens": 5.0, "total_tokens": 2015.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AddBuffer openai = %v, want %v", got, want)
	}

	// Claude shape: input_tokens gets the buffer; total_tokens is NOT computed
	// (the fallback needs prompt_tokens + completion_tokens, not input/output).
	claude := AddBuffer(map[string]any{"input_tokens": 5.0, "output_tokens": 2.0})
	wantClaude := map[string]any{"input_tokens": 2005.0, "output_tokens": 2.0}
	if !reflect.DeepEqual(claude, wantClaude) {
		t.Fatalf("AddBuffer claude = %v, want %v", claude, wantClaude)
	}

	// A usage carrying both shapes buffers both keys; the recomputed total uses
	// the already-buffered prompt.
	both := AddBuffer(map[string]any{"input_tokens": 5.0, "prompt_tokens": 10.0, "completion_tokens": 1.0})
	wantBoth := map[string]any{
		"input_tokens": 2005.0, "prompt_tokens": 2010.0, "completion_tokens": 1.0, "total_tokens": 2011.0,
	}
	if !reflect.DeepEqual(both, wantBoth) {
		t.Fatalf("AddBuffer both = %v, want %v", both, wantBoth)
	}

	// Prompt alone with no total and no completion stays total-less.
	solo := AddBuffer(map[string]any{"prompt_tokens": 10.0})
	if !reflect.DeepEqual(solo, map[string]any{"prompt_tokens": 2010.0}) {
		t.Fatalf("AddBuffer solo = %v", solo)
	}

	if AddBuffer(nil) != nil {
		t.Fatal("nil usage must stay nil")
	}
}

func TestEstimateResponses(t *testing.T) {
	// estimateUsage for a Responses client composes formatUsage's default
	// OpenAI shape (addBufferToUsage: prompt 2004 / total 2007) and then
	// convertUsageForFormat, which reads input_tokens ?? prompt_tokens and
	// keeps only input/output tokens + estimated — total_tokens is dropped.
	// Net: input_tokens = raw + 2000 (the buffer lands via prompt_tokens).
	got := EstimateResponses(estimateBody, 12)
	want := map[string]any{"input_tokens": 2004.0, "output_tokens": 3.0, "estimated": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EstimateResponses = %v, want %v", got, want)
	}
}

func TestSynthesizeThinking(t *testing.T) {
	type tc struct {
		name   string
		target string
		in     map[string]any
		want   map[string]any
		ratio  float64
	}
	ratio075 := 0.75
	cases := []tc{
		{"reported top-level reasoning wins", "chat",
			map[string]any{"completion_tokens": 100.0, "reasoning_tokens": 5.0},
			map[string]any{"completion_tokens": 100.0, "reasoning_tokens": 5.0}, ratio075},
		{"reported chat-details reasoning wins", "chat",
			map[string]any{"completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 6.0}},
			map[string]any{"completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 6.0}}, ratio075},
		{"reported responses-details reasoning wins", "responses",
			map[string]any{"output_tokens": 100.0,
				"output_tokens_details": map[string]any{"reasoning_tokens": 6.0}},
			map[string]any{"output_tokens": 100.0,
				"output_tokens_details": map[string]any{"reasoning_tokens": 6.0}}, ratio075},
		{"thoughtsTokenCount counts as reported", "chat",
			map[string]any{"candidatesTokenCount": 40.0, "thoughtsTokenCount": 5.0},
			map[string]any{"candidatesTokenCount": 40.0, "thoughtsTokenCount": 5.0}, ratio075},
		{"zero completion untouched", "chat",
			map[string]any{"completion_tokens": 0.0},
			map[string]any{"completion_tokens": 0.0}, ratio075},
		{"absent completion chain untouched", "chat",
			map[string]any{"prompt_tokens": 5.0},
			map[string]any{"prompt_tokens": 5.0}, ratio075},
		{"chat target writes completion_tokens_details", "chat",
			map[string]any{"prompt_tokens": 1.0, "completion_tokens": 100.0},
			map[string]any{"prompt_tokens": 1.0, "completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 75.0}}, ratio075},
		{"responses target writes output_tokens_details", "responses",
			map[string]any{"output_tokens": 100.0},
			map[string]any{"output_tokens": 100.0,
				"output_tokens_details": map[string]any{"reasoning_tokens": 75.0}}, ratio075},
		{"completion at threshold synthesizes 0", "chat",
			map[string]any{"completion_tokens": 10.0},
			map[string]any{"completion_tokens": 10.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 0.0}}, ratio075},
		{"just above threshold floors", "chat",
			map[string]any{"completion_tokens": 11.0},
			map[string]any{"completion_tokens": 11.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 8.0}}, ratio075},
		{"non-default ratio floors", "chat",
			map[string]any{"completion_tokens": 25.0},
			map[string]any{"completion_tokens": 25.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 12.0}}, 0.5},
		{"negative ratio is finite so it is used verbatim", "chat",
			map[string]any{"completion_tokens": 100.0},
			map[string]any{"completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": -50.0}}, -0.5},
		// The ?? chain takes the FIRST PRESENT value even when it is 0 — a
		// zero reasoning_tokens shadows details that do report a count, so
		// synthesis still runs.
		{"present zero reasoning short-circuits the nullish chain", "chat",
			map[string]any{"completion_tokens": 100.0, "reasoning_tokens": 0.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 6.0}},
			map[string]any{"completion_tokens": 100.0, "reasoning_tokens": 0.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 75.0}}, ratio075},
		{"existing details keys kept", "chat",
			map[string]any{"completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"accepted_prediction_tokens": 7.0}},
			map[string]any{"completion_tokens": 100.0,
				"completion_tokens_details": map[string]any{"accepted_prediction_tokens": 7.0, "reasoning_tokens": 75.0}}, ratio075},
		{"completion read from output_tokens", "chat",
			map[string]any{"output_tokens": 40.0},
			map[string]any{"output_tokens": 40.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 30.0}}, ratio075},
		{"completion read from candidatesTokenCount", "chat",
			map[string]any{"candidatesTokenCount": 40.0},
			map[string]any{"candidatesTokenCount": 40.0,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 30.0}}, ratio075},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SynthesizeThinking(c.in, c.target, c.ratio)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("SynthesizeThinking(%v, %s, %v) = %v, want %v", c.in, c.target, c.ratio, got, c.want)
			}
		})
	}

	// Invalid (non-finite) ratio falls back to the 0.75 default
	// (HIDDEN_THINKING_RATIO) — Number.isFinite is the only check, so 0 and
	// negatives are used verbatim (see the table case above).
	if got := SynthesizeThinking(map[string]any{"completion_tokens": 100.0}, "chat", math.NaN()); got["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != 75.0 {
		t.Fatalf("NaN ratio must fall back to the default: %v", got)
	}
	if got := SynthesizeThinking(map[string]any{"completion_tokens": 100.0}, "chat", math.Inf(1)); got["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != 75.0 {
		t.Fatalf("Inf ratio must fall back to the default: %v", got)
	}
	if got := SynthesizeThinking(nil, "chat", 0.75); got != nil {
		t.Fatalf("nil usage must stay nil: %v", got)
	}

	// synthesizeThinkingTokens returns a NEW object at every level (JS spread);
	// the stats side holding the original details object stays untouched.
	src := map[string]any{"completion_tokens": 100.0,
		"completion_tokens_details": map[string]any{"accepted_prediction_tokens": 7.0}}
	_ = SynthesizeThinking(src, "chat", 0.75)
	if !reflect.DeepEqual(src["completion_tokens_details"], map[string]any{"accepted_prediction_tokens": 7.0}) {
		t.Fatalf("source details must not be mutated: %v", src)
	}
}
