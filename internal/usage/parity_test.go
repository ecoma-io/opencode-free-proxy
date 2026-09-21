package usage

// JS-semantics parity tests for the Number()/String() coercions this package
// owns (NumOK, JSStr, JSNumStr, UTF16Len) and the object/array typing rules of
// Normalize/Merge/EstimateInput. Table entries marked "node 24" were verified
// against the JS engine directly.

import (
	"math"
	"reflect"
	"testing"
)

// NumOK's string arm must follow ECMA's StringToNumber exactly — every table
// entry below was verified against node 24. Go's strconv alone disagrees on
// several: it accepts "1_0", ParseFloat reads "0x1p2" as a hex float (4), and
// base-0 integers treat "077" as octal (63).
func TestNumOKStringGrammar(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{"0x10", 16, true},
		{"0X1F", 31, true},
		{"0b101", 5, true},
		{"0o17", 15, true},
		{"-0x10", 0, false}, // signed radix literals are NaN
		{"+0x10", 0, false},
		{"0x", 0, false},    // bare prefix
		{"0x1p2", 0, false}, // hex FLOAT is NaN (ParseFloat would say 4)
		{"077", 77, true},   // leading zero stays decimal (not octal 63)
		{"077.5", 77.5, true},
		{"1_0", 0, false},   // separators are literal-only
		{"0x1_0", 0, false}, // ...even inside radix literals
		{" 12 ", 12, true},  // surrounding whitespace trims
		{"5.e3", 5000, true},
		{".5", 0.5, true},
		{"+.5", 0.5, true},
		{"5.", 5, true},
		{"1e", 0, false},
		{"e3", 0, false},
		{"1e-999", 0, true}, // underflow is the finite 0
		{"1e309", 0, false}, // overflow is Infinity (not finite)
		{"Infinity", 0, false},
		{"infinity", 0, false},
		{"", 0, true}, // Number("") === 0
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := NumOK(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("NumOK(%#v) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// Number([x]) === Number(String([x])) — arrays join, null elements render
// empty, nested arrays flatten, and the joined string re-enters the string
// grammar (["0x10"] really is 16, [true] is NaN via "true").
func TestNumOKArrayCoercion(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{[]any{}, 0, true},
		{[]any{5.0}, 5, true},
		{[]any{"5"}, 5, true},
		{[]any{"0x10"}, 16, true},
		{[]any{nil}, 0, true},     // [null].join() is ""
		{[]any{[]any{}}, 0, true}, // [[]].join() is ""
		{[]any{1.0, 2.0}, 0, false},
		{[]any{true}, 0, false},
		{[]any{""}, 0, true},
		{[]any{map[string]any{}}, 0, false}, // "[object Object]"
	}
	for _, c := range cases {
		got, ok := NumOK(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("NumOK(%#v) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNumOKNonStringShapes(t *testing.T) {
	if f, ok := NumOK(true); !ok || f != 1 {
		t.Errorf("NumOK(true) = %v,%v", f, ok)
	}
	if f, ok := NumOK(false); !ok || f != 0 {
		t.Errorf("NumOK(false) = %v,%v", f, ok)
	}
	if _, ok := NumOK(nil); ok {
		t.Error("NumOK(nil) must not be finite")
	}
	if _, ok := NumOK(map[string]any{}); ok {
		t.Error("NumOK({}) must not be finite")
	}
	if _, ok := NumOK(math.NaN()); ok {
		t.Error("NaN is not finite")
	}
	if _, ok := NumOK(math.Inf(1)); ok {
		t.Error("Inf is not finite")
	}
}

// JS String(n) formatting, used by JSStr wherever JS concatenates a possibly
// numeric value into a string (tool-call name += fragment, template literals).
func TestJSNumStr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"}, // String(-0)
		{5, "5"},
		{-5, "-5"},
		{0.5, "0.5"},
		{1e-6, "0.000001"}, // boundary stays decimal
		{1e-7, "1e-7"},     // Go 'g' would say 1e-07
		{1.5e-7, "1.5e-7"}, //
		{1e21, "1e+21"},    // first exponent form
		{1.2345678901234568e20, "123456789012345680000"}, // < 1e21: every digit
	}
	for _, c := range cases {
		if got := JSNumStr(c.in); got != c.want {
			t.Errorf("JSNumStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "null"},
		{"x", "x"},
		{true, "true"},
		{false, "false"},
		{7.0, "7"},
		{[]any{1.0, "a"}, "1,a"},
		{[]any{nil, "a"}, ",a"}, // null joins empty
		{[]any{[]any{2.0}}, "2"},
		{map[string]any{}, "[object Object]"},
	}
	for _, c := range cases {
		if got := JSStr(c.in); got != c.want {
			t.Errorf("JSStr(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// normalizeUsage keeps array-shaped details: `typeof [] === "object"`
// (usageTracking.js:270-275).
func TestNormalizeKeepsArrayDetails(t *testing.T) {
	got := Normalize(map[string]any{
		"prompt_tokens":         5.0,
		"prompt_tokens_details": []any{1.0, nil},
	})
	want := map[string]any{
		"prompt_tokens":         5.0,
		"prompt_tokens_details": []any{1.0, nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize = %#v, want %#v", got, want)
	}
	if Normalize(map[string]any{"prompt_tokens_details": "x"}) != nil {
		t.Fatal("a scalar details value must still drop")
	}
}

// mergeUsage replaces arrays like objects — `v && typeof v === "object"`
// (usageTracking.js:462-466).
func TestMergeReplacesArrayValues(t *testing.T) {
	prev := map[string]any{
		"prompt_tokens":         5.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 2.0},
	}
	next := map[string]any{"prompt_tokens_details": []any{"vendor"}}
	got := Merge(prev, next)
	arr, isArr := got["prompt_tokens_details"].([]any)
	if !isArr || len(arr) != 1 || arr[0] != "vendor" {
		t.Fatalf("array value must replace the object: %#v", got)
	}
	if got["prompt_tokens"].(float64) != 5 {
		t.Fatalf("unrelated numeric field must survive: %#v", got)
	}
}

// EstimateInput must reproduce `Math.ceil(JSON.stringify(body).length / 4)`
// (usageTracking.js:476-481): UTF-16 units, <, >, & left raw, and 0 for
// anything `typeof !== "object"` — arrays included, scalars and null excluded.
func TestEstimateInputUtf16UnitsAndHTMLEscapes(t *testing.T) {
	cases := []struct {
		name string
		body any
		want float64
	}{
		// {"模型":"😀"} — 11 UTF-16 units (7 ASCII + 2 CJK + 2 emoji units).
		{"multibyte counts units not bytes", map[string]any{"模型": "😀"}, 3},
		// {"a":"<&>"} — stringify leaves <, >, & raw: 12 units.
		{"html chars stay raw", map[string]any{"a": "<&>"}, 3},
		// "[]" is 2 units — arrays are typeof "object" and DO stringify.
		{"empty array stringifies", []any{}, 1},
		{"string body", "😀", 0},
		{"number body", 42.0, 0},
		{"null body", nil, 0},
	}
	for _, c := range cases {
		if got := EstimateInput(c.body); got != c.want {
			t.Errorf("[%s] EstimateInput(%#v) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// UTF16Len counts what JS `str.length` reports over JSON.stringify output:
// astral runes are surrogate pairs (2 units), escapes are consumed atomically
// whatever spelling Go chose, and malformed tails degrade per character.
func TestUTF16Len(t *testing.T) {
	const bs = "\x5c" // backslash, transport-safe spelling
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"模型", 2},                        // two BMP runes
		{"😀", 2},                         // astral rune = surrogate pair
		{bs + `"`, 1},                    // escaped quote
		{bs + bs, 1},                     // escaped backslash
		{bs + "n", 1},                    // short escape
		{bs + "b", 1},                    // Go's six-char spelling still 1 unit
		{bs + "f", 1},                    //
		{bs + "u00e9", 1},                // BMP escape
		{bs + "ud83d" + bs + "ude00", 2}, // surrogate pair escapes = one JS character
		{bs + "ud800", 1},                // lone high surrogate
		{bs + "ud800x", 2},               // unpaired, then a plain char
		{bs + "udc00", 1},                // lone low surrogate
		{bs + "u00e", 5},                 // truncated escape: backslash + "u00e" raw
		{bs, 1},                          // dangling backslash counts itself
		{string(rune(0x2028)), 1},        // raw line separator (Go escape spelling also 1)
		{"\xff", 1},                      // invalid byte - one unit
	}
	for _, c := range cases {
		if got := UTF16Len(c.in); got != c.want {
			t.Errorf("UTF16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
