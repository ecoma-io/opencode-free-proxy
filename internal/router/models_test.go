package router

// Tests for GET /v1/models: the free-tier filter
// (src/app/api/providers/suggested-models/filters.js "opencode-free"), the
// reachable-empty `{data: []}` parity (route.js), and the unreachable-upstream
// fallback.
//
// NOTE on wiring: the JS route fetches a caller-supplied `url` and returns
// {data: []} when the fetch fails; the Go proxy serves /v1/models directly and
// derives the URL from the pinned runtime's upstream.base (OCFP_CONFIG
// upstream.base + "/zen/v1/models"), so the happy path is redirectable like
// the JS route. The FILTER logic is unit-tested through parseUpstreamModels;
// the handler tests below exercise the fallback (unreachable base) and the
// reachable-empty case, and e2e/models via the e2e tag exercises the happy
// path against a fake upstream.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/identity"
	"opencode-free-proxy/internal/upstream"
)

// TestParseUpstreamModels applies the filters.js opencode-free filter:
// keep ids ending in "-free" plus KNOWN_FREE_OPENCODE_MODELS (big-pickle),
// drop DEAD_FREE_OPENCODE_MODELS (deepseek-v4-flash-free).
func TestParseUpstreamModels(t *testing.T) {
	t.Run("filters the upstream zen list", func(t *testing.T) {
		raw := []byte(`{"data":[
			{"id":"gpt-5"},
			{"id":"qwen3-coder-free"},
			{"id":"big-pickle"},
			{"id":"deepseek-v4-flash-free"},
			{"id":"muse-spark-1.2-contributor-free"},
			{"id":"qwen3-coder-free"}
		]}`)
		entries := parseUpstreamModels(raw)
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Name != e.ID {
				t.Fatalf("filters.js maps {id, name: m.id}; got name %q for id %q", e.Name, e.ID)
			}
			got = append(got, e.ID)
		}
		// Sorted output; gpt-5 has no -free suffix, deepseek is dead, dup dropped.
		if !reflect.DeepEqual(got, []string{"big-pickle", "muse-spark-1.2-contributor-free", "qwen3-coder-free"}) {
			t.Fatalf("filtered ids = %v", got)
		}
	})

	t.Run("bare array payload is accepted", func(t *testing.T) {
		entries := parseUpstreamModels([]byte(`[{"id":"big-pickle"},{"id":"gpt-5"}]`))
		if len(entries) != 1 || entries[0].ID != "big-pickle" {
			t.Fatalf("entries = %#v, want just big-pickle", entries)
		}
	})

	t.Run("models key variant is parsed (route.js json.data ?? json.models)", func(t *testing.T) {
		entries := parseUpstreamModels([]byte(`{"models":[{"id":"big-pickle"},{"id":"gpt-5"}]}`))
		if len(entries) != 1 || entries[0].ID != "big-pickle" {
			t.Fatalf("entries = %#v, want just big-pickle", entries)
		}
	})

	t.Run("data key wins over models key (?? takes the first non-null)", func(t *testing.T) {
		entries := parseUpstreamModels([]byte(`{"data":[{"id":"qwen3-coder-free"}],"models":[{"id":"big-pickle"}]}`))
		if len(entries) != 1 || entries[0].ID != "qwen3-coder-free" {
			t.Fatalf("entries = %#v, want just qwen3-coder-free", entries)
		}
	})

	t.Run("empty data array is valid-empty, not a fallback (route.js returns {data: []})", func(t *testing.T) {
		if got := parseUpstreamModels([]byte(`{"data":[]}`)); got == nil || len(got) != 0 {
			t.Fatalf("entries = %#v, want a non-nil empty slice", got)
		}
	})

	t.Run("no surviving models is valid-empty, not a fallback", func(t *testing.T) {
		if got := parseUpstreamModels([]byte(`{"data":[{"id":"gpt-5"}]}`)); got == nil || len(got) != 0 {
			t.Fatalf("entries = %#v, want a non-nil empty slice", got)
		}
	})

	t.Run("bare empty array is valid-empty (the ?? json arm)", func(t *testing.T) {
		if got := parseUpstreamModels([]byte(`[]`)); got == nil || len(got) != 0 {
			t.Fatalf("entries = %#v, want a non-nil empty slice", got)
		}
	})

	t.Run("malformed payload yields nil (handler falls back)", func(t *testing.T) {
		if got := parseUpstreamModels([]byte("not json")); got != nil {
			t.Fatalf("entries = %#v, want nil", got)
		}
	})
}

// modelsServerBase wires a Server whose runtime pins the given upstream base
// (upstream.base from the config doc — same snapshot path as main.go), with a
// cold UA cache. modelsServer is the unreachable case: a closed port, so the
// /v1/models fetch fails and the static registry answers.
func modelsServerBase(t *testing.T, base string) *Server {
	t.Helper()
	doc := fmt.Sprintf("upstream:\n  base: %q\n", base) +
		"egress:\n  - {id: direct}\n" +
		"routes:\n  - {id: default, egress: [direct]}\n"
	p := writeCfg(t, t.TempDir(), doc)
	store, err := config.NewStore(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	return NewServer(store, identity.NewUserAgentCache(), upstream.NewClient(), nil)
}

func modelsServer(t *testing.T) *Server {
	t.Helper()
	return modelsServerBase(t, "http://127.0.0.1:1")
}

func TestHandleModelsFallsBackToStaticRegistry(t *testing.T) {
	s := modelsServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	s.HandleModels(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Object string        `json:"object"`
		Data   []modelsEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object = %q, want %q", body.Object, "list")
	}
	ids := map[string]bool{}
	for _, e := range body.Data {
		if e.ID == "" {
			t.Fatalf("entry without id: %#v", e)
		}
		ids[e.ID] = true
	}
	for _, want := range []string{"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free", "big-pickle"} {
		if !ids[want] {
			t.Fatalf("static registry fallback missing %q; got %v", want, ids)
		}
	}
}

// TestHandleModelsReachableEmptyUpstream: when the upstream list is reachable
// but filters to zero free ids the response is `{data: []}` — NOT the static
// registry (suggested-models/route.js returns `{data: []}` for a valid empty
// list). The fake upstream answers 200 with a bare non-free id.
func TestHandleModelsReachableEmptyUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5"}]}`)
	}))
	defer up.Close()
	s := modelsServerBase(t, up.URL)
	rec := httptest.NewRecorder()
	s.HandleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []modelsEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body.Data == nil {
		t.Fatalf("data = nil, want a non-nil empty array (`{data: []}`, not the static registry)")
	}
	if len(body.Data) != 0 {
		t.Fatalf("data = %#v, want [] (static registry must NOT answer a reachable empty list)", body.Data)
	}
}

// TestHandleModelsDrainGateAnswers503: a draining server refuses NEW
// /v1/models requests with 503, the same contract relay() applies to
func TestHandleModelsDrainGateAnswers503(t *testing.T) {
	s := modelsServer(t)
	s.Drain()
	rec := httptest.NewRecorder()
	s.HandleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
}
