package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"opencode-free-proxy/internal/config"
)

// modelsEntry is one /v1/models item.
type modelsEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HandleModels is GET /v1/models: fetches the upstream zen model list and
// filters it to the free tier (src/app/api/providers/suggested-models/
// filters.js "opencode-free"): ids ending in "-free" (plus big-pickle),
// minus known-dead ids. The JS route fetches a caller-supplied `url`; this
// proxy's equivalent is the configured upstream base (upstream.base in
// OCFP_CONFIG, default https://opencode.ai), so the whole list endpoint is
// redirectable for tests/self-hosting. Falls back to the static registry
// models when the upstream list is unreachable (fail-open, unchanged).
// The endpoint is intentionally NOT auth-gated — same as the removed env
// proxy and the 9router JS source: model discovery rides the public zen
// list with the upstream Bearer public credential, not the inbound keys.
//
// The fetch rides the SHARED direct client (s.Upstream) rather than a
// per-request http.Client: one connection pool, one transport configuration
// (its ResponseHeaderTimeout is the only upstream deadline). The total fetch
// bound is restored with a context deadline (config.ModelsFetchTimeout).
func (s *Server) HandleModels(w http.ResponseWriter, r *http.Request) {
	// Drain gate, same contract as relay(): a shutting-down server refuses
	// NEW work with 503 instead of starting an upstream fetch. The fail-open
	// fallback below applies only to fetch problems while serving.
	if s.Draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	var entries []modelsEntry
	if s.Upstream != nil && s.Upstream.HTTP != nil {
		ctx, cancel := context.WithTimeout(r.Context(), config.ModelsFetchTimeout)
		defer cancel()
		// ONE snapshot read for the base: two reads could straddle a reload,
		// and the fail-open fallback would mask the skew. The base rides the
		// current generation like every other request.
		rt := s.runtime()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.UpstreamBase()+config.ZenModelsPath, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+config.PublicBearer)
			if resp, err := s.Upstream.HTTP.Do(req); err == nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				_ = resp.Body.Close()
				entries = parseUpstreamModels(raw)
			}
		}
	}
	if entries == nil {
		entries = staticFreeModels()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   entries,
	})
}

// parseUpstreamModels applies the free filter to the upstream JSON.
func parseUpstreamModels(raw []byte) []modelsEntry {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Data) == 0 {
		// Some versions return a bare array.
		var arr []struct {
			ID string `json:"id"`
		}
		if err2 := json.Unmarshal(raw, &arr); err2 != nil {
			return nil
		}
		for _, m := range arr {
			parsed.Data = append(parsed.Data, struct {
				ID string `json:"id"`
			}{m.ID})
		}
		if len(parsed.Data) == 0 {
			return nil
		}
	}
	seen := map[string]bool{}
	out := make([]modelsEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		id := m.ID
		if id == "" || seen[id] {
			continue
		}
		if !strings.HasSuffix(id, "-free") && !knownFree(id) {
			continue
		}
		if deadFree(id) {
			continue
		}
		seen[id] = true
		out = append(out, modelsEntry{ID: id, Name: id})
	}
	if len(out) == 0 {
		return nil
	}
	sortModels(out)
	return out
}

func knownFree(id string) bool {
	for _, k := range strings.Split(config.KnownFreeModels, ",") {
		if strings.TrimSpace(k) == id {
			return true
		}
	}
	return false
}

func deadFree(id string) bool {
	for _, d := range strings.Split(config.DeadFreeModels, ",") {
		if strings.TrimSpace(d) == id {
			return true
		}
	}
	return false
}

// staticFreeModels is the registry fallback (providers/registry/opencode.js
// models + the known-free id).
func staticFreeModels() []modelsEntry {
	out := []modelsEntry{
		{ID: "muse-spark-1.2-contributor-free", Name: "Muse Spark 1.2 Contributor Free"},
		{ID: "muse-spark-1.3-contributor-free", Name: "Muse Spark 1.3 Contributor Free"},
	}
	if known := knownFree("big-pickle"); known {
		out = append(out, modelsEntry{ID: "big-pickle", Name: "big-pickle"})
	}
	return out
}

func sortModels(entries []modelsEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
}
