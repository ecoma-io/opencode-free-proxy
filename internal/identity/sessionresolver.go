// Session resolution ported from 9router open-sse/utils/sessionManager.js —
// the conversation-stable session id chain the opencode executor falls back
// to when the client sends no x-opencode-session header:
//
//	client headers/body → accumulated-assistant-text hash → runtime-store id
//
// The stores keep ids stable across requests so upstream prompt caching keeps
// hitting; entries expire after the TTL.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"opencode-free-proxy/internal/jsonx"
)

// Memory config (config/runtimeConfig.js MEMORY_CONFIG).
const (
	SessionTTL           = 2 * time.Hour
	SessionCleanupEvery  = 30 * time.Minute
	maxSessions          = 1000
	maxAssistantSessions = 5000
	assistantMinLen      = 50
	assistantCapLen      = 50
)

// sessionHeaderKeys are the generic inbound session headers, priority order.
var sessionHeaderKeys = []string{"x-session-id", "session-id", "session_id", "x-amp-thread-id"}

const claudeCodeSessionHeader = "x-claude-code-session-id"

var claudeCodeSessionRe = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// store is a TTL+size-capped id cache shared by both resolvers.
type store struct {
	mu      sync.Mutex
	entries map[string]storeEntry
	max     int
}

type storeEntry struct {
	id       string
	lastUsed time.Time
}

func newStore(max int) *store {
	return &store{entries: map[string]storeEntry{}, max: max}
}

// getOrCreate returns the cached id for key, creating it with gen otherwise.
// Insertion beyond max evicts the oldest entry (Go map order is random; the
// JS evicts insertion-oldest — with the TTL cleaner running this is
// behaviorally equivalent for id stability).
func (s *store) getOrCreate(key string, gen func() string) string {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, has := s.entries[key]; has {
		s.entries[key] = storeEntry{id: e.id, lastUsed: now}
		return e.id
	}
	if len(s.entries) >= s.max {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range s.entries {
			if first || e.lastUsed.Before(oldest) {
				oldestKey, oldest, first = k, e.lastUsed, false
			}
		}
		delete(s.entries, oldestKey)
	}
	id := gen()
	s.entries[key] = storeEntry{id: id, lastUsed: now}
	return id
}

var connectionStore = newStore(maxSessions)
var assistantStore = newStore(maxAssistantSessions)

func init() {
	// Periodic TTL eviction, mirroring the cleanup interval
	// (sessionManager.js:19-26).
	go func() {
		ticker := time.NewTicker(SessionCleanupEvery)
		defer ticker.Stop()
		for range ticker.C {
			evictExpired(time.Now().Add(-SessionTTL))
		}
	}()
}

// evictExpired is the janitor body shared by the ticker loop and tests:
// entries whose lastUsed predates cutoff are reclaimed from both stores.
func evictExpired(cutoff time.Time) {
	for _, s := range []*store{connectionStore, assistantStore} {
		s.mu.Lock()
		for k, e := range s.entries {
			if e.lastUsed.Before(cutoff) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

// normalizeSessionID trims and length-caps a candidate (normalizeSessionId).
func normalizeSessionID(value any) string {
	s, is := value.(string)
	if !is {
		return ""
	}
	v := strings.TrimSpace(s)
	if v == "" || len(v) > 256 {
		return ""
	}
	return v
}

func headerValue(headers map[string]string, key string) string {
	return normalizeSessionID(headers[key])
}

// extractClaudeCodeSession reads metadata.user_id (`_session_<uuid>` or JSON).
func extractClaudeCodeSession(body map[string]any) string {
	userID := jsonx.AsStr(jsonx.Get(body["metadata"], "user_id"))
	if userID == "" {
		return ""
	}
	if m := claudeCodeSessionRe.FindStringSubmatch(userID); m != nil {
		// JS returns m[1] raw (sessionManager.js:115) — no trim, no length
		// cap; the [a-f0-9-]+ capture cannot carry whitespace, and a
		// >256-char capture is unreachable for the uuid-shaped ids Claude
		// Code sends.
		return m[1]
	}
	if strings.HasPrefix(userID, "{") {
		var parsed map[string]any
		if json.Unmarshal([]byte(userID), &parsed) == nil {
			return normalizeSessionID(parsed["session_id"])
		}
	}
	return ""
}

// antigravityConvRe is JS ANTIGRAVITY_CONV_RE: `^[a-z]+\/([0-9a-f-]{36})\/`.
var antigravityConvRe = regexp.MustCompile(`(?i)^[a-z]+/([0-9a-f-]{36})/`)

// extractAntigravitySession reads request.sessionId / requestId conversation
// (sessionManager.js extractAntigravitySession).
func extractAntigravitySession(body map[string]any) string {
	// JS: sid != null && sid !== "" → normalizeSessionId(String(sid)) —
	// numbers and bools are String()-coerced too.
	if req := jsonx.AsObj(body["request"]); req != nil {
		if sid := req["sessionId"]; sid != nil {
			if s, is := sid.(string); is {
				if s != "" {
					return normalizeSessionID(s)
				}
			} else {
				return normalizeSessionID(fmt.Sprintf("%v", sid))
			}
		}
	}
	// JS: typeof body?.requestId === "string" ? body.requestId.match(RE) —
	// requestId is TOP-LEVEL on the body, not under request.
	if rid, is := body["requestId"].(string); is {
		if m := antigravityConvRe.FindStringSubmatch(rid); m != nil {
			return normalizeSessionID(m[1])
		}
	}
	return ""
}

// extractClientSessionID reads the client-provided session id
// (extractClientSessionId). Returns "" when none present.
func extractClientSessionID(headers map[string]string, body map[string]any) string {
	if claude := extractClaudeCodeSession(body); claude != "" {
		return "claude:" + claude
	}
	if v := headerValue(headers, claudeCodeSessionHeader); v != "" {
		return "claude:" + v
	}
	if ag := extractAntigravitySession(body); ag != "" {
		return "antigravity:" + ag
	}
	for _, key := range sessionHeaderKeys {
		if v := headerValue(headers, key); v != "" {
			return v
		}
	}
	if v := headerValue(headers, "x-client-request-id"); v != "" {
		return v
	}
	for _, path := range []struct{ obj, field string }{
		{"", "prompt_cache_key"},
		{"", "session_id"},
		{"", "conversation_id"},
		{"metadata", "user_id"},
	} {
		var v any
		if path.obj == "" {
			v = body[path.field]
		} else {
			v = jsonx.Get(body[path.obj], path.field)
		}
		if s := normalizeSessionID(v); s != "" {
			return s
		}
	}
	return ""
}

// accumulateAssistantText sums assistant text across messages/input items
// (cap-limited).
func accumulateAssistantText(body map[string]any) string {
	items := jsonx.AsArr(body["messages"])
	if items == nil {
		items = jsonx.AsArr(body["input"])
	}
	var b strings.Builder
	for _, raw := range items {
		item := jsonx.AsObj(raw)
		if item == nil || jsonx.AsStr(item["role"]) != "assistant" {
			continue
		}
		if s, is := item["content"].(string); is {
			b.WriteString(s)
		} else if content := jsonx.AsArr(item["content"]); content != nil {
			for _, cRaw := range content {
				c := jsonx.AsObj(cRaw)
				if c == nil {
					continue
				}
				// JS is first-truthy-wins: `text += c?.text || c?.output || ""`
				// (sessionManager.js:175) — a part carrying both fields
				// contributes only its text. A truthy NON-string value (never a
				// real content part) contributes "" rather than its JS String()
				// coercion.
				if jsonx.Truthy(c["text"]) {
					b.WriteString(jsonx.AsStr(c["text"]))
				} else if jsonx.Truthy(c["output"]) {
					b.WriteString(jsonx.AsStr(c["output"]))
				}
			}
		}
		if b.Len() >= assistantCapLen {
			break
		}
	}
	return b.String()
}

func sha16(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// generateBinaryStyleID mimics the binary's rs()+Date.now() format.
func generateBinaryStyleID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:]) + itoa64(time.Now().UnixMilli())
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ResolveSessionID ports the executor's resolveSessionId call
// (executors/opencode.js:146-151): client session → accumulated-assistant-text
// hash → stable terminal fallback.
func ResolveSessionID(body map[string]any, headers map[string]string) string {
	return ResolveSessionIDScoped(body, headers, "")
}

// ResolveSessionIDScoped is the full chain with an explicit connection scope.
func ResolveSessionIDScoped(body map[string]any, headers map[string]string, connectionID string) string {
	if client := extractClientSessionID(headers, body); client != "" {
		return client
	}
	if text := accumulateAssistantText(body); len(text) >= assistantMinLen {
		key := sha16("opencode:" + connectionID + ":" + text[:min(len(text), assistantCapLen)])
		return assistantStore.getOrCreate(key, generateBinaryStyleID)
	}
	// Terminal fallback — deriveSessionId (sessionManager.js:45-66): a STABLE
	// id from the runtime store. In JS the opencode executor always passes a
	// non-empty connectionId — opencode is a noAuth free provider
	// (providers/registry/opencode.js:13-14), so the router injects a virtual
	// "noauth" connection (src/sse/services/auth.js:46-82, id at :67) and
	// stamps it onto the credentials (auth.js:230), which resolveOpencodeSession
	// forwards (executors/opencode.js:149) — so deriveSessionId's
	// !connectionId fresh-per-call branch (sessionManager.js:46-48) is dead on
	// this path. The Go executor has no connection identity and calls with
	// connectionID == ""; a fresh id per request there churned the upstream
	// session/prompt-cache affinity for headerless first-turn clients, so the
	// empty scope is keyed in the SAME store instead — one stable id per
	// process, exactly the property JS guarantees. (Keying the empty scope as
	// "opencode:" rather than forwarding JS's literal "noauth" only relabels
	// the internal store key; the id space is identical.)
	//
	// Store lifecycle (AGENTS.md): survives config hot-reloads — it is
	// process-wide identity, not policy; resets only via the TTL janitor (an
	// entry idle > SessionTTL is reclaimed and the next request derives a
	// fresh stable id — sessionManager.js:19-26) or a process restart; who
	// still needs it — every headerless request below the assistant-text
	// minimum shares the entry, but no request pins it: each request
	// re-resolves, so janitor eviction mid-flight is safe.
	return connectionStore.getOrCreate("opencode:"+connectionID, generateBinaryStyleID)
}
