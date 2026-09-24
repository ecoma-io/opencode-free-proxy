package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/health"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/routing"
	"opencode-free-proxy/internal/upstream"
)

type discardResponseWriter struct {
	header http.Header
	status int
}

func newDiscardResponseWriter() *discardResponseWriter {
	return &discardResponseWriter{header: make(http.Header)}
}

func (w *discardResponseWriter) Header() http.Header { return w.header }

func (w *discardResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}

func (w *discardResponseWriter) WriteHeader(status int) { w.status = status }

func benchmarkRouter(b *testing.B, contentType, response string) *http.ServeMux {
	b.Helper()
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, response)
	}))
	b.Cleanup(upstreamServer.Close)

	path := b.TempDir() + "/config.yaml"
	doc := fmt.Sprintf("upstream:\n  base: %q\negress:\n  - id: direct\nroutes:\n  - id: default\n    egress: [direct]\n", upstreamServer.URL)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		b.Fatal(err)
	}
	store, err := config.NewStore(path, 0, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(store.Stop)

	s := NewServer(store, identity.NewUserAgentCache(), upstream.NewClient(), nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.HandleResponses)
	mux.HandleFunc("GET /v1/models", s.HandleModels)
	mux.HandleFunc("OPTIONS /", s.HandleOptions)
	return mux
}

func benchRequest(path, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
}

func BenchmarkRelayHotPath(b *testing.B) {
	cases := []struct {
		name        string
		path        string
		requestBody string
		contentType string
		response    string
		wantStatus  int
	}{
		{
			name:        "stream/chat",
			path:        "/v1/chat/completions",
			requestBody: `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			contentType: "text/event-stream",
			response:    chatStreamSSE,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "stream/responses",
			path:        "/v1/responses",
			requestBody: `{"model":"muse-spark-1.2-contributor-free","input":"hi","stream":true}`,
			contentType: "text/event-stream",
			response:    responsesStreamSSE,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "nonstream/forced-json",
			path:        "/v1/chat/completions",
			requestBody: `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			contentType: "text/event-stream",
			response:    chatReasoningStreamSSE,
			wantStatus:  http.StatusOK,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			mux := benchmarkRouter(b, tc.contentType, tc.response)
			b.SetBytes(int64(len(tc.requestBody)))

			// Sanity-check the fixed fixture once before the timer: the benchmark
			// measures the real mux path, and no growing recorder enters the loop.
			check := httptest.NewRecorder()
			mux.ServeHTTP(check, benchRequest(tc.path, tc.requestBody))
			if check.Code != tc.wantStatus || check.Body.Len() == 0 {
				b.Fatalf("fixture response = (%d, %q), want status %d and a body", check.Code, check.Body.String(), tc.wantStatus)
			}
			b.StartTimer()

			b.ReportAllocs()
			for b.Loop() {
				out := newDiscardResponseWriter()
				mux.ServeHTTP(out, benchRequest(tc.path, tc.requestBody))
				if out.status != http.StatusOK {
					b.Fatalf("status = %d, want 200", out.status)
				}
			}
		})
	}
}

func BenchmarkRelayHotPathParallel(b *testing.B) {
	body := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`
	mux := benchmarkRouter(b, "text/event-stream", chatStreamSSE)
	check := httptest.NewRecorder()
	mux.ServeHTTP(check, benchRequest("/v1/chat/completions", body))
	if check.Code != http.StatusOK || check.Body.Len() == 0 {
		b.Fatalf("fixture response = (%d, %q)", check.Code, check.Body.String())
	}
	b.StartTimer()

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			out := newDiscardResponseWriter()
			mux.ServeHTTP(out, benchRequest("/v1/chat/completions", body))
			if out.status != http.StatusOK {
				b.Errorf("status = %d, want 200", out.status)
			}
		}
	})
}

func BenchmarkRouteHeads(b *testing.B) {
	file := config.File{
		Egress: []config.Egress{
			{ID: "direct"},
			{ID: "proxy-a", Models: []string{"qwen*-free"}},
			{ID: "proxy-b", MaxBodyBytes: 2048},
		},
		Routes: []config.Route{{ID: "default", Egress: []string{"direct", "proxy-a", "proxy-b"}}},
	}
	rt, err := file.Resolve()
	if err != nil {
		b.Fatal(err)
	}
	route, ok := rt.MatchRoute(true, 256, "qwen3-coder-free")
	if !ok {
		b.Fatal("benchmark route did not match")
	}
	s := NewServer(nil, identity.NewUserAgentCache(), upstream.NewClient(), nil)
	profile := routing.Profile{Model: "qwen3-coder-free", Streaming: true, BodyBytes: 256}
	policy := health.PolicyFromSnapshot(rt)
	b.ReportAllocs()
	for b.Loop() {
		heads := s.routeHeads(rt, route, profile, policy)
		if len(heads) != 3 {
			b.Fatalf("heads = %v, want all egresses", heads)
		}
	}
}

func BenchmarkFlushWriter(b *testing.B) {
	payload := "data: {\"id\":\"chatcmpl-bench\",\"choices\":[]}\n\n"
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		var out bytes.Buffer
		fw := flushWriter{w: nopResponseWriter{Writer: &out}}
		if _, err := fw.WriteString(payload); err != nil {
			b.Fatal(err)
		}
		if out.String() != payload {
			b.Fatal("flush writer changed payload")
		}
	}
}

type nopResponseWriter struct {
	io.Writer
}

func (nopResponseWriter) Header() http.Header { return make(http.Header) }
func (nopResponseWriter) WriteHeader(int)     {}
