// Package config holds all runtime constants. Mirrors open-sse/config —
// values are never hardcoded outside this package.
package config

import (
	"os"
	"strconv"
	"time"
)

// Upstream (OpenCode Zen free tier).
const (
	UpstreamBase = "https://opencode.ai"

	// Upstream gate (verified live 2026-09-18, see 9router executors/opencode.js):
	// /zen/v1/chat/completions and /zen/v1/responses with `Authorization: Bearer
	// public` reject requests that do not look like the official OpenCode
	// agentic client. stream must be true; tools must include the fingerprint
	// quartet; User-Agent must look like opencode >= 1.17.
	// Zen* paths are the request suffixes on the CONFIGURED upstream base
	// (the OCFP_CONFIG upstream.base, default https://opencode.ai); request
	// URLs are never absolute outside this const set (upstream.BuildURL,
	// models.go, cloak).
	ZenChatPath      = "/zen/v1/chat/completions"
	ZenResponsesPath = "/zen/v1/responses"
	ZenModelsPath    = "/zen/v1/models"
	PublicBearer     = "public"

	// Free-model selection (mirrors src/app/api/providers/suggested-models/filters.js).
	// "big-pickle" is free without the -free suffix; deepseek-v4-flash-free
	// returns "Model is unavailable" upstream (2026-09-02).
	KnownFreeModels = "big-pickle"
	DeadFreeModels  = "deepseek-v4-flash-free"
)

// OpenCode client identity for the free-tier gate.
const (
	// Used when the GitHub release lookup fails or the cache is cold.
	ClientFallbackVersion = "1.18.31"
	// Upstream rejects User-Agent versions below opencode/1.17.x (426/403).
	ClientMinMajor    = 1
	ClientMinMinor    = 17
	GitHubReleasesURL = "https://api.github.com/repos/anomalyco/opencode/releases/latest"
	VersionCacheTTL   = 12 * time.Hour

	// The official CLI ships a COMPOUND User-Agent built from three distinct
	// versions (full source trace in docs/recon-opencode-ua.md):
	//
	//	opencode/<v> ai-sdk/provider-utils/<p> runtime/bun/<b>
	//
	//   - <v>: the release version (GitHub releases API probe).
	//   - <p>: @ai-sdk/provider-utils as resolved for packages/opencode's
	//     @ai-sdk/openai-compatible dependency — the package whose fetch
	//     actually stamps the suffix (session/llm/request.ts:18,187-204 build
	//     `opencode/<v>`; provider-utils' postJsonToApi appends
	//     `ai-sdk/provider-utils/<p> runtime/bun/<b>`). Captured live from the
	//     official v1.18.31 binary 2026-09-20: 4.0.23.
	//   - <b>: the Bun build runtime = root package.json `packageManager`
	//     pin at the same tag (1.3.14).
	//
	// These three constants are the compiled-in DEFAULT triple (the chosen
	// tag); identity.UserAgentCache replaces every segment at runtime from
	// the sync probe (raw.githubusercontent.com at the probed tag + bun.lock
	// resolution), so a new opencode release flows through without a rebuild.
	ClientFallbackProviderUtils = "4.0.23"
	ClientFallbackBun           = "1.3.14"

	// UA sync sources at the probed tag (see docs/recon-opencode-ua.md).
	GitHubRawBase       = "https://raw.githubusercontent.com/anomalyco/opencode"
	RootPackageJSONPath = "package.json"
	LockfilePath        = "bun.lock"

	// UASyncInterval is the default background sync cadence for the UA
	// triple. The request hot path NEVER triggers a fetch (documented
	// divergence from opencodeClientVersion.js lazy warm — the ticker keeps
	// the cache at most one interval stale instead). OCFP_CONFIG section
	// user_agent.sync_interval (integer seconds, 0 = disabled) overrides it
	// per generation.
	UASyncInterval = time.Hour
)

// Muse Spark free models are served by /zen/v1/responses (OpenAI Responses
// API); every other model stays on /chat/completions. Declared per-model, not
// per-provider (mirrors providers/registry/opencode.js).
const ResponsesURLModelsPattern = `(?i)^muse[-_]?spark(?:$|[-_:.\s])`

// forceAutoToolChoiceModels (registry quirks): Muse Spark free models are
// auto-only upstream — any explicit non-auto tool_choice is rejected with 400
// (verified live 2026-09-19, decolua/9router#4165). Exact ids only.
var ForceAutoToolChoiceModels = map[string]bool{
	"muse-spark-1.2-contributor-free": true,
	"muse-spark-1.3-contributor-free": true,
}

// Fingerprint quartet the free-tier gate demands in body.tools.
var FingerprintTools = []string{"bash", "glob", "grep", "read"}

// Wire limits (Responses API).
const (
	MaxToolNameLen     = 128
	MaxResponsesCallID = 64
	MaxSessionLength   = 256
	MinMaxOutputTokens = 16 // upstream 400s below this ("The number must be `>= 16`")
)

// Timeouts / retries (config/runtimeConfig.js defaults).
const (
	ConnectTimeout = 60 * time.Second  // response-headers timeout
	StreamStall    = 360 * time.Second // max gap between SSE chunks
	DefaultRatio   = 0.75              // hidden-thinking synthesis share
	SynthMaxOutput = 10                // below this output, no synthesis
)

// Retry matrix per upstream status: attempts/delay (default executor rules —
// note 429 is deliberately NOT retried: the free tier must fail fast).
type RetryRule struct {
	Attempts int
	Delay    time.Duration
}

var RetryRules = map[int]RetryRule{
	429: {Attempts: 0, Delay: 0},
	502: {Attempts: 3, Delay: 3 * time.Second},
	503: {Attempts: 3, Delay: 2 * time.Second},
	504: {Attempts: 2, Delay: 3 * time.Second},
}

// Server defaults.
const (
	DefaultPort = "8090"
)

// Client-facing OpenAI-compatible error typing (config/errorConfig.js).
type ErrorInfo struct{ Type, Code string }

var ErrorTypes = map[int]ErrorInfo{
	400: {"invalid_request_error", "bad_request"},
	401: {"authentication_error", "invalid_api_key"},
	402: {"billing_error", "payment_required"},
	403: {"permission_error", "insufficient_quota"},
	404: {"invalid_request_error", "model_not_found"},
	406: {"invalid_request_error", "model_not_supported"},
	429: {"rate_limit_error", "rate_limit_exceeded"},
	500: {"server_error", "internal_server_error"},
	502: {"server_error", "bad_gateway"},
	503: {"server_error", "service_unavailable"},
	504: {"server_error", "gateway_timeout"},
}

// DefaultErrorMessages fill in when an error carries no message.
var DefaultErrorMessages = map[int]string{
	400: "Bad request",
	401: "Invalid API key provided",
	402: "Payment required",
	403: "You exceeded your current quota",
	404: "Model not found",
	406: "Model not supported",
	429: "Rate limit exceeded",
	500: "Internal server error",
	502: "Bad gateway - upstream provider error",
	503: "Service temporarily unavailable",
	504: "Gateway timeout",
}

// FromEnv builds the process-bootstrap config from environment variables.
// Every SERVICE setting (upstream base, UA sync cadence)
// lives in the OCFP_CONFIG YAML document instead — see file.go; these four
// variables are deliberately not part of it. Every env var the process reads
// carries the OCFP_ prefix (OCFP_PORT included — never a bare PORT), so a
// deployment can identify the service's whole environment by one prefix.
type Config struct {
	Port string
	// ConfigPath is OCFP_CONFIG: the routing + service config file. Empty =
	// the built-in default runtime.
	ConfigPath string
	// ShutdownGrace is OCFP_SHUTDOWN_GRACE: how long draining waits for
	// active requests/streams before forced close.
	ShutdownGrace time.Duration
	// ConfigPoll is OCFP_CONFIG_POLL_MS: the hot-reload poll interval.
	ConfigPoll time.Duration
}

// DefaultShutdownGrace is used when OCFP_SHUTDOWN_GRACE is unset (ms).
const DefaultShutdownGrace = 30 * time.Second

// DefaultConfigPoll is the hot-reload poll interval (the rotation-proxy
// gateway design polled at 1 s; repeated writes coalesce).
const DefaultConfigPoll = time.Second

func FromEnv() *Config {
	return &Config{
		Port:       envOr("OCFP_PORT", DefaultPort),
		ConfigPath: os.Getenv("OCFP_CONFIG"),
		// OCFP_SHUTDOWN_GRACE is milliseconds like every other ms sibling
		// (OCFP_CONFIG_POLL_MS). A "30s" duration string would be silently
		// dropped by envMs' ParseDuration; envMs matches the documented
		// contract.
		ShutdownGrace: envMs("OCFP_SHUTDOWN_GRACE", DefaultShutdownGrace),
		ConfigPoll:    envMs("OCFP_CONFIG_POLL_MS", DefaultConfigPoll),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envMs(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return def
}

// Hardening bounds for the forced SSE→JSON conversion and the /v1/models
// fetch. These are Go-side defensive bounds with no JS counterpart — Node's
// undici streams are backpressured and cancellable, so the JS handlers read
// upstream bodies unbounded (sseToJsonHandler.js:306
// `await providerResponse.text()`); this process must not.
const (
	// MaxForcedSSEBytes caps how many upstream SSE bytes the forced
	// SSE→JSON aggregation will buffer for one request (the non-streaming
	// client path). Sized generously above any real conversation
	// aggregation — a full agentic turn is a few hundred KB of SSE even
	// with heavy tool-call arguments — while a hostile or broken upstream
	// can no longer grow the buffer without limit. Exceeding it answers the
	// client 502 exactly like any other forced-conversion failure.
	MaxForcedSSEBytes = 64 << 20

	// MaxResponsesOutputIndex is the highest `output_index` the Responses
	// SSE→JSON aggregation accepts for an output item
	// (streamToJsonConverter.js:29 stores it, :89-93 fills placeholders
	// densely up to the max index). Real Responses streams carry a few
	// dozen items at most; an index beyond this bound is hostile or broken,
	// and honoring it would make the dense placeholder fill allocate
	// super-linearly. Events beyond the bound are dropped (the rest of the
	// response still aggregates; see internal/relay/nonstream.go).
	MaxResponsesOutputIndex = 4096

	// ModelsFetchTimeout bounds the whole /v1/models upstream fetch (the
	// static-registry fallback keeps the endpoint fail-open). It was a
	// 10 s client timeout before the handler started reusing the shared
	// direct client, whose transports carry only a response-HEADER
	// deadline (ConnectTimeout) — this restores a total bound.
	ModelsFetchTimeout = 10 * time.Second
)
