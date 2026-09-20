// Package router exposes the OpenAI-compatible endpoints (/v1/chat/completions,
// /v1/responses, /v1/models) and drives the full request pipeline in the same
// order as 9router chatCore (plus the chat.js pre-resolution stages):
//
//	[1m] marker strip → missing-model 400 → test-connection probe →
//	bypass short-circuit → alias strip / format detect → snapshot thinking
//	intent → unsupported-modality strip → translateRequest prenorms →
//	format translation → ApplyThinking → FilterToOpenAIFormat (chat target) →
//	claude-client tool dedupe → executor transform → upstream call →
//	forced SSE→JSON (SSE upstream only) / relay aggregate.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"opencode-free-proxy/internal/caps"
	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/relay"
	"opencode-free-proxy/internal/routing"
	"opencode-free-proxy/internal/translate"
	"opencode-free-proxy/internal/upstream"
	"opencode-free-proxy/internal/usage"
)

// aliasRe strips the "oc/" provider alias prefix (PROVIDER_ID_TO_ALIAS
// resolution happens before chatCore in 9router; the id arriving here is
// already alias-free — accept both spellings).
var aliasRe = regexp.MustCompile(`^(?i)oc/`)

// maxBodyBytes bounds inbound request parsing (8 MB — upstream bodies are
// conversations; the JS router reads the raw stream unbounded).
const maxBodyBytes = 8 << 20

// corsHeaders are attached to every response (utils/error.js + sseConstants).
func corsHeaders(h http.Header, sse bool) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-opencode-client, x-opencode-session, x-opencode-request, x-opencode-project")
	if sse {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
	}
}

// writeError emits the OpenAI-compatible error envelope. The message is used
// verbatim (utils/error.js errorResponse) — only the upstream-error call site
// in relay() adds the formatProviderError `[status]: ` prefix, exactly once.
func writeError(w http.ResponseWriter, status int, message string) {
	body := upstream.BuildErrorBody(status, message)
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	corsHeaders(w.Header(), false)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// HandleOptions answers CORS preflights.
func (s *Server) HandleOptions(w http.ResponseWriter, _ *http.Request) {
	corsHeaders(w.Header(), false)
	w.WriteHeader(http.StatusNoContent)
}

// HandleChatCompletions is POST /v1/chat/completions.
func (s *Server) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.relay(w, r, relay.FormatChat)
}

// HandleResponses is POST /v1/responses.
func (s *Server) HandleResponses(w http.ResponseWriter, r *http.Request) {
	s.relay(w, r, relay.FormatResponses)
}

// relay is the shared chatCore pipeline. sourceFormat comes from the endpoint
// (translator/formats.js detectFormatByEndpoint: /v1/responses is always
// responses, /v1/chat/completions is openai — even with an input[] body).
func (s *Server) relay(w http.ResponseWriter, r *http.Request, sourceFormat relay.Format) {
	if s.Cfg.APIKey != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.Cfg.APIKey {
			writeError(w, http.StatusUnauthorized, "Invalid API key provided")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	// Drain gate: a shutting-down server rejects NEW requests with 503 before
	// reading the body (in-flight requests/streams finish under the shutdown
	// grace; nothing upstream is dialed after this point).
	if s.Draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(raw) > maxBodyBytes {
		writeError(w, http.StatusBadRequest, "Bad request — unreadable body")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "Bad request — body must be a JSON object")
		return
	}

	requested := jsonx.AsStr(body["model"])

	// [1m] context-marker strip (src/sse/handlers/chat.js:51-55 +
	// open-sse/utils/modelMarkers.js): Claude Code marks a 1M-context request
	// as `<model>[1m]`; the marker matches no model id so it must be stripped
	// before alias strip / resolution. The check runs on the TRIMMED model and
	// only a match rewrites it (chat.js:55 `if (contextMarker)`), so a
	// marker-less model keeps its original untrimmed spelling.
	if stripped, found := stripModelContextMarker(requested); found {
		requested = stripped
		body["model"] = stripped
	}

	if requested == "" {
		writeError(w, http.StatusBadRequest, "Model not found — body.model is required")
		return
	}

	// Test-connection probe (src/sse/handlers/chat.js:91-97): header presence
	// with any value answers a fixed synthetic completion with no upstream
	// call. Shared-handler scope — see isTestConnectionRequest.
	if isTestConnectionRequest(r) {
		createTestConnectionResponse(w, requested)
		return
	}

	// Bypass short-circuit (src/sse/handlers/chat.js:99-103 +
	// open-sse/utils/bypassHandler.js): claude-cli warmup / title / skip
	// requests are answered synthetically before alias strip, format
	// detection or the thinking snapshot. The model argument is the
	// marker-stripped requested model (chat.js passes modelStr).
	if s.handleBypassRequest(w, body, requested, r.Header.Get("User-Agent"), sourceFormat) {
		return
	}
	model := aliasRe.ReplaceAllString(requested, "")
	upstreamModel := model
	cleanModel := cloak.BaseModelID(upstreamModel)

	targetFormat := relay.FormatChat
	if cloak.IsResponsesModel(upstreamModel) {
		targetFormat = relay.FormatResponses
	}

	// Snapshot the client's thinking intent BEFORE any mutation — translation
	// and ApplyThinking strip thinking fields in place, and the usage seam's
	// synthesis gate needs the original signal (chatCore.js).
	intent := cloak.ExtractThinking(body)

	// ---- unsupported-modality strip (chatCore.js:169-180 +
	// translator/concerns/modality.js): drop media the model cannot read,
	// BEFORE translateRequest/prenorms, on the source-format body. JS gates it
	// on !passthrough, but this proxy's single provider (opencode) is never a
	// native passthrough pair for any client (clientDetector.js NATIVE_PAIRS).
	// Capabilities resolve against the suffix-free model id — chatCore.js:171
	// passes modelInfo.model — and the PROVIDER_CAPABILITIES step is skipped
	// (caps.Resolve documents why).
	translate.StripUnsupportedModalities(body, sourceFormat == relay.FormatResponses, caps.Resolve(cleanModel))

	clientRequestedStreaming := body["stream"] == true

	// ---- translateRequest prenorms (run for every request, even same-format).
	translate.NormalizeThinkingConfig(body)
	translate.EnsureToolCallIDs(body)
	translate.FixMissingToolResponses(body)

	// ---- format translation (source → openai → target pivot, net effect).
	var customToolNames map[string]bool
	if sourceFormat != targetFormat {
		if sourceFormat == relay.FormatChat {
			translated := translate.ChatRequestToResponsesRequest(upstreamModel, body)
			if translated == nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("Failed to translate request for %s → %s", sourceFormat, targetFormat))
				return
			}
			body = translated
		} else {
			body = translate.ResponsesToChatRequest(body)
			if body == nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("Failed to translate request for %s → %s", sourceFormat, targetFormat))
				return
			}
		}
	}
	// Consumed regardless of direction (chatCore.js:210-211): only the
	// responses→chat translator sets the marker, but it must never leak
	// upstream and feeds the response-side custom_tool_call conversion.
	if names := jsonx.AsArr(body["_customToolNames"]); names != nil {
		customToolNames = make(map[string]bool, len(names))
		for _, n := range names {
			customToolNames[jsonx.AsStr(n)] = true
		}
	}
	delete(body, "_customToolNames")

	// ---- thinking normalization to the upstream-native format.
	cloak.ApplyThinking(body, upstreamModel, intent)
	if targetFormat == relay.FormatChat {
		translate.FilterToOpenAIFormat(body)
	}
	body["model"] = cleanModel

	// ---- claude-client tool dedupe (chatCore.js:216-223 +
	// utils/toolDeduper.js): JS dedupes translatedBody.tools after translation
	// and before the executor runs; here that is before PrepareRequest injects
	// the fingerprint quartet, so only client-declared tools are stripped.
	if isClaudeClientTool(r.Header) {
		if tools, ok := body["tools"].([]any); ok {
			if deduped, stripped := dedupeTools(tools); len(stripped) > 0 {
				body["tools"] = deduped
			}
		}
	}

	// ---- routing: match the CURRENT snapshot, filter to the eligible head
	// set and let the scheduler order it. Health config is applied per
	// request so a reload of threshold/cooldown takes effect immediately;
	// routing (start selection), fallback (attempt loop) and health
	// (temporary eligibility) stay three separate decisions.
	rt := s.runtime()
	if s.Health != nil {
		s.Health.Configure(
			rt.Health.Enabled == nil || *rt.Health.Enabled,
			rt.Health.FailureThreshold,
			time.Duration(rt.Health.Cooldown),
		)
	}
	profile := routing.Profile{
		Model:     cleanModel,
		Streaming: clientRequestedStreaming,
		BodyBytes: int64(len(raw)),
		Endpoint:  string(sourceFormat),
	}
	route, ok := rt.MatchRoute(profile.Streaming, profile.BodyBytes, profile.Model)
	if !ok {
		writeError(w, http.StatusBadRequest, "No route matched this request")
		return
	}
	heads := s.routeHeads(rt, route, profile)
	if len(heads) == 0 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("No eligible egress for route %q", route.ID))
		return
	}
	plan := s.Scheduler.Plan(rt, route, heads)

	// ---- executor boundary (session resolve + transform + headers).
	downstream := captureDownstream(r)
	session := upstream.PrepareRequest(upstreamModel, body, downstream)
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to serialize request")
		return
	}

	url := upstream.BuildURL(s.Cfg.UpstreamBase, upstreamModel)
	reqCtx, cancelUpstream := context.WithCancel(r.Context())
	// base.js re-invokes transformRequest+buildHeaders inside the retry loop, so
	// a forged x-opencode-request id is fresh on every attempt; the closure is
	// what the client calls once per attempt.
	buildHeaders := func() map[string]string {
		return upstream.BuildHeaders(downstream, session, s.UA.Get())
	}
	policy := upstream.AttemptPolicy{
		FallbackEnabled: rt.Fallback.Enabled == nil || *rt.Fallback.Enabled,
		MaxAttempts:     rt.Fallback.MaxAttempts,
		MaxConcurrency:  make(map[string]int, len(heads)),
	}
	for _, id := range heads {
		if e, ok := rt.Egress(id); ok {
			policy.MaxConcurrency[id] = e.MaxConcurrency
		}
	}

	reqID := newRequestID()
	start := time.Now()
	resp, egID, attempts, class, uerr := s.Exec.Execute(reqCtx, url, buildHeaders, bodyJSON, plan, policy)
	latency := time.Since(start)
	if uerr != nil {
		cancelUpstream()
		s.logf("%s route=%s egress=%s attempts=%d class=%s status=%d latency_ms=%d model=%s endpoint=%s fallback=%t",
			reqID, plan.RouteID, egID, attempts, class, uerr.Status, latency.Milliseconds(), cleanModel, profile.Endpoint, attempts > 1)
		writeError(w, uerr.Status, fmt.Sprintf("[%d]: %s", uerr.Status, uerr.Message))
		return
	}
	w.Header().Set("X-OFP-Egress", egID)
	s.logf("%s route=%s egress=%s attempts=%d class=%s status=%d latency_ms=%d model=%s endpoint=%s fallback=%t",
		reqID, plan.RouteID, egID, attempts, class, resp.StatusCode, latency.Milliseconds(), cleanModel, profile.Endpoint, attempts > 1)

	// Forced SSE→JSON needs the upstream reply to actually be SSE
	// (sseToJsonHandler.js:185-188): when it is not, chatCore falls through to
	// the streaming path — a non-streaming client behind a non-SSE upstream
	// gets the stream handler (HTML guard → error; other bodies piped raw).
	if !clientRequestedStreaming && forcedUpstreamIsSSE(resp) {
		// The response body still streams under reqCtx — canceling before the
		// ReadAll would abort it. forcedSSEToJson closes the body; the defer
		// releases the context after the read completes.
		defer cancelUpstream()
		s.forcedSSEToJson(w, r, resp, sourceFormat, targetFormat, cleanModel, customToolNames, body, upstreamModel, intent)
		return
	}
	s.stream(w, r, resp, cancelUpstream, sourceFormat, targetFormat, body, upstreamModel, customToolNames, intent)
}

// captureDownstream collects the client headers the executor forwards.
func captureDownstream(r *http.Request) upstream.Downstream {
	h := map[string]string{}
	for _, name := range []string{
		"User-Agent", "X-Opencode-Client", "X-Opencode-Session",
		"X-Opencode-Request", "X-Opencode-Project",
		"X-Session-Id", "Session-Id", "Session_id", "X-Amp-Thread-Id",
		"X-Claude-Code-Session-Id", "X-Client-Request-Id",
		"X-App", // clientDetector.js x-app — claude-client detection input
	} {
		if v := r.Header.Get(name); v != "" {
			h[strings.ToLower(name)] = v
		}
	}
	return upstream.Downstream{Headers: h}
}

// nonstreamSynthesize builds the hidden-thinking seam closure for the
// SSE→JSON paths (sseToJsonHandler.js synthesize).
func nonstreamSynthesize(sourceFormat relay.Format, body map[string]any, model string, intent *cloak.ThinkingCfg) func(map[string]any) map[string]any {
	synthesis := relay.ResolveSynthesis(body, model, intent)
	return func(u map[string]any) map[string]any {
		if !synthesis.Enabled || u == nil {
			return u
		}
		return usage.SynthesizeThinking(u, string(sourceFormat), synthesis.Ratio)
	}
}
