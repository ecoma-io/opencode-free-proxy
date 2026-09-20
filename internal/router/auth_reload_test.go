package router

// Auth-as-snapshot tests: auth.keys lives in the per-generation Runtime like
// egresses and routes. A request is admitted against the generation it
// ARRIVES under; a reload that rotates the keys changes admission for the
// NEXT generation only. The completion log line carries the key NAME
// (api_key_name) — the credential value never appears in any log line.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// authUpstream always answers the chat stream; used via a proxied egress so
// the request path is identical to snapshot_test's (absolute-URI through the
// httptest proxy, base pinned by testUpstreamBasePrefix).
func authUpstream(t *testing.T) (*upstreamRecorder, *httptest.Server) {
	rec := &upstreamRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, upstreamCall{Path: r.URL.Path, Body: raw})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, chatStreamSSE)
	}))
	t.Cleanup(up.Close)
	return rec, up
}

func authDoc(keys, upstreamURL string) string {
	return fmt.Sprintf(`auth:
  keys:
%s
egress:
  - {id: a, proxy: {type: http, url: %q}}
routes:
  - {id: r, egress: [a]}
`, keys, upstreamURL)
}

func TestAuthRotationAcrossReload(t *testing.T) {
	dir := t.TempDir()
	rec, up := authUpstream(t)

	// Generation 1: two named keys.
	keys1 := "    - {name: first, key: sk-1}\n" +
		"    - {name: second, key: sk-2}\n"
	s, mux, store := snapshotRouter(t, dir, authDoc(keys1, up.URL), nil)
	_ = s
	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`

	admit := func(key string, want int) {
		t.Helper()
		res := postJSON(t, mux, "/v1/chat/completions", payload,
			map[string]string{"Authorization": "Bearer " + key})
		if res.Code != want {
			t.Fatalf("key %q: status = %d, want %d (body=%s)", key, res.Code, want, res.Body.String())
		}
	}

	admit("sk-1", http.StatusOK)
	admit("sk-2", http.StatusOK)
	admit("sk-3", http.StatusUnauthorized) // not in gen-1

	// Rotate: first is revoked, third added. A generation-2 request with the
	// REVOKED key must 401 — the old credential does not survive the reload.
	keys2 := "    - {name: second, key: sk-2}\n" +
		"    - {name: third, key: sk-3}\n"
	writeCfg(t, dir, testUpstreamBasePrefix+authDoc(keys2, up.URL))
	waitGeneration(t, store, 2)

	admit("sk-1", http.StatusUnauthorized) // revoked by gen-2
	admit("sk-3", http.StatusOK)           // admitted by gen-2
	admit("sk-2", http.StatusOK)           // still valid
	admit("sk-4", http.StatusUnauthorized) // never configured
	if n := rec.count(); n != 4 {
		t.Fatalf("upstream calls = %d, want 4 (only admitted requests dial out)", n)
	}
}

func TestAuthCompletionLogCarriesNameNotKey(t *testing.T) {
	dir := t.TempDir()
	_, up := authUpstream(t)

	var logsMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	const secret = "sk-top-secret-value"
	keys := "    - {name: prod, key: " + secret + "}\n"
	_, mux, _ := snapshotRouter(t, dir, authDoc(keys, up.URL), logf)

	payload := `{"model":"qwen3-coder-free","messages":[{"role":"user","content":"hi"}],"stream":true}`
	res := postJSON(t, mux, "/v1/chat/completions", payload,
		map[string]string{"Authorization": "Bearer " + secret})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", res.Code, res.Body.String())
	}

	logsMu.Lock()
	defer logsMu.Unlock()
	sawName := false
	for _, l := range logs {
		if strings.Contains(l, secret) {
			t.Fatalf("log line leaks the credential value: %q", l)
		}
		if strings.Contains(l, "api_key_name=\"prod\"") {
			sawName = true
		}
	}
	if !sawName {
		t.Fatalf("no completion line carries api_key_name=prod; logs: %v", logs)
	}
}
