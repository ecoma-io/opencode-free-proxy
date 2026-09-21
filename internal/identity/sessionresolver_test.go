// Session resolution chain tests — ported from 9router
// open-sse/utils/sessionManager.js (extractClientSessionId →
// assistantTextSessionId → deriveSessionId) as reached by
// open-sse/executors/opencode.js resolveOpencodeSession.
package identity

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExtractClientSessionIDPriority(t *testing.T) {
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	cases := []struct {
		note    string
		headers map[string]string
		body    map[string]any
		want    string
	}{
		{
			note: "claude metadata.user_id _session_<uuid>",
			body: map[string]any{"metadata": map[string]any{"user_id": "acct:_session_" + uuid}},
			want: "claude:" + uuid,
		},
		{
			note: "claude metadata.user_id JSON session_id",
			body: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"json-sid"}`}},
			want: "claude:json-sid",
		},
		{
			// JS returns m[1] raw (sessionManager.js:115) — no 256 cap on the
			// regex capture (only the JSON branch normalizes, :117).
			note: "claude _session_ capture passes through raw, uncapped",
			body: map[string]any{"metadata": map[string]any{
				"user_id": "acct:_session_" + strings.Repeat("ab", 150),
			}},
			want: "claude:" + strings.Repeat("ab", 150),
		},
		{
			// JS: normalizeSessionId(String(sid)) — numbers coerce
			// (sessionManager.js:133).
			note: "antigravity numeric sessionId is String()-coerced",
			body: map[string]any{"request": map[string]any{"sessionId": 42}},
			want: "antigravity:42",
		},
		{
			note:    "claude header fallback",
			headers: map[string]string{"x-claude-code-session-id": "hdr-sid"},
			want:    "claude:hdr-sid",
		},
		{
			note:    "body metadata beats the claude header",
			headers: map[string]string{"x-claude-code-session-id": "hdr-sid"},
			body:    map[string]any{"metadata": map[string]any{"user_id": "acct:_session_" + uuid}},
			want:    "claude:" + uuid,
		},
		{
			note: "antigravity request.sessionId",
			body: map[string]any{"request": map[string]any{"sessionId": "conv-abc-123"}},
			want: "antigravity:conv-abc-123",
		},
		{
			note: "generic session headers in priority order (first key wins)",
			headers: map[string]string{
				"x-session-id":    "a",
				"session-id":      "b",
				"session_id":      "c",
				"x-amp-thread-id": "d",
			},
			want: "a",
		},
		{
			note:    "second-priority header when the first is blank",
			headers: map[string]string{"x-session-id": "   ", "session-id": "b"},
			want:    "b",
		},
		{
			note:    "x-amp-thread-id alone",
			headers: map[string]string{"x-amp-thread-id": "amp-1"},
			want:    "amp-1",
		},
		{
			note:    "x-client-request-id after the session headers",
			headers: map[string]string{"x-client-request-id": "req-42"},
			want:    "req-42",
		},
		{
			note: "body prompt_cache_key",
			body: map[string]any{"prompt_cache_key": "pck-1"},
			want: "pck-1",
		},
		{
			note: "body session_id",
			body: map[string]any{"session_id": "sid-1"},
			want: "sid-1",
		},
		{
			note: "body conversation_id",
			body: map[string]any{"conversation_id": "conv-1"},
			want: "conv-1",
		},
		{
			note: "plain body metadata.user_id last",
			body: map[string]any{"metadata": map[string]any{"user_id": "user-1"}},
			want: "user-1",
		},
		{
			note: "overlong candidates are skipped (256 cap)",
			headers: map[string]string{
				"x-session-id": strings.Repeat("x", 257),
				"session-id":   "short",
			},
			want: "short",
		},
		{
			note: "non-string body fields are skipped",
			body: map[string]any{"session_id": 12345, "conversation_id": true},
			want: "",
		},
		{
			note:    "x-opencode-session is the executor's native header, not a resolver candidate",
			headers: map[string]string{"x-opencode-session": "ses_f534dfae8ffeCy4Ee4tLWNygDc"},
			want:    "",
		},
		{
			note: "nothing present",
			want: "",
		},
	}
	for _, tc := range cases {
		if got := extractClientSessionID(tc.headers, tc.body); got != tc.want {
			t.Errorf("[%s] extractClientSessionID() = %q, want %q", tc.note, got, tc.want)
		}
	}
}

// PARITY BUG repro (expected to FAIL until fixed — see task report):
// sessionManager.js extractAntigravitySession also reads the conversation uuid
// embedded in body.requestId (ANTIGRAVITY_CONV_RE = /^[a-z]+\/([0-9a-f-]{36})\//i),
// returning "antigravity:<uuid>". internal/identity/sessionresolver.go:144
// only implements request.sessionId and drops the requestId branch, so such
// bodies fall through to the weaker assistant-text/connection fallbacks and
// resolve to a different session id than the JS router.
func TestExtractClientSessionIDAntigravityRequestIdBUG(t *testing.T) {
	body := map[string]any{
		"requestId": "antigravity/550e8400-e29b-41d4-a716-446655440000/turn_1",
	}
	want := "antigravity:550e8400-e29b-41d4-a716-446655440000"
	if got := extractClientSessionID(nil, body); got != want {
		t.Errorf("extractClientSessionID(requestId conversation) = %q, JS returns %q", got, want)
	}
}

func TestNormalizeSessionIDCaps(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"  padded  ", "padded"},
		{"", ""},
		{"   ", ""},
		{12345, ""},
		{nil, ""},
		{strings.Repeat("x", 256), strings.Repeat("x", 256)},
		{strings.Repeat("x", 257), ""},
	}
	for _, tc := range cases {
		if got := normalizeSessionID(tc.in); got != tc.want {
			t.Errorf("normalizeSessionID(%v) = %q (len %d), want %q (len %d)", tc.in, got, len(got), tc.want, len(tc.want))
		}
	}
}

// assistantBody builds a body whose accumulated assistant text is prefix +
// n filler chars.
func assistantBody(prefix string, n int) map[string]any {
	return map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "message", "role": "assistant", "content": prefix + strings.Repeat("a", n)},
		},
	}
}

// The accumulated-assistant-text hash kicks in at >= 50 chars and yields a
// stable binary-style id keyed on (connection, first 50 chars of text).
func TestResolveSessionIDAssistantTextChain(t *testing.T) {
	binaryIDRe := regexp.MustCompile(`^[0-9a-f]{32}[0-9]+$`)

	first := ResolveSessionIDScoped(assistantBody("", 60), nil, "conn-1")
	if !binaryIDRe.MatchString(first) {
		t.Errorf("assistant-text session id %q does not match the rs()+Date.now() shape", first)
	}
	if second := ResolveSessionIDScoped(assistantBody("", 60), nil, "conn-1"); second != first {
		t.Errorf("same assistant text resolved %q then %q, want stable", first, second)
	}
	// A different text prefix must land on a different id.
	if other := ResolveSessionIDScoped(assistantBody("b", 60), nil, "conn-1"); other == first {
		t.Error("different assistant text must resolve to a different session id")
	}
	// A different connection keys the hash differently even for equal text.
	if sameConn := ResolveSessionIDScoped(assistantBody("", 60), nil, "conn-2"); sameConn == first {
		t.Error("different connections must not share the assistant-text hash key")
	}
	// Only the first 50 chars key the hash (ASSISTANT_CAP_LEN): bodies sharing
	// the first 50 chars but differing later resolve to the same id.
	shared := ResolveSessionIDScoped(assistantBody("", 60), nil, "conn-1")
	tail := map[string]any{
		"input": []any{
			map[string]any{"role": "assistant", "content": strings.Repeat("a", 60) + "-different-tail"},
		},
	}
	if got := ResolveSessionIDScoped(tail, nil, "conn-1"); got != shared {
		t.Errorf("texts sharing the first 50 chars resolved to %q and %q, want one stable id", got, shared)
	}
}

// Text below the 50-char minimum never hits the assistant chain and falls to
// the runtime-store terminal fallback (deriveSessionId, sessionManager.js:45-66)
// — STABLE per scope. The executor-facing empty scope is stable too: JS's
// opencode call site always carries the router's non-empty "noauth"
// connectionId (src/sse/services/auth.js:46-82,230 → executors/opencode.js:149),
// so deriveSessionId's !connectionId fresh-per-call branch is dead on this
// path and a fresh id per request would churn upstream session/prompt-cache
// affinity for headerless first-turn clients.
func TestResolveSessionIDFallbackChain(t *testing.T) {
	short := map[string]any{
		"messages": []any{map[string]any{"role": "assistant", "content": strings.Repeat("a", 49)}},
	}
	a := ResolveSessionID(short, nil)
	b := ResolveSessionID(short, nil)
	if a != b {
		t.Errorf("empty scope must reuse one stable process id, got %q then %q", a, b)
	}
	if a == "" {
		t.Error("fallback must still produce an id")
	}

	c1 := ResolveSessionIDScoped(short, nil, "conn-1")
	c2 := ResolveSessionIDScoped(short, nil, "conn-1")
	if c1 != c2 {
		t.Errorf("connection-scoped fallback unstable: %q then %q", c1, c2)
	}
	if c3 := ResolveSessionIDScoped(short, nil, "conn-2"); c3 == c1 {
		t.Error("different connections must get different fallback ids")
	}
	if c3 := ResolveSessionIDScoped(short, nil, "conn-2"); c3 == a {
		t.Error("the empty scope and named connections must not share a fallback id")
	}
}

// The store caps itself at max: the oldest-lastUsed entry is evicted so the
// newest key still resolves (sessionManager.js:57-61 MAX_SESSIONS eviction).
func TestStoreGetOrCreateEvictsAtCap(t *testing.T) {
	s := newStore(2)
	s.getOrCreate("k1", func() string { return "id-1" })
	s.getOrCreate("k2", func() string { return "id-2" })
	// k1 is now the oldest → the third insertion evicts it, not k2.
	if got := s.getOrCreate("k3", func() string { return "id-3" }); got != "id-3" {
		t.Fatalf("insertion at cap = %q, want the generated id", got)
	}
	s.mu.Lock()
	_, hasK1 := s.entries["k1"]
	_, hasK2 := s.entries["k2"]
	size := len(s.entries)
	s.mu.Unlock()
	if hasK1 {
		t.Error("oldest entry k1 must be evicted at cap")
	}
	if !hasK2 {
		t.Error("younger entry k2 must survive the eviction")
	}
	if size != 2 {
		t.Errorf("store size = %d, want capped at 2", size)
	}
	// A re-get of the evicted key regenerates (and returns) a fresh id.
	if got := s.getOrCreate("k1", func() string { return "id-1b" }); got != "id-1b" {
		t.Errorf("re-get after eviction = %q, want a regenerated id", got)
	}
}

// The janitor body reclaims entries idle past the TTL from both stores
// (sessionManager.js:19-26); a reclaimed terminal-fallback entry is simply
// re-derived on the next request — the documented reset path.
func TestEvictExpired(t *testing.T) {
	now := time.Now()
	connectionStore.mu.Lock()
	connectionStore.entries["opencode:janitor-stale"] = storeEntry{id: "old", lastUsed: now.Add(-SessionTTL - time.Minute)}
	connectionStore.entries["opencode:janitor-live"] = storeEntry{id: "live", lastUsed: now}
	connectionStore.mu.Unlock()
	assistantStore.mu.Lock()
	assistantStore.entries["opencode::janitor-stale"] = storeEntry{id: "old", lastUsed: now.Add(-SessionTTL - time.Minute)}
	assistantStore.mu.Unlock()

	evictExpired(now.Add(-SessionTTL))

	connectionStore.mu.Lock()
	_, staleConn := connectionStore.entries["opencode:janitor-stale"]
	liveID, liveConn := connectionStore.entries["opencode:janitor-live"]
	connectionStore.mu.Unlock()
	assistantStore.mu.Lock()
	_, staleAssist := assistantStore.entries["opencode::janitor-stale"]
	assistantStore.mu.Unlock()
	if staleConn || staleAssist {
		t.Error("entries idle past the TTL must be reclaimed from both stores")
	}
	if !liveConn || liveID.id != "live" {
		t.Error("entries used within the TTL must survive")
	}
}

// A client-provided session wins over every fallback (and is returned
// untranslated here — translation into ses_ form happens in the executor).
func TestResolveSessionIDClientSessionWins(t *testing.T) {
	body := assistantBody("", 60)
	body["prompt_cache_key"] = "client-sid"
	if got := ResolveSessionIDScoped(body, nil, "conn-1"); got != "client-sid" {
		t.Errorf("ResolveSessionIDScoped() = %q, want the client session %q", got, "client-sid")
	}
}

// accumulateAssistantText sums assistant-only text (string or block array)
// and stops once the 50-char cap is passed (the item that crosses the cap is
// included in full — JS appends then breaks).
func TestAccumulateAssistantText(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("s", 100)},
			map[string]any{"role": "assistant", "content": "one"},
			map[string]any{"role": "user", "content": strings.Repeat("u", 100)},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "two"},
			}},
			map[string]any{"role": "assistant", "content": strings.Repeat("z", 100)},
		},
	}
	got := accumulateAssistantText(body)
	want := "onetwo" + strings.Repeat("z", 100) // capped: loop breaks after the crossing item
	if got != want {
		t.Errorf("accumulateAssistantText() = %q (%d chars), want %q (%d chars)", got, len(got), want, len(want))
	}

	if got := accumulateAssistantText(map[string]any{"input": []any{
		map[string]any{"role": "assistant", "content": strings.Repeat("b", 60)},
	}}); got != strings.Repeat("b", 60) {
		t.Errorf("Responses input[] items must accumulate too, got %q", got)
	}

	if got := accumulateAssistantText(map[string]any{}); got != "" {
		t.Errorf("no messages → empty text, got %q", got)
	}

	// JS `c?.text || c?.output || ""` is first-truthy-wins
	// (sessionManager.js:175): a part with both fields contributes only its
	// text; an empty text defers to output; falsy both → nothing.
	both := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{
		map[string]any{"text": "TEXT", "output": "OUTPUT"},
		map[string]any{"text": "", "output": "OUT2"},
		map[string]any{"text": "", "output": ""},
	}}}}
	if got := accumulateAssistantText(both); got != "TEXTOUT2" {
		t.Errorf("first-truthy-wins accumulation = %q, want %q", got, "TEXTOUT2")
	}
}

// generateBinaryStyleID mirrors the binary's rs()+Date.now() fallback shape.
// (JS uses crypto.randomUUID(), which carries dashes; the Go port emits 32
// hex chars — the id is hashed into ses_ form before it reaches upstream, so
// only the internal store key differs. Documented deviation, not asserted.)
func TestGenerateBinaryStyleIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}[0-9]+$`)
	a := generateBinaryStyleID()
	if !re.MatchString(a) {
		t.Errorf("generateBinaryStyleID() = %q, want 32 hex + decimal timestamp", a)
	}
	if b := generateBinaryStyleID(); a == b {
		t.Error("two generated ids must differ")
	}
}

// sha16 keeps the first 16 hex chars of the sha256 (assistant-text hash key).
func TestSha16(t *testing.T) {
	if got := sha16("opencode"); len(got) != 16 {
		t.Errorf("sha16 length = %d, want 16", len(got))
	}
	if sha16("a") == sha16("b") {
		t.Error("different texts must hash differently")
	}
}
