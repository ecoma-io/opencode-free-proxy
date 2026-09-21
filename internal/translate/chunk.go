package translate

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"opencode-free-proxy/internal/jsonx"
)

// ModelFallback is the model name used when a stream carries none
// (schema/defaults.js MODEL_FALLBACK).
const ModelFallback = "unknown"

// BuildChunk builds an OpenAI chat.completion.chunk (concerns/chunk.js).
func BuildChunk(id string, created int64, model string, delta map[string]any, finishReason any) map[string]any {
	return jsonx.ObjOf(
		"id", id,
		"object", "chat.completion.chunk",
		"created", created,
		"model", model,
		"choices", jsonx.ArrOf(jsonx.ObjOf(
			"index", 0,
			"delta", delta,
			"finish_reason", finishReason,
		)),
	)
}

// BuildUsage builds an OpenAI usage object; detail blocks appear only when
// their counters are > 0 (concerns/usage.js buildUsage). Counters are `any`
// and embedded VERBATIM — the JS `||` chains upstream keep the raw truthy
// value, so a numeric-string token count ("5") flows through as a string and
// only the `> 0` gates coerce numerically.
func BuildUsage(prompt, completion, total, cached, cacheCreation, reasoning any) map[string]any {
	usage := jsonx.ObjOf(
		"prompt_tokens", prompt,
		"completion_tokens", completion,
		"total_tokens", total,
	)
	if jsGT0(cached) || jsGT0(cacheCreation) {
		details := jsonx.ObjOf()
		if jsGT0(cached) {
			details["cached_tokens"] = cached
		}
		if jsGT0(cacheCreation) {
			details["cache_creation_tokens"] = cacheCreation
		}
		usage["prompt_tokens_details"] = details
	}
	if jsGT0(reasoning) {
		usage["completion_tokens_details"] = jsonx.ObjOf("reasoning_tokens", reasoning)
	}
	return usage
}

// ReasoningDelta is the vendor-neutral reasoning delta field. The value is
// embedded verbatim (concerns/reasoning.js:4-8 returns the argument as-is).
func ReasoningDelta(text any) map[string]any {
	return jsonx.ObjOf("reasoning_content", text)
}

// ExtractReasoningText reads a streamed delta's reasoning across vendor
// shapes: reasoning_content, reasoning, reasoning_details[].
func ExtractReasoningText(delta map[string]any) string {
	if delta == nil {
		return ""
	}
	if s, is := delta["reasoning_content"].(string); is && s != "" {
		return s
	}
	if s, is := delta["reasoning"].(string); is && s != "" {
		return s
	}
	if details, is := delta["reasoning_details"].([]any); is {
		var sb strings.Builder
		for _, d := range details {
			switch v := d.(type) {
			case string:
				sb.WriteString(v)
			case map[string]any:
				if s, is := v["text"].(string); is && s != "" {
					sb.WriteString(s)
					continue
				}
				if s, is := v["content"].(string); is && s != "" {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// FallbackToolCallID mirrors concerns/toolCall.js fallbackToolCallId().
func FallbackToolCallID() string {
	return fmt.Sprintf("call_%d", time.Now().UnixMilli())
}

// JSONStringifyStr serializes v as JSON when it isn't already a string.
func JSONStringifyStr(v any) string {
	if s, is := v.(string); is {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// jsonStringifyOf is JSON.stringify(v) for decoded JSON values. Go's encoder
// escapes <, >, & by default; JS does not — the encoder is configured to
// match JS output byte-for-byte for string payloads. Map keys marshal sorted
// while JS keeps insertion order — the documented repo-wide serialization
// divergence (AGENTS.md porting discipline #4).
func jsonStringifyOf(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// jsNumString renders a decoded JSON number the way JS ToString does for the
// values that reach coercion points: integral magnitudes below 1e21 print
// plain, larger (and fractional) ones use the shortest round-trip exponent
// form. (JS switches to exponent notation below 1e-6 as well; token counts
// and indices never live there.)
func jsNumString(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsStringOf is JS String(v) over decoded JSON values — the "buf += x" and
// template-literal coercion points ("fc_" + id, "msg_" + id + "_" + idx).
// Arrays join their elements with "," (null holes become ""); objects are
// "[object Object]".
func jsStringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumString(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if e == nil {
				parts = append(parts, "") // Array.join: null/undefined → ""
				continue
			}
			parts = append(parts, jsStringOf(e))
		}
		return strings.Join(parts, ",")
	case nil:
		return "null"
	default:
		return "[object Object]"
	}
}

// jsAdd is JS `a + b` over decoded JSON values (usage.js totalTokens math):
// a string/object/array operand makes it string concatenation (ToPrimitive),
// otherwise numeric addition (null → 0, bool → 1/0).
func jsAdd(a, b any) any {
	if jsConcatOperand(a) || jsConcatOperand(b) {
		return jsStringOf(a) + jsStringOf(b)
	}
	return jsonx.NumCoerce(a) + jsonx.NumCoerce(b)
}

func jsConcatOperand(v any) bool {
	switch v.(type) {
	case string, []any, map[string]any:
		return true
	}
	return false
}

// jsGT0 is JS `v > 0` — numeric comparison after ToNumber, so "80" counts as
// present while a non-numeric string/object (NaN) never does. NumCoerce only
// speaks decoded-JSON numbers, so Go int callers (the `|| 0` fallback literals)
// are unwrapped here first.
func jsGT0(v any) bool {
	switch t := v.(type) {
	case int:
		return t > 0
	case int64:
		return t > 0
	}
	return jsonx.NumCoerce(v) > 0
}

// mapKeyOf renders v as a Go map key with JS Map semantics (SameValueZero) —
// openai-responses.js:453 `state.respToolChatIndex ??= new Map()`: primitives
// stay distinct by type AND value (5 and "5" are different entries), with the
// type-tagged prefix keeping that distinction. Arrays/objects (unhashable in
// Go) key on their stable JSON; JS would give each reference its own entry —
// unreachable for real item ids, which are strings.
func mapKeyOf(v any) string {
	switch t := v.(type) {
	case string:
		return "s:" + t
	case float64:
		return "n:" + jsNumString(t)
	case int:
		return "n:" + strconv.Itoa(t)
	case bool:
		if t {
			return "b:true"
		}
		return "b:false"
	default:
		return "j:" + jsonStringifyOf(v)
	}
}

// jsIndex canonicalizes a chunk index onto the Go int key of the
// ChatToRespState maps the way JS plain-object property keys coerce
// (initState declares them `{}`, index.js:252-263): numbers and integer-like
// numeric strings share one integer slot ("5" and 5 index the same property).
// Degenerate non-numeric index values (true, "", objects) keep a distinct
// string property key in JS; Go folds them onto slot 0 — Chat Completions
// choices/tool_calls indices are integers per spec, so the fold is
// unreachable for conformant upstreams.
func jsIndex(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return 0
}
