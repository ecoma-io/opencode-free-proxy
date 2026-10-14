// Cloak surface tests — ported from the golden vitest vectors in
// tests/unit/opencode-free-tool-choice.test.js,
// tests/unit/opencode-muse-spark-thinking.test.js and the mirrored JS in
// open-sse/executors/opencode.js (ensureChatFingerprintTools,
// ensureResponsesFingerprintTools, normalizeResponsesTools,
// sanitizeResponsesItems, normalizeOpencodeReasoning),
// open-sse/translator/formats/responsesApi.js and
// open-sse/providers/models/helpers.js isMuseSparkModel.
package cloak

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"opencode-free-proxy/internal/config"
)

const (
	free12 = "muse-spark-1.2-contributor-free"
	free13 = "muse-spark-1.3-contributor-free"
)

// ---------------------------------------------------------------------------
// Model routing (baseModelId / isMuseSparkModel / buildUrl)
// ---------------------------------------------------------------------------

func TestBaseModelID(t *testing.T) {
	cases := []struct{ in, want string }{
		{free12 + "(high)", free12},
		{"model(high)", "model"},
		{"m(x)(y)", "m(x)"},      // only the last parenthesised group goes
		{"model", "model"},       // no suffix
		{"model()", "model()"},   // empty group is not a suffix
		{"  padded  ", "padded"}, // trimmed
		{free13 + "(max)  ", free13},
	}
	for _, tc := range cases {
		if got := BaseModelID(tc.in); got != tc.want {
			t.Errorf("BaseModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpstreamModelID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"oc/" + free12 + "(high)", free12},
		{"oc/big-pickle", "big-pickle"},
		{"big-pickle", "big-pickle"},
		{"model(high)", "model"},
		{free13, free13},
	}
	for _, tc := range cases {
		if got := UpstreamModelID(tc.in); got != tc.want {
			t.Errorf("UpstreamModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The Responses routing rule mirrors isMuseSparkModel:
// (?i)^muse[-_]?spark(?:$|[-_:.\s]) applied to the suffix-stripped, last "/"
// segment.
func TestIsResponsesModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
		note  string
	}{
		{free12, true, "registry id"},
		{free13, true, "registry id"},
		{free13 + "(high)", true, "thinking suffix stripped"},
		{"oc/" + free12, true, "provider prefix stripped"},
		{"muse-spark-1.4-contributor-free", true, "future muse-spark ids route the same way"},
		{"muse-spark", true, "bare spark token ends the match"},
		{"muse_spark-x-free", true, "underscore separator accepted"},
		{"muse-spark 1.2", true, "space after spark accepted (\\s)"},
		{"MUSE-SPARK-1.3-contributor-free", true, "case-insensitive"},
		{"musespark", true, "no separator required before the trailing anchor (JS-verified)"},
		{"Muse Spark 1.2", false, "space between muse and spark is not a separator (JS-verified)"},
		{"muse-sparkish", false, "suffix must end at a separator"},
		{"notmuse-spark-1", false, "prefix must be at the start"},
		{"spark", false, "no muse prefix"},
		{"big-pickle", false, "chat model"},
		{"mimo-v2.5-free", false, "chat model"},
		{"deepseek-v4-flash-free", false, "chat model"},
		{"", false, "empty"},
	}
	for _, tc := range cases {
		if got := IsResponsesModel(tc.model); got != tc.want {
			t.Errorf("IsResponsesModel(%q) [%s] = %v, want %v", tc.model, tc.note, got, tc.want)
		}
	}
}

func TestURL(t *testing.T) {
	const base = "https://opencode.ai"
	cases := []struct {
		model string
		want  string
	}{
		{free12, base + config.ZenResponsesPath},
		{free13, base + config.ZenResponsesPath},
		{"oc/" + free12 + "(high)", base + config.ZenResponsesPath},
		{"muse-spark-1.4-contributor-free", base + config.ZenResponsesPath},
		{"big-pickle", base + config.ZenChatPath},
		{"mimo-v2.5-free", base + config.ZenChatPath},
	}
	for _, tc := range cases {
		if got := URL(base, tc.model); got != tc.want {
			t.Errorf("URL(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Fingerprint quartet — chat shape (golden: opencode-free-tool-choice.test.js)
// ---------------------------------------------------------------------------

func chatToolName(tool any) string {
	o, _ := tool.(map[string]any)
	if o == nil {
		return ""
	}
	fn, _ := o["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	name, _ := fn["name"].(string)
	return strings.TrimSpace(name)
}

// anyToolName reads a tool name from either wire shape (mirrors cloak's
// toolNameOf): responses-flat tool.name, else chat tool.function.name.
func anyToolName(tool any) string {
	o, _ := tool.(map[string]any)
	if o == nil {
		return ""
	}
	if n, _ := o["name"].(string); n != "" {
		return strings.TrimSpace(n)
	}
	fn, _ := o["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	n, _ := fn["name"].(string)
	return strings.TrimSpace(n)
}

func toolNames(tools []any, flat bool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if flat {
			o, _ := tool.(map[string]any)
			n, _ := o["name"].(string)
			names = append(names, strings.TrimSpace(n))
			continue
		}
		names = append(names, chatToolName(tool))
	}
	return names
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// A caller with no tools gets the quartet and tool_choice "none" so the decoy
// declarations are never called on plain chat.
func TestEnsureChatFingerprintToolsInjectsQuartet(t *testing.T) {
	body := map[string]any{"model": "big-pickle", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	EnsureChatFingerprintTools(body)

	tools, _ := body["tools"].([]any)
	if len(tools) != len(config.FingerprintTools) {
		t.Fatalf("tools count = %d, want %d", len(tools), len(config.FingerprintTools))
	}
	names := toolNames(tools, false)
	for _, required := range config.FingerprintTools {
		if !containsString(names, required) {
			t.Errorf("chat tools %v missing fingerprint %q", names, required)
		}
	}
	if got := body["tool_choice"]; got != "none" {
		t.Errorf("tool_choice = %v, want \"none\" for a toolless caller", got)
	}
	// Declared shape: chat-shape carrying the REAL client parameter schema.
	// The gate reads these schemas — four declarations with an empty
	// `properties` map are refused upstream (measured 2026-10-15, see
	// config.FingerprintTools).
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn == nil {
		t.Fatalf("injected chat tool shape = %v", tool)
	}
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if params["type"] != "object" || len(props) == 0 {
		t.Errorf("parameters = %v, want a non-empty object schema (the gate reads it)", fn["parameters"])
	}
	if _, has := props["command"]; !has {
		t.Errorf("shell parameters = %v, want the real client properties", props)
	}
	required, _ := params["required"].([]any)
	if len(required) != 1 || required[0] != "command" {
		t.Errorf("required = %v, want [command]", params["required"])
	}
	want := config.FingerprintToolSchemas["shell"].Description
	if desc, _ := fn["description"].(string); desc != want {
		t.Errorf("description = %q, want the real client text %q", desc, want)
	}
}

// Every fingerprint name the injector reaches for must carry a real schema:
// a missing entry would silently degrade to the empty object schema the gate
// refuses, which is exactly the 2026-10-15 regression.
func TestFingerprintToolsHaveSchemas(t *testing.T) {
	for _, name := range config.FingerprintTools {
		def, ok := config.FingerprintToolSchemas[name]
		if !ok {
			t.Errorf("fingerprint tool %q has no schema entry", name)
			continue
		}
		if len(def.Properties) == 0 {
			t.Errorf("fingerprint tool %q has no parameters", name)
		}
		if def.Description == "" {
			t.Errorf("fingerprint tool %q has no description", name)
		}
		if len(def.Required) == 0 {
			t.Errorf("fingerprint tool %q declares no required property", name)
		}
		for _, r := range def.Required {
			if _, has := def.Properties[r]; !has {
				t.Errorf("fingerprint tool %q requires %q, which is not a declared property", name, r)
			}
		}
	}
}

// A caller with own tools keeps them first; only missing quartet members are
// appended (golden: names[0] === "my_tool" and the quartet present).
func TestEnsureChatFingerprintToolsPreservesCallerTools(t *testing.T) {
	caller := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "my_tool",
			"description": "m",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
	body := map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":       []any{caller},
		"tool_choice": "auto",
	}
	EnsureChatFingerprintTools(body)

	names := toolNames(body["tools"].([]any), false)
	if len(names) == 0 || names[0] != "my_tool" {
		t.Errorf("caller tool must stay first, names = %v", names)
	}
	for _, required := range config.FingerprintTools {
		if !containsString(names, required) {
			t.Errorf("merged chat tools %v missing %q", names, required)
		}
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("a caller with tools keeps its tool_choice, got %v", body["tool_choice"])
	}
}

// An existing quartet member (by flat or nested name) is not duplicated.
func TestEnsureChatFingerprintToolsNoDuplicates(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": " read "}},
		},
	}
	EnsureChatFingerprintTools(body)
	names := toolNames(body["tools"].([]any), false)
	if len(names) != len(config.FingerprintTools) {
		t.Fatalf("tools = %v, want exactly %d entries", names, len(config.FingerprintTools))
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	for _, n := range config.FingerprintTools {
		if seen[n] != 1 {
			t.Errorf("tool %q appears %d times, want 1 (names = %v)", n, seen[n], names)
		}
	}
}

// JS truthiness: absent/null/""/false tool_choice counts as unset.
func TestEnsureChatFingerprintToolChoiceTruthiness(t *testing.T) {
	cases := []struct {
		choice any
		set    bool // whether the JS value is truthy (left untouched)
		note   string
	}{
		{nil, false, "explicit null"},
		{"", false, "empty string"},
		{false, false, "false"},
		{float64(0), false, "zero"},
		{"auto", true, "auto"},
		{"required", true, "required"},
		{map[string]any{"type": "function", "name": "x"}, true, "object choice"},
	}
	for _, tc := range cases {
		body := map[string]any{"tools": []any{}, "tool_choice": tc.choice}
		EnsureChatFingerprintTools(body)
		got := body["tool_choice"]
		if tc.set {
			if !reflect.DeepEqual(got, tc.choice) {
				t.Errorf("[%s] tool_choice %v must stay untouched, got %v", tc.note, tc.choice, got)
			}
		} else if got != "none" {
			t.Errorf("[%s] falsy tool_choice must default to \"none\", got %v", tc.note, got)
		}
	}

	// A truly absent choice behaves like null.
	absent := map[string]any{"tools": []any{}}
	EnsureChatFingerprintTools(absent)
	if absent["tool_choice"] != "none" {
		t.Errorf("absent tool_choice must default to \"none\", got %v", absent["tool_choice"])
	}
}

// A non-array tools field is replaced by an empty array before the merge.
func TestEnsureChatFingerprintToolsNonArrayTools(t *testing.T) {
	body := map[string]any{"tools": "nope"}
	EnsureChatFingerprintTools(body)
	names := toolNames(body["tools"].([]any), false)
	if len(names) != 4 {
		t.Errorf("tools = %v, want the quartet", names)
	}
	if body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want \"none\"", body["tool_choice"])
	}
}

func TestEnsureChatFingerprintToolsNilBody(t *testing.T) {
	EnsureChatFingerprintTools(nil) // must not panic
}

// ---------------------------------------------------------------------------
// Fingerprint quartet — Responses flat shape
// ---------------------------------------------------------------------------

// Golden: the fingerprint is injected into Responses bodies and a missing
// choice defaults to "auto" so the gate never sees tools without tool_choice.
func TestEnsureResponsesFingerprintTools(t *testing.T) {
	body := map[string]any{"input": []any{map[string]any{"type": "message", "role": "user"}}}
	EnsureResponsesFingerprintTools(body)

	names := toolNames(body["tools"].([]any), true)
	if !reflect.DeepEqual(names, []string{"shell", "glob", "grep", "read"}) {
		t.Errorf("tools = %v, want the quartet in order", names)
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", body["tool_choice"])
	}
	tool, _ := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "shell" {
		t.Errorf("flat tool shape = %v", tool)
	}
	params, _ := tool["parameters"].(map[string]any)
	if props, _ := params["properties"].(map[string]any); len(props) == 0 {
		t.Errorf("flat tool parameters = %v, want a non-empty object schema", tool["parameters"])
	}
}

// Caller tools are preserved; a caller-present quartet member (matched by the
// trimmed flat or nested name) is not duplicated; an explicit choice stays.
func TestEnsureResponsesFingerprintToolsCallerTools(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "get_weather"},
			map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
		},
		"tool_choice": map[string]any{"type": "function", "name": "get_weather"},
	}
	EnsureResponsesFingerprintTools(body)

	tools := body["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, anyToolName(tool))
	}
	want := []string{"get_weather", "read", "shell", "glob", "grep"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tools = %v, want %v (missing members only, no duplicates)", names, want)
	}
	if _, ok := body["tool_choice"].(map[string]any); !ok {
		t.Errorf("object tool_choice must stay untouched, got %v", body["tool_choice"])
	}

	// Falsy choices still default to auto.
	for _, falsy := range []any{nil, "", false} {
		b := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "t"}}, "tool_choice": falsy}
		EnsureResponsesFingerprintTools(b)
		if b["tool_choice"] != "auto" {
			t.Errorf("falsy tool_choice %v must default to auto, got %v", falsy, b["tool_choice"])
		}
	}
}

func TestEnsureResponsesFingerprintToolsNilBody(t *testing.T) {
	EnsureResponsesFingerprintTools(nil) // must not panic
}

// ---------------------------------------------------------------------------
// normalizeResponsesTools
// ---------------------------------------------------------------------------

// Chat-shaped declarations flatten to the Responses shape; names are trimmed
// and capped at 128; missing parameters get the empty object schema
// (golden: muse-spark sanitize test expects the flattened shell tool).
func TestNormalizeResponsesToolsFlattens(t *testing.T) {
	longName := strings.Repeat("n", 130)
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "  shell  ",
					"description": "Run shell command",
					"parameters":  map[string]any{"type": "object"},
				},
			},
			map[string]any{ // already-flat tool without parameters or description
				"type": "function",
				"name": longName,
			},
		},
	}
	NormalizeResponsesTools(body)

	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d entries, want 2", len(tools))
	}
	first, _ := tools[0].(map[string]any)
	wantFirst := map[string]any{
		"type":        "function",
		"name":        "shell",
		"description": "Run shell command",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Errorf("flattened tool = %v, want %v", first, wantFirst)
	}
	second, _ := tools[1].(map[string]any)
	if second["name"] != strings.Repeat("n", 128) {
		t.Errorf("name not truncated to %d chars: len %d", config.MaxToolNameLen, len(second["name"].(string)))
	}
	if _, has := second["description"]; has {
		t.Error("empty description must be omitted (JS: if (description) …)")
	}
	if !reflect.DeepEqual(second["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Errorf("missing parameters must fall back to the empty object schema, got %v", second["parameters"])
	}
}

// Hosted (nameless) tools and non-object entries are dropped.
func TestNormalizeResponsesToolsDropsNameless(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "web_search"},              // hosted, no name
			map[string]any{"type": "function"},                // nameless function
			map[string]any{"type": "function", "name": "   "}, // blank name
			map[string]any{"type": "function", "function": map[string]any{"name": "keep"}},
			"junk", // non-object entry
			nil,
		},
	}
	NormalizeResponsesTools(body)
	names := toolNames(body["tools"].([]any), true)
	if !reflect.DeepEqual(names, []string{"keep"}) {
		t.Errorf("tools = %v, want only [keep]", names)
	}
}

// tool_choice objects naming an unknown function are deleted; known names and
// non-function objects are kept. The check runs only when tools is an array.
func TestNormalizeResponsesToolsToolChoice(t *testing.T) {
	base := func(tools any) map[string]any {
		return map[string]any{"tools": tools}
	}

	known := base([]any{map[string]any{"type": "function", "name": "my_tool"}})
	known["tool_choice"] = map[string]any{"type": "function", "name": "my_tool"}
	NormalizeResponsesTools(known)
	if _, has := known["tool_choice"]; !has {
		t.Error("tool_choice naming a known tool must be kept")
	}

	trimmed := base([]any{map[string]any{"type": "function", "name": "my_tool"}})
	trimmed["tool_choice"] = map[string]any{"type": "function", "name": "  my_tool  "}
	NormalizeResponsesTools(trimmed)
	if _, has := trimmed["tool_choice"]; !has {
		t.Error("tool_choice name must be trimmed before the known-name lookup")
	}

	unknown := base([]any{map[string]any{"type": "function", "name": "my_tool"}})
	unknown["tool_choice"] = map[string]any{"type": "function", "name": "unknown_tool"}
	NormalizeResponsesTools(unknown)
	if _, has := unknown["tool_choice"]; has {
		t.Error("tool_choice naming an unknown tool must be deleted")
	}

	nameless := base([]any{map[string]any{"type": "function", "name": "my_tool"}})
	nameless["tool_choice"] = map[string]any{"type": "function"}
	NormalizeResponsesTools(nameless)
	if _, has := nameless["tool_choice"]; has {
		t.Error("function tool_choice without a name must be deleted")
	}

	nonFunction := base([]any{map[string]any{"type": "function", "name": "my_tool"}})
	nonFunction["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto"}
	NormalizeResponsesTools(nonFunction)
	if _, has := nonFunction["tool_choice"]; !has {
		t.Error("non-function object tool_choice must be kept")
	}

	// No tools array → the whole function returns early (JS: !Array.isArray).
	absent := map[string]any{"tool_choice": map[string]any{"type": "function", "name": "ghost"}}
	NormalizeResponsesTools(absent)
	if _, has := absent["tool_choice"]; !has {
		t.Error("without a tools array the tool_choice check must not run")
	}
	if absent["tools"] != nil {
		t.Error("without a tools array the body must stay untouched")
	}
}

// PARITY BUG repro (expected to FAIL until fixed — see task report):
// opencode.js normalizeResponsesTools backfills the empty properties object on
// JS falsiness (`if (parameters.type === "object" && !parameters.properties)`),
// so `properties: null` is replaced with `{}`.
// internal/cloak/cloak.go:231 tests key PRESENCE instead, so a null properties
// survives and strict upstreams reject it exactly like an absent one.
func TestNormalizeResponsesToolsPropertiesNullBUG(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"type":       "function",
				"name":       "sloppy",
				"parameters": map[string]any{"type": "object", "properties": nil},
			},
		},
	}
	NormalizeResponsesTools(body)
	tool := body["tools"].([]any)[0].(map[string]any)
	params := tool["parameters"].(map[string]any)
	if !reflect.DeepEqual(params["properties"], map[string]any{}) {
		t.Errorf("JS backfills properties:null to {} (falsy check), Go kept %v", params["properties"])
	}
}

// Existing (truthy) properties are never replaced — including an empty object.
func TestNormalizeResponsesToolsKeepsExistingProperties(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	body := map[string]any{
		"tools": []any{map[string]any{"type": "function", "name": "t", "parameters": params}},
	}
	NormalizeResponsesTools(body)
	got := body["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if !reflect.DeepEqual(got, params) {
		t.Errorf("declared properties must survive, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// normalizeResponsesInput
// ---------------------------------------------------------------------------

func TestNormalizeResponsesInput(t *testing.T) {
	t.Run("string becomes a user message array", func(t *testing.T) {
		got := NormalizeResponsesInput("hello")
		want := []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("whitespace-only string becomes the placeholder", func(t *testing.T) {
		got := NormalizeResponsesInput("   \n\t ")
		text := got[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != "..." {
			t.Errorf("whitespace-only input text = %q, want \"...\"", text)
		}
	})
	t.Run("empty string becomes the placeholder", func(t *testing.T) {
		got := NormalizeResponsesInput("")
		text := got[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != "..." {
			t.Errorf("empty input text = %q, want \"...\"", text)
		}
	})
	t.Run("non-empty string preserved UNTRIMMED", func(t *testing.T) {
		got := NormalizeResponsesInput("  keep my spaces  ")
		text := got[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != "  keep my spaces  " {
			t.Errorf("text = %q, want the untrimmed original", text)
		}
	})
	t.Run("empty array becomes the placeholder", func(t *testing.T) {
		got := NormalizeResponsesInput([]any{})
		if len(got) != 1 {
			t.Fatalf("got %d items, want 1 placeholder", len(got))
		}
		text := got[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != "..." {
			t.Errorf("placeholder text = %q, want \"...\"", text)
		}
	})
	t.Run("non-empty array passes through", func(t *testing.T) {
		items := []any{map[string]any{"type": "message", "role": "user"}}
		if got := NormalizeResponsesInput(items); !reflect.DeepEqual(got, items) {
			t.Errorf("got %v, want the original slice", got)
		}
	})
	t.Run("other shapes are invalid", func(t *testing.T) {
		for _, v := range []any{nil, float64(42), map[string]any{}, true} {
			if got := NormalizeResponsesInput(v); got != nil {
				t.Errorf("NormalizeResponsesInput(%v) = %v, want nil", v, got)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// sanitizeResponsesItems + call-id clamp + coercions
// ---------------------------------------------------------------------------

// Golden (muse-spark sanitize test): prior-turn reasoning items are dropped,
// encrypted blobs stripped, function items coerced, and the surviving item
// order preserved.
func TestSanitizeResponsesItemsGolden(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "say hi"}}},
			map[string]any{
				"type": "reasoning", "id": "rs_123",
				"encrypted_content": "ENC_BLOB_TURN_1",
				"summary":           []any{map[string]any{"type": "summary_text", "text": "thinking text"}},
			},
			map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1",
				"name": "shell", "arguments": `{"command":"echo hi"}`,
			},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "hi"},
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "now say bye"}}},
		},
	}
	SanitizeResponsesItems(body)

	items, _ := body["input"].([]any)
	types := make([]string, 0, len(items))
	for _, item := range items {
		o, _ := item.(map[string]any)
		typ, _ := o["type"].(string)
		types = append(types, typ)
	}
	if !reflect.DeepEqual(types, []string{"message", "function_call", "function_call_output", "message"}) {
		t.Errorf("item types = %v, want reasoning dropped and order preserved", types)
	}
	if serialized, _ := json.Marshal(items); strings.Contains(string(serialized), "ENC_BLOB_TURN_1") {
		t.Error("encrypted reasoning content must never survive sanitization")
	}
}

// encrypted_content / reasoning_encrypted_content are stripped from every
// kept item; malformed function items are coerced or dropped.
func TestSanitizeResponsesItemsCoercions(t *testing.T) {
	longName := strings.Repeat("f", 130)
	longCallID := strings.Repeat("c", 65)
	body := map[string]any{
		"input": []any{
			map[string]any{
				"type": "function_call", "name": longName, "call_id": longCallID,
				"arguments": map[string]any{"a": float64(1)}, "encrypted_content": "blob",
			},
			map[string]any{"type": "function_call", "name": "  spaced  ", "call_id": "", "arguments": "not-json"},
			map[string]any{"type": "function_call", "name": "", "call_id": "x"},
			map[string]any{"type": "function_call_output", "call_id": nil,
				"output": []any{map[string]any{"text": "a"}, map[string]any{"text": "b"}}},
			map[string]any{"type": "message", "role": "user", "encrypted_content": "blob2",
				"reasoning_encrypted_content": "blob3"},
			"raw-string-item",
		},
	}
	SanitizeResponsesItems(body)

	items, _ := body["input"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %d, want 5 (nameless function_call dropped)", len(items))
	}

	fc := items[0].(map[string]any)
	if fc["name"] != strings.Repeat("f", 128) {
		t.Errorf("function_call name not truncated: len %d", len(fc["name"].(string)))
	}
	if fc["call_id"] != strings.Repeat("c", 64) {
		t.Errorf("call_id not truncated to 64: %v", fc["call_id"])
	}
	if fc["arguments"] != `{"a":1}` {
		t.Errorf("object arguments must stringify once, got %v", fc["arguments"])
	}
	if _, has := fc["encrypted_content"]; has {
		t.Error("encrypted_content must be stripped from kept items")
	}

	fc2 := items[1].(map[string]any)
	if fc2["name"] != "spaced" {
		t.Errorf("name must be trimmed, got %v", fc2["name"])
	}
	if fc2["arguments"] != "{}" {
		t.Errorf("invalid JSON-string arguments must fall back to \"{}\", got %v", fc2["arguments"])
	}
	synthesized1, _ := fc2["call_id"].(string)

	fco := items[2].(map[string]any)
	if fco["output"] != "ab" {
		t.Errorf("array output must join its text fields, got %v", fco["output"])
	}
	synthesized2, _ := fco["call_id"].(string)

	if !strings.HasPrefix(synthesized1, "call_") || !strings.HasPrefix(synthesized2, "call_") {
		t.Errorf("synthesized call ids must start with call_, got %q / %q", synthesized1, synthesized2)
	}
	if synthesized1 == synthesized2 {
		t.Errorf("synthesized call ids must be unique, got %q twice", synthesized1)
	}

	msg := items[3].(map[string]any)
	if _, has := msg["encrypted_content"]; has {
		t.Error("encrypted_content must be stripped from message items too")
	}
	if _, has := msg["reasoning_encrypted_content"]; has {
		t.Error("reasoning_encrypted_content must be stripped from message items too")
	}

	if items[4] != "raw-string-item" {
		t.Errorf("non-object items must pass through, got %v", items[4])
	}
}

func TestClampResponsesCallID(t *testing.T) {
	if got := ClampResponsesCallID(nil); !strings.HasPrefix(got, "call_") {
		t.Errorf("ClampResponsesCallID(nil) = %q, want a synthesized call_ id", got)
	}
	if got := ClampResponsesCallID(""); !strings.HasPrefix(got, "call_") {
		t.Errorf("ClampResponsesCallID(\"\") = %q, want a synthesized call_ id", got)
	}
	if got := ClampResponsesCallID(float64(7)); !strings.HasPrefix(got, "call_") {
		t.Errorf("non-string ids synthesize, got %q", got)
	}
	exact := strings.Repeat("x", 64)
	if got := ClampResponsesCallID(exact); got != exact {
		t.Error("a 64-char call id must pass through unchanged")
	}
	if got := ClampResponsesCallID(exact + "y"); got != exact {
		t.Errorf("a 65-char call id must truncate to 64, got len %d", len(got))
	}
	a, b := ClampResponsesCallID(nil), ClampResponsesCallID(nil)
	if a == b {
		t.Errorf("synthesized ids must be unique, got %q twice", a)
	}
}

func TestCoerceResponsesArguments(t *testing.T) {
	cases := []struct {
		in   any
		want string
		note string
	}{
		{nil, "{}", "null"},
		{"", "{}", "empty string"},
		{"  ", "{}", "whitespace is not valid JSON"},
		{map[string]any{"a": float64(1)}, `{"a":1}`, "object stringified once"},
		{[]any{float64(1), float64(2)}, "[1,2]", "array stringified"},
		{float64(5), "5", "scalar marshaled"},
		{true, "true", "bool marshaled"},
		{`{"a":1}`, `{"a":1}`, "valid JSON string untouched"},
		{`[]`, `[]`, "empty JSON array untouched"},
		{`{"a":1,}`, "{}", "invalid JSON string"},
		{"partial call arg", "{}", "fragment"},
	}
	for _, tc := range cases {
		if got := CoerceResponsesArguments(tc.in); got != tc.want {
			t.Errorf("[%s] CoerceResponsesArguments(%v) = %q, want %q", tc.note, tc.in, got, tc.want)
		}
	}
}

func TestCoerceResponsesOutput(t *testing.T) {
	cases := []struct {
		in   any
		want string
		note string
	}{
		{"plain", "plain", "string untouched"},
		{"", "", "empty string untouched"},
		{nil, "", "null → empty"},
		{float64(5), "5", "scalar marshaled"},
		{true, "true", "bool marshaled"},
		{map[string]any{"a": float64(1)}, `{"a":1}`, "object marshaled"},
		{
			[]any{map[string]any{"text": "a"}, map[string]any{"text": "b"}},
			"ab", "text fields joined with no separator",
		},
		{
			[]any{map[string]any{"text": "x"}, map[string]any{"other": float64(1)}},
			`x{"other":1}`, "items without text stringify whole",
		},
		{
			[]any{map[string]any{"text": float64(7)}},
			"7", "non-string text marshaled",
		},
		{[]any{"raw"}, `"raw"`, "string items stringify"},
		{[]any{nil}, "null", "null items stringify"},
	}
	for _, tc := range cases {
		if got := CoerceResponsesOutput(tc.in); got != tc.want {
			t.Errorf("[%s] CoerceResponsesOutput(%v) = %q, want %q", tc.note, tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// demoteToolChoice (golden: opencode-free-tool-choice.test.js)
// ---------------------------------------------------------------------------

func TestDemoteToolChoice(t *testing.T) {
	named := map[string]any{"type": "function", "name": "get_weather"}
	allow := []string{free12, free13, free13 + "(max)"}

	for _, model := range allow {
		for _, choice := range []any{named, "required", "none", false, nil} {
			body := map[string]any{"tool_choice": choice}
			DemoteToolChoice(body, model)
			if body["tool_choice"] != "auto" {
				t.Errorf("model %q: tool_choice %v must demote to auto, got %v", model, choice, body["tool_choice"])
			}
		}
	}

	auto := map[string]any{"tool_choice": "auto"}
	DemoteToolChoice(auto, free13)
	if auto["tool_choice"] != "auto" {
		t.Error("an existing auto choice stays untouched")
	}

	absent := map[string]any{}
	DemoteToolChoice(absent, free13)
	if _, has := absent["tool_choice"]; has {
		t.Error("an absent choice is not defaulted here (ensureResponsesFingerprintTools does that)")
	}

	// Non-allowlisted models keep the caller's choice (golden: future 1.4-Free,
	// the Go id without -free, and non-Muse chat ids).
	for _, model := range []string{
		"muse-spark-1.4-contributor-free",
		"muse-spark-1.3-contributor",
		"big-pickle",
		"muse-sparkish-free",
	} {
		body := map[string]any{"tool_choice": "required"}
		DemoteToolChoice(body, model)
		if body["tool_choice"] != "required" {
			t.Errorf("model %q must keep \"required\", got %v", model, body["tool_choice"])
		}
	}

	DemoteToolChoice(nil, free13) // must not panic
}

// ---------------------------------------------------------------------------
// injectReasoningContent (utils/reasoningContentInjector.js, model rule only)
// ---------------------------------------------------------------------------

func TestInjectReasoningContent(t *testing.T) {
	newBody := func(messages ...map[string]any) map[string]any {
		arr := make([]any, 0, len(messages))
		for _, m := range messages {
			arr = append(arr, m)
		}
		return map[string]any{"messages": arr}
	}

	t.Run("deepseek model fills missing reasoning_content", func(t *testing.T) {
		body := newBody(
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "assistant", "content": "echoed", "reasoning_content": "already here"},
			map[string]any{"role": "assistant", "content": "blank", "reasoning_content": ""},
			map[string]any{"role": "assistant", "content": "nulled", "reasoning_content": nil},
		)
		InjectReasoningContent("deepseek-v4-flash-free", body)
		msgs := body["messages"].([]any)
		if got := msgs[0].(map[string]any)["reasoning_content"]; got != nil {
			t.Errorf("user message must stay untouched, got %v", got)
		}
		if got := msgs[1].(map[string]any)["reasoning_content"]; got != " " {
			t.Errorf("assistant without reasoning_content must get the placeholder, got %q", got)
		}
		if got := msgs[2].(map[string]any)["reasoning_content"]; got != "already here" {
			t.Errorf("non-empty reasoning_content must stay, got %q", got)
		}
		if got := msgs[3].(map[string]any)["reasoning_content"]; got != " " {
			t.Errorf("empty reasoning_content must be replaced, got %q", got)
		}
		if got := msgs[4].(map[string]any)["reasoning_content"]; got != " " {
			t.Errorf("null reasoning_content must be replaced (JS typeof check), got %v", got)
		}
	})

	t.Run("model match is case-insensitive", func(t *testing.T) {
		body := newBody(map[string]any{"role": "assistant", "content": "x"})
		InjectReasoningContent("DeepSeek-V4.1", body)
		if got := body["messages"].([]any)[0].(map[string]any)["reasoning_content"]; got != " " {
			t.Errorf("case-insensitive /deepseek/i match failed, got %v", got)
		}
	})

	t.Run("non-deepseek models untouched", func(t *testing.T) {
		for _, model := range []string{free12, free13, "big-pickle", "mimo-v2.5-free", ""} {
			body := newBody(map[string]any{"role": "assistant", "content": "x"})
			InjectReasoningContent(model, body)
			if _, has := body["messages"].([]any)[0].(map[string]any)["reasoning_content"]; has {
				t.Errorf("model %q must not inject reasoning_content", model)
			}
		}
	})

	t.Run("bodies without message arrays are no-ops", func(t *testing.T) {
		InjectReasoningContent("deepseek-v4-flash-free", nil)
		InjectReasoningContent("deepseek-v4-flash-free", map[string]any{})
		InjectReasoningContent("deepseek-v4-flash-free", map[string]any{"messages": "nope"})
	})
}
