package relay

import (
	"fmt"
	"io"
	"strings"
	"time"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/translate"
	"opencode-free-proxy/internal/usage"
)

// TranslateRelay is stream.js STREAM_MODE.TRANSLATE: an upstream speaking one
// format translated into the client's format.
//   - source=chat, upstream=responses → chat client + muse-spark (case 2)
//   - source=responses, upstream=chat → responses client + chat model (case 3b)
type TranslateRelay struct {
	out           io.Writer
	body          map[string]any
	model         string
	source        Format
	synthesis     Synthesis
	now           func() time.Time
	respToChat    *translate.RespToChatState
	chatToResp    *translate.ChatToRespState
	totalLen      int
	content       strings.Builder
	thinking      strings.Builder
	trackedUsage  map[string]any // state.usage (stats side)
	FinalUsage    map[string]any
	FinalContent  string
	FinalThinking string
	finalized     bool
}

// NewTranslateRelay picks the state machine from the source format. responseID
// seeds the chat→responses machine's resp_ id (initState: `resp_${Date.now()}`).
// intent is the pre-translation thinking snapshot (nil when absent).
func NewTranslateRelay(out io.Writer, body map[string]any, model string, source Format, responseID string, intent *cloak.ThinkingCfg) *TranslateRelay {
	r := &TranslateRelay{
		out:       out,
		body:      body,
		model:     model,
		source:    source,
		synthesis: ResolveSynthesis(body, model, intent),
		now:       time.Now,
	}
	if source == FormatChat {
		r.respToChat = translate.NewRespToChatState(model)
	} else {
		r.chatToResp = translate.NewChatToRespState(model, r.now().UnixMilli()/1000, responseID)
	}
	return r
}

// SetCustomToolNames feeds the request translator's marker into the state
// machines (stream.js:132 — customToolNames: new Set(customToolNames || []));
// the chat→responses state uses it to announce custom_tool_call items.
func (r *TranslateRelay) SetCustomToolNames(names map[string]bool) {
	if r.chatToResp != nil {
		for n := range names {
			r.chatToResp.CustomToolNames[n] = true
		}
	}
}

// firstChoice returns choices[0] of a chat-shaped item.
func firstChoice(item map[string]any) map[string]any {
	choices := jsonx.AsArr(item["choices"])
	if len(choices) == 0 {
		return nil
	}
	return jsonx.AsObj(choices[0])
}

// applyUsageSeam ports the translate-mode usage seam onto a client-bound item.
func (r *TranslateRelay) applyUsageSeam(item map[string]any) map[string]any {
	// stream.js:185 — `item.type === "message_delta"` is a strict equality,
	// but `item.choices?.[0]?.finish_reason` is TRUTHINESS: a boolean/numeric
	// finish_reason marks the finish item too.
	isFinishChunk := jsonx.AsStr(item["type"]) == "message_delta" ||
		jsTruthy(jsonx.Get(firstChoice(item), "finish_reason"))
	u, hasU := item["usage"].(map[string]any)
	respUsage, hasRU := jsonx.Get(item["response"], "usage").(map[string]any)
	carriesUsage := (hasU && usage.HasValid(u)) || (hasRU && usage.HasValid(respUsage))

	switch {
	case isFinishChunk && !carriesUsage && !usage.HasValid(r.trackedUsage) && r.totalLen > 0:
		estimated := r.estimate()
		item["usage"] = r.buildClientUsage(estimated)
		r.trackedUsage = estimated
	case carriesUsage:
		if hasRU && !hasU {
			jsonx.Set(item, "response.usage", r.synthesizeClient(respUsage))
		} else if hasU {
			item["usage"] = r.synthesizeClient(u)
		}
	case isFinishChunk && r.trackedUsage != nil:
		item["usage"] = r.buildClientUsage(usage.AddBuffer(r.trackedUsage))
	}
	return item
}

// estimate mirrors estimateUsage: formatUsage only knows the OpenAI canonical
// shape (Claude is the other family upstream; we don't need it), so even a
// responses client's estimate starts chat-shaped and is renamed per item.
func (r *TranslateRelay) estimate() map[string]any {
	return usage.Estimate(r.body, r.totalLen)
}

func (r *TranslateRelay) synthesizeClient(u map[string]any) map[string]any {
	if !r.synthesis.Enabled {
		return u
	}
	return usage.SynthesizeThinking(u, string(r.source), r.synthesis.Ratio)
}

// buildClientUsage: rename → synthesize → filter (client-facing copies only).
func (r *TranslateRelay) buildClientUsage(u map[string]any) map[string]any {
	converted := convertUsageForFormat(u, r.source)
	if r.synthesis.Enabled {
		converted = usage.SynthesizeThinking(converted, string(r.source), r.synthesis.Ratio)
	}
	return filterUsageForFormat(converted, r.source)
}

// ProcessLine consumes one upstream SSE line (split on "\n", newline stripped).
func (r *TranslateRelay) ProcessLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	// Non-data lines (event:, comments) never parse — parseSSELine requires
	// the "d" prefix — and neither translate case re-frames upstream events.
	parsed, isDone := parseDataLine(line)
	if parsed == nil {
		_ = isDone // [DONE] sentinel: never translated, never forwarded —
		// translate mode sends no [DONE] to either client family.
		// A data line whose payload is valid JSON but not an object (42 / "x"
		// / [1,2] — parseSSELine's JSON.parse accepts them,
		// streamHelpers.js:28) produces no client output here either: every
		// accumulator/seam read is optional-chained and the translators emit
		// nothing for it, so dropping is wire-equivalent to JS
		// (stream.js:390-511).
		return nil
	}

	// Accumulate content/thinking from the raw upstream chunk (both shapes).
	if choices := jsonx.AsArr(parsed["choices"]); len(choices) > 0 {
		if choice := jsonx.AsObj(choices[0]); choice != nil {
			accumulate(&r.totalLen, &r.content, &r.thinking, jsonx.AsObj(choice["delta"]))
		}
	}

	if extracted := extractAnyUsage(parsed); extracted != nil {
		r.trackedUsage = r.mergeTracked(r.trackedUsage, extracted)
	}

	var items []map[string]any
	if r.source == FormatChat {
		if chunk := r.respToChat.Convert(parsed); chunk != nil {
			items = append(items, chunk)
		}
	} else {
		for _, ev := range r.chatToResp.Convert(parsed) {
			items = append(items, framedEvent(ev.Event, ev.Data))
		}
	}

	for _, item := range items {
		if !valuableForFormat(item, r.source) {
			continue
		}
		item = r.applyUsageSeam(item)
		out := FormatData(item)
		if _, err := io.WriteString(r.out, out); err != nil {
			return err
		}
	}
	return nil
}

// ProcessTail feeds the unterminated final segment through the same pipeline:
// the JS translate flush parses the residual buffer with the SAME parseSSELine
// as the transform loop and runs the full transform + usage seam over it
// (stream.js:549-586), so the tail is just one more line. An unparsable (or
// non-object) tail is dropped by ProcessLine, matching the JS
// `if (parsed && !isDoneSentinel)` guard at stream.js:558-559; the closing
// items and any terminal synthesis follow in Flush, exactly like
// stream.js:588-626.
func (r *TranslateRelay) ProcessTail(line string) error {
	return r.ProcessLine(line)
}

// Flush translates any leftovers, emits the state machines' closing items, and
// synthesizes response.failed + [DONE] for responses clients when the upstream
// never reached a terminal event. Chat clients get no [DONE] (stream.js
// translate-mode parity).
func (r *TranslateRelay) Flush() error {
	var items []map[string]any
	if r.source == FormatChat {
		if chunk := r.respToChat.Convert(nil); chunk != nil {
			items = append(items, chunk)
		}
	} else {
		for _, ev := range r.chatToResp.Convert(nil) {
			items = append(items, framedEvent(ev.Event, ev.Data))
		}
	}
	for _, item := range items {
		if !valuableForFormat(item, r.source) {
			continue
		}
		item = r.applyUsageSeam(item)
		if _, err := io.WriteString(r.out, FormatData(item)); err != nil {
			return err
		}
	}

	// No [DONE] for either translate case: keepsOpenAIResponsesFormat requires
	// target==responses too, and chat clients of stream.js translate mode never
	// receive the sentinel. The state machines' final items are the terminators.

	r.finalize()
	return nil
}

func (r *TranslateRelay) finalize() {
	if r.finalized {
		return
	}
	r.finalized = true
	final := r.trackedUsage
	if !usage.HasValid(final) && r.totalLen > 0 {
		final = r.estimate()
		r.trackedUsage = final
	}
	r.FinalUsage = final
	r.FinalContent = r.content.String()
	r.FinalThinking = r.thinking.String()
}

// mergeTracked: stream.js:168-169 `prev?.estimated ? next : mergeUsage(prev,
// next)` — the estimate marker check is JS TRUTHINESS, not a literal-true
// comparison.
func (r *TranslateRelay) mergeTracked(prev, next map[string]any) map[string]any {
	if next == nil {
		return prev
	}
	if prev != nil && jsTruthy(prev["estimated"]) {
		return next
	}
	return usage.Merge(prev, next)
}

// valuableForFormat ports hasValuableContent for the client-bound item — chat
// items are filtered, framed responses events all pass (JS format is OPENAI
// for chat items; responses events carry no choices so the filter yields true).
func valuableForFormat(item map[string]any, source Format) bool {
	if source == FormatResponses {
		return true
	}
	return hasValuableContent(item)
}

// framedEvent wraps a named event + data as a single serializable object for
// FormatData (which detects the event key and renders event framing).
func framedEvent(event string, data map[string]any) map[string]any {
	return jsonx.ObjOf("event", event, "data", data)
}

// FormatIncompleteResponsesFailure is the synthesized response.failed frame
// for streams that closed without a terminal event
// (responsesStreamHelpers.formatIncompleteOpenAIResponsesStreamFailure).
func FormatIncompleteResponsesFailure() string {
	return FormatEvent("response.failed", jsonx.ObjOf(
		"type", "response.failed",
		"response", jsonx.ObjOf(
			// JS: `resp_${Date.now()}` — decimal millis, not base36.
			"id", fmt.Sprintf("resp_%d", time.Now().UnixMilli()),
			"status", "failed",
			"error", jsonx.ObjOf(
				"type", "stream_error",
				"code", "stream_disconnected",
				"message", "stream closed before response.completed",
			),
		),
	))
}
