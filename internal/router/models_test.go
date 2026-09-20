package router

// Tests for GET /v1/models: the free-tier filter
// (src/app/api/providers/suggested-models/filters.js "opencode-free") and the
// unreachable-upstream fallback.
//
// NOTE on wiring: the JS route fetches a caller-supplied `url` and returns
// {data: []} when the fetch fails; the Go proxy serves /v1/models directly and
// derives the URL from the configured upstream base (s.Cfg.UpstreamBase +
// "/zen/v1/models"), so the happy path is redirectable like the JS route. The
// FILTER logic is unit-tested through parseUpstreamModels; the handler test
// below exercises the fallback (unreachable base), and e2e/models via the
// e2e tag exercises the happy path against a fake upstream.

import (
	"encoding/json"
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

	t.Run("no surviving models yields nil (handler falls back)", func(t *testing.T) {
		if got := parseUpstreamModels([]byte(`{"data":[{"id":"gpt-5"}]}`)); got != nil {
			t.Fatalf("entries = %#v, want nil", got)
		}
	})

	t.Run("malformed payload yields nil", func(t *testing.T) {
		if got := parseUpstreamModels([]byte("not json")); got != nil {
			t.Fatalf("entries = %#v, want nil", got)
		}
	})
}

// TestHandleModelsFallsBackToStaticRegistry: when the upstream model list is
// unreachable the static registry models are served (registry/opencode.js
// models + the known-free id). The test's upstream base is a closed port, so
// the fetch fails and the fallback kicks in.
func TestHandleModelsFallsBackToStaticRegistry(t *testing.T) {
	s := NewServer(
		&config.Config{Port: "0", UpstreamBase: "http://127.0.0.1:1"},
		config.NewDefault(),
		identity.NewUserAgentCache(),
		upstream.NewClient(),
		nil, nil,
	)
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
