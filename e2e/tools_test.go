//go:build e2e

// Tool-pipeline e2e: clients with different tool interfaces (Claude Code,
// plain OpenAI callers, Responses clients) must all reach the upstream
// correctly through the wire — dedupe, fingerprint injection, tool_choice
// normalization, tool-call round trips and session/header personas.
package e2e

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"opencode-free-proxy/internal/jsonx"
)

// chatTool builds a Chat Completions tool declaration.
func chatTool(name, desc string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc, "parameters": map[string]any{"type": "object"},
	}}
}

// upstreamToolNames fetches the tool-name set of the last recorded upstream
// body of the given kind.
func upstreamToolNames(t *testing.T, kind string) map[string]bool {
	t.Helper()
	body := fake.lastChatBody()
	if kind == "responses" {
		body = fake.lastRespBody()
	}
	if body == nil {
		t.Fatalf("upstream never received a %s call", kind)
	}
	return toolNames(body)
}

func setUpstreamUA(t *testing.T, ua string) func(*http.Request) {
	t.Helper()
	return func(r *http.Request) { r.Header.Set("User-Agent", ua) }
}

// toolCallSSE is a canned chat stream where the model calls a tool: the
// tool_call id/name arrive in chunk 1 and the arguments continue in chunk 2
// (finish_reason tool_calls) — the aggregation seam must reassemble them.
const toolCallSSE = "data: {\"id\":\"" + fakeChatID + "\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"" + testedModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc123\",\"type\":\"function\",\"function\":{\"name\":\"read_\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"" + fakeChatID + "\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"" + testedModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"file\",\"arguments\":\"{\\\"path\\\":\\\"main.go\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// claudeCodeTools is what a Claude Code session typically declares: built-ins
// (capitalized), plus an Exa MCP server whose presence must strip the
// duplicated built-in web tools (toolDeduper rule 1).
func claudeCodeTools() []any {
	return []any{
		chatTool("WebSearch", "built-in web search"),
		chatTool("WebFetch", "built-in web fetch"),
		map[string]any{"type": "function", "function": map[string]any{
			"name": "mcp__exa__web_search_exa", "parameters": map[string]any{"type": "object"},
		}},
		chatTool("Bash", "run shell commands"),
	}
}

// TestClaudeCodeToolsDedupedAndFingerprinted: the claude persona's WebSearch/
// WebFetch are dropped because the Exa MCP tool is present, every other tool
// survives verbatim, and the lowercase fingerprint quartet is injected — the
// client's capitalized "Bash" does NOT satisfy the gate, so "bash" must be
// added alongside it.
func TestClaudeCodeToolsDedupedAndFingerprinted(t *testing.T) {
	resp := postChat(t, map[string]any{
		"model":    testedModel,
		"messages": []any{map[string]any{"role": "user", "content": "search the web"}},
		"tools":    claudeCodeTools(),
		"stream":   false,
	}, setUpstreamUA(t, "claude-code/2.0.14 (external, cli)"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	names := upstreamToolNames(t, "chat")
	for _, gone := range []string{"WebSearch", "WebFetch", "mcp__workspace__web_fetch"} {
		if names[gone] {
			t.Fatalf("tool %q survived dedupe; upstream tools = %v", gone, names)
		}
	}
	for _, kept := range []string{"mcp__exa__web_search_exa", "Bash"} {
		if !names[kept] {
			t.Fatalf("client tool %q was dropped; upstream tools = %v", kept, names)
		}
	}
	for _, fp := range []string{"bash", "glob", "grep", "read"} {
		if !names[fp] {
			t.Fatalf("fingerprint %q missing; upstream tools = %v", fp, names)
		}
	}

	// A caller that sent its own tools must not be forced to tool_choice none.
	up := fake.lastChatBody()
	if choice, has := up["tool_choice"]; has && choice == "none" {
		t.Fatal("tool_choice forced to none despite caller-declared tools")
	}
}

// TestXAppCliDetectsClaudeClient: the x-app: cli header is an alternative
// claude-client signal (clientDetector.js) — dedupe fires without a claude UA.
func TestXAppCliDetectsClaudeClient(t *testing.T) {
	resp := postChat(t, map[string]any{
		"model":    testedModel,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    claudeCodeTools(),
		"stream":   false,
	}, func(r *http.Request) {
		r.Header.Set("X-App", "cli")
	})
	defer resp.Body.Close()
	names := upstreamToolNames(t, "chat")
	if names["WebSearch"] {
		t.Fatalf("x-app:cli did not trigger dedupe; upstream tools = %v", names)
	}
}

// TestNonClaudeClientToolsKept: the same tool list from a non-claude client
// passes through untouched — dedupe is claude-gated, and the fingerprint
// names are still merged in.
func TestNonClaudeClientToolsKept(t *testing.T) {
	resp := postChat(t, map[string]any{
		"model":    testedModel,
		"messages": []any{map[string]any{"role": "user", "content": "search the web"}},
		"tools":    claudeCodeTools(),
		"stream":   false,
	}, setUpstreamUA(t, "opencode/1.18.31"))
	defer resp.Body.Close()
	names := upstreamToolNames(t, "chat")
	for _, want := range []string{"WebSearch", "WebFetch", "mcp__exa__web_search_exa", "Bash", "bash", "glob", "grep", "read"} {
		if !names[want] {
			t.Fatalf("tool %q missing from non-claude client's upstream tools = %v", want, names)
		}
	}
}

// TestPlainChatGetsQuartetAndNoneChoice: a tools-less caller gets exactly the
// fingerprint quartet plus tool_choice "none" — the model must never call the
// injected no-op declarations on plain chat.
func TestPlainChatGetsQuartetAndNoneChoice(t *testing.T) {
	resp := postChat(t, chatBody(testedModel), nil)
	defer resp.Body.Close()
	names := upstreamToolNames(t, "chat")
	if len(names) != 4 {
		t.Fatalf("upstream tools = %v, want exactly the quartet", names)
	}
	if choice := fake.lastChatBody()["tool_choice"]; choice != "none" {
		t.Fatalf("tool_choice = %v, want none", choice)
	}
}

// TestMuseChatClientUpstreamResponsesShape: a CHAT-format client asking a
// muse-spark model is translated to the Responses API upstream — messages
// become input items, tools flatten to the responses shape, tool_choice is
// demoted to auto (muse is auto-only upstream), store is disabled.
func TestMuseChatClientUpstreamResponsesShape(t *testing.T) {
	resp := postChat(t, map[string]any{
		"model":       museModel,
		"messages":    []any{map[string]any{"role": "user", "content": "weather in Hanoi?"}},
		"tools":       []any{chatTool("get_weather", "lookup weather")},
		"tool_choice": "required",
		"stream":      false,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	up := fake.lastRespBody()
	if up == nil {
		t.Fatal("muse request did not reach /zen/v1/responses")
	}
	if up["tool_choice"] != "auto" {
		t.Fatalf("upstream tool_choice = %v, want auto (muse is auto-only)", up["tool_choice"])
	}
	if up["store"] != false {
		t.Fatalf("upstream store = %v, want false", up["store"])
	}
	if !toolNames(up)["get_weather"] {
		t.Fatalf("client tool missing upstream: %v", up["tools"])
	}
	input := jsonx.AsArr(up["input"])
	if len(input) == 0 {
		t.Fatalf("upstream input empty: %v", up["input"])
	}
	item := jsonx.AsObj(input[0])
	if item["role"] != "user" {
		t.Fatalf("input[0].role = %v", item["role"])
	}
	blocks := jsonx.AsArr(item["content"])
	if len(blocks) == 0 || jsonx.AsObj(blocks[0])["text"] != "weather in Hanoi?" {
		t.Fatalf("input[0].content = %v, want the user text", item["content"])
	}
}

// TestMuseResponsesToolChoiceDemoted: a native Responses client's explicit
// non-auto tool_choice is demoted for muse-spark the same way.
func TestMuseResponsesToolChoiceDemoted(t *testing.T) {
	resp := doRequest(t, "POST", "/v1/responses", map[string]any{
		"model":       museModel,
		"input":       "hi",
		"tools":       []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{}}},
		"tool_choice": "required",
		"stream":      false,
	}, nil)
	defer resp.Body.Close()
	if fake.lastRespBody()["tool_choice"] != "auto" {
		t.Fatalf("upstream tool_choice = %v, want auto", fake.lastRespBody()["tool_choice"])
	}
}

// TestUpstreamToolCallAggregatedForChatClient: the upstream streams a split
// tool_call (id+partial name, then the rest of the name and the arguments);
// a non-streaming chat client gets one chat.completion with the reassembled
// tool_call and finish_reason tool_calls.
func TestUpstreamToolCallAggregatedForChatClient(t *testing.T) {
	fake.setReplies(toolCallSSE, "")
	defer fake.setReplies("", "")
	resp := postChat(t, chatBody(testedModel), nil)
	defer resp.Body.Close()
	body := decodeJSON(t, resp)
	arr := jsonx.AsArr(body["choices"])
	choice := jsonx.AsObj(arr[0])
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	msg := jsonx.AsObj(choice["message"])
	calls := jsonx.AsArr(msg["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	tc := jsonx.AsObj(calls[0])
	if tc["id"] != "call_abc123" {
		t.Fatalf("tool_call id = %v", tc["id"])
	}
	fn := jsonx.AsObj(tc["function"])
	if fn["name"] != "read_file" || fn["arguments"] != `{"path":"main.go"}` {
		t.Fatalf("reassembled function = %v/%v, want read_file/{\"path\":\"main.go\"}", fn["name"], fn["arguments"])
	}
	if msg["content"] != nil {
		t.Fatalf("content = %v, want null for a pure tool-call turn", msg["content"])
	}
}

// TestUpstreamToolCallStreamedFragments: a streaming client sees the fragments
// as they arrive — both deltas, the id, and the terminal [DONE].
func TestUpstreamToolCallStreamedFragments(t *testing.T) {
	fake.setReplies(toolCallSSE, "")
	defer fake.setReplies("", "")
	body := chatBody(testedModel)
	body["stream"] = true
	resp := postChat(t, body, nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	sse := string(raw)
	for _, frag := range []string{"call_abc123", `"name":"read_"`, `"name":"file"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(sse, frag) {
			t.Fatalf("stream missing %s:\n%s", frag, sse)
		}
	}
}

// TestToolResultRoundTripValidIDs: the follow-up turn of an agent loop —
// assistant tool_calls + a role:"tool" result — must reach the upstream with
// the id linkage intact.
func TestToolResultRoundTripValidIDs(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "list files"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_abc123", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_abc123", "content": "main.go"},
	}
	resp := postChat(t, map[string]any{"model": testedModel, "messages": messages, "stream": false}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	upMsgs := jsonx.AsArr(fake.lastChatBody()["messages"])
	if len(upMsgs) != 3 {
		t.Fatalf("upstream messages = %d, want 3 (no repair messages inserted)", len(upMsgs))
	}
	assistant := jsonx.AsObj(upMsgs[1])
	tc := jsonx.AsObj(jsonx.AsArr(assistant["tool_calls"])[0])
	if tc["id"] != "call_abc123" {
		t.Fatalf("assistant tool_call id = %v, want verbatim call_abc123", tc["id"])
	}
	toolMsg := jsonx.AsObj(upMsgs[2])
	if toolMsg["tool_call_id"] != "call_abc123" || toolMsg["role"] != "tool" {
		t.Fatalf("tool result = %v", toolMsg)
	}
}

// TestBrokenToolCallIDRepaired: a sloppy client that omits the tool_call id
// gets a deterministic generated one upstream (EnsureToolCallIDs prenorm) so
// the upstream never 400s on the dangling tool call.
func TestBrokenToolCallIDRepaired(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "read main.go"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"type": "function",
				"function": map[string]any{"name": "read_file", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_msg1_tc0_read_file", "content": "ok"},
	}
	resp := postChat(t, map[string]any{"model": testedModel, "messages": messages, "stream": false}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assistant := jsonx.AsObj(jsonx.AsArr(fake.lastChatBody()["messages"])[1])
	tc := jsonx.AsObj(jsonx.AsArr(assistant["tool_calls"])[0])
	// GenerateToolCallID(msgIndex=1, tcIndex=0, "read_file").
	if tc["id"] != "call_msg1_tc0_read_file" {
		t.Fatalf("repaired id = %v, want call_msg1_tc0_read_file", tc["id"])
	}
}

// TestResponsesClientToolCallFromChatUpstream: a Responses-format client on a
// chat-native model gets the upstream chat tool_call back as a function_call
// output item — the full cross-interface round trip.
func TestResponsesClientToolCallFromChatUpstream(t *testing.T) {
	fake.setReplies(toolCallSSE, "")
	defer fake.setReplies("", "")
	resp := doRequest(t, "POST", "/v1/responses", map[string]any{
		"model":  testedModel,
		"input":  "read main.go",
		"tools":  []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{}}},
		"stream": false,
	}, nil)
	defer resp.Body.Close()
	body := decodeJSON(t, resp)
	if body["status"] != "completed" {
		t.Fatalf("status = %v", body["status"])
	}
	var fnItem map[string]any
	for _, raw := range jsonx.AsArr(body["output"]) {
		o := jsonx.AsObj(raw)
		if jsonx.AsStr(o["type"]) == "function_call" {
			fnItem = o
		}
	}
	if fnItem == nil {
		t.Fatalf("no function_call item in output %v", body["output"])
	}
	if fnItem["name"] != "read_file" || fnItem["call_id"] != "call_abc123" || fnItem["arguments"] != `{"path":"main.go"}` {
		t.Fatalf("function_call item = %v", fnItem)
	}
}

// TestSessionAndClientHeaderPersonas: opencode-native session ids pass
// through verbatim, foreign clients' session ids are translated to the
// opencode shape, and a client-declared x-opencode-client is honored over the
// "desktop" default.
func TestSessionAndClientHeaderPersonas(t *testing.T) {
	native := "ses_abcdef012345ABCDEFGHIJKLMN"

	t.Run("native opencode session passes through", func(t *testing.T) {
		resp := postChat(t, chatBody(testedModel), func(r *http.Request) {
			r.Header.Set("X-Opencode-Session", native)
		})
		defer resp.Body.Close()
		if got := fake.upstreamHeader(t, "x-opencode-session"); got != native {
			t.Fatalf("session = %q, want verbatim %q", got, native)
		}
	})

	t.Run("claude code session is translated", func(t *testing.T) {
		resp := postChat(t, chatBody(testedModel), func(r *http.Request) {
			r.Header.Set("User-Agent", "claude-code/2.0.14 (external, cli)")
			r.Header.Set("X-Claude-Code-Session-Id", "not-an-opencode-id")
		})
		defer resp.Body.Close()
		got := fake.upstreamHeader(t, "x-opencode-session")
		if got == "not-an-opencode-id" || !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(got) {
			t.Fatalf("session = %q, want a translated opencode-shaped id", got)
		}
	})

	t.Run("client header honored", func(t *testing.T) {
		resp := postChat(t, chatBody(testedModel), func(r *http.Request) {
			r.Header.Set("X-Opencode-Client", "cli")
		})
		defer resp.Body.Close()
		if got := fake.upstreamHeader(t, "x-opencode-client"); got != "cli" {
			t.Fatalf("x-opencode-client = %q, want cli passthrough", got)
		}
	})
}
