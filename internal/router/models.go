package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// models only when the upstream list is unreachable or its body carries no
// usable shape (fail-open); a REACHABLE list that filters to zero free ids is
// served as an empty data array — JS returns `{data: []}` for that case too
// (suggested-models/route.js:21-26), and phantom registry models would
// misadvertise a dropped free tier.
// The endpoint rides the public zen list with the upstream Bearer public
// credential, like the 9router JS source.
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
		// The base is resolved (Resolve validated it), but Parse keeps the
		// host comparison honest: a base URL whose parse fails would leave nil,
		// and the check below must refuse every redirect rather than panic. The
		// addressable failure (unbuildable request) is the fetch's own.
		base, _ := url.Parse(rt.UpstreamBase())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.UpstreamBase()+config.ZenModelsPath, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+config.PublicBearer)
			// Redirect allow-list (GHSA-5472-vw5j-wjvg): the models fetch is
			// the one direct-client flow whose target is the operator's
			// upstream base (model list redirectable for self-hosting), so a
			// hostile 3xx from that origin must not steer the fetch at a
			// private network through the host's own network. The shared
			// s.Upstream client cannot carry this permanently — the UA sync
			// loop legitimately follows cross-host redirects (api.github.com →
			// raw.githubusercontent.com → registry.npmjs.org) through the same
			// client — so the follow policy is a per-request CLONE bound to
			// the snapshot's base host. The Transport is shared, so pooling is
			// unaffected; the CheckRedirect only constrains this request. A
			// refused redirect fails the fetch and lands the fail-open static
			// registry, never an internal address.
			client := *s.Upstream.HTTP // copy: caller's CheckRedirect only, same Transport
			client.CheckRedirect = func(next *http.Request, _ []*http.Request) error {
				if base == nil || !strings.EqualFold(next.URL.Hostname(), base.Hostname()) {
					return fmt.Errorf("redirect refused: %s leaves the upstream base", boundedHost(next.URL.Hostname()))
				}
				return nil
			}
			if resp, err := client.Do(req); err == nil {
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

// parseUpstreamModels applies the free filter to the upstream JSON. It ports
// the shape chain `json.data ?? json.models ?? json`
// (suggested-models/route.js:23): the object's `data` array first, then its
// `models` array, then a top-level bare array. Any body carrying one of those
// shapes is VALID — the result stays a non-nil (possibly empty) slice, so a
// 200 with a valid but empty free list is served as `{data: []}` exactly like
// JS (route.js:21-26 — the filter result is returned verbatim, empty or not).
// Only a body carrying none of the shapes returns nil, which the handler
// answers with the static registry: this proxy's documented fail-open
// divergence (AGENTS.md porting rule 6 "models fallback to the static
// registry") narrows JS's own `{data: []}` catch-all to genuinely unusable
// bodies.
func parseUpstreamModels(raw []byte) []modelsEntry {
	type idList []struct {
		ID string `json:"id"`
	}
	var obj struct {
		Data   idList `json:"data"`
		Models idList `json:"models"`
	}
	var list idList
	if err := json.Unmarshal(raw, &obj); err == nil {
		// route.js:23 — json.data ?? json.models: an absent or null key leaves
		// the Go slice nil, so nil-ness IS the `??` coalescing. A JSON object
		// with neither key falls through JS's `json` arm to the not-an-array
		// case → `[]` (:24), which the non-nil empty result below reproduces.
		list = obj.Data
		if list == nil {
			list = obj.Models
		}
	} else {
		// The `?? json` arm: some versions return a top-level bare array.
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil
		}
	}
	seen := map[string]bool{}
	out := make([]modelsEntry, 0, len(list)) // non-nil: valid shapes stay valid-empty
	for _, m := range list {
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

// boundedHost caps the base-host echo in the redirect-refusal error: the host
// may carry attacker-chosen characters and the refusal is a client-facing
// message, so the echo stays short (the message itself never reaches a log —
// it is the fail-open trigger, and the static fallback answered).
func boundedHost(host string) string {
	const max = 100
	if len(host) > max {
		return host[:max] + "…"
	}
	return host
}
