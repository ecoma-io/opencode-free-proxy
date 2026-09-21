package upstream

// Tests for the OpenCode executor boundary (executors/opencode.js):
// transformRequest (exact statement order), buildHeaders, resolveOpencodeSession,
// and buildUrl.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/jsonx"
)

func bodyFrom(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	return body
}

// toolNames flattens a tools array into name strings for either wire shape
// (chat: tool.function.name, responses: tool.name) — opencode.js toolNameOf.
func toolNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	tools := jsonx.AsArr(body["tools"])
	if tools == nil {
		t.Fatalf("body has no tools array: %#v", body["tools"])
	}
	var names []string
	for _, raw := range tools {
		tool := jsonx.AsObj(raw)
		if tool == nil {
			continue
		}
		if n := jsonx.AsStr(tool["name"]); n != "" {
			names = append(names, n)
			continue
		}
		names = append(names, jsonx.AsStr(jsonx.Get(tool["function"], "name")))
	}
	return names
}

// TestTransformRequestChatModel covers the non-Responses branch of
// opencode.js transformRequest: falsy model refill, forced stream:true,
// the fingerprint quartet with tool_choice "none" for tool-less callers, and
// the reasoning_content injector being a no-op for non-deepseek models.
func TestTransformRequestChatModel(t *testing.T) {
	body := bodyFrom(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	TransformRequest("qwen3-coder-free", body)

	if body["model"] != "qwen3-coder-free" {
		t.Fatalf("model = %v, want the executor model refilled", body["model"])
	}
	// Free-tier gate: stream:false → 403 FreeTierError, so always stream upstream
	// (opencode.js transformRequest: `body.stream = true`).
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	// ensureChatFingerprintTools: {bash, glob, grep, read} in registry order.
	names := toolNames(t, body)
	if !reflect.DeepEqual(names, []string{"bash", "glob", "grep", "read"}) {
		t.Fatalf("tools = %v, want the fingerprint quartet", names)
	}
	// Cloak contract: no caller tools → tool_choice "none".
	if body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want %q", body["tool_choice"], "none")
	}
	// injectReasoningContent only fires for /deepseek/i models.
	if _, has := body["reasoning_content"]; has {
		t.Fatal("reasoning_content must not appear at body level")
	}
}

// TestTransformRequestCallerToolsPreserved: opencode.js ensureChatFingerprintTools
// keeps caller tools verbatim, appends only missing quartet members, and
// leaves tool_choice alone when the caller sent tools.
func TestTransformRequestCallerToolsPreserved(t *testing.T) {
	body := bodyFrom(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`)
	TransformRequest("qwen3-coder-free", body)

	names := toolNames(t, body)
	if !reflect.DeepEqual(names, []string{"bash", "glob", "grep", "read"}) {
		t.Fatalf("tools = %v, want caller bash kept + missing quartet members appended", names)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want the caller's choice preserved", body["tool_choice"])
	}
}

// TestTransformRequestModelFillTable: JS `model && !body.model` — falsy model
// fields (absent/null/""/0) are refilled, truthy ones survive.
func TestTransformRequestModelFillTable(t *testing.T) {
	cases := []struct {
		name   string
		model  any // value for body.model; nil sentinel `absent` = key missing
		absent bool
		want   any // expected body.model after the transform
	}{
		{name: "absent", absent: true, want: "qwen3-coder-free"},
		{name: "null", model: nil, want: "qwen3-coder-free"},
		{name: "empty string", model: "", want: "qwen3-coder-free"},
		{name: "zero", model: float64(0), want: "qwen3-coder-free"},
		{name: "truthy id kept", model: "caller-model", want: "caller-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"messages": []any{}}
			if !tc.absent {
				body["model"] = tc.model
			}
			TransformRequest("qwen3-coder-free", body)
			if body["model"] != tc.want {
				t.Fatalf("model = %v, want %v", body["model"], tc.want)
			}
		})
	}
}

// TestTransformRequestMuseResponses covers the Responses branch of
// opencode.js transformRequest in statement order: input normalization,
// max_output_tokens folding + the >=16 clamp, normalizeOpencodeReasoning,
// store:false, the flat fingerprint quartet with tool_choice auto, the
// muse-spark-only tool_choice demotion, and reasoning item sanitization.
func TestTransformRequestMuseResponses(t *testing.T) {
	const muse = "muse-spark-1.2-contributor-free"

	t.Run("input normalization and field folding", func(t *testing.T) {
		body := bodyFrom(t, `{
			"input":"hello",
			"max_tokens":5,
			"reasoning_effort":"high",
			"tool_choice":"required"
		}`)
		TransformRequest(muse, body)

		if body["model"] != muse {
			t.Fatalf("model = %v, want %q", body["model"], muse)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v, want true", body["stream"])
		}
		// normalizeResponsesInput("hello") → one user message with input_text.
		input := jsonx.AsArr(body["input"])
		if len(input) != 1 {
			t.Fatalf("input = %#v, want one message item", body["input"])
		}
		msg := jsonx.AsObj(input[0])
		if jsonx.AsStr(msg["type"]) != "message" || jsonx.AsStr(msg["role"]) != "user" {
			t.Fatalf("input item = %#v, want a user message", msg)
		}
		content := jsonx.AsArr(msg["content"])
		part := jsonx.AsObj(content[0])
		if jsonx.AsStr(part["type"]) != "input_text" || jsonx.AsStr(part["text"]) != "hello" {
			t.Fatalf("content part = %#v, want input_text hello", part)
		}
		// max_tokens:5 folds into max_output_tokens and is clamped to the
		// upstream floor 16 (`Math.max(16, Number(x) || 0)`).
		if got := jsonx.AsF64(body["max_output_tokens"]); got != config.MinMaxOutputTokens {
			t.Fatalf("max_output_tokens = %v, want 16", body["max_output_tokens"])
		}
		if _, has := body["max_tokens"]; has {
			t.Fatal("max_tokens must be deleted")
		}
		if _, has := body["max_completion_tokens"]; has {
			t.Fatal("max_completion_tokens must be deleted")
		}
		// normalizeOpencodeReasoning: reasoning_effort → reasoning:{effort,summary:auto}.
		reasoning := jsonx.AsObj(body["reasoning"])
		if reasoning == nil || jsonx.AsStr(reasoning["effort"]) != "high" || jsonx.AsStr(reasoning["summary"]) != "auto" {
			t.Fatalf("reasoning = %#v, want {effort:high,summary:auto}", body["reasoning"])
		}
		if _, has := body["reasoning_effort"]; has {
			t.Fatal("reasoning_effort must be folded into reasoning and deleted")
		}
		// store:false (stateless free tier).
		if body["store"] != false {
			t.Fatalf("store = %v, want false", body["store"])
		}
		// Flat (Responses-shape) fingerprint quartet.
		tools := jsonx.AsArr(body["tools"])
		if len(tools) != 4 {
			t.Fatalf("tools = %d items, want the 4 fingerprints", len(tools))
		}
		if n := toolNames(t, body); !reflect.DeepEqual(n, []string{"bash", "glob", "grep", "read"}) {
			t.Fatalf("tools = %v, want the quartet", n)
		}
		if tool := jsonx.AsObj(tools[0]); jsonx.AsStr(tool["function"]) != "" && jsonx.AsObj(tool["function"]) != nil {
			t.Fatalf("responses tools must be flat, got %#v", tool)
		}
		// Muse Spark free models are auto-only upstream: "required" → "auto".
		if body["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %v, want demoted to auto", body["tool_choice"])
		}
	})

	t.Run("empty input becomes the ... placeholder", func(t *testing.T) {
		for name, input := range map[string]any{
			"empty string": "",
			"whitespace":   "   ",
			"empty array":  []any{},
		} {
			body := map[string]any{"input": input}
			TransformRequest(muse, body)
			arr := jsonx.AsArr(body["input"])
			if len(arr) != 1 {
				t.Fatalf("%s: input = %#v, want placeholder message", name, body["input"])
			}
			msg := jsonx.AsObj(arr[0])
			part := jsonx.AsObj(jsonx.AsArr(msg["content"])[0])
			if got := jsonx.AsStr(part["text"]); got != "..." {
				t.Fatalf("%s: placeholder text = %q, want \"...\"", name, got)
			}
		}
	})

	t.Run("non-array garbage input is replaced by the placeholder", func(t *testing.T) {
		body := map[string]any{"input": float64(42)}
		TransformRequest(muse, body)
		if len(jsonx.AsArr(body["input"])) != 1 {
			t.Fatalf("input = %#v, want the placeholder message array", body["input"])
		}
	})

	t.Run("max_output_tokens clamping table", func(t *testing.T) {
		cases := []struct {
			field string
			value any
			want  float64
		}{
			{"max_tokens", "5", 16},         // Number("5") = 5 → clamp to 16
			{"max_tokens", float64(20), 20}, // unchanged
			{"max_completion_tokens", float64(7), 16},
			{"max_completion_tokens", float64(1024), 1024},
		}
		for _, tc := range cases {
			body := map[string]any{tc.field: tc.value}
			TransformRequest(muse, body)
			if got := jsonx.AsF64(body["max_output_tokens"]); got != tc.want {
				t.Fatalf("%s=%v → max_output_tokens %v, want %v", tc.field, tc.value, body["max_output_tokens"], tc.want)
			}
			if _, has := body[tc.field]; has {
				t.Fatalf("%s must be deleted", tc.field)
			}
		}
	})

	// String coercion table for Math.max(16, Number(x) || 0)
	// (executors/opencode.js:362). Numeric strings fold to their Number();
	// garbage strings are NaN → 0 → the 16 floor; the exact Infinity
	// spellings survive `|| 0` (Infinity is truthy) and reach the wire as
	// null — JSON.stringify serializes non-finite numbers as null
	// (executors/base.js:141), which Go mirrors with nil so json.Marshal
	// (router/handler.go:303) emits the same bytes instead of erroring.
	t.Run("max_output_tokens string coercion", func(t *testing.T) {
		cases := []struct {
			name  string
			value any
			want  any
		}{
			{"numeric string above the floor", "1024", float64(1024)},
			{"partial prefix is NaN in JS", "12abc", float64(config.MinMaxOutputTokens)},
			{"trailing junk is NaN in JS", "50abc", float64(config.MinMaxOutputTokens)},
			{"numeric separator is NaN in JS", "1_000", float64(config.MinMaxOutputTokens)},
			{"inf is not a JS infinity spelling", "inf", float64(config.MinMaxOutputTokens)},
			{"NaN word is falsy in JS", "NaN", float64(config.MinMaxOutputTokens)},
			{"JSON null is 0 in JS", nil, float64(config.MinMaxOutputTokens)},
			{"-Infinity clamps to the floor (Math.max(16,-Infinity))", "-Infinity", float64(config.MinMaxOutputTokens)},
			{"Infinity survives || 0 and wires as null", "Infinity", nil},
			{"+Infinity survives || 0 and wires as null", "+Infinity", nil},
			{"overflow literal is Infinity in JS (Number(\"1e999\"))", "1e999", nil},
		}
		for _, tc := range cases {
			body := map[string]any{"max_output_tokens": tc.value}
			TransformRequest(muse, body)
			if got := body["max_output_tokens"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: max_output_tokens(%v) = %#v, want %#v", tc.name, tc.value, got, tc.want)
			}
			// Every row must stay marshalable: a +Inf/NaN that leaked here
			// would kill the whole request at router/handler.go:303.
			if b, err := json.Marshal(body); err != nil {
				t.Fatalf("%s: marshal after clamp: %v", tc.name, err)
			} else if tc.want == nil && !strings.Contains(string(b), `"max_output_tokens":null`) {
				t.Fatalf("%s: wire = %s, want max_output_tokens:null", tc.name, b)
			}
		}
	})

	t.Run("existing reasoning object survives the fold", func(t *testing.T) {
		// JS: body.reasoning = { ...currentReasoning, effort }; summary kept when truthy.
		body := map[string]any{
			"input":            "hello",
			"reasoning":        map[string]any{"summary": "concise"},
			"reasoning_effort": "low",
		}
		TransformRequest(muse, body)
		reasoning := jsonx.AsObj(body["reasoning"])
		if reasoning == nil || jsonx.AsStr(reasoning["effort"]) != "low" || jsonx.AsStr(reasoning["summary"]) != "concise" {
			t.Fatalf("reasoning = %#v, want {effort:low,summary:concise}", body["reasoning"])
		}
	})

	t.Run("reasoning items are stripped and tool items coerced", func(t *testing.T) {
		body := bodyFrom(t, `{
			"input":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"hmm"}],"encrypted_content":"blob"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
			]
		}`)
		TransformRequest(muse, body)
		input := jsonx.AsArr(body["input"])
		if len(input) != 1 {
			t.Fatalf("input = %#v, want the reasoning item dropped", body["input"])
		}
		if jsonx.AsStr(jsonx.AsObj(input[0])["type"]) != "message" {
			t.Fatalf("surviving item = %#v, want the user message", input[0])
		}
	})

	t.Run("effort demotion ladder", func(t *testing.T) {
		for _, level := range []string{"high", "xhigh", "none"} {
			body := map[string]any{"input": "hello", "reasoning_effort": level}
			TransformRequest(muse, body)
			if got := jsonx.AsStr(jsonx.Get(body["reasoning"], "effort")); got != level {
				t.Fatalf("effort %q demoted to %q, want it kept", level, got)
			}
		}
		// max/ultra are not on the muse-spark ladder → xhigh.
		body := map[string]any{"input": "hello", "reasoning_effort": "max"}
		TransformRequest(muse, body)
		if got := jsonx.AsStr(jsonx.Get(body["reasoning"], "effort")); got != "xhigh" {
			t.Fatalf("effort max → %q, want xhigh", got)
		}
	})
}

// TestTransformRequestDeepseekReasoning ports utils/reasoningContentInjector.js:
// /deepseek/i models get a non-empty reasoning_content placeholder on every
// assistant message that lacks one.
func TestTransformRequestDeepseekReasoning(t *testing.T) {
	body := bodyFrom(t, `{
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"assistant","content":"kept","reasoning_content":"already here"}
		]
	}`)
	TransformRequest("deepseek-v4-flash-free", body)

	messages := jsonx.AsArr(body["messages"])
	first := jsonx.AsObj(messages[1])
	if got := jsonx.AsStr(first["reasoning_content"]); got != " " {
		t.Fatalf("assistant reasoning_content = %q, want the \" \" placeholder", got)
	}
	second := jsonx.AsObj(messages[2])
	if got := jsonx.AsStr(second["reasoning_content"]); got != "already here" {
		t.Fatalf("existing reasoning_content clobbered: %q", got)
	}
	if _, has := jsonx.AsObj(messages[0])["reasoning_content"]; has {
		t.Fatal("user messages must not get reasoning_content")
	}
}

// TestBuildURL: muse-spark → /zen/v1/responses, everything else →
// /zen/v1/chat/completions (opencode.js buildUrl), independent of any Server.
func TestBuildURL(t *testing.T) {
	base := "http://upstream.test"
	if got := BuildURL(base, "muse-spark-1.3-contributor-free"); got != base+"/zen/v1/responses" {
		t.Fatalf("muse URL = %q", got)
	}
	if got := BuildURL(base, "muse-spark-1.2-contributor-free(high)"); got != base+"/zen/v1/responses" {
		t.Fatalf("suffixed muse URL = %q (thinking suffix must be ignored for routing)", got)
	}
	if got := BuildURL(base, "qwen3-coder-free"); got != base+"/zen/v1/chat/completions" {
		t.Fatalf("chat URL = %q", got)
	}
	if got := BuildURL(base, "big-pickle"); got != base+"/zen/v1/chat/completions" {
		t.Fatalf("big-pickle URL = %q", got)
	}
}

// TestBuildHeaders ports OpenCodeExecutor.buildHeaders.
func TestBuildHeaders(t *testing.T) {
	t.Run("valid downstream UA passes through", func(t *testing.T) {
		h := BuildHeaders(Downstream{Headers: map[string]string{"user-agent": "opencode/1.19.0"}},
			"ses_native", identity.FallbackUA())
		if h["User-Agent"] != "opencode/1.19.0" {
			t.Fatalf("User-Agent = %q, want the downstream value (>= 1.17)", h["User-Agent"])
		}
	})

	t.Run("invalid or absent downstream UA is forged", func(t *testing.T) {
		for _, ua := range []string{"", "curl/8.0", "opencode/1.16.9", "opencode"} {
			headers := map[string]string{}
			if ua != "" {
				headers["user-agent"] = ua
			}
			h := BuildHeaders(Downstream{Headers: headers}, "ses_native", identity.FallbackUA())
			if h["User-Agent"] != identity.FallbackUA() {
				t.Fatalf("UA %q → User-Agent %q, want the pinned fallback %q", ua, h["User-Agent"], identity.FallbackUA())
			}
		}
	})

	t.Run("stable header set", func(t *testing.T) {
		h := BuildHeaders(Downstream{Headers: map[string]string{}}, "ses_abc", identity.FallbackUA())
		if h["Authorization"] != "Bearer "+config.PublicBearer {
			t.Fatalf("Authorization = %q", h["Authorization"])
		}
		if h["Accept"] != "text/event-stream" {
			t.Fatalf("Accept = %q (upstream always streams)", h["Accept"])
		}
		if h["Content-Type"] != "application/json" {
			t.Fatalf("Content-Type = %q", h["Content-Type"])
		}
		if h["x-opencode-session"] != "ses_abc" {
			t.Fatalf("x-opencode-session = %q, want the resolved session argument", h["x-opencode-session"])
		}
		if h["x-opencode-client"] != "desktop" {
			t.Fatalf("x-opencode-client = %q, want the desktop default", h["x-opencode-client"])
		}
		if h["x-opencode-project"] != "global" {
			t.Fatalf("x-opencode-project = %q, want the global default", h["x-opencode-project"])
		}
		if !identity.RequestRE.MatchString(h["x-opencode-request"]) {
			t.Fatalf("x-opencode-request = %q, want a generated msg_ id", h["x-opencode-request"])
		}
	})

	t.Run("client passthroughs win when set", func(t *testing.T) {
		h := BuildHeaders(Downstream{Headers: map[string]string{
			"x-opencode-client":  "cli",
			"x-opencode-request": "msg_downstream",
			"x-opencode-project": "proj-7",
		}}, "ses_abc", identity.FallbackUA())
		if h["x-opencode-client"] != "cli" || h["x-opencode-request"] != "msg_downstream" || h["x-opencode-project"] != "proj-7" {
			t.Fatalf("passthroughs lost: %#v", h)
		}
	})

	t.Run("generated request ids are unique per call", func(t *testing.T) {
		a := BuildHeaders(Downstream{}, "s", identity.FallbackUA())["x-opencode-request"]
		time.Sleep(time.Millisecond)
		b := BuildHeaders(Downstream{}, "s", identity.FallbackUA())["x-opencode-request"]
		if a == b {
			t.Fatalf("request ids must differ per call, got %q twice", a)
		}
	})
}

// TestResolveSession ports resolveOpencodeSession.
func TestResolveSession(t *testing.T) {
	// 12 hex + 14 base62 after the ses_ prefix (OPENCODE_SESSION_RE).
	const native = "ses_abcdef012345ABCDEF01234567"

	t.Run("native downstream session passes verbatim", func(t *testing.T) {
		d := Downstream{Headers: map[string]string{"x-opencode-session": native}}
		if got := ResolveSession(map[string]any{}, d); got != native {
			t.Fatalf("session = %q, want %q untouched", got, native)
		}
	})

	t.Run("invalid native session is translated deterministically", func(t *testing.T) {
		d := Downstream{Headers: map[string]string{"x-opencode-session": "not-a-session"}}
		first := ResolveSession(map[string]any{}, d)
		second := ResolveSession(map[string]any{}, d)
		if first != second {
			t.Fatalf("translation not deterministic: %q vs %q", first, second)
		}
		if !identity.SessionRE.MatchString(first) {
			t.Fatalf("translated session %q is not a valid opencode id", first)
		}
		if first == "not-a-session" {
			t.Fatal("invalid session must not pass through verbatim")
		}
		// translateSessionId hashes `opencode\0generic\0<session>` (clientTool
		// defaults to "generic") — a 26-char suffix.
		if len(strings.TrimPrefix(first, "ses_")) != 26 {
			t.Fatalf("translated id %q should be ses_ + 26 chars", first)
		}
	})

	t.Run("body prompt_cache_key is translated when no header is sent", func(t *testing.T) {
		body := map[string]any{"prompt_cache_key": "conv-42"}
		got := ResolveSession(body, Downstream{Headers: map[string]string{}})
		want := identity.TranslateSessionID("conv-42", "generic")
		if got != want {
			t.Fatalf("session = %q, want the translated prompt_cache_key %q", got, want)
		}
		if !identity.SessionRE.MatchString(got) {
			t.Fatalf("session %q is not a valid opencode id", got)
		}
	})

	t.Run("nothing at all generates a fresh valid session id", func(t *testing.T) {
		got := ResolveSession(map[string]any{}, Downstream{Headers: map[string]string{}})
		if !identity.SessionRE.MatchString(got) {
			t.Fatalf("generated session %q is not a valid opencode id", got)
		}
		// The generated id runs through translateSessionId like every other
		// fallback, so re-translating it is a fixed point.
		if again := identity.TranslateSessionID(got, "generic"); again != got {
			t.Fatalf("generated id %q is not canonical (translate fixed point gave %q)", got, again)
		}
	})
}
