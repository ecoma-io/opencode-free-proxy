// Package caps resolves per-model INPUT-modality support (vision, pdf,
// audioInput, videoInput) — the only capability keys the modality strip
// consumes (open-sse/providers/capabilities.js MODALITY_KEYS, capabilities.js:413).
//
// Ported from open-sse/providers/capabilities.js getCapabilitiesForModel
// (capabilities.js:453-479) with this proxy's single provider in mind:
//
//  1. PROVIDER_CAPABILITIES[provider][model] — SKIPPED: the table
//     (capabilities.js:146-254) has no "opencode" key, so step 1 can never
//     match for this proxy (verified — the entry would be dead code).
//  2. MODEL_CAPABILITIES exact id (baseModel, then the raw model).
//  3. PATTERN_CAPABILITIES glob rows, first match wins (order matters).
//  4. DEFAULT floor, refined by the visionPatterns name heuristic.
//
// Non-modality capability keys (reasoning, thinkingFormat, contextWindow,
// search, …) are deliberately omitted — thinking behavior lives in
// internal/cloak, and nothing else in this proxy reads the catalog.
package caps

import (
	"regexp"
	"strings"
)

// Modality carries the four input-modality keys the modality strip reads.
// The JS tables only ever declare these `true` (a `false` never appears in a
// MODEL_/PATTERN_ row), so merging a row over the DEFAULT floor is a plain OR.
type Modality struct {
	Vision     bool // read images (capabilities.js:44)
	PDF        bool // read PDF / documents (capabilities.js:45)
	AudioInput bool // read audio (capabilities.js:46)
	VideoInput bool // read video (capabilities.js:47)
}

// Default is the DEFAULT_CAPABILITIES floor restricted to the modality keys —
// every value false (capabilities.js:42-47). The strip only ever fires when a
// key is exactly false, so this floor is what makes an unknown model text-only.
var Default = Modality{}

// with merges a table row over the floor (the `{ ...DEFAULT_CAPABILITIES, ...row }`
// spread of capabilities.js:467/468). OR is exact here because no row declares
// a modality false — see the Modality doc comment.
func (d Modality) with(row Modality) Modality {
	return Modality{
		Vision:     d.Vision || row.Vision,
		PDF:        d.PDF || row.PDF,
		AudioInput: d.AudioInput || row.AudioInput,
		VideoInput: d.VideoInput || row.VideoInput,
	}
}

// Resolve ports getCapabilitiesForModel for this proxy's fixed provider.
// Callers pass the alias-free, suffix-free id (cloak.BaseModelID); chatCore.js:171
// instead passes modelInfo.model, which can still carry the thinking suffix
// "model(level)" at that point — stripThinkingSuffix only runs when the
// upstream body is built (chatCore.js:187/212). The outcomes are equivalent:
// no MODEL_CAPABILITIES row is keyed with a paren suffix, so a suffixed id
// always misses the exact table and falls to the PATTERN globs, whose
// trailing `*` (every row ends in one) absorbs the suffix — the first
// matching row, and its flags, are the same for the level-name/number suffix
// values the proxy ever derives.
func Resolve(model string) Modality {
	if model == "" {
		// capabilities.js:454 — empty model short-circuits to the floor, with
		// no vision-heuristic refine.
		return Default
	}

	// Canonical exact lookup strips the vendor prefix:
	// "anthropic/claude-opus-4.7" -> "claude-opus-4.7" (capabilities.js:457).
	baseModel := model
	if i := strings.LastIndex(baseModel, "/"); i >= 0 {
		baseModel = baseModel[i+1:]
	}

	// 2. Canonical exact — baseModel first, then the raw id. These return
	// BEFORE the refine step, so an exact hit never gets the vision-name
	// heuristic applied on top (capabilities.js:467-468 vs :470-478).
	if m, ok := modelCapabilities[baseModel]; ok {
		return Default.with(m)
	}
	if m, ok := modelCapabilities[model]; ok {
		return Default.with(m)
	}

	// 3. Pattern match (first match wins), refined by the name heuristic
	// (capabilities.js:471-475). matchPattern tests baseModel OR the raw model.
	for _, row := range patternCapabilities {
		if row.re.MatchString(baseModel) || row.re.MatchString(model) {
			return refine(row.caps, model)
		}
	}

	// 4. Floor, refined (capabilities.js:478).
	return refine(Modality{}, model)
}

// refine ports capabilities.js refine (:430-451) for the modality keys. The
// synced-catalog branch (:433-446, installed via setCatalogSource) does not
// exist in this proxy — there is no models.dev catalog file to read — so the
// only refinement left is the name heuristic, which only ever turns vision ON.
func refine(base Modality, model string) Modality {
	result := Default.with(base)
	if !result.Vision && LooksLikeVisionModel(model) {
		result.Vision = true // capabilities.js:448
	}
	return result
}

// patternRow is one PATTERN_CAPABILITIES entry compiled at init.
type patternRow struct {
	re   *regexp.Regexp
	caps Modality
}

// compilePattern ports matchPattern (open-sse/providers/pricing.js:356-358):
// split on "*", regex-escape each segment, join with ".*", and anchor +
// case-fold so a pattern must match the FULL model id.
func compilePattern(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?i)^")
	for i, seg := range strings.Split(pattern, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(seg))
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// patternCapabilities ports PATTERN_CAPABILITIES (capabilities.js:262-403)
// in original order — specific families before generic catch-alls, so the
// first match wins exactly as in JS. Rows whose only flags are non-modality
// (thinkingFormat/reasoning/contextWindow/…) carry the zero Modality but must
// stay in the table: a match short-circuits later rows and the vision
// heuristic (capabilities.js:471-475 returns before :478's heuristic could
// only apply via refine — which still runs, but on the row's own flags).
var patternCapabilities = buildPatterns([]struct {
	pattern string
	caps    Modality
}{
	// ── Claude (capabilities.js:264-276) ─────────────────────────────
	{`*claude*opus-5*`, Modality{Vision: true}},
	{`*claude*opus-4.6*`, Modality{Vision: true}},
	{`*claude*opus-4.7*`, Modality{Vision: true}},
	{`*claude*opus-4.8*`, Modality{Vision: true}},
	{`*claude*sonnet-4.6*`, Modality{Vision: true}},
	{`*claude*sonnet-4.7*`, Modality{Vision: true}},
	{`*claude*haiku*`, Modality{Vision: true}},
	{`*claude*opus*`, Modality{Vision: true}},
	{`*claude*sonnet*`, Modality{Vision: true}},
	{`*claude*fable*`, Modality{Vision: true}},
	{`*claude*mythos*`, Modality{Vision: true}},
	{`*claude-3*`, Modality{Vision: true}},
	{`*claude*`, Modality{Vision: true}},

	// ── Gemini (capabilities.js:279-288) ─────────────────────────────
	{`*gemini*image*`, Modality{Vision: true}},
	{`*gemini-3.8*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini-3.7*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini-3*pro*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini-3*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini-2.5*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini-2*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*gemini*`, Modality{Vision: true}},
	{`*gemma*`, Modality{Vision: true}},
	{`*nanobanana*`, Modality{Vision: true}},

	// ── OpenAI GPT (capabilities.js:291-302) ─────────────────────────
	{`*gpt-6*`, Modality{Vision: true}},
	{`*gpt-5*image*`, Modality{}}, // imageOutput only (capabilities.js:294)
	{`*gpt-5*codex*`, Modality{}}, // reasoning only (capabilities.js:295)
	{`*gpt-5*`, Modality{Vision: true}},
	{`*gpt-4o*`, Modality{Vision: true}},
	{`*gpt-4.1*`, Modality{Vision: true}},
	{`*gpt-4-turbo*`, Modality{Vision: true}},
	{`*gpt-4*`, Modality{}},
	{`*gpt-3.5*`, Modality{}},
	{`*gpt-oss*`, Modality{}},

	// ── OpenAI o-series (capabilities.js:305-308) ────────────────────
	{`*o1-mini*`, Modality{}},
	{`*o1*`, Modality{Vision: true}},
	{`*o3*`, Modality{Vision: true}},
	{`*o4*`, Modality{Vision: true}},

	// ── Grok (capabilities.js:311-319) ───────────────────────────────
	{`*grok*image*`, Modality{}}, // imageOutput only (capabilities.js:311)
	{`*grok-code*`, Modality{}},
	{`*grok-4.6*`, Modality{Vision: true}},
	{`*grok-4.5*`, Modality{Vision: true}},
	{`*grok-4*`, Modality{Vision: true}},
	{`*grok-3*`, Modality{Vision: true}},
	{`*grok*`, Modality{Vision: true}},

	// ── Qwen (capabilities.js:322-332) ───────────────────────────────
	{`*qwen*vl*`, Modality{Vision: true}},
	{`*qwen*omni*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*qwen*coder*`, Modality{}},
	{`*qwen*max*`, Modality{}},
	{`*qwen3.5*`, Modality{Vision: true, VideoInput: true}},
	{`*qwen3.6*`, Modality{Vision: true, VideoInput: true}},
	{`*qwen3.7*`, Modality{Vision: true, VideoInput: true}},
	{`*qwen*plus*`, Modality{Vision: true}},
	{`*qwen*235b*`, Modality{}},
	{`*qwq*`, Modality{}},
	{`*qwen*`, Modality{}},

	// ── Kimi (capabilities.js:335-339) ───────────────────────────────
	{`*kimi*k3*`, Modality{Vision: true, VideoInput: true}},
	{`*kimi*for-coding*`, Modality{Vision: true, VideoInput: true}},
	{`*kimi*k2.7*code*`, Modality{Vision: true, VideoInput: true}},
	{`*kimi*k2*`, Modality{Vision: true}},
	{`*kimi*`, Modality{}},

	// ── GLM / Z.ai (capabilities.js:344-349) — reasoning-only family ─
	{`*glm-5.3*`, Modality{}},
	{`*glm-5.2*`, Modality{}},
	{`*glm-5*`, Modality{}},
	{`*glm-4.7*`, Modality{}},
	{`*glm-4*`, Modality{}},
	{`*glm*`, Modality{}},

	// ── DeepSeek (capabilities.js:352-356) — reasoning-only family ───
	{`*deepseek-v4*`, Modality{}},
	{`*reasoner*`, Modality{}},
	{`*deepseek-r*`, Modality{}},
	{`*deepseek-chat*`, Modality{}},
	{`*deepseek*`, Modality{}},

	// ── MiniMax (capabilities.js:359-362) ────────────────────────────
	{`*minimax*image*`, Modality{}}, // imageOutput only (capabilities.js:359)
	{`*minimax-m3*`, Modality{Vision: true}},
	{`*minimax-m2.7*`, Modality{}},
	{`*minimax*`, Modality{}},

	// ── Xiaomi MiMo (capabilities.js:365-367) ────────────────────────
	{`*mimo*v2.5*`, Modality{Vision: true, AudioInput: true, VideoInput: true}},
	{`*mimo*omni*`, Modality{Vision: true, AudioInput: true}},
	{`*mimo*`, Modality{Vision: true}},

	// ── Llama / Mistral / Cohere (capabilities.js:370-380) ───────────
	{`*llama-4*`, Modality{Vision: true}},
	{`*llama*`, Modality{}},
	{`*codestral*`, Modality{}},
	{`*mistral-large*`, Modality{Vision: true}},
	{`*mistral*`, Modality{}},
	{`*command-a-vision*`, Modality{Vision: true}},
	{`*command*`, Modality{}},

	// ── Perplexity (capabilities.js:383-385) — search-only ───────────
	{`*sonar*`, Modality{}},
	{`*pplx*`, Modality{}},
	{`*perplexity*`, Modality{}},

	// ── Poolside Laguna (capabilities.js:390-392) — reasoning-only ───
	{`*laguna-s-2.1*free*`, Modality{}},
	{`*laguna-s-2.1*`, Modality{}},
	{`*laguna*`, Modality{}},

	// ── OpenCode Free Muse Spark (capabilities.js:396) ───────────────
	{`*muse*spark*`, Modality{Vision: true}},

	// ── Others (capabilities.js:398-402) ─────────────────────────────
	{`*hunyuan*`, Modality{}},
	{`hy3*`, Modality{}},
	{`*step-*`, Modality{}},
	{`*nemotron*`, Modality{}},
	{`*ling-*`, Modality{}},
})

func buildPatterns(rows []struct {
	pattern string
	caps    Modality
}) []patternRow {
	out := make([]patternRow, len(rows))
	for i, row := range rows {
		out[i] = patternRow{re: compilePattern(row.pattern), caps: row.caps}
	}
	return out
}

// modelCapabilities ports the MODEL_CAPABILITIES exact-id rows restricted to
// the modality keys (capabilities.js:85-134). Zero-value entries (gpt-image-1,
// coder-model) declare no input modalities but MUST stay: an exact hit returns
// before the pattern table and the vision heuristic are consulted
// (capabilities.js:467-468).
var modelCapabilities = map[string]Modality{
	// Claude rows (capabilities.js:87-105) — every entry vision:true.
	"claude-fable-5-1":                 {Vision: true},
	"claude-opus-5":                    {Vision: true},
	"claude-opus-5-thinking":           {Vision: true},
	"claude-opus-5-agentic":            {Vision: true},
	"claude-opus-5-thinking-agentic":   {Vision: true},
	"claude-opus-4.6":                  {Vision: true},
	"claude-opus-4.7":                  {Vision: true},
	"claude-opus-4-7":                  {Vision: true},
	"claude-opus-4.8":                  {Vision: true},
	"claude-opus-4-6":                  {Vision: true},
	"claude-opus-4-8":                  {Vision: true},
	"claude-opus-4.8-thinking":         {Vision: true},
	"claude-opus-4-8-thinking":         {Vision: true},
	"claude-sonnet-4.6":                {Vision: true},
	"claude-sonnet-4-6":                {Vision: true},
	"claude-sonnet-5":                  {Vision: true},
	"claude-sonnet-5-thinking":         {Vision: true},
	"claude-sonnet-5-agentic":          {Vision: true},
	"claude-sonnet-5-thinking-agentic": {Vision: true},

	// capabilities.js:108 — image GENERATION only, takes no image input.
	"gpt-image-1": {},

	// GLM vision variants (capabilities.js:112-114) — text GLM has no vision.
	"glm-5.3-flash": {Vision: true, VideoInput: true, PDF: true},
	"glm-4.6v":      {Vision: true, VideoInput: true},
	"glm-4.5v":      {Vision: true, VideoInput: true},

	// capabilities.js:117 — DeepSeek's first V4 model with image input.
	"deepseek-v4-flash-vision-exp": {Vision: true},

	// Qwen registry aliases (capabilities.js:120-121).
	"vision-model": {Vision: true},
	"coder-model":  {},

	// Kimi flagship + coding (capabilities.js:124-129).
	"kimi-k3":                   {Vision: true, VideoInput: true},
	"k3":                        {Vision: true, VideoInput: true},
	"kimi-for-coding":           {Vision: true, VideoInput: true},
	"kimi-for-coding-highspeed": {Vision: true, VideoInput: true},
	"kimi-k2.7-code":            {Vision: true, VideoInput: true},
	"kimi-k2.7-code-highspeed":  {Vision: true, VideoInput: true},

	// OpenCode Free Muse Spark (capabilities.js:132-133) — multimodal per
	// models.dev meta/muse-spark via OpenAI Responses input_image.
	"muse-spark-1.2-contributor-free": {Vision: true},
	"muse-spark-1.3-contributor-free": {Vision: true},
}
