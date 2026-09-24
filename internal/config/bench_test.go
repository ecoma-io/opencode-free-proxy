package config

import "testing"

func benchmarkRuntime(b *testing.B) *Runtime {
	b.Helper()
	file := File{
		Egress: []Egress{
			{ID: "direct"},
			{ID: "proxy-a", Proxy: &Proxy{Type: ProxyHTTP, URL: "http://proxy-a.example:8080"}, Models: []string{"qwen*-free"}},
			{ID: "proxy-b", Proxy: &Proxy{Type: ProxySOCKS5, URL: "socks5://proxy-b.example:1080"}, Models: []string{"muse-*-free"}},
		},
		Routes: []Route{
			{ID: "streaming", Priority: 20, Match: Match{Streaming: new(true)}, Egress: []string{"direct", "proxy-a"}},
			{ID: "muse", Priority: 10, Match: Match{Models: []string{"muse-*-free"}}, Egress: []string{"proxy-b", "direct"}},
			{ID: "default", Egress: []string{"direct", "proxy-a", "proxy-b"}},
		},
	}
	rt, err := file.Resolve()
	if err != nil {
		b.Fatal(err)
	}
	return rt
}

func BenchmarkRuntimeEgress(b *testing.B) {
	rt := benchmarkRuntime(b)
	b.ReportAllocs()
	for b.Loop() {
		e, ok := rt.Egress("proxy-a")
		if !ok || e.ID != "proxy-a" {
			b.Fatal("proxy-a did not resolve")
		}
	}
}

func BenchmarkRuntimeMatchRoute(b *testing.B) {
	rt := benchmarkRuntime(b)
	profiles := []struct {
		streaming bool
		bodyBytes int64
		model     string
	}{
		{streaming: true, bodyBytes: 256, model: "qwen3-coder-free"},
		{bodyBytes: 512, model: "muse-spark-1.3-contributor-free"},
		{bodyBytes: 1024, model: "big-pickle"},
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		p := profiles[i%len(profiles)]
		route, ok := rt.MatchRoute(p.streaming, p.bodyBytes, p.model)
		if !ok || route.ID == "" {
			b.Fatal("route did not match")
		}
	}
}
