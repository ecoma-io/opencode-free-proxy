package relay

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/usage"
)

// PassthroughRelay is stream.js STREAM_MODE.PASSTHROUGH: same-format relay
// with per-chunk normalization, the usage seam, and the [DONE] guarantee.
// Serves case 1 (chat client + chat model) and case 3a (responses client +
// muse-spark — chat-shape normalizations no-op on Responses events and
// framing flows through verbatim).
//
// Same-format means sourceFormat == targetFormat, so format == FormatResponses
// IS the responses→responses route. JS never runs passthrough mode there (its
// buildTransformStream picks translate, which short-circuits untranslated for
// same-format pairs); the two terminal-state flags below port the translate
// mode's Responses bookkeeping that route relies on:
//   - terminalSeen: openAIResponsesTerminalSeen (stream.js:145, set at
//     400-402) — a terminal event was seen,
//   - doneSent: streamDoneSent/openAIResponsesDoneSent (stream.js:146-147,
//     set at 421-422) — the [DONE] sentinel was already emitted.
type PassthroughRelay struct {
	out           io.Writer
	body          map[string]any
	model         string
	format        Format
	synthesis     Synthesis
	now           func() time.Time
	terminalSeen  bool
	doneSent      bool
	finalized     bool
	totalLen      int
	content       strings.Builder
	thinking      strings.Builder
	trackedUsage  map[string]any
	FinalUsage    map[string]any
	FinalContent  string
	FinalThinking string
}

// NewPassthroughRelay wires a relay writing raw SSE bytes to out. intent is
// the pre-translation thinking snapshot (nil when the client asked for none).
func NewPassthroughRelay(out io.Writer, body map[string]any, model string, format Format, intent *cloak.ThinkingCfg) *PassthroughRelay {
	return &PassthroughRelay{
		out:       out,
		body:      body,
		model:     model,
		format:    format,
		synthesis: ResolveSynthesis(body, model, intent),
		now:       time.Now,
	}
}

// buildClientUsage renames to the wire format, synthesizes the reasoning
// field, and filters — client-facing copies only.
func (r *PassthroughRelay) buildClientUsage(u map[string]any) map[string]any {
	converted := convertUsageForFormat(u, r.format)
	if r.synthesis.Enabled {
		converted = usage.SynthesizeThinking(converted, string(r.format), r.synthesis.Ratio)
	}
	return filterUsageForFormat(converted, r.format)
}

// convertUsageForFormat renames canonical usage into the target family's
// field names (usageTracking.js convertUsageForFormat, 158-190 — the chat
// family is identity, only the Responses branch renames). Every read below is
// JS-faithful:
//   - the source fields go through NULLISH chains (`input_tokens ??
//     prompt_tokens`, 178-180): a PRESENT 0 beats a populated sibling, and
//     only then does `num()` coerce through Number — numeric strings ("7")
//     and booleans included;
//   - `num(v) ?? 0` (159): non-finite coercions land on 0;
//   - `if (cached)` / `if (reasoning)` (183-186) are TRUTHINESS gates — a
//     NEGATIVE count is kept, 0/NaN/undefined skip the details object;
//   - `if (usage.estimated) converted.estimated = true` (185/189): the marker
//     is gated on truthiness but always written as the literal true.
func convertUsageForFormat(u map[string]any, format Format) map[string]any {
	if u == nil {
		return u
	}
	if format == FormatResponses {
		converted := map[string]any{
			"input_tokens":  jsNumOr0(jsNullish(u["input_tokens"], u["prompt_tokens"])),
			"output_tokens": jsNumOr0(jsNullish(u["output_tokens"], u["completion_tokens"])),
		}
		if cached, ok := usage.NumOK(jsNullish(
			jsonx.Get(u["input_tokens_details"], "cached_tokens"),
			u["cached_tokens"],
			jsonx.Get(u["prompt_tokens_details"], "cached_tokens"),
		)); ok && cached != 0 {
			converted["input_tokens_details"] = jsonx.ObjOf("cached_tokens", cached)
		}
		if reasoning, ok := usage.NumOK(jsNullish(
			jsonx.Get(u["output_tokens_details"], "reasoning_tokens"),
			u["reasoning_tokens"],
			u["thoughtsTokenCount"],
		)); ok && reasoning != 0 {
			converted["output_tokens_details"] = jsonx.ObjOf("reasoning_tokens", reasoning)
		}
		if jsTruthy(u["estimated"]) {
			converted["estimated"] = true
		}
		return converted
	}
	// Chat family: identity (fields already carry OpenAI names).
	return u
}

// filterUsageForFormat whitelists fields per family (chat keeps OpenAI names).
func filterUsageForFormat(u map[string]any, format Format) map[string]any {
	if u == nil {
		return u
	}
	if format == FormatResponses {
		allowed := []string{"input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details", "estimated"}
		out := map[string]any{}
		for _, k := range allowed {
			if v, has := u[k]; has {
				out[k] = v
			}
		}
		return out
	}
	allowed := []string{"prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "reasoning_tokens", "prompt_tokens_details", "completion_tokens_details", "estimated"}
	out := map[string]any{}
	for _, k := range allowed {
		if v, has := u[k]; has {
			out[k] = v
		}
	}
	return out
}

// numOr mirrors JS `a || b || 0` over all-numeric chains — the first truthy
// (non-zero) float64 wins, else the literal 0 (sseToJsonHandler.js:103-107
// and the responses→chat usage folds, where every operand is a token count).
func numOr(values ...any) float64 {
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			if n != 0 {
				return n
			}
		}
	}
	return 0
}

// ProcessLine consumes one upstream SSE line (stream.js passthrough branch,
// 265-385). The line classes: a non-data line or the [DONE] sentinel forwards
// verbatim (stream.js:270 excludes the sentinel from the seam and the
// !injectedUsage tail at 372-378 re-emits it) — except on the
// responses→responses route, where the sentinel triggers the failed-stream
// synthesis (processDoneLine); a data line that does not parse as an OBJECT is
// either skipped silently (unparsable garbage or a JSON `null`, whose property
// read throws in JS — stream.js:364-369) or forwarded verbatim (any other
// non-object JSON — see below); and every parsable non-DONE chunk flows
// through the full normalization + usage seam, re-serialized only when the
// seam touched it.
func (r *PassthroughRelay) ProcessLine(line string) error {
	parsed, isData, isDone, nonObjectJSON := classifyDataLine(line)
	if !isData {
		return r.writeRaw(normalizeDataPrefix(line))
	}
	if isDone {
		if r.format == FormatResponses {
			return r.processDoneLine(line)
		}
		// Chat passthrough raw-forwards the sentinel WITHOUT setting
		// streamDoneSent (stream.js:372-378 — the !injectedUsage tail), so
		// Flush emits the canonical one again: a faithful double [DONE]
		// (stream.js:539-543 only ever guards the flush emission).
		return r.writeRaw(normalizeDataPrefix(line))
	}
	if parsed == nil {
		if !nonObjectJSON {
			return nil // unparsable JSON (or `null`) data line — the JS try/catch skips it (stream.js:364-369)
		}
		// Valid JSON that is not an object — a number, string, array or bool:
		// JSON.parse accepts it (stream.js:272), every mutation guard then
		// skips (parsed.choices !== undefined / parsed?.choices are both falsy
		// and hasValuableContent falls through to true,
		// streamHelpers.js:66), so the line reaches the !injectedUsage tail
		// and is re-emitted verbatim (stream.js:372-378).
		return r.writeRaw(normalizeDataPrefix(line))
	}

	output := ""
	injected := false

	// stream.js:288-309 iterate parsed.choices with for..of, reading a property
	// of EVERY element (`choice.content_filter_results`,
	// `choice.delta?.tool_calls`): a truthy choices that is neither an array
	// nor a string ({} / 5 / true) throws TypeError on the iteration itself,
	// and a NULL ELEMENT throws on the property read — the catch at
	// stream.js:364-369 silently drops the WHOLE chunk either way. Both throws
	// happen BEFORE the terminal-event probe at stream.js:332, so neither may
	// set terminalSeen (a hostile `choices:[null]` terminal-looking chunk must
	// not suppress the failed-stream synthesis). Strings ARE iterable and
	// per-character iteration mutates nothing; scalars box harmlessly
	// (property reads on primitives yield undefined) — both stay in play. A
	// falsy choices (null/0/""/false) never reaches the loops at all
	// (stream.js:288 `parsed?.choices`).
	if choicesValue, has := parsed["choices"]; has && jsTruthy(choicesValue) {
		switch cv := choicesValue.(type) {
		case []any:
			for _, el := range cv {
				if el == nil {
					return nil // stream.js:288-294 — property read on null throws
				}
			}
		case string:
			// iterable; the loops below iterate per character and mutate nothing
		default:
			return nil // non-iterable — for..of throws (stream.js:289/303)
		}
	}

	// stream.js:332: passthrough never captures an `event:` line
	// (currentOpenAIResponsesEvent stays null), so terminality comes from the
	// chunk.type fallback in getOpenAIResponsesEventName. The same signal feeds
	// openAIResponsesTerminalSeen (stream.js:400-402 — in JS evaluate order the
	// flag is set BEFORE any filtering), which gates the failed-stream
	// synthesis at [DONE]/flush time.
	responsesTerminal := TerminalResponsesEvent("", parsed)
	if responsesTerminal {
		r.terminalSeen = true
	}

	idFixed := fixInvalidID(parsed)

	fieldsInjected := false
	// stream.js:278 gates on PRESENCE (parsed.choices !== undefined): a
	// `"choices": null` chunk still enters. The two field checks are JS FALSY
	// checks (stream.js:279-280), so `"object": null / "" / 0 / false` and
	// `"created": 0 / null / ""` are all rewritten.
	if _, hasChoices := parsed["choices"]; hasChoices {
		if !jsTruthy(parsed["object"]) {
			parsed["object"] = "chat.completion.chunk"
			fieldsInjected = true
		}
		if !jsTruthy(parsed["created"]) {
			parsed["created"] = r.now().UnixMilli() / 1000
			fieldsInjected = true
		}
	}

	// Azure filter-result fields.
	if _, has := parsed["prompt_filter_results"]; has {
		delete(parsed, "prompt_filter_results")
		fieldsInjected = true
	}
	// Azure content_filter_results strip (stream.js:288-294).
	for _, choice := range jsonx.AsArr(parsed["choices"]) {
		c := jsonx.AsObj(choice)
		if c == nil {
			continue
		}
		if _, has := c["content_filter_results"]; has {
			delete(c, "content_filter_results")
			fieldsInjected = true
		}
	}

	// Empty tool_calls arrays break AI SDK reasoning tracking — strip them.
	for _, choice := range jsonx.AsArr(parsed["choices"]) {
		c := jsonx.AsObj(choice)
		delta := jsonx.AsObj(jsonx.Get(c, "delta"))
		if delta == nil {
			continue
		}
		if tc, is := delta["tool_calls"].([]any); is && len(tc) == 0 {
			delete(delta, "tool_calls")
			fieldsInjected = true
		}
	}

	if !hasValuableContent(parsed) {
		return nil
	}

	accumulate(&r.totalLen, &r.content, &r.thinking, firstDelta(parsed))

	if extracted := extractAnyUsage(parsed); extracted != nil {
		r.trackedUsage = r.mergeTracked(r.trackedUsage, extracted)
	}

	choices := jsonx.AsArr(parsed["choices"])
	var choice0 map[string]any
	if len(choices) > 0 {
		choice0, _ = choices[0].(map[string]any)
	}
	// stream.js:334 `parsed.choices?.[0]?.finish_reason` — TRUTHINESS, not a
	// string check: `finish_reason: true / 1 / []` marks the finish chunk too.
	isFinishChunk := jsTruthy(jsonx.Get(choice0, "finish_reason"))
	u, hasUsageObj := parsed["usage"].(map[string]any)
	carriesUsage := hasUsageObj && usage.HasValid(u)

	switch {
	case isFinishChunk && !carriesUsage && !usage.HasValid(r.trackedUsage):
		// stream.js:341 has no accumulated-content gate: a tool-call-only
		// stream never adds to totalContentLength (only content and
		// reasoning_content do) yet still gets the estimate.
		estimated := usage.Estimate(r.body, r.totalLen)
		parsed["usage"] = r.buildClientUsage(estimated)
		output = formatDataLine(parsed)
		r.trackedUsage = estimated
		injected = true
	case carriesUsage:
		if r.synthesis.Enabled {
			parsed["usage"] = usage.SynthesizeThinking(u, string(r.format), r.synthesis.Ratio)
		}
		output = formatDataLine(parsed)
		injected = true
	case isFinishChunk && r.trackedUsage != nil:
		buffered := usage.AddBuffer(r.trackedUsage)
		parsed["usage"] = r.buildClientUsage(buffered)
		output = formatDataLine(parsed)
		injected = true
	case idFixed || fieldsInjected:
		output = formatDataLine(parsed)
		injected = true
	}

	if !injected {
		output = normalizeDataPrefix(line)
	}
	if err := r.writeRaw(output); err != nil {
		return err
	}
	if responsesTerminal {
		r.finalize()
	}
	return nil
}

// ProcessTail consumes the unterminated final segment — upstream closed
// mid-line, no trailing newline (stream.js passthrough flush, 523-531): the
// residual buffer is forwarded RAW with only the "data:"-prefix fix. No
// newline is added, no parse, no seam, no [DONE] handling.
//
// Responses→responses exception: JS never runs passthrough there (translate
// mode instead), and the translate flush parses the residual buffer with the
// same parseSSELine as the loop (stream.js:553) — a tail that parses as the
// [DONE] sentinel therefore goes through the failed-stream synthesis of
// processDoneLine (stream.js:610-624 emits the failed frame + sentinel from
// flush). Any other responses→responses tail forwards raw: a documented
// adaptation boundary (JS would re-frame it through the translator).
func (r *PassthroughRelay) ProcessTail(line string) error {
	if line == "" { // stream.js:524 `if (buffer)`
		return nil
	}
	if r.format == FormatResponses {
		if _, isData, isDone, _ := classifyDataLine(line); isData && isDone {
			// The translate flush does NOT raw-forward a sentinel tail: it
			// parses the buffer and, being the done-sentinel, emits the
			// canonical `data: [DONE]\n\n` from its tail end (stream.js:553,
			// 558 + 617-619).
			return r.emitResponsesDone("data: [DONE]\n\n")
		}
	}
	if strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "data: ") {
		// stream.js:526-527 — prefix fix only, NO trailing newline.
		return r.writeRaw("data: " + line[len("data:"):])
	}
	return r.writeRaw(line)
}

// processDoneLine handles one `data: [DONE]` line on the responses→responses
// route. JS runs translate mode there, and its done-branch
// (stream.js:406-424) synthesizes response.failed immediately BEFORE the
// sentinel whenever the upstream ended without a terminal event, then marks
// the stream done so flush emits no second sentinel (streamDoneSent /
// openAIResponsesDoneSent, stream.js:421-422 → 618-624).
func (r *PassthroughRelay) processDoneLine(line string) error {
	// The transform loop forwards the sentinel line RAW (one trailing newline,
	// stream.js:372-378 — passthrough has no done-branch to intercept it).
	return r.emitResponsesDone(normalizeDataPrefix(line))
}

// emitResponsesDone writes the failed-stream synthesis (when no terminal was
// seen) followed by the given sentinel frame — the shared body of the
// transform-loop done-branch (stream.js:406-424) and of the responses→responses
// flush (stream.js:609-624).
func (r *PassthroughRelay) emitResponsesDone(sentinel string) error {
	if !r.terminalSeen {
		if err := r.writeRaw(FormatIncompleteResponsesFailure()); err != nil {
			return err
		}
		r.terminalSeen = true // stream.js:412/615 — the synthesis marks the stream terminal
	}
	if r.doneSent {
		// stream.js:416/618 `!streamDoneSent`: the sentinel is emitted exactly
		// once — a duplicate is swallowed, never raw-forwarded.
		return nil
	}
	if err := r.writeRaw(sentinel); err != nil {
		return err
	}
	r.doneSent = true
	return nil
}

// Flush terminates the stream: the guaranteed data: [DONE] sentinel, then
// usage finalization. On the responses→responses route JS runs translate
// mode, whose flush synthesizes the failed frame before the sentinel when no
// terminal event was seen (stream.js:609-616) and suppresses the sentinel
// once already sent (stream.js:618-624, the doneSent interception).
// Chat passthrough has neither: stream.js passthrough never sets
// streamDoneSent from a forwarded line, so a raw-forwarded [DONE] plus the
// flush sentinel is a faithful double [DONE].
func (r *PassthroughRelay) Flush() error {
	if r.format == FormatResponses {
		if !r.terminalSeen {
			if err := r.writeRaw(FormatIncompleteResponsesFailure()); err != nil {
				return err
			}
			r.terminalSeen = true
		}
		if !r.doneSent {
			if err := r.writeRaw("data: [DONE]\n\n"); err != nil {
				return err
			}
			r.doneSent = true
		}
		r.finalize()
		return nil
	}
	if err := r.writeRaw("data: [DONE]\n\n"); err != nil {
		return err
	}
	r.finalize()
	return nil
}

func (r *PassthroughRelay) finalize() {
	if r.finalized {
		return
	}
	r.finalized = true
	final := r.trackedUsage
	if !usage.HasValid(final) && r.totalLen > 0 {
		final = usage.Estimate(r.body, r.totalLen)
		r.trackedUsage = final
	}
	r.FinalUsage = final
	r.FinalContent = r.content.String()
	r.FinalThinking = r.thinking.String()
}

// mergeTracked: real usage replaces an estimate instead of max-merging into it
// (stream.js:168-169 `prev?.estimated ? next : mergeUsage(prev, next)` — the
// marker check is JS TRUTHINESS, so any truthy estimated value marks the
// estimate, not just the literal true).
func (r *PassthroughRelay) mergeTracked(prev, next map[string]any) map[string]any {
	if next == nil {
		return prev
	}
	if prev != nil && jsTruthy(prev["estimated"]) {
		return next
	}
	return usage.Merge(prev, next)
}

func (r *PassthroughRelay) writeRaw(s string) error {
	_, err := io.WriteString(r.out, s)
	return err
}

// normalizeDataPrefix re-adds the canonical "data: " prefix when upstream sent
// "data:{...}" without the space.
func normalizeDataPrefix(line string) string {
	if strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "data: ") {
		return "data: " + line[len("data:"):] + "\n"
	}
	if !strings.HasSuffix(line, "\n") {
		return line + "\n"
	}
	return line
}

// formatDataLine serializes a mutated chunk as `data: ${JSON.stringify(parsed)}\n`
// — BARE JSON (stream.js:346/351/358/361, single trailing newline).
// cleanUsagePayload is applied only by formatSSE (streamHelpers.js:119, the
// translate-mode formatter), so a passthrough re-emit keeps Azure-style
// `"usage": null` and `usage.perf_metrics: null` exactly as upstream sent
// them. Unmutated lines never reach here (verbatim tail, stream.js:372-378).
func formatDataLine(parsed map[string]any) string {
	b, _ := json.Marshal(parsed)
	return "data: " + string(b) + "\n"
}

// firstDelta extracts choices[0].delta for content accumulation.
func firstDelta(parsed map[string]any) map[string]any {
	choices := jsonx.AsArr(parsed["choices"])
	if len(choices) == 0 {
		return nil
	}
	choice := jsonx.AsObj(choices[0])
	if choice == nil {
		return nil
	}
	delta := jsonx.AsObj(choice["delta"])
	if delta == nil {
		return map[string]any{}
	}
	return delta
}

// extractAnyUsage reads usage from either family (extractUsage for the shapes
// this proxy's upstreams emit).
func extractAnyUsage(parsed map[string]any) map[string]any {
	if u := usage.ExtractFromResponses(parsed); u != nil {
		return u
	}
	return usage.ExtractFromChat(parsed)
}
