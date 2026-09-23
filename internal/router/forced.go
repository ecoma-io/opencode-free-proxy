package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/relay"
	"opencode-free-proxy/internal/upstream"
)

// forcedUpstreamIsSSE ports the handleForcedSSEToJson gate
// (open-sse/handlers/chatCore/sseToJsonHandler.js:185-188):
//
//	isSSE = contentType.includes("text/event-stream")
//	        || (contentType === "" && isResponsesProvider(provider))
//
// isResponsesProvider(p) is `PROVIDERS[p]?.format === FORMATS.OPENAI_RESPONSES`
// (sseToJsonHandler.js:12). For this proxy's single provider the transport
// declares no format, so PROVIDERS["opencode"].format is the schema default
// "openai" (providers/index.js:14 + providers/schema.js:44-46;
// providers/registry/opencode.js has no transport.format — its muse models
// carry a per-model targetFormat instead, which is NOT what
// isResponsesProvider reads). The empty-content-type disjunct is therefore
// dead here, and a non-streaming client whose upstream reply lacks both an
// SSE content type and a parseable stream falls through to s.stream(...) —
// exactly the JS flow when the gate returns null (chatCore.js:456-470 falls
// through to handleStreamingResponse).
func forcedUpstreamIsSSE(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

// forcedSSEToJson ports handleForcedSSEToJson: the upstream always streams
// (forceStream quirk) but the client asked for JSON. Branching follows the
// UPSTREAM format — a Responses client behind a chat-native upstream still
// gets the standard path. The SSE content-type gate lives at the relay() call
// site (forcedUpstreamIsSSE above); reaching this function with a non-SSE
// body is the JS parse-failure case, which answers 502 below
// (sseToJsonHandler.js "Failed to convert streaming response to JSON").
//
// delivered is the authorship of the response being read, as the executor
// proved it (OriginUpstream, or OriginAmbiguous on a hop an HTTP intermediary
// carried — issue #63): every abort row below describes what became of a
// response this call already received, so a constant would blame the provider
// for a proxy's death.
func (s *Server) forcedSSEToJson(w http.ResponseWriter, r *http.Request, resp *http.Response, ev *evidenceLog, egID string, delivered upstream.Origin, sourceFormat, targetFormat relay.Format, model string, customToolNames map[string]bool, reqBody map[string]any, upstreamModel string, intent *cloak.ThinkingCfg) {
	defer func() { _ = resp.Body.Close() }()
	started := time.Now()

	// The read is bounded BOTH ways (hardening with no JS counterpart —
	// sseToJsonHandler.js:306 `await providerResponse.text()` leans on
	// undici's cancellable streams): a progress-stall deadline (the same
	// config.StreamStall watchdog the streaming ScanLines path enforces —
	// ResponseHeaderTimeout is satisfied once headers arrive, so without this
	// a proxy that trickles forever would hang the goroutine + connection
	// indefinitely) and a total byte cap (config.MaxForcedSSEBytes). Either
	// bound answers the same 502 envelope as any other forced-conversion
	// failure (sseToJsonHandler.js:308-313, 373-376); the specific reason goes
	// to the evidence stream only.
	raw, err := readBoundedSSE(r.Context(), resp.Body, config.MaxForcedSSEBytes, config.StreamStall)
	if err != nil {
		origin, reason := forcedAbortReason(r, err, delivered)
		ev.ForcedAbort(egID, resp.StatusCode, origin, reason, time.Since(started).Milliseconds())
		// The 502 is THIS process's, synthesized over a live 2xx stream — the
		// record relabel must say so, or the success-path origin (upstream or
		// ambiguous, handler.go:395) would assert the provider authored a status
		// it never sent (issue #72). response_started forbids a replay of the
		// already-answered call.
		setGatewayResponseStarted(w.Header())
		writeError(w, http.StatusBadGateway, "Failed to convert streaming response to JSON")
		return
	}
	synthesize := nonstreamSynthesize(sourceFormat, reqBody, upstreamModel, intent)

	if targetFormat == relay.FormatResponses {
		// Muse Spark upstream spoke Responses SSE.
		jsonResponse := relay.ConvertResponsesStreamToJson(string(raw))
		if sourceFormat == relay.FormatResponses {
			// Responses client: return the aggregated object as-is (usage
			// synthesized on the client-facing copy only).
			if usage, has := jsonResponse["usage"].(map[string]any); has {
				jsonResponse["usage"] = synthesize(usage)
			}
			writeJSON(w, http.StatusOK, jsonResponse)
			return
		}
		writeJSON(w, http.StatusOK, relay.BuildChatFromResponses(jsonResponse, model, synthesize))
		return
	}

	// Standard Chat Completions SSE path.
	parsed, errBody, ok := relay.ParseSSEToOpenAIResponse(string(raw), model)
	if !ok {
		ev.ForcedAbort(egID, resp.StatusCode, delivered, "convert", time.Since(started).Milliseconds())
		setGatewayResponseStarted(w.Header())
		writeError(w, http.StatusBadGateway, "Invalid SSE response for non-streaming request")
		return
	}
	if errBody != nil {
		// The stream itself carried an error frame — an upstream failure that
		// began mid-stream (after the 200 headers), recorded as such.
		ev.ForcedAbort(egID, resp.StatusCode, delivered, "sse_error_frame", time.Since(started).Milliseconds())
		// This 502 reports an error the STREAM carried — a provider-authored
		// status would be a fabrication here, so the 502 is this process's,
		// labelled gateway/response_started over the live 2xx stream. The
		// evidence row keeps the delivered origin for the record; the wire
		// label must not let a caller replay a call that was already answered.
		setGatewayResponseStarted(w.Header())
		msg, _ := errBody["message"].(string)
		if msg == "" {
			msg = "Upstream SSE stream failed"
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}
	if usage, has := parsed["usage"].(map[string]any); has && len(usage) > 0 {
		parsed["usage"] = synthesize(usage)
	}
	relay.StripRedundantReasoning(parsed)
	if sourceFormat == relay.FormatResponses {
		writeJSON(w, http.StatusOK, synthesizeFinal(relay.ChatCompletionToResponses(parsed, customToolNames), synthesize))
		return
	}
	writeJSON(w, http.StatusOK, parsed)
}

// synthesizeFinal re-applies the seam on the converted shape —
// chatCompletionToResponses rebuilds usage from scratch and drops the details
// objects (sseToJsonHandler.js final synthesize).
func synthesizeFinal(body map[string]any, synthesize func(map[string]any) map[string]any) map[string]any {
	if usage, has := body["usage"].(map[string]any); has {
		body["usage"] = synthesize(usage)
	}
	return body
}

// writeJSON emits a JSON body with CORS headers.
func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	corsHeaders(w.Header(), false)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// errForcedSSEStall / errForcedSSETooLarge are the two bounded-read
// violations. Both surface to the client as the generic forced-conversion 502;
// they are distinguished only in the evidence stream (the client never learns
// internals).
var (
	errForcedSSEStall    = errors.New("upstream SSE stalled: no bytes within the stall deadline")
	errForcedSSETooLarge = errors.New("upstream SSE response exceeded the forced-conversion size cap")
)

// forcedAbortReason reduces a bounded-read failure to the side it belongs to
// and its phase label: stall, size cap, client cancel (the request context
// died), or a read error from the body itself. Every one of these happens
// after the upstream's headers were already delivered, so the response is the
// DELIVERING path's (issue #63: normally the provider's, but a hop an HTTP
// intermediary carried may be the proxy's) — except a cancel, which is the
// caller's and must not be recorded against the egress.
func forcedAbortReason(r *http.Request, err error, delivered upstream.Origin) (upstream.Origin, string) {
	switch {
	case errors.Is(err, errForcedSSEStall):
		return delivered, "stall"
	case errors.Is(err, errForcedSSETooLarge):
		return delivered, "too_large"
	case r.Context().Err() != nil || errors.Is(err, context.Canceled):
		return upstream.OriginClient, "cancel"
	default:
		return delivered, "read"
	}
}

// readBoundedSSE drains an upstream SSE body under three abort conditions:
//
//   - stall: no bytes arrived for the given deadline (progress-reset, the
//     same STREAM_STALL semantics as upstream.ScanLines on the streaming
//     path — every chunk that arrives buys a fresh window);
//   - size: more than maxBytes in total would be buffered;
//   - ctx: the request context died (client disconnect).
//
// The chunked design keeps memory bounded even against a newline-free flood:
// each Read is delivered through a fresh slice and copied into the result
// before the next read is consumed, so nothing — not the accumulator, not a
// line buffer — can grow past one chunk beyond the cap. A stalled read leaves
// its reader goroutine parked inside body.Read; the caller's body Close
// (and the request-context cancellation wired to the same request) unblocks
// it, so nothing leaks past the request's lifetime.
func readBoundedSSE(ctx context.Context, body io.Reader, maxBytes int64, stall time.Duration) ([]byte, error) {
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			buf := make([]byte, 32*1024)
			n, err := body.Read(buf)
			select {
			case chunks <- chunk{data: buf[:n], err: err}:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var out []byte
	timer := time.NewTimer(stall)
	defer timer.Stop()
	for {
		select {
		case ch := <-chunks:
			if len(ch.data) > 0 {
				if int64(len(out))+int64(len(ch.data)) > maxBytes {
					return nil, errForcedSSETooLarge
				}
				out = append(out, ch.data...)
				// Progress: buy a fresh stall window.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(stall)
			}
			if ch.err != nil {
				if errors.Is(ch.err, io.EOF) {
					return out, nil
				}
				return nil, ch.err
			}
		case <-timer.C:
			return nil, errForcedSSEStall
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
