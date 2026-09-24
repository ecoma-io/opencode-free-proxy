//go:build e2e

// Package e2e holds black-box end-to-end tests: the real server binary is
// compiled, launched as a subprocess with its service settings (upstream
// base) in an OCFP_CONFIG document pointing at a fake
// OpenCode Zen upstream, and then spoken
// to over HTTP exactly like an external client. Nothing is imported from
// internal/ except small JSON helpers and read-only config constants (test
// budget math only, timeouts_test.go) — every assertion goes through the wire.
//
// These tests are behind the `e2e` build tag so the default `go test ./...`
// (offline unit suite) never builds them. Run with:
//
//	go test -tags e2e ./e2e/
//
// live_test.go in this package additionally covers a real deployment and is
// gated behind E2E_LIVE=1.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/jsonx"
)

// officialUARe is the compound User-Agent shape the official opencode CLI
// sends; the forged UA for non-opencode clients must match it.
var officialUARe = regexp.MustCompile(
	`^opencode/\d+\.\d+\.\d+ ai-sdk/provider-utils/\d+\.\d+\.\d+ runtime/bun/\d+\.\d+\.\d+$`)

const (
	fakeChatID   = "chatcmpl-fake0001"
	fakeRespID   = "resp_fake0001"
	testedModel  = "qwen3-coder-free"
	museModel    = "muse-spark-1.2-contributor-free"
	bypassAnswer = "CLI Command Execution: Clear Terminal"
)

// chatSSE is the canned Chat Completions stream the fake upstream answers
// with (two chunks — the second carries usage — plus [DONE]).
const chatSSE = "data: {\"id\":\"" + fakeChatID + "\",\"object\":\"chat.completion.chunk\",\"created\":" +
	"1700000000,\"model\":\"" + testedModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"," +
	"\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"" + fakeChatID + "\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"" +
	testedModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

// responsesSSE is the canned Responses API stream for muse-spark (event-framed
// like the real upstream, terminal response.completed present, no failure).
const responsesSSE = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"" + fakeRespID + "\",\"created_at\":1700000000}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + fakeRespID + "\",\"status\":\"completed\"," +
	"\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n" +
	"data: [DONE]\n\n"

// fakeUpstream is a stand-in for https://opencode.ai/zen/v1: it mirrors the
// real free-tier gate closely enough to catch wiring regressions (stream must
// be true, the UA must look like opencode, Bearer public auth) and records
// every request for per-test assertions.
type fakeUpstream struct {
	*httptest.Server

	mu          sync.Mutex
	chatCalls   int
	respCalls   int
	modelsCalls int
	lastAuth    string
	lastUA      string
	lastSession string
	lastClient  string
	lastChat    map[string]any
	lastResp    map[string]any
	chatReply   string // settable canned chat SSE ("" = chatSSE default)
	respReply   string // settable canned responses SSE ("" = responsesSSE default)
	override    int    // when non-zero, chat/responses answer this status
}

func (f *fakeUpstream) record(kind string, r *http.Request, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "chat":
		f.chatCalls++
		f.lastChat = body
	case "responses":
		f.respCalls++
		f.lastResp = body
	case "models":
		f.modelsCalls++
	}
	f.lastAuth = r.Header.Get("Authorization")
	f.lastUA = r.Header.Get("User-Agent")
	f.lastSession = r.Header.Get("x-opencode-session")
	f.lastClient = r.Header.Get("x-opencode-client")
}

// setReplies swaps the canned SSE bodies (per-test); empty args keep the
// current value.
func (f *fakeUpstream) setReplies(chat, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if chat != "" {
		f.chatReply = chat
	}
	if resp != "" {
		f.respReply = resp
	}
}

// resetReplies restores the default canned SSE bodies. setReplies keeps the
// current value on empty args, so a `defer setReplies("", "")` is a NO-OP —
// the call sites that meant "restore after this test" were leaking their
// canned bytes into later tests on the shared instance. This is the explicit
// reverse of that intent.
func (f *fakeUpstream) resetReplies() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatReply = ""
	f.respReply = ""
}

func (f *fakeUpstream) replyFor(kind string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == "chat" {
		if f.chatReply != "" {
			return f.chatReply
		}
		return chatSSE
	}
	if f.respReply != "" {
		return f.respReply
	}
	return responsesSSE
}

func (f *fakeUpstream) upstreamHeader(t *testing.T, name string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "authorization":
		return f.lastAuth
	case "user-agent":
		return f.lastUA
	case "x-opencode-session":
		return f.lastSession
	case "x-opencode-client":
		return f.lastClient
	}
	t.Fatalf("untracked upstream header %q", name)
	return ""
}

func (f *fakeUpstream) snapshot() (chat, resp, models int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls, f.respCalls, f.modelsCalls
}

func (f *fakeUpstream) setOverride(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override = status
}

func (f *fakeUpstream) getOverride() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.override
}

func (f *fakeUpstream) lastChatBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastChat
}

func (f *fakeUpstream) lastRespBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastResp
}

// gate rejects calls the real free-tier upstream would reject: streaming is
// mandatory and the client must look like opencode (executor.go:85 forces
// stream; BuildHeaders forges the UA — a wiring regression fails here).
func (f *fakeUpstream) gate(w http.ResponseWriter, r *http.Request, body map[string]any) bool {
	if body["stream"] != true {
		http.Error(w, `{"error":{"message":"stream must be true"}}`, http.StatusBadRequest)
		return false
	}
	if !strings.Contains(r.Header.Get("User-Agent"), "opencode/") {
		http.Error(w, `{"error":{"message":"client gate"}}`, http.StatusForbidden)
		return false
	}
	return true
}

func (f *fakeUpstream) handleGeneration(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.record(kind, r, body)

		if st := f.getOverride(); st != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
			return
		}
		if !f.gate(w, r, body) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if kind == "chat" {
			_, _ = io.WriteString(w, f.replyFor("chat"))
		} else {
			_, _ = io.WriteString(w, f.replyFor("responses"))
		}
	}
}

func (f *fakeUpstream) handleModels(w http.ResponseWriter, r *http.Request) {
	f.record("models", r, nil)
	w.Header().Set("Content-Type", "application/json")
	// Unfiltered upstream list: one non-free id, one dead id, three keepers.
	_, _ = io.WriteString(w, `{"data":[
		{"id":"gpt-5"},
		{"id":"`+testedModel+`"},
		{"id":"big-pickle"},
		{"id":"deepseek-v4-flash-free"},
		{"id":"`+museModel+`"}
	]}`)
}

// syncBuf is a goroutine-safe buffer for proxy subprocess output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var (
	proxyBase string
	fake      *fakeUpstream
	proxyOut  syncBuf
	proxyCmd  *exec.Cmd
	proxyBin  string // server binary path, available to per-test spawns
)

func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e harness: %v\nproxy log:\n%s\n", err, proxyOut.String())
		code = 1
	}
	os.Exit(code)
}

func runSuite(m *testing.M) (int, error) {
	root, err := moduleRoot()
	if err != nil {
		return 1, err
	}

	tmp, err := os.MkdirTemp("", "ofp-e2e-*")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "server")

	// Tests that need their OWN server lifecycle (hot reload, graceful
	// shutdown) spawn proxyBin as a subprocess with bespoke env; the shared
	// suite instance keeps using bin via proxyCmd below.

	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goBin); err != nil {
		goBin = "go"
	}
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/server")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return 1, fmt.Errorf("go build: %v\n%s", err, out)
	}
	proxyBin = bin

	fake = &fakeUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /zen/v1/chat/completions", fake.handleGeneration("chat"))
	mux.HandleFunc("POST /zen/v1/responses", fake.handleGeneration("responses"))
	mux.HandleFunc("GET /zen/v1/models", fake.handleModels)
	fake.Server = httptest.NewServer(mux)
	defer fake.Close()

	port, err := freePort()
	if err != nil {
		return 1, err
	}
	proxyBase = "http://127.0.0.1:" + port

	// The subprocess takes its service settings (upstream.base) from an
	// OCFP_CONFIG document — the removed env vars no longer exist.
	cfgDoc := fmt.Sprintf(`upstream:
  base: %q
egress:
  - {id: direct}
routes:
  - {id: default, egress: [direct]}
`, fake.URL)
	cfgPath := filepath.Join(tmp, "cfg.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgDoc), 0o600); err != nil {
		return 1, err
	}

	proxyCmd = exec.Command(bin)
	proxyCmd.Env = append(filteredEnv("OCFP_PORT", "OCFP_CONFIG"),
		"OCFP_PORT="+port,
		"OCFP_CONFIG="+cfgPath)
	proxyCmd.Stdout = &proxyOut
	proxyCmd.Stderr = &proxyOut
	if err := proxyCmd.Start(); err != nil {
		return 1, fmt.Errorf("start server: %v", err)
	}
	defer func() {
		_ = proxyCmd.Process.Kill()
		_, _ = proxyCmd.Process.Wait()
	}()

	if err := waitHealthy(proxyBase+"/healthz", 15*time.Second); err != nil {
		return 1, err
	}
	return m.Run(), nil
}

func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..")), nil
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

// filteredEnv drops the given keys from the inherited environment so the
// subprocess config comes only from the explicit values appended after.
func filteredEnv(keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	return out
}

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("proxy never became healthy at %s", url)
}

// ---- request helpers ----

var e2eClient = &http.Client{Timeout: 30 * time.Second}

func doRequest(t *testing.T, method, path string, body any, mutate func(*http.Request)) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, proxyBase+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := e2eClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func postChat(t *testing.T, body map[string]any, mutate func(*http.Request)) *http.Response {
	t.Helper()
	return doRequest(t, "POST", "/v1/chat/completions", body, mutate)
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("response is not JSON (status %d): %v", resp.StatusCode, err)
	}
	return out
}

func errorEnvelope(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", body)
	}
	return e
}

// TestConfigDrivenServiceSettings: the shared suite proxy is spawned from an
// OCFP_CONFIG document (upstream.base) — no service env vars remain. This
// test asserts the config actually drove the runtime: the upstream call
// reached the configured base, which the suite only wires as upstream.base.
func TestConfigDrivenServiceSettings(t *testing.T) {
	resp := postChat(t, chatBody(testedModel), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	up := fake.lastChatBody()
	if up == nil {
		t.Fatal("upstream never received the chat call")
	}
}

// chatBody is the minimal non-streaming chat request used by most tests.
func chatBody(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	}
}

// toolNames collects tool names from an upstream body (both the bare
// {"name":...} and function-wrapped spellings).
func toolNames(body map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, raw := range jsonx.AsArr(body["tools"]) {
		obj := jsonx.AsObj(raw)
		if obj == nil {
			continue
		}
		if n := jsonx.AsStr(obj["name"]); n != "" {
			names[n] = true
		}
		if fn := jsonx.AsObj(obj["function"]); fn != nil {
			if n := jsonx.AsStr(fn["name"]); n != "" {
				names[n] = true
			}
		}
	}
	return names
}

// ---- tests ----

// TestOfficialUAPassthrough: a client that already sends the exact compound
// official User-Agent (opencode >= 1.17) has it forwarded byte-identical —
// the forging path must not rewrite a valid downstream UA.
func TestOfficialUAPassthrough(t *testing.T) {
	// Captured from the official v1.18.31 binary (docs/recon-opencode-ua.md).
	official := "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	resp := postChat(t, chatBody(testedModel), setUpstreamUA(t, official))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := fake.upstreamHeader(t, "user-agent"); got != official {
		t.Fatalf("upstream UA = %q, want byte-identical passthrough of %q", got, official)
	}
}

func TestHealthz(t *testing.T) {
	resp := doRequest(t, "GET", "/healthz", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Fatalf("body = %q, want %q", b, "ok")
	}
}

func TestChatNonStreaming(t *testing.T) {
	resp := postChat(t, chatBody(testedModel), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", body["object"])
	}
	if body["id"] != fakeChatID {
		t.Fatalf("id = %v, want %s (folded from the upstream stream)", body["id"], fakeChatID)
	}
	arr := jsonx.AsArr(body["choices"])
	if len(arr) != 1 {
		t.Fatalf("choices = %v", body["choices"])
	}
	choice := jsonx.AsObj(arr[0])
	msg := jsonx.AsObj(choice["message"])
	if got := jsonx.AsStr(msg["content"]); got != "Hello world" {
		t.Fatalf("content = %q, want aggregated %q", got, "Hello world")
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := jsonx.AsObj(body["usage"])
	if usage == nil || usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(2) || usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %v, want 5/2/7 from the final chunk", body["usage"])
	}

	// Upstream-side assertions: the executor forced streaming, cleaned the
	// model, forged the public bearer + opencode UA and injected the
	// fingerprint quartet.
	up := fake.lastChatBody()
	if up == nil {
		t.Fatal("upstream never received the chat call")
	}
	if up["stream"] != true {
		t.Fatalf("upstream stream = %v, want true (free-tier gate)", up["stream"])
	}
	if up["model"] != testedModel {
		t.Fatalf("upstream model = %v, want %s", up["model"], testedModel)
	}
	fake.mu.Lock()
	auth, ua := fake.lastAuth, fake.lastUA
	fake.mu.Unlock()
	if auth != "Bearer public" {
		t.Fatalf("upstream auth = %q, want Bearer public", auth)
	}
	if !strings.Contains(ua, "opencode/") {
		t.Fatalf("upstream UA = %q, want opencode/*", ua)
	}
	// The default client UA is not opencode, so this hit the FORGING path:
	// it must render the full compound shape the official CLI sends, not a
	// bare opencode/<version>.
	if !officialUARe.MatchString(ua) {
		t.Fatalf("forged upstream UA = %q, want official compound shape opencode/<v> ai-sdk/provider-utils/<v> runtime/bun/<v>", ua)
	}
	for _, want := range []string{"bash", "glob", "grep", "read"} {
		if !toolNames(up)[want] {
			t.Fatalf("fingerprint tool %q missing from upstream tools %v", want, up["tools"])
		}
	}
}

func TestChatStreaming(t *testing.T) {
	body := chatBody(testedModel)
	body["stream"] = true
	resp := postChat(t, body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	sse := string(raw)
	if !strings.Contains(sse, `"content":"Hello"`) || !strings.Contains(sse, `"content":" world"`) {
		t.Fatalf("stream missing content deltas:\n%s", sse)
	}
	// The usage-bearing chunk is re-serialized by the usage seam (Go marshals
	// map keys sorted), so assert the numbers, not a key order.
	for _, frag := range []string{`"prompt_tokens":5`, `"completion_tokens":2`, `"total_tokens":7`} {
		if !strings.Contains(sse, frag) {
			t.Fatalf("usage chunk not forwarded (missing %s):\n%s", frag, sse)
		}
	}
	if !strings.HasSuffix(sse, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with data: [DONE]:\n%s", sse)
	}
	// chat→chat parity: the upstream [DONE] is forwarded AND the relay adds
	// its own terminator (verified against 9router chat.js in round 2).
	if n := strings.Count(sse, "data: [DONE]"); n != 2 {
		t.Fatalf("data: [DONE] count = %d, want 2 (forwarded + relay terminator):\n%s", n, sse)
	}
}

func TestResponsesNonStreaming(t *testing.T) {
	resp := doRequest(t, "POST", "/v1/responses", map[string]any{
		"model":  museModel,
		"input":  "hi",
		"stream": false,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["object"] != "response" {
		t.Fatalf("object = %v, want response", body["object"])
	}
	if body["id"] != fakeRespID {
		t.Fatalf("id = %v, want %s", body["id"], fakeRespID)
	}
	if body["status"] != "completed" {
		t.Fatalf("status = %v, want completed (response.completed seen)", body["status"])
	}
	usage := jsonx.AsObj(body["usage"])
	if usage == nil || usage["total_tokens"] != float64(4) {
		t.Fatalf("usage = %v, want total 4", body["usage"])
	}

	// The muse-spark request must have gone to /zen/v1/responses, streamed.
	fake.mu.Lock()
	auth := fake.lastAuth
	fake.mu.Unlock()
	if auth != "Bearer public" {
		t.Fatalf("upstream auth = %q", auth)
	}
	up := fake.lastRespBody()
	if up == nil {
		t.Fatal("upstream never received the responses call")
	}
	if up["stream"] != true {
		t.Fatalf("upstream stream = %v, want true", up["stream"])
	}
}

func TestResponsesStreaming(t *testing.T) {
	resp := doRequest(t, "POST", "/v1/responses", map[string]any{
		"model":  museModel,
		"input":  "hi",
		"stream": true,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	sse := string(raw)
	if !strings.Contains(sse, fakeRespID) {
		t.Fatalf("stream missing response id:\n%s", sse)
	}
	if !strings.Contains(sse, "response.output_text.delta") {
		t.Fatalf("stream missing delta events:\n%s", sse)
	}
	// Terminal event present → the failed-synthesis seam must NOT fire.
	if strings.Contains(sse, "response.failed") {
		t.Fatalf("response.failed synthesized despite a completed stream:\n%s", sse)
	}
	if !strings.HasSuffix(sse, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with data: [DONE]:\n%s", sse)
	}
}

func TestUpstreamErrorSurfacesAsEnvelope(t *testing.T) {
	fake.setOverride(http.StatusForbidden)
	defer fake.setOverride(0)
	resp := postChat(t, chatBody(testedModel), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	e := errorEnvelope(t, body)
	msg, _ := e["message"].(string)
	if !strings.HasPrefix(msg, "[403]: ") || !strings.Contains(msg, "quota exhausted") {
		t.Fatalf("message = %q, want %q prefix with upstream text", msg, "[403]: ")
	}
	if e["code"] != "insufficient_quota" || e["type"] != "permission_error" {
		t.Fatalf("error envelope = %v", e)
	}
}

func TestTestConnectionProbe(t *testing.T) {
	chatBefore, respBefore, _ := fake.snapshot()
	resp := postChat(t, chatBody(testedModel), func(r *http.Request) {
		r.Header.Set("X-Test-Connection", "1")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v", body["object"])
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "router-") || len(id) != len("router-")+32 {
		t.Fatalf("id = %q, want router-<32 hex>", id)
	}
	if body["model"] != testedModel {
		t.Fatalf("model = %v, want echoed %s", body["model"], testedModel)
	}
	arr := jsonx.AsArr(body["choices"])
	if len(arr) != 1 {
		t.Fatalf("choices = %v", body["choices"])
	}
	msg := jsonx.AsObj(jsonx.AsObj(arr[0])["message"])
	if msg["content"] != "Hello!" {
		t.Fatalf("content = %v, want fixed probe text", msg["content"])
	}
	usage := jsonx.AsObj(body["usage"])
	if usage == nil || usage["prompt_tokens"] != float64(18) || usage["completion_tokens"] != float64(1) || usage["total_tokens"] != float64(19) {
		t.Fatalf("usage = %v, want probe constants 18/1/19", body["usage"])
	}
	chatAfter, respAfter, _ := fake.snapshot()
	if chatBefore != chatAfter || respBefore != respAfter {
		t.Fatalf("probe hit upstream: chat %d→%d responses %d→%d", chatBefore, chatAfter, respBefore, respAfter)
	}
}

func TestClaudeCLIBypassWarmup(t *testing.T) {
	chatBefore, respBefore, _ := fake.snapshot()
	resp := postChat(t, map[string]any{
		"model":    testedModel,
		"messages": []any{map[string]any{"role": "user", "content": "Warmup"}},
		"stream":   false,
	}, func(r *http.Request) {
		r.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v", body["object"])
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %q, want chatcmpl- prefix", id)
	}
	arr := jsonx.AsArr(body["choices"])
	if len(arr) != 1 {
		t.Fatalf("choices = %v", body["choices"])
	}
	msg := jsonx.AsObj(jsonx.AsObj(arr[0])["message"])
	if msg["content"] != bypassAnswer {
		t.Fatalf("content = %v, want bypass text %q", msg["content"], bypassAnswer)
	}
	chatAfter, respAfter, _ := fake.snapshot()
	if chatBefore != chatAfter || respBefore != respAfter {
		t.Fatalf("bypass hit upstream: chat %d→%d responses %d→%d", chatBefore, chatAfter, respBefore, respAfter)
	}
}

func TestMarkerStrippedBeforeDispatch(t *testing.T) {
	resp := postChat(t, chatBody("big-pickle[1m]"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (marker stripped, model resolved)", resp.StatusCode)
	}
	up := fake.lastChatBody()
	if up["model"] != "big-pickle" {
		t.Fatalf("upstream model = %v, want [1m] stripped to big-pickle", up["model"])
	}
}

func TestImageStrippedForNonVisionModel(t *testing.T) {
	body := map[string]any{
		"model": testedModel, // *qwen*coder* → no vision
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}}},
		"stream": false,
	}
	resp := postChat(t, body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := json.Marshal(fake.lastChatBody())
	if err != nil {
		t.Fatal(err)
	}
	up := string(raw)
	if strings.Contains(up, "image_url") || strings.Contains(up, "AAAA") {
		t.Fatalf("image data reached the upstream body:\n%s", up)
	}
	if !strings.Contains(up, "[image omitted: model has no vision support]") {
		t.Fatalf("vision placeholder missing from upstream body:\n%s", up)
	}
}

func TestModelsFiltered(t *testing.T) {
	resp := doRequest(t, "GET", "/v1/models", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["object"] != "list" {
		t.Fatalf("object = %v, want list", body["object"])
	}
	var ids []string
	for _, raw := range jsonx.AsArr(body["data"]) {
		e := jsonx.AsObj(raw)
		ids = append(ids, jsonx.AsStr(e["id"]))
	}
	want := []string{"big-pickle", museModel, testedModel}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want exactly %v (non-free and dead filtered)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want sorted %v", ids, want)
		}
	}
	fake.mu.Lock()
	auth := fake.lastAuth
	fake.mu.Unlock()
	if auth != "Bearer public" {
		t.Fatalf("models upstream auth = %q, want Bearer public", auth)
	}
}
