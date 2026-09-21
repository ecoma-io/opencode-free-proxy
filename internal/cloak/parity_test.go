package cloak

// JS-semantics parity tests for the thinking-knob coercions in thinking.go
// (Number() over budget fields, the result-based truthy `||` chain of the
// Gemini envelopes) and the Clone guard in NormalizeResponsesTools.

import (
	"reflect"
	"testing"
)

// All three budget knobs go through `Number(x)` before their range checks
// (thinkingUnified.js:71-79, 82-92, 94-99): numeric strings — hex included —
// coerce, non-numbers fall through to the knob's default.
func TestExtractThinkingNumericStringBudgets(t *testing.T) {
	t.Run("claude decimal string budget", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": "8192"},
		})
		if cfg == nil || cfg.Mode != "budget" || cfg.Budget != 8192 {
			t.Fatalf("cfg = %+v, want budget 8192 (Number(\"8192\"))", cfg)
		}
	})
	t.Run("claude hex string budget", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinking": map[string]any{"type": "adaptive", "budget_tokens": "0x1000"},
		})
		if cfg == nil || cfg.Mode != "budget" || cfg.Budget != 4096 {
			t.Fatalf("cfg = %+v, want budget 4096 (Number(\"0x1000\"))", cfg)
		}
	})
	t.Run("claude non-number budget degrades to auto", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": "lots"},
		})
		if cfg == nil || cfg.Mode != "auto" {
			t.Fatalf("cfg = %+v, want auto (Number(\"lots\") is NaN)", cfg)
		}
	})
	t.Run("gemini string budget", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinkingConfig": map[string]any{"thinkingBudget": "2048"},
		})
		if cfg == nil || cfg.Mode != "budget" || cfg.Budget != 2048 {
			t.Fatalf("cfg = %+v, want budget 2048", cfg)
		}
	})
	t.Run("gemini zero and negative string budgets", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinkingConfig": map[string]any{"thinkingBudget": "0"},
		})
		if cfg == nil || cfg.Mode != "none" {
			t.Fatalf("cfg = %+v, want none (Number(\"0\") === 0)", cfg)
		}
		cfg = ExtractThinking(map[string]any{
			"thinkingConfig": map[string]any{"thinkingBudget": "-1"},
		})
		if cfg == nil || cfg.Mode != "auto" {
			t.Fatalf("cfg = %+v, want auto (negative budget)", cfg)
		}
	})
	t.Run("qwen string budget", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"enable_thinking": true, "thinking_budget": "2048",
		})
		if cfg == nil || cfg.Mode != "budget" || cfg.Budget != 2048 {
			t.Fatalf("cfg = %+v, want budget 2048", cfg)
		}
		cfg = ExtractThinking(map[string]any{"enable_thinking": true})
		if cfg == nil || cfg.Mode != "auto" {
			t.Fatalf("cfg = %+v, want auto", cfg)
		}
	})
}

// The Gemini chain (thinkingUnified.js:82-92) is a truthy `||` evaluated on
// each operand's RESULT, then a separate `typeof === "object"` gate: a truthy
// non-object thinkingConfig WINS the chain and fails the gate (the nested
// fallbacks are never consulted), while a FALSY nested thinkingConfig falls
// through to the request envelope — whose value is the chain's last operand
// even when falsy.
func TestExtractThinkingGeminiChainResultSemantics(t *testing.T) {
	t.Run("truthy non-object blocks the nested fallback", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"thinkingConfig":   5.0,
			"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingBudget": 8192.0}},
		})
		if cfg != nil {
			t.Fatalf("cfg = %+v, want nil (the 5 wins the chain, fails typeof object)", cfg)
		}
	})
	t.Run("falsy nested thinkingConfig falls through to the request envelope", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"generationConfig": map[string]any{"thinkingConfig": 0.0},
			"request": map[string]any{
				"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingBudget": 2048.0}},
			},
		})
		if cfg == nil || cfg.Mode != "budget" || cfg.Budget != 2048 {
			t.Fatalf("cfg = %+v, want the request envelope's budget 2048", cfg)
		}
	})
	t.Run("falsy last operand is taken and fails the object gate", func(t *testing.T) {
		cfg := ExtractThinking(map[string]any{
			"request": map[string]any{
				"generationConfig": map[string]any{"thinkingConfig": 0.0},
			},
		})
		if cfg != nil {
			t.Fatalf("cfg = %+v, want nil (last operand taken even when falsy)", cfg)
		}
	})
}

// Defensive hardening: a Clone that cannot round-trip (unmarshalable value —
// impossible via decoded JSON, possible via the Go API) must not panic the
// properties default; the empty object schema is substituted instead.
func TestNormalizeResponsesToolsCloneGuard(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{
		"type":       "function",
		"name":       "f",
		"parameters": map[string]any{"type": "object", "properties": nil, "zz": func() {}},
	}}}
	NormalizeResponsesTools(body)
	params := body["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	want := map[string]any{"properties": map[string]any{}}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("parameters = %#v, want the empty-object schema %v", params, want)
	}
}
