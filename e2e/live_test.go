//go:build e2e

// Live smoke tests against an ALREADY-RUNNING proxy talking to the real
// OpenCode Zen upstream. They are opt-in (they spend free-tier quota and need
// egress) and skip unless E2E_LIVE=1:
//
//	E2E_LIVE=1 go test -tags e2e ./e2e/ -run TestLive
//
// Configuration of the target proxy:
//
//	E2E_BASE_URL  proxy base (default http://127.0.0.1:8090)
//	E2E_API_KEY   a key configured in the proxy's auth.keys (omit when auth is off)
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/jsonx"
)

func liveTarget(t *testing.T) (base, key string) {
	t.Helper()
	if os.Getenv("E2E_LIVE") != "1" {
		t.Skip("live tests disabled — set E2E_LIVE=1 against a running proxy")
	}
	base = os.Getenv("E2E_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	return base, os.Getenv("E2E_API_KEY")
}

func livePost(t *testing.T, base, path, key string, body map[string]any) (*http.Response, string) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(raw)
}

// TestLiveModels: the live free-tier list is served and shaped correctly.
func TestLiveModels(t *testing.T) {
	base, key := liveTarget(t)
	req, err := http.NewRequest("GET", base+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	ids := []string{}
	for _, e := range jsonx.AsArr(body["data"]) {
		ids = append(ids, jsonx.AsStr(jsonx.AsObj(e)["id"]))
	}
	if len(ids) == 0 {
		t.Fatalf("empty model list: %s", raw)
	}
	for _, id := range ids {
		if id != "big-pickle" && !strings.HasSuffix(id, "-free") {
			t.Fatalf("non-free id %q leaked into the list: %v", id, ids)
		}
	}
}

// TestLiveChatCompletion: one tiny non-streaming completion on the free tier.
func TestLiveChatCompletion(t *testing.T) {
	base, key := liveTarget(t)
	resp, raw := livePost(t, base, "/v1/chat/completions", key, map[string]any{
		"model":    testedModel,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with the single word: ok"}},
		"stream":   false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v", body["object"])
	}
	choices := jsonx.AsArr(body["choices"])
	if len(choices) == 0 {
		t.Fatalf("no choices: %s", raw)
	}
	msg := jsonx.AsObj(jsonx.AsObj(choices[0])["message"])
	if jsonx.AsStr(msg["content"]) == "" {
		t.Fatalf("empty completion content: %s", raw)
	}
}

// TestLiveResponsesStream: muse-spark streams Responses SSE end to end.
func TestLiveResponsesStream(t *testing.T) {
	base, key := liveTarget(t)
	resp, raw := livePost(t, base, "/v1/responses", key, map[string]any{
		"model":  museModel,
		"input":  "Reply with the single word: ok",
		"stream": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(raw, "response.output_text.delta") {
		t.Fatalf("no delta events:\n%s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("no terminal [DONE]:\n%s", raw)
	}
	// A stream that reaches [DONE] must never grow a synthesized failure.
	if strings.Contains(raw, "response.failed") {
		t.Fatalf("response.failed synthesized on a live stream:\n%s", raw)
	}
}
