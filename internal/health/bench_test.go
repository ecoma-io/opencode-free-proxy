package health

import (
	"testing"
	"time"
)

func benchmarkRegistry(b *testing.B) (*Registry, Policy) {
	b.Helper()
	r, _ := newTest(time.Unix(1_700_000_000, 0))
	p := policy(true, 2, time.Minute)
	r.Observe(keyA, false, p)
	r.Observe(keyA, false, p)
	if r.Healthy(keyA, p) {
		b.Fatal("benchmark fixture did not arm cooldown")
	}
	return r, p
}

func BenchmarkRegistryHealthy(b *testing.B) {
	r, p := benchmarkRegistry(b)
	b.ReportAllocs()
	for b.Loop() {
		if r.Healthy(keyA, p) {
			b.Fatal("cooling egress became healthy")
		}
	}
}

func BenchmarkRegistryHealthyParallel(b *testing.B) {
	r, p := benchmarkRegistry(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if r.Healthy(keyA, p) {
				b.Error("cooling egress became healthy")
			}
		}
	})
}

func BenchmarkRegistryObserve(b *testing.B) {
	r, _ := newTest(time.Unix(1_700_000_000, 0))
	p := policy(true, 1_000_000_000, time.Minute)
	b.ReportAllocs()
	for b.Loop() {
		r.Observe(keyA, false, p)
	}
}

func BenchmarkRegistryPinRelease(b *testing.B) {
	r, _ := newTest(time.Unix(1_700_000_000, 0))
	keys := []string{keyA, keyB}
	b.ReportAllocs()
	for b.Loop() {
		release := r.Pin(keys)
		release()
	}
}
