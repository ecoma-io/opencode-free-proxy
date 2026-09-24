package router

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/upstream"
)

const benchmarkGoldenRequest = `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`

const benchmarkGoldenResponse = "data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"created\":1700000000,\"id\":\"chatcmpl-123\",\"model\":\"qwen3-coder-free\",\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":2,\"prompt_tokens\":5,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n" +
	"data: [DONE]\n\n"

func TestBenchmarkGoldenStreamingResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatStreamSSE))
	}))
	defer up.Close()

	store, mux := goldenRouter(t, up.URL, logger)
	defer store.Stop()
	res := postJSON(t, mux, "/v1/chat/completions", benchmarkGoldenRequest, nil)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", res.Code, res.Body.String())
	}
	if got := res.Body.String(); got != benchmarkGoldenResponse {
		t.Fatalf("response bytes changed:\n got: %q\nwant: %q", got, benchmarkGoldenResponse)
	}
	if got := res.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := res.Header().Get(headerEgress); got != "direct" {
		t.Fatalf("%s = %q, want direct", headerEgress, got)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Methods"); got != "POST, GET, OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want POST, GET, OPTIONS", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want *", got)
	}

	line := strings.TrimSpace(logs.String())
	if !strings.Contains(line, `"route":"default"`) ||
		!strings.Contains(line, `"egress":"direct"`) ||
		!strings.Contains(line, `"attempts":1`) ||
		!strings.Contains(line, `"status":200`) ||
		!strings.Contains(line, `"endpoint":"chat"`) ||
		!strings.Contains(line, `request completed generation=1 route=default egress=direct attempts=1 class=success status=200`) {
		t.Fatalf("completion line lost fixed fields: %q", line)
	}
}

func goldenRouter(t *testing.T, upstreamURL string, logger zerolog.Logger) (*config.Store, *http.ServeMux) {
	t.Helper()
	doc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`, upstreamURL)
	path := writeCfg(t, t.TempDir(), doc)
	store, err := config.NewStore(path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(store, identity.NewUserAgentCache(), upstream.NewClient(), logger)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.HandleResponses)
	mux.HandleFunc("GET /v1/models", s.HandleModels)
	mux.HandleFunc("OPTIONS /", s.HandleOptions)
	return store, mux
}
