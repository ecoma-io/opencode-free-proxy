package upstream

import (
	"context"
	"strings"
	"testing"
	"time"
)

const benchmarkSSE = "data: {\"id\":\"chatcmpl-bench\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-bench\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func BenchmarkScanLines(b *testing.B) {
	b.SetBytes(int64(len(benchmarkSSE)))
	b.ReportAllocs()
	for b.Loop() {
		lines := 0
		err := ScanLines(context.Background(), strings.NewReader(benchmarkSSE), time.Second,
			func(string) error {
				lines++
				return nil
			},
			nil,
		)
		if err != nil {
			b.Fatal(err)
		}
		if lines != 6 {
			b.Fatalf("lines = %d, want 6", lines)
		}
	}
}

func BenchmarkLimiter(b *testing.B) {
	b.Run("unlimited", func(b *testing.B) {
		limiter := NewLimiter()
		b.ReportAllocs()
		for b.Loop() {
			if !limiter.Acquire("egress", 0) {
				b.Fatal("unlimited acquire failed")
			}
			limiter.Release("egress")
		}
	})
	b.Run("capped", func(b *testing.B) {
		limiter := NewLimiter()
		b.ReportAllocs()
		for b.Loop() {
			if !limiter.Acquire("egress", 4) {
				b.Fatal("capped acquire failed")
			}
			limiter.Release("egress")
		}
	})
}

func BenchmarkLimiterParallel(b *testing.B) {
	limiter := NewLimiter()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !limiter.Acquire("egress", 1_000_000) {
				b.Error("acquire failed below capacity")
				continue
			}
			limiter.Release("egress")
		}
	})
}
