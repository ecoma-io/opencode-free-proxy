package jsonx

// Pins JS truthiness for decoded JSON values — the `||` operand gate, e.g.
// utils/error.js:80 `json.error?.message || json.message || json.error ||
// bodyText`. Only null, false, 0 and "" are falsy; everything else —
// including empty objects/arrays and the strings "0"/"false" — is truthy.

import (
	"encoding/json"
	"math"
	"testing"
)

func TestTruthy(t *testing.T) {
	decode := func(raw string) any {
		t.Helper()
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return v
	}
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil is falsy", nil, false},
		{"JSON null is falsy", decode(`null`), false},
		{"false is falsy", decode(`false`), false},
		{"true is truthy", decode(`true`), true},
		{"zero is falsy", decode(`0`), false},
		{"negative zero is falsy", decode(`-0`), false},
		{"nonzero number is truthy", decode(`5`), true},
		{"negative number is truthy", decode(`-1.5`), true},
		{"tiny fraction is truthy", decode(`0.0001`), true},
		{"empty string is falsy", decode(`""`), false},
		{"zero string is truthy", decode(`"0"`), true},
		{"false string is truthy", decode(`"false"`), true},
		{"nonempty string is truthy", decode(`"x"`), true},
		{"empty object is truthy", decode(`{}`), true},
		{"empty array is truthy", decode(`[]`), true},
		// IEEE edge values are unreachable through encoding/json (JSON has no
		// NaN/Inf literals), and +-Inf are truthy in both JS and Go. NaN is
		// the one genuinely wedged corner: JS ToBoolean(NaN) is false, but
		// Truthy's float64 arm (`t != 0`) answers true because NaN != 0 holds
		// in IEEE. The divergence is DEAD ON THE WIRE — NaN cannot enter a
		// decoded JSON value, and jsonx.NumCoerce neutralizes NaN inputs to 0
		// before anything downstream reads them — so the Go answer is pinned
		// here (with the JS truthy spelling in the name) rather than
		// "fixed" into a false nobody can observe.
		{"NaN is truthy in Go, falsy in unreachable JS", float64NaN(), true},
		{"+Inf is truthy", float64Inf(false), true},
		{"-Inf is truthy", float64Inf(true), true},
		{"negative zero is falsy (direct)", float64NegZero(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truthy(tc.v); got != tc.want {
				t.Fatalf("Truthy(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// float64NaN / float64Inf / float64NegZero build the IEEE edge values the
// JSON decoder can never emit from the wire, so the tests can exercise
// Truthy's float64 arm directly.
func float64NaN() float64 {
	var z float64
	return z / z // 0/0 = NaN
}

func float64Inf(neg bool) float64 {
	var one float64 = 1
	var zero float64
	if neg {
		return -one / zero
	}
	return one / zero
}

func float64NegZero() float64 {
	return math.Copysign(0, -1)
}
