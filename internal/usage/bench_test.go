package usage

import "testing"

func BenchmarkEstimateInput(b *testing.B) {
	body := map[string]any{
		"model": "qwen3-coder-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "Use <safe> & concise answers."},
			map[string]any{"role": "user", "content": "Hello 👋"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "weather", "description": "Look up weather."}},
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		if got := EstimateInput(body); got <= 0 {
			b.Fatalf("EstimateInput = %v, want positive", got)
		}
	}
}
