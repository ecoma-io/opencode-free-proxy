// Package jsonx provides small helpers for working with arbitrary JSON
// request/response bodies as map[string]any — the Go equivalent of the
// mutate-in-place object handling the upstream 9router JS engine performs.
package jsonx

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// AsObj returns v as a JSON object, or nil when v is not one.
func AsObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// AsArr returns v as a JSON array, or nil when v is not one.
func AsArr(v any) []any {
	a, _ := v.([]any)
	return a
}

// AsStr returns v as a string, or "" when v is not a string.
func AsStr(v any) string {
	s, _ := v.(string)
	return s
}

// AsF64 returns v as a float64 (the encoding/json number type), or 0.
func AsF64(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Truthy reports JS truthiness for a decoded JSON value — the `||` operand
// gate (e.g. utils/error.js:80 `json.error?.message || json.message ||
// json.error || bodyText`). Falsy: nil (null), false, 0, "" — everything
// else is truthy, including empty objects/arrays, "0" and any other number.
// JSON numbers decode as float64.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	default:
		// Objects, arrays and any other decoded shape are truthy in JS.
		return true
	}
}

// jsDecimalRe is the decimal production of the JS Number(string) grammar
// (ECMA-262 StringNumericLiteral): optional sign, digits with optional
// fraction, optional exponent. StringNumericLiteral is always decimal —
// "0123" is 123, no legacy octal — exactly like strconv.ParseFloat.
var jsDecimalRe = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// jsNumberString ports the string arm of JS Number(s) (ECMA-262
// StringNumericLiteral): "" → 0, decimal literals, and the exact spellings
// Infinity/+Infinity/-Infinity. Go's ParseFloat additionally accepts "inf"/
// "nan" in any casing and "1_000" separators, which JS rejects — hence the
// grammar gate before it. Out-of-range literals keep ParseFloat's rounded
// value, which matches JS: "1e999" → +Inf, "1e-999" → 0. ok=false marks the
// strings JS turns into NaN.
func jsNumberString(s string) (float64, bool) {
	if s == "" {
		return 0, true // Number("") === 0
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if !jsDecimalRe.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Past the grammar gate the only possible error is ErrRange, where
		// ParseFloat still yields the rounded value Go shares with JS.
		if errors.Is(err, strconv.ErrRange) {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// NumCoerce mirrors JS `Number(v) || 0` (executors/opencode.js:362): numbers
// pass, bools map to 1/0, strings parse per the full-string Number()
// grammar — partial prefixes ("12abc"), separator forms ("1_000") and
// mis-spelled infinities ("inf") are NaN → 0 — and everything else (null,
// objects, arrays) yields 0. It never returns NaN (NaN||0 = 0); ±Inf escape
// only from an ±Inf input or the exact Infinity spellings, because Infinity
// is truthy and survives `|| 0`.
//
// Deliberate divergences: hex/binary/octal string literals, which JS coerces
// (Number("0x10")=16, unsigned only), yield 0 — not wire-realistic for the
// sole caller, a token cap; and JS's toString bridge for single-element
// arrays (Number([5])=5) is not replicated — a decoded JSON token cap is a
// number, string or null, never an array.
func NumCoerce(v any) float64 {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) {
			return 0 // Number(NaN) || 0
		}
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		// strings.TrimSpace ≈ the JS StrWhiteSpace trim; the sets differ only
		// on U+0085 (Go trims, JS doesn't) and U+FEFF (JS trims, Go doesn't) —
		// neither occurs in a wire field.
		if n, ok := jsNumberString(strings.TrimSpace(t)); ok {
			return n
		}
		return 0 // Number(garbage) is NaN → NaN || 0
	default:
		return 0
	}
}

// Get returns m[key] when m is a JSON object.
func Get(m any, key string) any {
	if o := AsObj(m); o != nil {
		return o[key]
	}
	return nil
}

// Set sets m[key]=v when m is a JSON object.
func Set(m any, key string, v any) {
	if o := AsObj(m); o != nil {
		o[key] = v
	}
}

// Delete removes keys from m when m is a JSON object.
func Delete(m any, keys ...string) {
	if o := AsObj(m); o != nil {
		for _, k := range keys {
			delete(o, k)
		}
	}
}

// Has reports whether m is an object containing key.
func Has(m any, key string) bool {
	o := AsObj(m)
	if o == nil {
		return false
	}
	_, ok := o[key]
	return ok
}

// Clone deep-copies v through a JSON round-trip. Any non-marshalable value
// (channels, funcs) makes the whole clone nil. Deliberate divergence from
// JSON.stringify, which silently drops such properties and still stringifies
// the rest — callers here only Clone already-decoded JSON, where the
// marshal-error path cannot fire.
func Clone(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// ObjOf builds {"k": v, ...} preserving call order.
func ObjOf(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// ArrOf builds an array preserving call order.
func ArrOf(items ...any) []any {
	if items == nil {
		return []any{}
	}
	return items
}
