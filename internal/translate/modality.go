package translate

import (
	"strings"

	"opencode-free-proxy/internal/caps"
	"opencode-free-proxy/internal/jsonx"
)

// StripUnsupportedModalities ports open-sse/translator/concerns/modality.js
// stripUnsupportedModalities (:135-168): drop multimodal content blocks the
// target model cannot read, BEFORE translation, and replace them with a short
// text placeholder so messages never become empty. Runs on the SOURCE-format
// body at the chatCore.js:170-180 call site (before translateRequest).
//
// Only the two reachable source formats of this proxy are wired: the OpenAI
// chat shape (messages[].content[]) and the OpenAI Responses shape
// (input[].content[]). stripClaude (:85-93) and stripGeminiParts (:112-126)
// are unreachable — this proxy has no Claude or Gemini source format — and are
// not ported.
//
// Returns true when the model is strip-ELIGIBLE (some consumed modality is
// false — modality.js:133-138), mirroring the JS return the caller only logs.
func StripUnsupportedModalities(body map[string]any, sourceIsResponses bool, m caps.Modality) bool {
	if body == nil {
		return false // modality.js:136
	}
	// Fast exit: model supports everything we would strip (modality.js:138).
	// videoInput is resolved but never stripped — no block type maps to it.
	if m.Vision && m.AudioInput && m.PDF {
		return false
	}

	if sourceIsResponses {
		// FORMATS.OPENAI_RESPONSES branch (modality.js:151-154).
		stripResponsesInput(body, m)
	} else {
		// FORMATS.OPENAI branch + default (modality.js:141-146, 164-165).
		stripOpenAIMessages(body, m)
	}
	return true
}

// Placeholder text inserted where a media block was removed (modality.js:6-19).
// Current turn: explain the active model can't read what the user just sent;
// earlier turns: neutral (a combo may route to a different model each turn).
var placeholderCurrent = map[string]string{
	"vision":     "[image omitted: model has no vision support]",  // modality.js:9
	"audioInput": "[audio omitted: model has no audio support]",   // modality.js:10
	"pdf":        "[file omitted: model has no document support]", // modality.js:11
}

var placeholderPrev = map[string]string{
	"vision":     "[Previous image omitted from context.]", // modality.js:15
	"audioInput": "[Previous audio omitted from context.]", // modality.js:16
	"pdf":        "[Previous file omitted from context.]",  // modality.js:17
}

// placeholder picks the current/previous text for a removed capability kind
// (modality.js:19).
func placeholder(capKind string, isLast bool) string {
	if isLast {
		return placeholderCurrent[capKind]
	}
	return placeholderPrev[capKind]
}

// capForOpenAIBlock ports modality.js capForOpenAIBlock (:31-37): an OpenAI
// chat content block -> required capability ("" = plain text/other, keep).
func capForOpenAIBlock(block map[string]any) string {
	switch jsonx.AsStr(block["type"]) {
	case "image_url", "image":
		return "vision"
	case "input_audio", "audio_url":
		return "audioInput"
	case "file":
		return "pdf"
	}
	return ""
}

// filterBlocks ports modality.js filterBlocks (:49-58): drop unsupported
// blocks and append ONE placeholder per removed capability KIND, in
// first-removed order — the JS `for (const cap of removed)` walks the Set in
// insertion order, so duplicates collapse into a single placeholder.
func filterBlocks(blocks []any, m caps.Modality, isLast bool, capOf func(map[string]any) string) []any {
	out := make([]any, 0, len(blocks))
	var removed []string // ordered set — JS `new Set()` insertion order
	for _, raw := range blocks {
		block := jsonx.AsObj(raw)
		capKind := ""
		if block != nil {
			capKind = capOf(block)
		}
		if capKind != "" && !modalityAllowed(m, capKind) {
			removed = appendUnique(removed, capKind) // modality.js:53
			continue
		}
		out = append(out, raw)
	}
	for _, capKind := range removed {
		out = append(out, jsonx.ObjOf("type", "text", "text", placeholder(capKind, isLast))) // modality.js:56
	}
	return out
}

// modalityAllowed maps a capability key onto the resolved flags.
func modalityAllowed(m caps.Modality, capKind string) bool {
	switch capKind {
	case "vision":
		return m.Vision
	case "audioInput":
		return m.AudioInput
	case "pdf":
		return m.PDF
	}
	return true
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// stripOpenAIMessages ports modality.js stripOpenAI (:61-82): OpenAI /
// OpenAI-compatible chat messages[].content[], plus the vision-gated
// msg.images / experimental_attachments / attachments cleanup.
func stripOpenAIMessages(body map[string]any, m caps.Modality) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return // modality.js:62
	}
	last := len(messages) - 1
	for i, raw := range messages {
		msg := jsonx.AsObj(raw)
		if msg == nil {
			continue
		}
		if !m.Vision {
			// modality.js:65-77 — key deletions/filters gated on vision only.
			// :66 deletes msg.images only when it IS an array
			// (`Array.isArray(msg.images)`) — a non-array images value is
			// not attachments metadata and survives the strip.
			if jsonx.AsArr(msg["images"]) != nil {
				delete(msg, "images")
			}
			stripImageAttachments(msg, "experimental_attachments")
			stripImageAttachments(msg, "attachments")
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue // modality.js:78 — non-array content is left untouched
		}
		msg["content"] = filterBlocks(content, m, i == last, capForOpenAIBlock) // :79-80
	}
}

// stripImageAttachments ports the experimental_attachments/attachments filter
// of modality.js:67-76: drop entries that are images either by contentType or
// by an inline data:image/ URL.
func stripImageAttachments(msg map[string]any, key string) {
	arr, ok := msg[key].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(arr))
	for _, raw := range arr {
		a := jsonx.AsObj(raw)
		if a != nil && isImageAttachment(a) {
			continue
		}
		out = append(out, raw)
	}
	msg[key] = out
}

func isImageAttachment(a map[string]any) bool {
	if strings.HasPrefix(jsonx.AsStr(a["contentType"]), "image/") {
		return true
	}
	if u, isStr := a["url"].(string); isStr && strings.HasPrefix(u, "data:image/") {
		return true
	}
	return false
}

// stripResponsesInput ports modality.js stripResponses (:96-109): OpenAI
// Responses input[].content[] with input_image / input_file blocks.
func stripResponsesInput(body map[string]any, m caps.Modality) {
	input, ok := body["input"].([]any)
	if !ok {
		return // modality.js:97
	}
	last := len(input) - 1
	for i, raw := range input {
		item := jsonx.AsObj(raw)
		if item == nil {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue // modality.js:100
		}
		out := make([]any, 0, len(content))
		var removed []string
		for _, b := range content {
			block := jsonx.AsObj(b)
			capKind := ""
			if block != nil {
				switch jsonx.AsStr(block["type"]) { // modality.js:103
				case "input_image":
					capKind = "vision"
				case "input_file":
					capKind = "pdf"
				}
			}
			if capKind != "" && !modalityAllowed(m, capKind) {
				removed = appendUnique(removed, capKind)
				continue
			}
			out = append(out, b)
		}
		for _, capKind := range removed {
			// modality.js:107 — the Responses placeholder is an input_text part.
			out = append(out, jsonx.ObjOf("type", "input_text", "text", placeholder(capKind, i == last)))
		}
		item["content"] = out
	}
}

// NOTE — prefetchRemoteImages (open-sse/translator/concerns/prefetch.js,
// called from chatCore.js:175-179 right after this strip) is NOT ported: it
// converts remote image URLs to base64 only for target formats in
// TARGETS_NEED_BASE64 (prefetch.js:8-11 = GEMINI, GEMINI_CLI, VERTEX,
// ANTIGRAVITY, OLLAMA, KIRO) and returns 0 for everything else at the
// prefetch.js:80 gate. This proxy's only targets are OpenAI chat and OpenAI
// Responses, so the function can never fire here.
