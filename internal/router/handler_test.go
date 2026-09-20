package router

// Integration tests for the /v1/chat/completions and /v1/responses pipeline
// (chatCore.js order: translate → applyThinking → executor transform → retrying
// upstream call → relay/aggregate), driven through the same method-mux wiring
// cmd/server/main.go uses, with an httptest server standing in for OpenCode Zen.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/upstream"
)

// ---------------------------------------------------------------------------
// Fixtures — the SSE dialects the two upstream endpoints speak.
// ---------------------------------------------------------------------------

// chatStreamSSE is a Chat Completions stream: two content deltas, a finish
// chunk carrying usage, and the [DONE] sentinel.
const chatStreamSSE = "data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n" +
	"\n" +
	"data: [DONE]\n\n"

// chatReasoningStreamSSE adds a reasoning_content delta between content and
// the finish chunk (sseToJsonHandler.js conditional reasoning strip).
const chatReasoningStreamSSE = "data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking hard\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n" +
	"\n" +
	"data: [DONE]\n\n"

// chatStreamNoUsageSSE has no usage anywhere (the usage seam must estimate).
const chatStreamNoUsageSSE = "data: {\"id\":\"chatcmpl-9\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hey\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-9\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"qwen3-coder-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
	"\n" +
	"data: [DONE]\n\n"

// responsesStreamSSE is a Muse Spark Responses stream: created → output_text
// deltas → completed with usage, framed with event: lines.
const responsesStreamSSE = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_abc\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"Hi\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_abc\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n" +
	"data: [DONE]\n\n"

// ---------------------------------------------------------------------------
// Test harness.
// ---------------------------------------------------------------------------

type upstreamCall struct {
	Path   string
	Header http.Header
	Body   []byte
}

type upstreamRecorder struct {
	mu    sync.Mutex
	calls []upstreamCall
}

func (r *upstreamRecorder) snapshot() []upstreamCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]upstreamCall(nil), r.calls...)
}

func (r *upstreamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// newScriptedUpstream records every request and always answers with the given
// status / content type / body.
func newScriptedUpstream(t *testing.T, rec *upstreamRecorder, status int, contentType, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading upstream body: %v", err)
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: raw})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// newRouter wires a Server exactly like cmd/server/main.go, with the retry
// sleeps elided and a cold UA cache (Get() is network-free and returns the
// pinned fallback).
func newRouter(upstreamURL, apiKey string) (*Server, *http.ServeMux) {
	c := upstream.NewClient()
	s := NewServer(
		&config.Config{Port: "0", APIKey: apiKey, UpstreamBase: upstreamURL},
		config.NewDefault(),
		identity.NewUserAgentCache(),
		c,
		nil,
		func(time.Duration) {}, // no-op sleep: retry matrices run instantly
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.HandleResponses)
	mux.HandleFunc("GET /v1/models", s.HandleModels)
	mux.HandleFunc("OPTIONS /", s.HandleOptions)
	return s, mux
}

func postJSON(t *testing.T, mux *http.ServeMux, path, raw string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// dataObjects parses every `data: {json}` line of an SSE payload.
func dataObjects(t *testing.T, out string) []map[string]any {
	t.Helper()
	var objs []map[string]any
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var o map[string]any
		if err := json.Unmarshal([]byte(payload), &o); err != nil {
			t.Fatalf("unparsable data line %q: %v", payload, err)
		}
		objs = append(objs, o)
	}
	return objs
}

func jobj(t *testing.T, v any, where string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: %#v is not an object", where, v)
	}
	return m
}

func jarr(t *testing.T, v any, where string) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: %#v is not an array", where, v)
	}
	return a
}

func jstr(t *testing.T, v any, where string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s: %#v is not a string", where, v)
	}
	return s
}

func jf64(t *testing.T, v any, where string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: %#v is not a number", where, v)
	}
	return f
}

// toolNames flattens a tools array into name strings (chat nested
// function.name or the flat responses tool.name).
func toolNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	var names []string
	for _, raw := range jarr(t, body["tools"], "body.tools") {
		tool := jobj(t, raw, "tool")
		if n := jsonx.AsStr(tool["name"]); n != "" {
			names = append(names, n)
			continue
		}
		names = append(names, jsonx.AsStr(jsonx.Get(tool["function"], "name")))
	}
	return names
}

var (
	sessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	requestRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
)

// TestChatStreamingPassthrough: chat client + chat model → STREAM_MODE
// passthrough (streamingHandler.js buildTransformStream needsTranslation
// false): chunks normalized and forwarded, [DONE] guaranteed, and the
// upstream request fully cloaked as the official OpenCode client.
func TestChatStreamingPassthrough(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"oc/qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	if ct := res.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream (SSE_HEADERS_CORS)", ct)
	}
	out := res.Body.String()
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with the guaranteed [DONE] sentinel, got %q", out)
	}
	objs := dataObjects(t, out)
	if len(objs) != 3 {
		t.Fatalf("expected 3 data chunks (2 deltas + finish), got %d: %s", len(objs), out)
	}
	choice0 := jobj(t, jarr(t, objs[0]["choices"], "choices")[0], "choice0")
	got0 := jstr(t, jobj(t, choice0["delta"], "delta")["content"], "content0")
	if got0 != "Hel" {
		t.Fatalf("first delta content = %q, want %q", got0, "Hel")
	}
	fin := objs[2]
	choice := jobj(t, jarr(t, fin["choices"], "choices")[0], "choice")
	if jstr(t, choice["finish_reason"], "finish_reason") != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	usage := jobj(t, fin["usage"], "usage")
	if jf64(t, usage["prompt_tokens"], "prompt_tokens") != 5 || jf64(t, usage["completion_tokens"], "completion_tokens") != 2 {
		t.Fatalf("usage = %#v, want the upstream numbers passed through", usage)
	}

	// ---- upstream request assertions.
	if rec.count() != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", rec.count())
	}
	call := rec.snapshot()[0]
	if call.Path != "/zen/v1/chat/completions" {
		t.Fatalf("upstream path = %q", call.Path)
	}
	h := call.Header
	if got := h.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization = %q, want Bearer public", got)
	}
	if got := h.Get("User-Agent"); got != identity.FallbackUA() {
		t.Fatalf("User-Agent = %q, want the pinned fallback (cold cache, no downstream UA)", got)
	}
	if got := h.Get("X-Opencode-Client"); got != "desktop" {
		t.Fatalf("x-opencode-client = %q, want desktop", got)
	}
	if got := h.Get("X-Opencode-Project"); got != "global" {
		t.Fatalf("x-opencode-project = %q, want global", got)
	}
	if got := h.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
	if sess := h.Get("X-Opencode-Session"); !sessionRe.MatchString(sess) {
		t.Fatalf("x-opencode-session = %q, want a valid opencode session id", sess)
	}
	if reqID := h.Get("X-Opencode-Request"); !requestRe.MatchString(reqID) {
		t.Fatalf("x-opencode-request = %q, want a valid opencode request id", reqID)
	}

	body := jobj(t, mustJSON(t, call.Body), "upstream body")
	if body["stream"] != true {
		t.Fatalf("upstream stream = %v, want true (free-tier gate)", body["stream"])
	}
	if jstr(t, body["model"], "model") != "qwen3-coder-free" {
		t.Fatalf("upstream model = %v, want the oc/ alias stripped", body["model"])
	}
	names := toolNames(t, body)
	if !reflect.DeepEqual(names, []string{"bash", "glob", "grep", "read"}) {
		t.Fatalf("upstream tools = %v, want the fingerprint quartet", names)
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want none for a tool-less caller", body["tool_choice"])
	}
}

func mustJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, raw)
	}
	return body
}

// TestChatNonStreamingForcedJSON: the upstream always streams (forceStream
// quirk) so a stream:false client is served by the forced SSE→JSON path
// (chatCore.js handleForcedSSEToJson → parseSSEToOpenAIResponse), with
// reasoning_content stripped because content is non-empty.
func TestChatNonStreamingForcedJSON(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatReasoningStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	if ct := res.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := mustJSON(t, res.Body.Bytes())
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", body["object"])
	}
	if body["id"] != "chatcmpl-123" {
		t.Fatalf("id = %v, want the stream's id", body["id"])
	}
	choice := jobj(t, jarr(t, body["choices"], "choices")[0], "choice")
	if jstr(t, choice["finish_reason"], "finish_reason") != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := jobj(t, choice["message"], "message")
	if jstr(t, msg["content"], "content") != "Hello" {
		t.Fatalf("content = %v, want the deltas joined", msg["content"])
	}
	if _, has := msg["reasoning_content"]; has {
		t.Fatalf("reasoning_content must be stripped when content is non-empty, got %#v", msg)
	}
	usage := jobj(t, body["usage"], "usage")
	if jf64(t, usage["prompt_tokens"], "prompt_tokens") != 5 || jf64(t, usage["total_tokens"], "total_tokens") != 7 {
		t.Fatalf("usage = %#v, want the stream usage", usage)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", rec.count())
	}
}

// TestUpstreamErrorPassthrough is a DELIBERATE PARITY-BUG REPRO for the 403/429
// subtests and is expected to FAIL until the port catches up.
//
// JS applies the `[status]: ` prefix exactly once — chatCore.js:442
// `formatProviderError(...)` → `[${code}]: ${message}` (error.js:146) — and
// createErrorResult wraps that message verbatim, so a client sees
// `[403]: FreeTierError`. The Go relay prefixes the message itself
// (internal/router/handler.go:184) and then writeError prefixes again
// (internal/router/handler.go:60), so clients get the doubled
// `[403]: [403]: FreeTierError`.
//
// The hit-once assertion (no retries outside the retry matrix) still holds.
func TestUpstreamErrorPassthrough(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantType string
		wantCode string
	}{
		{name: "403 FreeTierError", status: 403, wantType: "permission_error", wantCode: "insufficient_quota"},
		{name: "429 rate limited", status: 429, wantType: "rate_limit_error", wantCode: "rate_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &upstreamRecorder{}
			up := newScriptedUpstream(t, rec, tc.status, "application/json",
				`{"error":{"message":"FreeTierError"}}`)
			defer up.Close()
			_, mux := newRouter(up.URL, "")

			res := postJSON(t, mux, "/v1/chat/completions",
				`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", res.Code, tc.status, res.Body.String())
			}
			body := mustJSON(t, res.Body.Bytes())
			errObj := jobj(t, body["error"], "error")
			if jstr(t, errObj["message"], "message") != "[403]: FreeTierError" && tc.status == 403 {
				t.Fatalf("message = %v, want the formatProviderError prefix", errObj["message"])
			}
			if tc.status == 429 && jstr(t, errObj["message"], "message") != "[429]: FreeTierError" {
				t.Fatalf("message = %v, want the formatProviderError prefix", errObj["message"])
			}
			if jstr(t, errObj["type"], "type") != tc.wantType {
				t.Fatalf("type = %v, want %q", errObj["type"], tc.wantType)
			}
			if jstr(t, errObj["code"], "code") != tc.wantCode {
				t.Fatalf("code = %v, want %q", errObj["code"], tc.wantCode)
			}
			// 403/429 are outside the retry matrix → exactly one upstream hit.
			if n := rec.count(); n != 1 {
				t.Fatalf("upstream hit %d times, want exactly 1 (no retries)", n)
			}
		})
	}
}

// TestMuseSparkTranslateToChatClient: chat client + muse-spark model →
// STREAM_MODE translate (responses upstream → chat client). The upstream call
// goes to /zen/v1/responses with the Responses body shape; the client sees
// chat chunks and — JS translate-mode parity — NO [DONE] sentinel.
func TestMuseSparkTranslateToChatClient(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", responsesStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"oc/muse-spark-1.2-contributor-free(high)","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	out := res.Body.String()
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("translate mode never emits [DONE] to chat clients, got %q", out)
	}
	objs := dataObjects(t, out)
	if len(objs) != 2 {
		t.Fatalf("expected content delta + final chunk, got %d: %s", len(objs), out)
	}
	delta := jobj(t, jobj(t, jarr(t, objs[0]["choices"], "choices")[0], "choice")["delta"], "delta")
	if jstr(t, delta["content"], "content") != "Hi" {
		t.Fatalf("delta = %#v, want content Hi", delta)
	}
	choice := jobj(t, jarr(t, objs[1]["choices"], "choices")[0], "choice")
	if jstr(t, choice["finish_reason"], "finish_reason") != "stop" {
		t.Fatalf("finish_reason = %v, want stop from response.completed", choice["finish_reason"])
	}
	usage := jobj(t, objs[1]["usage"], "usage")
	if jf64(t, usage["prompt_tokens"], "prompt_tokens") != 10 || jf64(t, usage["completion_tokens"], "completion_tokens") != 5 {
		t.Fatalf("usage = %#v, want the responses usage renamed to chat", usage)
	}
	if id := jstr(t, objs[0]["id"], "id"); !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("chat chunk id = %q, want the chatcmpl- prefix", id)
	}

	// ---- upstream request: the muse model routes to /zen/v1/responses.
	if rec.count() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", rec.count())
	}
	call := rec.snapshot()[0]
	if call.Path != "/zen/v1/responses" {
		t.Fatalf("upstream path = %q, want /zen/v1/responses", call.Path)
	}
	body := mustJSON(t, call.Body)
	if jstr(t, body["model"], "model") != "muse-spark-1.2-contributor-free" {
		t.Fatalf("upstream model = %v, want the thinking suffix stripped", body["model"])
	}
	input := jarr(t, body["input"], "input")
	if len(input) != 1 {
		t.Fatalf("input = %#v, want the translated message array", body["input"])
	}
	if jstr(t, jobj(t, input[0], "input[0]")["role"], "role") != "user" {
		t.Fatalf("input[0] = %#v, want the user message item", input[0])
	}
	reasoning := jobj(t, body["reasoning"], "reasoning")
	if jstr(t, reasoning["effort"], "effort") != "high" || jstr(t, reasoning["summary"], "summary") != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:high,summary:auto} from the (high) suffix", reasoning)
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatal("reasoning_effort must be folded into reasoning for /responses")
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto (muse-spark is auto-only)", body["tool_choice"])
	}
	if body["store"] != false {
		t.Fatalf("store = %v, want false", body["store"])
	}
}

// TestMuseSparkResponsesPassthrough: responses client + muse-spark model →
// passthrough: framing preserved, [DONE] guaranteed on flush.
func TestMuseSparkResponsesPassthrough(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", responsesStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/responses",
		`{"model":"muse-spark-1.2-contributor-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	out := res.Body.String()
	for _, want := range []string{"event: response.created\n", "event: response.output_text.delta\n", "event: response.completed\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("event framing lost: missing %q in %q", want, out)
		}
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("passthrough must end with the [DONE] sentinel, got %q", out)
	}
	if !strings.Contains(out, `"delta":"Hi"`) {
		t.Fatalf("delta payload not forwarded verbatim: %q", out)
	}

	call := rec.snapshot()[0]
	if call.Path != "/zen/v1/responses" {
		t.Fatalf("upstream path = %q", call.Path)
	}
	body := mustJSON(t, call.Body)
	if jstr(t, body["model"], "model") != "muse-spark-1.2-contributor-free" {
		t.Fatalf("upstream model = %v", body["model"])
	}
	if names := toolNames(t, body); !reflect.DeepEqual(names, []string{"bash", "glob", "grep", "read"}) {
		t.Fatalf("upstream tools = %v, want the flat fingerprint quartet", names)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want the auto default", body["tool_choice"])
	}
}

// TestMuseSparkResponsesPassthroughAbortTerminal: when the upstream stream
// dies mid-body, a Responses passthrough client still gets a parseable
// terminal — response.failed (stream_disconnected) + [DONE]
// (buildAbortedResponsesTerminalBytes).
func TestMuseSparkResponsesPassthroughAbortTerminal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("expected a hijackable upstream connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Declare more body bytes than we send, then drop the connection: the
		// client sees response headers, partial SSE, and a transport error.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 4096\r\n\r\n"))
		_, _ = conn.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_x\",\"status\":\"in_progress\"}}\n\n"))
	}))
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/responses",
		`{"model":"muse-spark-1.2-contributor-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	out := res.Body.String()
	if !strings.Contains(out, "event: response.created") {
		t.Fatalf("partial upstream events must still be forwarded, got %q", out)
	}
	if !strings.Contains(out, "event: response.failed") {
		t.Fatalf("missing the synthesized response.failed frame: %q", out)
	}
	if !strings.Contains(out, `"code":"stream_disconnected"`) {
		t.Fatalf("response.failed must carry code stream_disconnected, got %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("abort terminal must end with [DONE], got %q", out)
	}
}

// TestResponsesClientChatModelTranslate: responses client + chat model →
// STREAM_MODE translate (chat upstream → responses client): framed events, no
// [DONE], chat-shaped upstream request.
func TestResponsesClientChatModelTranslate(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamNoUsageSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	res := postJSON(t, mux, "/v1/responses",
		`{"model":"qwen3-coder-free","input":"hello","stream":true}`, nil)

	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	out := res.Body.String()
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("translate mode never emits [DONE], got %q", out)
	}
	for _, want := range []string{"event: response.output_item.added\n", "event: response.output_text.delta\n", "event: response.completed\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing framed event %q in %q", want, out)
		}
	}
	if !strings.Contains(out, `"delta":"Hey"`) {
		t.Fatalf("output_text delta missing: %q", out)
	}
	objs := dataObjects(t, out)
	last := objs[len(objs)-1]
	if jstr(t, last["type"], "type") != "response.completed" {
		t.Fatalf("final event = %#v, want response.completed", last)
	}

	// ---- upstream request: chat model stays on /chat/completions.
	call := rec.snapshot()[0]
	if call.Path != "/zen/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /zen/v1/chat/completions", call.Path)
	}
	body := mustJSON(t, call.Body)
	messages := jarr(t, body["messages"], "messages")
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want the input string translated to one message", messages)
	}
	msg := jobj(t, messages[0], "message")
	if jstr(t, msg["role"], "role") != "user" {
		t.Fatalf("role = %v, want user", msg["role"])
	}
	// normalizeResponsesInput("hello") → content parts, mapped to {type:text}
	// blocks by the translator (JS keeps them as an array too — the string is
	// never carried over verbatim).
	content := jarr(t, msg["content"], "content")
	if len(content) != 1 {
		t.Fatalf("content = %#v, want one text part", msg["content"])
	}
	part := jobj(t, content[0], "content[0]")
	if jstr(t, part["type"], "type") != "text" || jstr(t, part["text"], "text") != "hello" {
		t.Fatalf("content part = %#v, want {type:text,text:hello}", part)
	}
	if jstr(t, body["model"], "model") != "qwen3-coder-free" {
		t.Fatalf("upstream model = %v", body["model"])
	}
	if names := toolNames(t, body); !reflect.DeepEqual(names, []string{"bash", "glob", "grep", "read"}) {
		t.Fatalf("upstream tools = %v, want the chat-shaped quartet", names)
	}
}

// TestAuth: Cfg.APIKey gates every endpoint with the 401 error envelope.
func TestAuth(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "sk-secret")
	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`

	t.Run("missing key", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions", payload, nil)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.Code)
		}
		errObj := jobj(t, mustJSON(t, res.Body.Bytes())["error"], "error")
		if jstr(t, errObj["message"], "message") != "Invalid API key provided" { // JS: errorResponse(401, msg) — no prefix (src/sse/handlers/chat.js:82)
			t.Fatalf("message = %v", errObj["message"])
		}
		if jstr(t, errObj["type"], "type") != "authentication_error" || jstr(t, errObj["code"], "code") != "invalid_api_key" {
			t.Fatalf("envelope = %#v", errObj)
		}
		if n := rec.count(); n != 0 {
			t.Fatalf("upstream must not be reached without a valid key, got %d calls", n)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions", payload,
			map[string]string{"Authorization": "Bearer nope"})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.Code)
		}
	})

	t.Run("correct key passes", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions", payload,
			map[string]string{"Authorization": "Bearer sk-secret"})
		if res.Code != 200 {
			t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
		}
		if n := rec.count(); n != 1 {
			t.Fatalf("expected the request to reach upstream, got %d calls", n)
		}
	})
}

// TestBadRequestsAndMethods: missing model, unparsable body, and the method
// mux (Go 1.22 "POST /v1/..." patterns 405 everything else).
func TestBadRequestsAndMethods(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	t.Run("missing model", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions", `{"messages":[]}`, nil)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.Code)
		}
		errObj := jobj(t, mustJSON(t, res.Body.Bytes())["error"], "error")
		if jstr(t, errObj["type"], "type") != "invalid_request_error" || jstr(t, errObj["code"], "code") != "bad_request" {
			t.Fatalf("envelope = %#v", errObj)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions", `{not json`, nil)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.Code)
		}
	})

	t.Run("JSON null body", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/responses", `null`, nil)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.Code)
		}
	})

	t.Run("GET on a POST endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
		got := httptest.NewRecorder()
		mux.ServeHTTP(got, req)
		if got.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", got.Code)
		}
	})

	t.Run("POST on the models endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader("{}"))
		got := httptest.NewRecorder()
		mux.ServeHTTP(got, req)
		if got.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", got.Code)
		}
	})
}

// TestModelAliasAndThinkingSuffix: the oc/ alias prefix and the "(level)"
// thinking suffix never reach upstream; on the reasoning-capable muse models
// the suffix becomes reasoning.effort.
func TestModelAliasAndThinkingSuffix(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	t.Run("oc/ prefix stripped", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"oc/qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
		if res.Code != 200 {
			t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
		}
		body := mustJSON(t, rec.snapshot()[0].Body)
		if jstr(t, body["model"], "model") != "qwen3-coder-free" {
			t.Fatalf("upstream model = %v, want the alias prefix stripped", body["model"])
		}
	})

	t.Run("(high) suffix stripped on chat models", func(t *testing.T) {
		postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free(high)","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
		body := mustJSON(t, rec.snapshot()[1].Body)
		if jstr(t, body["model"], "model") != "qwen3-coder-free" {
			t.Fatalf("upstream model = %v, want the thinking suffix stripped", body["model"])
		}
		if _, has := body["reasoning_effort"]; has {
			t.Fatalf("non-reasoning models must have thinking stripped, got %#v", body)
		}
	})

	t.Run("muse suffix drives reasoning.effort", func(t *testing.T) {
		museUp := newScriptedUpstream(t, rec, 200, "text/event-stream", responsesStreamSSE)
		defer museUp.Close()
		_, museMux := newRouter(museUp.URL, "")
		for _, tc := range []struct{ suffix, want string }{
			{"(high)", "high"},
			{"(xhigh)", "xhigh"},
			{"(none)", "none"},
		} {
			model := "muse-spark-1.2-contributor-free" + tc.suffix
			res := postJSON(t, museMux, "/v1/chat/completions",
				`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
			if res.Code != 200 {
				t.Fatalf("%s: status = %d, body %s", tc.suffix, res.Code, res.Body.String())
			}
			call := rec.snapshot()[rec.count()-1]
			if call.Path != "/zen/v1/responses" {
				t.Fatalf("%s: upstream path = %q", tc.suffix, call.Path)
			}
			body := mustJSON(t, call.Body)
			if jstr(t, body["model"], "model") != "muse-spark-1.2-contributor-free" {
				t.Fatalf("%s: upstream model = %v", tc.suffix, body["model"])
			}
			reasoning := jobj(t, body["reasoning"], "reasoning")
			if got := jstr(t, reasoning["effort"], "effort"); got != tc.want {
				t.Fatalf("%s: reasoning.effort = %q, want %q", tc.suffix, got, tc.want)
			}
		}
	})
}

// TestDownstreamCapture: the executor forwards only the captured client
// headers (captureDownstream → BuildHeaders).
func TestDownstreamCapture(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(up.URL, "")

	t.Run("valid opencode UA passes through", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			map[string]string{
				"User-Agent":         "opencode/1.19.0",
				"X-Opencode-Client":  "cli",
				"X-Opencode-Request": "msg_downstream00000000",
				"X-Opencode-Project": "team-x",
			})
		if res.Code != 200 {
			t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
		}
		h := rec.snapshot()[rec.count()-1].Header
		if got := h.Get("User-Agent"); got != "opencode/1.19.0" {
			t.Fatalf("User-Agent = %q, want the downstream value", got)
		}
		if got := h.Get("X-Opencode-Client"); got != "cli" {
			t.Fatalf("x-opencode-client = %q, want the downstream passthrough", got)
		}
		if got := h.Get("X-Opencode-Request"); got != "msg_downstream00000000" {
			t.Fatalf("x-opencode-request = %q, want the downstream passthrough", got)
		}
		if got := h.Get("X-Opencode-Project"); got != "team-x" {
			t.Fatalf("x-opencode-project = %q, want the downstream passthrough", got)
		}
	})

	t.Run("foreign UA is forged", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			map[string]string{"User-Agent": "curl/8.0.1"})
		if res.Code != 200 {
			t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
		}
		if got := rec.snapshot()[rec.count()-1].Header.Get("User-Agent"); got != identity.FallbackUA() {
			t.Fatalf("User-Agent = %q, want the pinned fallback", got)
		}
	})

	t.Run("inbound session header reaches upstream translated", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			map[string]string{"X-Session-Id": "conv-abc"})
		if res.Code != 200 {
			t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
		}
		sess := rec.snapshot()[rec.count()-1].Header.Get("X-Opencode-Session")
		if want := identity.TranslateSessionID("conv-abc", "generic"); sess != want {
			t.Fatalf("x-opencode-session = %q, want the translated conv-abc %q", sess, want)
		}
	})
}

// TestDrainGateRejectsNewRequests: Drain() must make relay answer 503 BEFORE
// reading the body — nothing upstream is dialed after the gate trips. This is
// the deterministic unit counterpart to the e2e shutdown tests, whose drain
// poll races the listener close (connection-refused) and can never reliably
// observe the 503 branch itself.
func TestDrainGateRejectsNewRequests(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	s, mux := newRouter(up.URL, "")

	// Before draining: the request flows to the upstream.
	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		nil)
	if res.Code != 200 {
		t.Fatalf("pre-drain status = %d, body %s", res.Code, res.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream calls before drain = %d, want 1", rec.count())
	}

	s.Drain()
	res = postJSON(t, mux, "/v1/chat/completions",
		`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("drained status = %d, want 503", res.Code)
	}
	if !strings.Contains(res.Body.String(), "shutting down") {
		t.Fatalf("drained body = %q, want the shutdown error envelope", res.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream calls after drain = %d, want 1 (gate must reject before dialing)", rec.count())
	}
}
