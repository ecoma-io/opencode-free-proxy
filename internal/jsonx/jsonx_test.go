package jsonx

// Pins the jsonx helpers against JS semantics (ECMA-262 ToNumber/ToBoolean
// and the 9router mutate-in-place object idiom). Every Number() case cites
// its grammar or the parity site (executors/opencode.js:362).

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// decode mirrors a wire value arriving through encoding/json.
func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func TestNumCoerce(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want float64
	}{
		// Numbers pass through. -0 passes as -0 where JS `Number(-0)||0`
		// would normalize to +0 (falsy operand) — unobservable at the sole
		// call site, where the <16 floor catches both zeros.
		{"integer", float64(42), 42},
		{"fraction", 1.5, 1.5},
		{"negative", -7.25, -7.25},
		{"exponential", 1e3, 1000},
		{"zero", float64(0), 0},
		{"negative zero", math.Copysign(0, -1), 0},

		// Bools map to 1/0 (ToNumber).
		{"true", true, 1},
		{"false", false, 0},

		// Canonical decimal strings — the token-cap wires arrive through
		// encoding/json, so a numeric string is always in decimal form.
		{"plain string", "1024", 1024},
		{"plus sign", "+1024", 1024},
		{"minus sign", "-1024", -1024},
		{"leading zero", "0123", 123}, // StringNumericLiteral: decimal, not octal
		{"integer dot", "5.", 5},
		{"leading dot", ".5", 0.5},
		{"plus leading dot", "+.5", 0.5},
		{"negative dot", "-.5", -0.5},
		{"exponent", "1.5e3", 1500},
		{"plus exponent", "2E+3", 2000},
		{"minus exponent", "5e-2", 0.05},
		{"signed-exponent form", "1e+5", 100000},
		{"whitespace", " 1024 ", 1024}, // JS StrWhiteSpace trim
		{"empty", "", 0},               // Number("") === 0

		// Out-of-range literals round like JS: Number("1e999") is Infinity and
		// Number("1e-999") is 0; both survive the `|| 0` coercion as-is.
		{"overflow to infinity", "1e999", math.Inf(1)},
		{"underflow to zero", "1e-999", 0},

		// Garbage → NaN → NaN||0 = 0.
		{"partial prefix", "12abc", 0},       // Number("12abc") is NaN; Sscanf %g used to read 12
		{"trailing junk", "50abc", 0},        // Number("50abc") is NaN; Sscanf %g used to read 50
		{"underscore separator", "1_000", 0}, // numeric separators are literal-only syntax, never Number()
		{"garbage", "abc", 0},
		{"nan word", "NaN", 0},      // Number("NaN") is NaN → 0
		{"nan lowercase", "nan", 0}, // Go ParseFloat accepts "nan"; JS does not
		{"infinity embedded in words", "Infinity word", 0},
		{"mis-cased infinity", "infinity", 0}, // Go ParseFloat accepts any casing; JS does not
		{"abbreviated infinity", "inf", 0},    // Go ParseFloat knows "inf"; JS does not
		{"all caps infinity", "INF", 0},

		// Exact Infinity spellings are the JS StringNumericLiterals. Infinity
		// is truthy and survives `Number(x) || 0`.
		{"positive infinity", "Infinity", math.Inf(1)},
		{"explicit plus infinity", "+Infinity", math.Inf(1)},
		{"negative infinity", "-Infinity", math.Inf(-1)},

		// Deliberate divergence: JS coerces hex/binary/octal string literals
		// (Number("0x10")=16) but this helper does not — a decoded JSON token
		// cap is a number, string or null, never a hex literal (see
		// NumCoerce). Signed hex is NaN even in JS.
		{"hex string", "0x10", 0},
		{"signed hex string", "-0x10", 0},

		// Number([5]) is 5 via String(5) → 5; the 9router token cap is never
		// a decoded array, so the toString bridge is intentionally dropped.
		{"array", []any{float64(5)}, 0},
		{"object", map[string]any{"n": float64(5)}, 0},
		{"null is zero", nil, 0}, // Number(null) === 0
		{"JSON null is zero", decode(t, `null`), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NumCoerce(tc.v)
			if got != tc.want && !(math.IsNaN(got) && math.IsNaN(tc.want)) {
				t.Fatalf("NumCoerce(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestNumCoerceNeverNaN(t *testing.T) {
	// `Number(x) || 0` — NaN is falsy, so the helper as a whole never leaks
	// NaN regardless of the input shape.
	for _, v := range []any{"NaN", decode(t, `"NaN"`), decode(t, `"12abc"`), 1.5, "16", nil} {
		if got := NumCoerce(v); math.IsNaN(got) {
			t.Fatalf("NumCoerce(%#v) = NaN, want a coercible finite value", v)
		}
	}
}

func TestAsF64(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want float64
	}{
		{"number passes", float64(3.5), 3.5},
		{"int is not float64", int64(3), 0},
		{"zero", float64(0), 0},
		{"string is zero", "3", 0},
		{"nil is zero", nil, 0},
		{"json number", decode(t, `7`), 7},
		{"json string is zero", decode(t, `"7"`), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AsF64(tc.v); got != tc.want {
				t.Fatalf("AsF64(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestAsObjAsArrAsStr(t *testing.T) {
	// Type-guard families: wrong shape → the zero value, never a panic.
	m := map[string]any{"k": "v"}
	if got := AsObj(m); !reflect.DeepEqual(got, m) {
		t.Fatalf("AsObj(map) = %#v, want the map", got)
	}
	if got := AsObj([]any{float64(1)}); got != nil {
		t.Fatalf("AsObj(array) = %#v, want nil", got)
	}
	if got := AsObj(nil); got != nil {
		t.Fatalf("AsObj(nil) = %#v, want nil", got)
	}
	if got := AsArr([]any{float64(1)}); !reflect.DeepEqual(got, []any{float64(1)}) {
		t.Fatalf("AsArr(array) = %#v, want the array", got)
	}
	if got := AsArr(m); got != nil {
		t.Fatalf("AsArr(map) = %#v, want nil", got)
	}
	if got := AsStr("x"); got != "x" {
		t.Fatalf("AsStr(string) = %q, want the string", got)
	}
	if got := AsStr(float64(1)); got != "" {
		t.Fatalf("AsStr(number) = %q, want empty", got)
	}
}

func TestClone(t *testing.T) {
	t.Run("deep copy round trip", func(t *testing.T) {
		in := decode(t, `{"a":[1,{"b":"c"}],"d":null,"e":false}`)
		out := Clone(in)
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("Clone(%#v) = %#v, want an equal deep copy", in, out)
		}
		// Mutating the copy must not touch the source.
		if o := AsObj(out); o != nil {
			o["a"] = "clobbered"
		}
		if reflect.DeepEqual(out, in) {
			t.Fatal("Clone shared state: mutating the copy changed the source")
		}
	})

	t.Run("scalar passthrough", func(t *testing.T) {
		for _, v := range []any{"x", float64(3), true, nil, []any{float64(1)}} {
			if got := Clone(v); !reflect.DeepEqual(got, v) {
				t.Fatalf("Clone(%#v) = %#v, want equal value", v, got)
			}
		}
	})

	t.Run("nil input returns nil", func(t *testing.T) {
		if got := Clone(nil); got != nil {
			t.Fatalf("Clone(nil) = %#v, want nil", got)
		}
	})

	t.Run("marshal failure returns nil", func(t *testing.T) {
		// A non-marshalable value (JSON.stringify would silently drop it) kills
		// the whole clone instead — documented divergence on Clone.
		if got := Clone(map[string]any{"ch": make(chan int)}); got != nil {
			t.Fatalf("Clone(non-marshalable) = %#v, want nil", got)
		}
		// +Inf is never marshalable — the exact failure mode clampMaxOutput
		// exists to keep out of the wire body.
		if got := Clone(map[string]any{"inf": math.Inf(1)}); got != nil {
			t.Fatalf("Clone(+Inf) = %#v, want nil", got)
		}
	})
}

func TestSetHasDeleteGet(t *testing.T) {
	t.Run("object operations", func(t *testing.T) {
		m := decode(t, `{"a":1}`).(map[string]any)
		if Has(m, "a") != true || Has(m, "z") != false {
			t.Fatal("Has must report exact key presence")
		}
		Set(m, "b", "x")
		if _, has := m["b"]; !has {
			t.Fatal("Set must add the key")
		}
		if got := Get(m, "b"); got != "x" {
			t.Fatalf("Get = %#v, want the set value", got)
		}
		Delete(m, "a", "missing")
		if Has(m, "a") {
			t.Fatal("Delete must remove the key")
		}
		if len(m) != 1 {
			t.Fatalf("Delete removed wrong keys: %#v", m)
		}
	})

	t.Run("non-object no-ops", func(t *testing.T) {
		// The 9router `o.level` idiom on a non-object body (`body?.store`)
		// degrades to no-ops, not panics.
		for _, v := range []any{nil, []any{float64(1)}, "str", float64(5)} {
			if Has(v, "k") {
				t.Fatalf("Has(%#v) on non-object = true, want false", v)
			}
			Set(v, "k", 1) // must not panic
			Delete(v, "k") // must not panic
			if got := Get(v, "k"); got != nil {
				t.Fatalf("Get(%#v, k) = %#v, want nil", v, got)
			}
		}
	})

	t.Run("nil object no-ops", func(t *testing.T) {
		var m map[string]any // nil map — the falsy `json?.` arm
		if Has(m, "k") {
			t.Fatal("Has(nil map) = true, want false")
		}
		Set(m, "k", 1)
		Delete(m, "k")
		if got := Get(m, "k"); got != nil {
			t.Fatalf("Get(nil map, k) = %#v, want nil", got)
		}
	})
}

func TestObjOfArrOf(t *testing.T) {
	t.Run("ObjOf preserves call order", func(t *testing.T) {
		o := ObjOf("a", float64(1), "b", "x")
		if !reflect.DeepEqual(o, map[string]any{"a": float64(1), "b": "x"}) {
			t.Fatalf("ObjOf = %#v, want the ordered pairs", o)
		}
	})

	t.Run("ObjOf even-count only", func(t *testing.T) {
		// Odd trailing arg is ignored — i+1 < len(kv) guard.
		if o := ObjOf("a"); len(o) != 0 {
			t.Fatalf("ObjOf(odd) = %#v, want the empty map", o)
		}
	})

	t.Run("ArrOf passthrough and no-nil", func(t *testing.T) {
		if got := ArrOf("a", float64(2)); !reflect.DeepEqual(got, []any{"a", float64(2)}) {
			t.Fatalf("ArrOf = %#v, want the items", got)
		}
		if got := ArrOf(); got == nil {
			t.Fatal("ArrOf() = nil, want a non-nil empty slice")
		}
	})

	t.Run("Get edge: nested object returns the object", func(t *testing.T) {
		o := decode(t, `{"nested":{"k":1}}`)
		sub := Get(o, "nested")
		if AsObj(sub) == nil || AsF64(Get(sub, "k")) != 1 {
			t.Fatalf("Get(nested) = %#v, want the nested object", sub)
		}
	})
}
