package routing

import (
	"testing"

	"opencode-free-proxy/internal/config"
)

func BenchmarkSchedulerPlan(b *testing.B) {
	cases := []struct {
		name     string
		strategy config.Strategy
		weights  []int
	}{
		{name: "round_robin", strategy: config.StrategyRoundRobin, weights: []int{1, 1, 1}},
		{name: "weighted_round_robin", strategy: config.StrategyWeightedRR, weights: []int{5, 2, 1}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			egresses := make([]config.Egress, len(tc.weights))
			heads := make([]string, len(tc.weights))
			for i, weight := range tc.weights {
				id := string(rune('a' + i))
				egresses[i] = egress(id, weight)
				heads[i] = id
			}
			route := route("benchmark", tc.strategy, heads...)
			file := config.File{Egress: egresses, Routes: []config.Route{route}}
			rt, err := file.Resolve()
			if err != nil {
				b.Fatal(err)
			}
			scheduler := NewScheduler()
			b.ReportAllocs()
			for b.Loop() {
				plan := scheduler.Plan(rt, route, heads)
				if len(plan.Attempts) != len(heads) {
					b.Fatalf("attempts = %d, want %d", len(plan.Attempts), len(heads))
				}
			}
		})
	}
}

func BenchmarkModelAllowed(b *testing.B) {
	patterns := []string{"qwen3-coder-free", "muse-*-free", "big-pickle"}
	models := []string{"qwen3-coder-free", "muse-spark-1.3-contributor-free", "not-free"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		_ = ModelAllowed(patterns, models[i%len(models)])
	}
}
