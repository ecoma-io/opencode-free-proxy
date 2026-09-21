package cloak

import (
	"regexp"
	"strconv"
	"strings"

	"opencode-free-proxy/internal/jsonx"
	"opencode-free-proxy/internal/usage"
)

// Thinking knob names removed by StripAll, mirroring
// translator/concerns/thinkingUnified.js stripAll — every alias any translator
// format has ever emitted must go, because a stray knob on a non-reasoning
// model is an upstream 400.
var thinkingKnobs = []string{
	"thinking",
	"reasoning_effort",
	"reasoning",
	"thinkingConfig",
	"enable_thinking",
	"thinking_budget",
	"output_config",
	"generationConfig", // only its thinkingConfig child is removed
	"request",          // only its generationConfig.thinkingConfig chain is removed
}

// StripAll removes every thinking knob from a request body.
func StripAll(body map[string]any) {
	if body == nil {
		return
	}
	for _, k := range thinkingKnobs {
		switch k {
		case "generationConfig":
			if gc := jsonx.AsObj(body["generationConfig"]); gc != nil {
				delete(gc, "thinkingConfig")
			}
		case "request":
			if req := jsonx.AsObj(body["request"]); req != nil {
				if gc := jsonx.AsObj(req["generationConfig"]); gc != nil {
					delete(gc, "thinkingConfig")
				}
			}
		default:
			delete(body, k)
		}
	}
}

// LevelToBudget maps discrete effort levels to budget tokens
// (concerns/thinking.js LEVEL_TO_BUDGET, web-standard values).
var LevelToBudget = map[string]float64{
	"none":    0,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// BudgetToLevel maps a numeric budget to the nearest discrete level
// (thinking.js budgetToLevel). "" when budget <= 0.
func BudgetToLevel(budget float64) string {
	if budget <= 0 {
		return ""
	}
	switch {
	case budget <= 768:
		return "minimal"
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	case budget <= 28672:
		return "high"
	default:
		return "xhigh"
	}
}

var suffixCaptureRe = regexp.MustCompile(`^(.*)\(([^()]+)\)\s*$`)
var digitsRe = regexp.MustCompile(`^\d+$`)

// ThinkingCfg is the unified thinking intent { mode, level?, budget? }.
type ThinkingCfg struct {
	Mode   string // "none" | "auto" | "level" | "budget"
	Level  string
	Budget float64
}

// ParseSuffix ports thinkingUnified parseSuffix: "model(value)" → cleanModel +
// override. Greedy capture takes the LAST parenthesized group; value is
// trimmed + lowercased; bare digits are budgets; known level names are levels;
// anything else yields no override.
func ParseSuffix(model string) (clean string, cfg *ThinkingCfg) {
	m := suffixCaptureRe.FindStringSubmatch(model)
	if m == nil {
		return model, nil
	}
	clean = strings.TrimSpace(m[1])
	raw := strings.ToLower(strings.TrimSpace(m[2]))
	_, isLevel := LevelToBudget[raw]
	switch {
	case raw == "none" || raw == "off":
		return clean, &ThinkingCfg{Mode: "none"}
	case raw == "auto":
		return clean, &ThinkingCfg{Mode: "auto"}
	case raw == "ultra":
		return clean, &ThinkingCfg{Mode: "level", Level: "ultra"}
	case digitsRe.MatchString(raw):
		budget, _ := strconv.ParseFloat(raw, 64)
		return clean, &ThinkingCfg{Mode: "budget", Budget: budget}
	case isLevel:
		return clean, &ThinkingCfg{Mode: "level", Level: raw}
	default:
		return clean, nil
	}
}

// ExtractThinking ports thinkingUnified extractThinking for the shapes this
// proxy's clients send (OpenAI chat/Responses + Claude + Gemini + Qwen).
// Nil when no thinking intent present.
func ExtractThinking(body map[string]any) *ThinkingCfg {
	if body == nil {
		return nil
	}
	lowerEffort := func(v any) *ThinkingCfg {
		s, is := v.(string)
		if !is || s == "" {
			return nil
		}
		e := strings.ToLower(s)
		switch e {
		case "none", "off":
			return &ThinkingCfg{Mode: "none"}
		case "auto":
			return &ThinkingCfg{Mode: "auto"}
		default:
			return &ThinkingCfg{Mode: "level", Level: e}
		}
	}

	// Claude output_config.effort — explicit, wins.
	if oc := jsonx.AsObj(body["output_config"]); oc != nil {
		if cfg := lowerEffort(oc["effort"]); cfg != nil {
			return cfg
		}
	}

	// OpenAI chat / Responses: effort first (zai sends both shapes).
	// JS: body.reasoning_effort ?? reasoning?.effort — `??` only falls through
	// on null/undefined, so a present-but-empty or non-string reasoning_effort
	// BLOCKS the reasoning.effort fallback (and yields no intent itself).
	if v, has := body["reasoning_effort"]; has && v != nil {
		if s, is := v.(string); is && s != "" {
			if cfg := lowerEffort(s); cfg != nil {
				return cfg
			}
		}
	} else if r := jsonx.AsObj(body["reasoning"]); r != nil {
		if cfg := lowerEffort(r["effort"]); cfg != nil {
			return cfg
		}
	}

	// Claude thinking block (thinkingUnified.js:71-79): the budget goes
	// through `Number(t.budget_tokens)` + Number.isFinite — numeric STRINGS
	// coerce (budget_tokens: "8192" is a 8192 budget) before the > 0 check.
	if t := jsonx.AsObj(body["thinking"]); t != nil {
		switch jsonx.AsStr(t["type"]) {
		case "disabled":
			return &ThinkingCfg{Mode: "none"}
		case "adaptive", "enabled":
			if budget, ok := usage.NumOK(t["budget_tokens"]); ok && budget > 0 {
				return &ThinkingCfg{Mode: "budget", Budget: budget}
			}
			return &ThinkingCfg{Mode: "auto"}
		}
	}

	// Gemini: top-level, generationConfig, or request envelope
	// (thinkingUnified.js:82-92). The chain is a TRUTHY `||` evaluated on
	// each operand's RESULT: a truthy non-object thinkingConfig (42, "x",
	// true) wins the chain and then fails the `typeof === "object"` gate —
	// the nested fallbacks are never consulted — while a FALSY nested
	// thinkingConfig falls through to the request operand, whose value is
	// taken even when falsy (it is the chain's last operand). Then
	// `typeof tc.thinkingLevel === "string"` runs (an empty string counts),
	// and thinkingBudget coerces through Number() like the Claude budget.
	tcAny := body["thinkingConfig"]
	if !truthy(tcAny) {
		tcAny = jsonx.Get(body["generationConfig"], "thinkingConfig")
	}
	if !truthy(tcAny) {
		tcAny = jsonx.Get(jsonx.Get(body["request"], "generationConfig"), "thinkingConfig")
	}
	if tc, isObj := tcAny.(map[string]any); tcAny != nil && isObj {
		if s, is := tc["thinkingLevel"].(string); is {
			return &ThinkingCfg{Mode: "level", Level: strings.ToLower(s)}
		}
		if tb, ok := usage.NumOK(tc["thinkingBudget"]); ok {
			if tb == 0 {
				return &ThinkingCfg{Mode: "none"}
			}
			if tb < 0 {
				return &ThinkingCfg{Mode: "auto"}
			}
			return &ThinkingCfg{Mode: "budget", Budget: tb}
		}
	}

	// Qwen (thinkingUnified.js:94-99): strict boolean checks; the budget
	// coerces through Number() exactly like the Claude/Gemini budgets.
	if b, is := body["enable_thinking"].(bool); is {
		if !b {
			return &ThinkingCfg{Mode: "none"}
		}
		if tb, ok := usage.NumOK(body["thinking_budget"]); ok && tb > 0 {
			return &ThinkingCfg{Mode: "budget", Budget: tb}
		}
		return &ThinkingCfg{Mode: "auto"}
	}

	return nil
}

// normalizeOpenAILevel demotes max/ultra to the model's top supported level
// (thinkingUnified normalizeOpenAILevel).
func normalizeOpenAILevel(level string, supportedLevels []string) string {
	if level != "max" && level != "ultra" {
		return level
	}
	if contains(supportedLevels, level) {
		return level
	}
	if level == "ultra" && contains(supportedLevels, "max") {
		return "max"
	}
	return "xhigh"
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// toLevel renders a cfg as a discrete level string (thinkingUnified toLevel).
func toLevel(cfg *ThinkingCfg) string {
	switch cfg.Mode {
	case "level":
		return cfg.Level
	case "budget":
		if lvl := BudgetToLevel(cfg.Budget); lvl != "" {
			return lvl
		}
		return "medium"
	case "auto":
		return "auto"
	default:
		return ""
	}
}

// IsReasoningModel reports whether the model supports thinking — for this
// proxy only the muse-spark free models reason; everything else gets StripAll.
func IsReasoningModel(model string) bool {
	return IsResponsesModel(model)
}

// MuseSparkLevels is the muse-spark capability ladder (capabilities.js).
var MuseSparkLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// ApplyThinking ports thinkingUnified applyThinking for target wire formats
// openai / openai-responses (both resolve to native format "openai" — a
// reasoning_effort field):
//   - non-reasoning model → StripAll, always
//   - reasoning model with no intent anywhere → untouched
//   - otherwise strip then set reasoning_effort to the normalized level
//
// intent is the client config snapshotted before translation; suffixOverride
// comes from the model name and wins over it.
func ApplyThinking(body map[string]any, model string, intent *ThinkingCfg) {
	if body == nil {
		return
	}
	clean, override := ParseSuffix(model)
	var cfg *ThinkingCfg
	if override != nil {
		cfg = override
	} else {
		cfg = intent
	}
	if cfg == nil {
		cfg = ExtractThinking(body)
	}

	if !IsReasoningModel(clean) {
		StripAll(body)
		return
	}
	if cfg == nil {
		return
	}
	StripAll(body)

	switch cfg.Mode {
	case "none":
		// thinkingCanDisable defaults true for muse-spark.
		body["reasoning_effort"] = "none"
	default:
		if level := toLevel(cfg); level != "" {
			body["reasoning_effort"] = normalizeOpenAILevel(level, MuseSparkLevels)
		}
	}
}

// NormalizeOpencodeReasoning ports executors/opencode.js
// normalizeOpencodeReasoning: fold reasoning_effort into the Responses
// reasoning:{effort,summary} object, demoting max/ultra to xhigh (muse-spark
// ladder has no max).
func NormalizeOpencodeReasoning(body map[string]any) {
	if body == nil {
		return
	}
	currentReasoning := jsonx.AsObj(body["reasoning"])
	requestedEffort, isStr := body["reasoning_effort"].(string)
	if !isStr {
		if currentReasoning == nil {
			return
		}
		requestedEffort, isStr = currentReasoning["effort"].(string)
		if !isStr {
			return
		}
	}

	effort := strings.ToLower(strings.TrimSpace(requestedEffort))
	if (effort == "max" || effort == "ultra") && !contains(MuseSparkLevels, effort) {
		if effort == "ultra" && contains(MuseSparkLevels, "max") {
			effort = "max"
		} else if contains(MuseSparkLevels, "xhigh") {
			effort = "xhigh"
		}
	}

	reasoning := map[string]any{}
	for k, v := range currentReasoning {
		reasoning[k] = v
	}
	reasoning["effort"] = effort
	body["reasoning"] = reasoning
	// JS `if (!body.reasoning.summary)`: absent/null/""/false/0 all default.
	if !truthy(reasoning["summary"]) {
		reasoning["summary"] = "auto"
	}
	delete(body, "reasoning_effort")
}
