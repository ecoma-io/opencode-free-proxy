package router

// Tests for the test-connection probe (src/sse/handlers/chat.js:91-97 +
// open-sse/utils/testConnectionHandler.js + runtimeConfig.js:102-111).

import (
	"regexp"
	"strings"
	"testing"
)

var testConnectionIDRe = regexp.MustCompile(`^router-[0-9a-f]{32}$`) // :103 + randomHex32 (:31-38)

func TestTestConnectionProbeAnswersFixedBody(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{"X-Test-Connection": "1"})
	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	// Always non-streaming JSON, even for a stream:true client
	// (testConnectionHandler.js:16-17 doc block).
	if ct := res.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if acao := res.Header().Get("Access-Control-Allow-Origin"); acao != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want * (:63)", acao)
	}
	body := mustJSON(t, res.Body.Bytes())
	if id := jstr(t, body["id"], "id"); !testConnectionIDRe.MatchString(id) {
		t.Fatalf("id = %q, want the router- + 32-hex shape", id)
	}
	if body["object"] != "chat.completion" { // :104
		t.Fatalf("object = %v, want chat.completion", body["object"])
	}
	// The model is echoed verbatim — the marker-stripped requested model.
	if body["model"] != "qwen3-coder-free" {
		t.Fatalf("model = %v, want the request model echoed", body["model"])
	}
	choice := jobj(t, jarr(t, body["choices"], "choices")[0], "choice")
	if jf64(t, choice["index"], "index") != 0 {
		t.Fatalf("choice index = %v, want 0", choice["index"])
	}
	msg := jobj(t, choice["message"], "message")
	if msg["role"] != "assistant" || msg["content"] != "Hello!" { // :105
		t.Fatalf("message = %#v, want the TEST_CONNECTION_CONTENT constant", msg)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	usage := jobj(t, body["usage"], "usage")
	if jf64(t, usage["prompt_tokens"], "prompt_tokens") != 18 ||
		jf64(t, usage["completion_tokens"], "completion_tokens") != 1 ||
		jf64(t, usage["total_tokens"], "total_tokens") != 19 {
		t.Fatalf("usage = %#v, want the TEST_CONNECTION_USAGE constant (runtimeConfig.js:106-111)", usage)
	}
	if got := jf64(t, jobj(t, usage["prompt_tokens_details"], "prompt_tokens_details")["cached_tokens"], "cached_tokens"); got != 0 {
		t.Fatalf("cached_tokens = %v, want 0", got)
	}
	// No provider call, no rotation work (chat.js:91-93).
	if n := rec.count(); n != 0 {
		t.Fatalf("probe must not reach upstream, got %d calls", n)
	}
}

func TestTestConnectionProbePresenceOnly(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	t.Run("any value triggers", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"X-Test-Connection": "false"})
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"id":"router-`) {
			t.Fatalf("status = %d body = %s, want the fixed probe body", res.Code, res.Body.String())
		}
	})

	t.Run("empty value still triggers (headers.get != null)", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"X-Test-Connection": ""})
		if !strings.Contains(res.Body.String(), `"id":"router-`) {
			t.Fatalf("body = %s, want the probe body for a present-but-empty header", res.Body.String())
		}
	})

	t.Run("case-insensitive header name", func(t *testing.T) {
		res := postJSON(t, mux, "/v1/chat/completions",
			`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-TEST-connection": "yes"})
		if !strings.Contains(res.Body.String(), `"id":"router-`) {
			t.Fatalf("body = %s, want the probe body", res.Body.String())
		}
	})
}

func TestTestConnectionProbeAbsentTakesNormalPath(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), `"Hello!"`) {
		t.Fatalf("normal request must not look like a probe: %s", res.Body.String())
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("expected the normal upstream call, got %d", n)
	}
}

func TestTestConnectionProbeStillNeedsModel(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	// The probe sits AFTER the missing-model 400 (chat.js:86-97).
	res := postJSON(t, mux, "/v1/chat/completions", `{"messages":[]}`,
		map[string]string{"X-Test-Connection": "1"})
	if res.Code != 400 {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	errObj := jobj(t, mustJSON(t, res.Body.Bytes())["error"], "error")
	if jstr(t, errObj["message"], "message") != "Model not found — body.model is required" {
		t.Fatalf("message = %v", errObj["message"])
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("upstream must not be reached, got %d calls", n)
	}
}

func TestTestConnectionProbeSharedHandlerScope(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", responsesStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	// Verified scope: the probe's only call site is src/sse/handlers/chat.js:94,
	// and handleChat serves /v1/chat/completions, /v1/messages, /v1/responses
	// AND /v1/responses/compact (each route.js imports handleChat) — so
	// /v1/responses answers the probe too, exactly like the shared relay here.
	res := postJSON(t, mux, "/v1/responses",
		`{"model":"muse-spark-1.2-contributor-free","input":"hi","stream":false}`,
		map[string]string{"X-Test-Connection": "1"})
	if res.Code != 200 {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"id":"router-`) {
		t.Fatalf("/v1/responses must answer the probe (chat.js serves both), got %s", res.Body.String())
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("probe must not reach upstream, got %d calls", n)
	}
}

func TestTestConnectionProbeEchoesMarkerStrippedModel(t *testing.T) {
	rec := &upstreamRecorder{}
	up := newScriptedUpstream(t, rec, 200, "text/event-stream", chatStreamSSE)
	defer up.Close()
	_, mux := newRouter(t, up.URL, "")

	// chat.js:96 passes modelStr — the marker-stripped value, alias intact.
	res := postJSON(t, mux, "/v1/chat/completions",
		`{"model":"oc/qwen3-coder-free[1m]","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Test-Connection": "1"})
	if !strings.Contains(res.Body.String(), `"model":"oc/qwen3-coder-free"`) {
		t.Fatalf("probe must echo the stripped model, got %s", res.Body.String())
	}
}
