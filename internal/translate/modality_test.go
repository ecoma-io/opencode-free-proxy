package translate

// Unit tests for the unsupported-modality strip, ported from
// open-sse/translator/concerns/modality.js. Every expectation is derived from
// the JS file (indices in comments), not from the Go code.

import (
	"testing"

	"opencode-free-proxy/internal/caps"
	"opencode-free-proxy/internal/jsonx"
)

func TestStripFastExitWhenAllSupported(t *testing.T) {
	body := jb(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`)
	// modality.js:138 — vision+audioInput+pdf all true short-circuits.
	if got := StripUnsupportedModalities(body, false, caps.Modality{Vision: true, AudioInput: true, PDF: true}); got {
		t.Fatalf("fast exit must report false, got true")
	}
	eq(t, "fast exit leaves the body untouched", body, jb(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`))
}

func TestStripOpenAIVisionLastAndEarlierTurns(t *testing.T) {
	// modality.js:9/15 — the last message gets the explanatory placeholder,
	// earlier turns the neutral one.
	body := jb(t, `{"messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"https://x/old.png"}},
			{"type":"text","text":"what changed?"}]},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})

	earlier := jsonx.AsArr(dig(t, body, "messages", 0, "content"))
	if len(earlier) != 2 {
		t.Fatalf("earlier turn = %s, want kept text + placeholder", js(earlier))
	}
	// Kept blocks stay in place; placeholders are APPENDED at the end
	// (modality.js:51-56 pushes after the keep loop).
	eq(t, "kept block stays in place", earlier[0],
		jb(t, `{"type":"text","text":"what changed?"}`))
	ph := jsonx.AsObj(earlier[1])
	if ph["type"] != "text" || ph["text"] != "[Previous image omitted from context.]" {
		t.Fatalf("earlier-turn placeholder = %s, want the neutral text", js(ph))
	}

	lastContent := jsonx.AsArr(dig(t, body, "messages", 2, "content"))
	if len(lastContent) != 1 {
		t.Fatalf("last turn = %s, want the placeholder only", js(lastContent))
	}
	if got := jsonx.AsObj(lastContent[0])["text"]; got != "[image omitted: model has no vision support]" {
		t.Fatalf("last-turn placeholder = %v, want the explanatory text", got)
	}
}

func TestStripOpenAIAudioAndPdf(t *testing.T) {
	// modality.js:33-35 — input_audio/audio_url need audioInput, file needs pdf.
	body := jb(t, `{"messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}},
		{"type":"audio_url","audio_url":{"url":"https://x/a.mp3"}},
		{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,AAA"}},
		{"type":"text","text":"hi"}]}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})

	content := jsonx.AsArr(dig(t, body, "messages", 0, "content"))
	// modality.js:56 — one placeholder per removed KIND, in first-removed order.
	var texts []string
	for _, b := range content {
		if o := jsonx.AsObj(b); o != nil && o["type"] == "text" {
			texts = append(texts, o["text"].(string))
		}
	}
	eq(t, "placeholders appended after kept text (first-removed order, deduped)", texts, []string{
		"hi",
		"[audio omitted: model has no audio support]",
		"[file omitted: model has no document support]",
	})
}

func TestStripOpenAIDuplicateKindSinglePlaceholder(t *testing.T) {
	// Two image blocks → ONE placeholder (JS Set semantics, modality.js:53).
	body := jb(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://x/1.png"}},
		{"type":"image","url":"https://x/2.png"}]}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})

	content := jsonx.AsArr(dig(t, body, "messages", 0, "content"))
	if len(content) != 1 {
		t.Fatalf("content = %s, want a single placeholder", js(content))
	}
	if txt := jsonx.AsObj(content[0])["text"]; txt != "[image omitted: model has no vision support]" {
		t.Fatalf("placeholder text = %v", txt)
	}
}

func TestStripOpenAIVisionGatedAttachmentFields(t *testing.T) {
	// modality.js:65-77 — images key deleted, image attachments filtered out,
	// non-image attachments kept.
	body := jb(t, `{"messages":[{"role":"user","content":"hi",
		"images":[{"url":"https://x/1.png"}],
		"experimental_attachments":[
			{"contentType":"image/png","url":"https://x/2.png"},
			{"contentType":"application/pdf","url":"https://x/a.pdf"}],
		"attachments":[
			{"contentType":"text/plain","url":"data:text/plain;base64,AAA"},
			{"url":"data:image/jpeg;base64,BBB"}]}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})

	msg := msgAt(t, body, 0)
	if key(msg, "images") {
		t.Fatalf("msg.images must be deleted (modality.js:66): %s", js(msg))
	}
	att := jsonx.AsArr(msg["experimental_attachments"])
	if len(att) != 1 || jsonx.AsObj(att[0])["contentType"] != "application/pdf" {
		t.Fatalf("experimental_attachments = %s, want the pdf kept", js(att))
	}
	plain := jsonx.AsArr(msg["attachments"])
	if len(plain) != 1 || jsonx.AsObj(plain[0])["contentType"] != "text/plain" {
		t.Fatalf("attachments = %s, want the data:image entry dropped", js(plain))
	}
}

func TestStripOpenAIKeepsNonArrayContentAndStringContent(t *testing.T) {
	// modality.js:78 — a message whose content is not an array is untouched.
	body := jb(t, `{"messages":[
		{"role":"user","content":"plain text"},
		{"role":"assistant"}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})

	if got := dgs(t, body, "messages", 0, "content"); got != "plain text" {
		t.Fatalf("string content must survive, got %q", got)
	}
	if key(msgAt(t, body, 1), "content") {
		t.Fatalf("missing content must not be synthesized: %s", js(body))
	}
}

func TestStripOpenAINonArrayImagesSurvives(t *testing.T) {
	// modality.js:66 deletes msg.images only when `Array.isArray(msg.images)`
	// — a non-array images value is not attachments metadata and survives the
	// vision strip untouched.
	body := jb(t, `{"messages":[{"role":"user","content":"hi","images":"not-a-list"}]}`)
	StripUnsupportedModalities(body, false, caps.Modality{})
	eq(t, "non-array images kept", msgAt(t, body, 0)["images"], "not-a-list")

	objBody := jb(t, `{"messages":[{"role":"user","content":"hi","images":{"url":"https://x/1.png"}}]}`)
	StripUnsupportedModalities(objBody, false, caps.Modality{})
	eq(t, "object images kept", msgAt(t, objBody, 0)["images"], jb(t, `{"url":"https://x/1.png"}`))
}

func TestStripResponsesInput(t *testing.T) {
	// modality.js:96-109 — input[].content[] with input_image/input_file.
	body := jb(t, `{"input":[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"https://x/old.png"},
			{"type":"input_text","text":"and this"}]},
		{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,AAA"},
			{"type":"input_image","image_url":"https://x/new.png"}]}]}`)
	StripUnsupportedModalities(body, true, caps.Modality{})

	earlier := jsonx.AsArr(dig(t, body, "input", 0, "content"))
	if len(earlier) != 2 {
		t.Fatalf("earlier item = %s, want image + placeholder", js(earlier))
	}
	ph := jsonx.AsObj(earlier[1])
	if ph["type"] != "input_text" || ph["text"] != "[Previous image omitted from context.]" {
		t.Fatalf("earlier placeholder = %s, want the input_text Previous form", js(ph))
	}

	last := jsonx.AsArr(dig(t, body, "input", 1, "content"))
	var texts []string
	for _, b := range last {
		if o := jsonx.AsObj(b); o != nil && o["type"] == "input_text" {
			texts = append(texts, o["text"].(string))
		}
	}
	eq(t, "last-turn placeholders (first-removed order)", texts, []string{
		"[file omitted: model has no document support]",
		"[image omitted: model has no vision support]",
	})
}

func TestStripResponsesFastExitKeepsBody(t *testing.T) {
	raw := `{"input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"hi"}]}]}`
	body := jb(t, raw)
	if StripUnsupportedModalities(body, true, caps.Modality{Vision: true, AudioInput: true, PDF: true}) {
		t.Fatalf("all-supported model must fast-exit false")
	}
	eq(t, "body untouched", body, jb(t, raw))
}
