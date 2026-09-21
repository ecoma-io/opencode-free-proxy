// Package usage ports open-sse/utils/usageTracking.js for the two wire
// families this proxy speaks (OpenAI chat + Responses). Token accounting
// conventions here feed both client-facing usage blocks and the usage seam;
// keep them bit-identical to the JS engine.
package usage

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"opencode-free-proxy/internal/jsonx"
)

const bufferTokens = 2000

// SynthMaxOutput and DefaultRatio gate hidden-thinking synthesis: completions
// at or below the threshold are assumed reasoning-free; above it, a share of
// output tokens is attributed to thinking.
const (
	SynthMaxOutput = 10
	DefaultRatio   = 0.75
)

// NumOK mirrors `Number.isFinite(Number(v))` over decoded-JSON values: the
// numeric value plus whether JS would consider it finite. Non-numeric strings,
// null and objects are not finite (normalizeUsage's assignNumber drops them);
// the empty string, false and [] coerce to 0.
//
// The string arm follows ECMA's StringToNumber grammar, which Go's
// strconv.ParseFloat approximates but does not match (all verified against
// node 24):
//   - numeric separators are NOT accepted in strings (Number("1_0") and
//     Number("0x1_0") are both NaN, despite literals allowing them) — rejected
//     up front;
//   - hex/octal/binary LITERALS ("0x10"/"0X1F"/"0o17"/"0b101") parse with the
//     prefix intact and NO sign (Number("-0x10") is NaN), so they route
//     through ParseUint base 0 — never ParseFloat, which reads "0x1p2" as 4
//     while Number("0x1p2") is NaN;
//   - a leading-zero DECIMAL is not octal (Number("077") is 77, not 63);
//   - underflow is 0 and finite (Number("1e-999") === 0): Go reports ErrRange
//     with the value 0, which is kept;
//   - overflow ("1e309" → Infinity) parses but is dropped as non-finite.
func NumOK(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, true // Number("") === 0
		}
		if strings.ContainsRune(s, '_') {
			return 0, false // Number("1_0") is NaN — separators are literal-only
		}
		if len(s) >= 2 && s[0] == '0' {
			switch s[1] {
			case 'x', 'X', 'b', 'B', 'o', 'O': // radix literal, sign not allowed
				u, err := strconv.ParseUint(s, 0, 64)
				if err != nil {
					return 0, false
				}
				return float64(u), true
			}
		}
		f, err := strconv.ParseFloat(s, 64) // full-string parse, like Number()
		if err != nil {
			if errors.Is(err, strconv.ErrRange) && f == 0 {
				return 0, true // underflow: Number("1e-999") === 0, finite
			}
			return 0, false
		}
		if math.IsInf(f, 0) {
			return 0, false // "1e309" → Infinity — parseable but not finite
		}
		return f, true
	case map[string]any:
		return 0, false // Number({}) is NaN
	case []any:
		// Number([x]) === Number(String([x])) — Array.prototype.toString is
		// join(","), with null/undefined elements rendering as "".
		return NumOK(arrayJoin(n))
	default:
		return 0, false
	}
}

// arrayJoin renders an array the way JS string coercion sees it:
// Array.prototype.toString → join(","), null/undefined elements as "".
func arrayJoin(a []any) string {
	parts := make([]string, len(a))
	for i, e := range a {
		if e == nil {
			parts[i] = ""
			continue
		}
		parts[i] = JSStr(e)
	}
	return strings.Join(parts, ",")
}

// JSStr is JS String() coercion over decoded-JSON values, used wherever JS
// concatenates a possibly non-string into a string (`a += b`, template
// literals). Numbers use JS's shortest round-trip formatting; arrays join
// recursively; objects render "[object Object]".
func JSStr(v any) string {
	switch n := v.(type) {
	case nil:
		return "null"
	case string:
		return n
	case bool:
		if n {
			return "true"
		}
		return "false"
	case float64:
		return JSNumStr(n)
	case []any:
		return arrayJoin(n)
	default:
		return "[object Object]"
	}
}

// JSNumStr renders a float64 the way JS String(n) does: plain decimal
// notation for |n| in [1e-6, 1e21) (integral values print every digit,
// String(123456789012345680000) has no exponent), exponent form outside that
// range, and -0 renders "0". Go's exponent spelling carries a leading zero
// ("1e-07") where JS does not ("1e-7") — normalized here.
func JSNumStr(f float64) string {
	if f == 0 {
		return "0" // String(-0) === "0"
	}
	a := math.Abs(f)
	s := ""
	if a >= 1e-6 && a < 1e21 {
		s = strconv.FormatFloat(f, 'f', -1, 64)
	} else {
		s = strconv.FormatFloat(f, 'g', -1, 64)
	}
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := "+"
		if exp[0] == '+' || exp[0] == '-' {
			sign, exp = string(exp[0]), exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		s = mant + "e" + sign + exp
	}
	return s
}

// num coerces any value to a number, mapping everything non-finite to 0 —
// canonicalizeUsage's local `Number.isFinite(Number(v)) ? Number(v) : 0`
// idiom, used on arithmetic paths.
func num(v any) float64 {
	f, _ := NumOK(v)
	return f
}

// jsTruthy mirrors JS truthiness over decoded-JSON values: null, false, 0,
// NaN and "" are falsy; every object/array (even empty) is truthy.
func jsTruthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case float64:
		return n != 0 && !math.IsNaN(n)
	case string:
		return n != ""
	default:
		return true
	}
}

// jsOr mirrors JS `a || b || c`: the first truthy value wins, else the LAST
// operand's value — even when it is falsy (`undefined || 0` is 0, not
// undefined).
func jsOr(vals ...any) any {
	for _, v := range vals {
		if jsTruthy(v) {
			return v
		}
	}
	if len(vals) > 0 {
		return vals[len(vals)-1]
	}
	return nil
}

// firstNonNull returns the first argument that is not nil — the JS `??`
// nullish chain over already-read values (an absent key and JSON null are both
// nullish; a present 0 is not).
func firstNonNull(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

// Normalize keeps only known numeric fields plus nested details objects, the
// way normalizeUsage's assignNumber does: absent/null fields are omitted (not
// zeroed) and values Number cannot render finite drop entirely, while numeric
// strings coerce (Number("7") === 7). Nil when nothing numeric survives
// (normalizeUsage returns null for an empty object).
func Normalize(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{
		"prompt_tokens", "completion_tokens", "total_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"cached_tokens", "reasoning_tokens",
	} {
		if v := u[k]; v != nil {
			if f, ok := NumOK(v); ok {
				out[k] = f
			}
		}
	}
	for _, k := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		switch d := u[k].(type) {
		case map[string]any:
			out[k] = d
		case []any:
			// typeof [] === "object" — an array-shaped details block passes
			// normalizeUsage's `usage?.prompt_tokens_details && typeof ===
			// "object"` check verbatim (usageTracking.js:270-275).
			out[k] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Canonical folds cache into prompt exactly once:
//
//	prompt_tokens = total input INCLUDING cache read + cache creation
//	cached_tokens = cache-read subset; cache_creation_input_tokens = write subset
//
// Claude-style reports (cache_read_input_tokens present, cached_tokens absent)
// fold; OpenAI-style (cached_tokens or nested details) pass through.
// Idempotent: the folded output always carries cached_tokens. Mirrors
// canonicalizeUsage key-for-key: the five canonical counts are ALWAYS present
// (0 included); only reasoning_tokens is conditional (> 0). The fallback
// chains are nullish (??), not zeroish: a present 0 beats an absent sibling,
// and the nested prompt_tokens_details shapes are only consulted when the
// top-level key is absent/null.
func Canonical(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	completion := num(firstNonNull(u["completion_tokens"], u["output_tokens"]))
	reasoning := num(u["reasoning_tokens"])
	cacheCreation := num(firstNonNull(u["cache_creation_input_tokens"],
		jsonx.Get(u["prompt_tokens_details"], "cache_creation_tokens")))

	prompt := num(firstNonNull(u["prompt_tokens"], u["input_tokens"]))
	var cached float64
	_, hasCached := u["cached_tokens"]
	if !hasCached && (has(u, "cache_read_input_tokens") || has(u, "cache_creation_input_tokens")) {
		cached = num(u["cache_read_input_tokens"])
		prompt = prompt + cached + cacheCreation
	} else {
		cached = num(firstNonNull(u["cached_tokens"],
			jsonx.Get(u["prompt_tokens_details"], "cached_tokens")))
	}

	result := map[string]any{
		"prompt_tokens":               prompt,
		"completion_tokens":           completion,
		"total_tokens":                prompt + completion,
		"cached_tokens":               cached,
		"cache_creation_input_tokens": cacheCreation,
	}
	if reasoning > 0 {
		result["reasoning_tokens"] = reasoning
	}
	return result
}

// Merge field-wise maxes two usage maps (Anthropic-style split events; a
// no-op for single complete usage objects). Nested objects: latest wins.
// mergeUsage only Math.maxes when the previous value is itself a number
// (typeof check) and only replaces objects — strings and scalars never cross.
func Merge(prev, next map[string]any) map[string]any {
	if prev == nil {
		return next
	}
	if next == nil {
		return prev
	}
	merged := map[string]any{}
	for k, v := range prev {
		merged[k] = v
	}
	for k, v := range next {
		switch n := v.(type) {
		case float64:
			// typeof NaN === "number" — guard with Number.isFinite so one
			// malformed chunk can't poison the accumulation (the JS comment's
			// own rationale; Math.max(x, NaN) is NaN on both sides).
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				cur, _ := merged[k].(float64) // non-number prev bases at 0
				merged[k] = math.Max(cur, n)
			}
		default:
			// `v && typeof v === "object"` — objects AND arrays replace
			// (typeof [] === "object", arrays always truthy,
			// usageTracking.js:462-466); strings and other scalars never cross.
			if o := jsonx.AsObj(v); o != nil {
				merged[k] = o
			} else if a, isArr := v.([]any); isArr {
				merged[k] = a
			}
		}
	}
	return merged
}

// HasValid reports whether any known token field holds a real number > 0.
// hasValidUsage requires `typeof usage[field] === "number"` — numeric strings
// never validate.
func HasValid(u map[string]any) bool {
	if u == nil {
		return false
	}
	for _, k := range []string{
		"prompt_tokens", "completion_tokens", "total_tokens",
		"input_tokens", "output_tokens",
		"promptTokenCount", "candidatesTokenCount",
	} {
		if n, ok := u[k].(float64); ok && n > 0 {
			return true
		}
	}
	return false
}

// ExtractFromChat reads usage out of a Chat Completions chunk — extractUsage's
// OpenAI branch, which also covers DeepSeek's prompt_cache_hit_tokens. The
// candidate object funnels through Normalize exactly as the JS hands its
// literal to normalizeUsage: prompt_tokens forwards raw, the details objects
// forward RAW (never synthesized — a DeepSeek hit-only stream yields a bare
// cached_tokens and no prompt_tokens_details).
func ExtractFromChat(chunk map[string]any) map[string]any {
	u := jsonx.AsObj(chunk["usage"])
	if u == nil {
		return nil
	}
	if _, has := u["prompt_tokens"]; !has {
		return nil
	}
	return Normalize(map[string]any{
		"prompt_tokens":             u["prompt_tokens"],                // raw: a null drops the key
		"completion_tokens":         jsOr(u["completion_tokens"], 0.0), // `completion_tokens || 0`
		"cached_tokens":             jsOr(jsonx.Get(u["prompt_tokens_details"], "cached_tokens"), u["prompt_cache_hit_tokens"]),
		"reasoning_tokens":          jsonx.Get(u["completion_tokens_details"], "reasoning_tokens"),
		"prompt_tokens_details":     u["prompt_tokens_details"],
		"completion_tokens_details": u["completion_tokens_details"],
	})
}

// ExtractFromResponses reads usage out of a response.completed / response.done
// event — extractUsage's Responses branch. The `||` chains fall back to the
// chat-shaped field names, and the result funnels through Normalize:
// cached_tokens/reasoning_tokens keep an explicit 0 (finite after Number),
// while the synthesized prompt_tokens_details only appears when cached_tokens
// is truthy (`cachedTokens ? { cached_tokens: cachedTokens } : undefined`).
// JS never forwards input_tokens_details/output_tokens_details here.
func ExtractFromResponses(chunk map[string]any) map[string]any {
	t := jsonx.AsStr(chunk["type"])
	if t != "response.completed" && t != "response.done" {
		return nil
	}
	u := jsonx.AsObj(jsonx.Get(chunk["response"], "usage"))
	if u == nil {
		return nil
	}
	cached := jsonx.Get(u["input_tokens_details"], "cached_tokens")
	var ptd any
	if jsTruthy(cached) {
		ptd = jsonx.ObjOf("cached_tokens", cached) // raw, un-coerced value
	}
	return Normalize(map[string]any{
		"prompt_tokens":         jsOr(u["input_tokens"], u["prompt_tokens"], 0.0),
		"completion_tokens":     jsOr(u["output_tokens"], u["completion_tokens"], 0.0),
		"cached_tokens":         cached,
		"reasoning_tokens":      jsonx.Get(u["output_tokens_details"], "reasoning_tokens"),
		"prompt_tokens_details": ptd,
	})
}

// AddBuffer inflates usage by bufferTokens — context-error headroom on
// estimated usage. addBufferToUsage buffers BOTH input_tokens (Claude shape)
// and prompt_tokens (OpenAI shape), then buffers total_tokens or computes it
// from the already-buffered prompt + completion.
//
// Divergence note: the JS `+=` on a string field would string-concatenate
// ("10" + 2000 → "102000") — unreachable here, because every caller passes a
// Normalize/Estimate product whose counts are already numbers (assignNumber
// drops non-numbers; Estimate writes float64s), so `num()` coercing to 0 for
// a non-number is wire-identical on every reachable path.
func AddBuffer(u map[string]any) map[string]any {
	if u == nil {
		return u
	}
	out := map[string]any{}
	for k, v := range u {
		out[k] = v
	}
	if _, has := out["input_tokens"]; has { // Claude format
		out["input_tokens"] = num(out["input_tokens"]) + bufferTokens
	}
	if _, has := out["prompt_tokens"]; has { // OpenAI format
		out["prompt_tokens"] = num(out["prompt_tokens"]) + bufferTokens
	}
	if _, has := out["total_tokens"]; has {
		out["total_tokens"] = num(out["total_tokens"]) + bufferTokens
	} else if _, hasP := out["prompt_tokens"]; hasP {
		if _, hasC := out["completion_tokens"]; hasC {
			out["total_tokens"] = num(out["prompt_tokens"]) + num(out["completion_tokens"])
		}
	}
	return out
}

// EstimateInput sums the whole request body /4 (~chars per token).
// JS: `JSON.stringify(body).length / 4` (usageTracking.js:480-481) — .length
// counts UTF-16 CODE UNITS and stringify leaves <, >, & raw, so the count is
// unit-aware, not byte-aware. Remaining byte-level divergences (each
// sub-token, both spellings decode to the same units): Go still escapes
// U+2028/U+2029 where JS emits the raw
// character, and spells the short-escaped controls (backspace, form feed)
// as six-char unicode escapes where JS uses two-char forms — UTF16Len
// consumes escapes atomically, so every spelling counts as the one unit the
// decoded character occupies in JS.
//
// Guard: estimateInputTokens returns 0 for `!body || typeof body !==
// "object"` (usageTracking.js:476) — scalars and null never stringify.
func EstimateInput(body any) float64 {
	switch body.(type) {
	case map[string]any, []any: // typeof "object" (arrays included)
	default:
		return 0
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false) // JS stringify leaves <, >, & raw
	if err := enc.Encode(body); err != nil {
		return 0
	}
	s := strings.TrimSuffix(sb.String(), "\n") // Encoder appends a newline
	return math.Ceil(float64(UTF16Len(s)) / 4)
}

// UTF16Len counts the UTF-16 code units a JSON-serialized string occupies
// after unescaping — what JS `str.length` reports on JSON.stringify output.
// Escapes are consumed atomically: a six-char unicode escape for U+2028
// (Go's spelling of the one character JS emits raw) counts 1 unit, an
// astral-plane emoji escape pair counts 2, and a literal double backslash
// can never masquerade as an escape start.
func UTF16Len(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			switch c := s[i+1]; c {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				n++
				i += 2
				continue
			case 'u':
				if cp, ok := hex4(s[i+2:]); ok {
					if cp >= 0xD800 && cp < 0xDC00 && i+6 < len(s) &&
						s[i+6] == '\\' && i+7 < len(s) && s[i+7] == 'u' {
						if lo, ok2 := hex4(s[i+8:]); ok2 && lo >= 0xDC00 && lo <= 0xDFFF {
							n += 2 // surrogate pair = one JS "character", 2 units
							i += 12
							continue
						}
					}
					n++ // lone escape or unpaired surrogate: 1 unit
					i += 6
					continue
				}
			}
			n++ // malformed escape tail — count the backslash itself
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			n++ // invalid byte — one unit, keep scanning
			i++
			continue
		}
		if r > 0xFFFF {
			n += 2 // astral plane rune = surrogate pair in UTF-16
		} else {
			n++
		}
		i += size
	}
	return n
}

// hex4 parses four hex digits into a code unit value.
func hex4(s string) (int, bool) {
	if len(s) < 4 {
		return 0, false
	}
	v := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | int(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// EstimateOutput is content length /4 with a one-token floor —
// estimateOutputTokens returns Math.max(1, Math.floor(contentLength / 4)),
// so even a 1-3 char completion still counts 1 output token.
func EstimateOutput(contentLen int) float64 {
	if contentLen <= 0 {
		return 0
	}
	return math.Max(1, math.Floor(float64(contentLen)/4))
}

// Estimate builds estimated usage for the OpenAI chat shape with buffer
// (estimateUsage -> formatUsage's default branch -> addBufferToUsage).
func Estimate(body any, contentLen int) map[string]any {
	in := EstimateInput(body)
	out := EstimateOutput(contentLen)
	u := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
		"estimated":         true,
	}
	return AddBuffer(u)
}

// EstimateResponses is estimateUsage for a Responses-format client. JS composes
// it in the canonical OpenAI shape — formatUsage's default branch, buffered by
// addBufferToUsage on prompt_tokens AND total_tokens — and then renames it
// with convertUsageForFormat, whose Responses family reads
// input_tokens ?? prompt_tokens and keeps only input/output tokens + estimated
// (total_tokens dropped). Net: input_tokens = raw + bufferTokens.
func EstimateResponses(body any, contentLen int) map[string]any {
	buffered := Estimate(body, contentLen)
	return map[string]any{
		"input_tokens":  num(buffered["prompt_tokens"]), // input_tokens ?? prompt_tokens
		"output_tokens": num(buffered["completion_tokens"]),
		"estimated":     true,
	}
}

// SynthesizeThinking fills reasoning tokens when upstream thinks silently —
// synthesizeThinkingTokens. completion is read through the nullish chain
// completion_tokens ?? output_tokens ?? candidatesTokenCount (a present 0
// short-circuits, and completion <= 0 leaves usage untouched); usage that
// already reports reasoning (reasoning_tokens ?? completion_tokens_details
// .reasoning_tokens ?? output_tokens_details.reasoning_tokens ??
// thoughtsTokenCount, then || 0) passes through. completion <= 10 → 0; else
// floor(ratio × completion); only a NON-FINITE ratio falls back to
// DefaultRatio (JS checks Number.isFinite — no sign check). target "responses"
// writes output_tokens_details.reasoning_tokens, "chat" (the JS default
// branch) writes completion_tokens_details.reasoning_tokens; the Claude/Gemini
// family outputs are outside this port's two-family scope. Every level of the
// result is a new object (JS spreads), so the stats side is never mutated.
func SynthesizeThinking(u map[string]any, target string, ratio float64) map[string]any {
	if u == nil {
		return u
	}
	completion := num(firstNonNull(
		u["completion_tokens"],
		u["output_tokens"],
		u["candidatesTokenCount"],
	))
	if completion <= 0 {
		return u
	}
	reported := num(firstNonNull(
		u["reasoning_tokens"],
		jsonx.Get(u["completion_tokens_details"], "reasoning_tokens"),
		jsonx.Get(u["output_tokens_details"], "reasoning_tokens"),
		u["thoughtsTokenCount"],
	))
	if reported > 0 {
		return u
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		ratio = DefaultRatio
	}
	synthesized := 0.0
	if completion > SynthMaxOutput {
		synthesized = math.Floor(completion * ratio)
	}
	out := map[string]any{}
	for k, v := range u {
		out[k] = v
	}
	key := "completion_tokens_details"
	if target == "responses" {
		key = "output_tokens_details"
	}
	src := jsonx.AsObj(u[key])
	details := make(map[string]any, len(src)+1)
	for k, v := range src { // JS spread: new object, not in-place mutation
		details[k] = v
	}
	details["reasoning_tokens"] = synthesized
	out[key] = details
	return out
}
