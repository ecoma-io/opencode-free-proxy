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

	// DialTimeout and TLSHandshakeTimeout bound the two transport phases
	// ResponseHeaderTimeout cannot reach on the DIRECT paths. The JS router
	// needs neither as a separate knob: base.js:133-138 arms ONE
	// AbortController over FETCH_CONNECT_TIMEOUT_MS (runtimeConfig.js:58,
	// 60 s) around the whole fetch and merges it with the caller's signal
	// (AbortSignal.any), so DNS + dial + TLS + request + response headers
	// share a single 60 s budget that aborts the fetch wherever it happens
	// to be. Go's transport splits those phases across three independent
	// fields, so the port approximates the single budget per phase: each
	// phase gets the same 60 s, and each phase's expiry reaches the fetch
	// as a transport error that classifies ClassTimeout like the JS abort
	// (base.js:169-178 network-exception branch). Worst case one attempt
	// spends ~3x the JS budget (dial + TLS + headers); a blackholed
	// upstream fails at the FIRST phase, so real blackhole latency stays
	// ~60 s + DNS. The tunneled CONNECT transport (connect.go) needs
	// neither: its DialTLSContext bounds dial + CONNECT + origin TLS under
	// one conn deadline already.
	DialTimeout         = 60 * time.Second
	TLSHandshakeTimeout = 60 * time.Second
)

// There is deliberately NO retry matrix here. The JS router retried upstream
// STATUSES inside one egress (base.js:104-125, the retryAttemptsByUrl budget)
// and this port carried that table as RetryRules until the recovery-ownership
// split made it wrong: a provider HTTP response of any status means the
// provider received the request and answered it, so OFP relays it. Provider-
// level retry belongs to the Injector (docs/recovery-semantics.md), and an
// egress is left only for a failure that provably happened before the request
// was sent — a policy, not a table.

// MaxRedirects is how many redirects ONE upstream attempt follows before
// giving up with a network-class error. A redirect hop is not a retry: the
// chain is one logical request, followed hop by hop, and the failure that
// ends it is whatever the last hop's transport reported.
// The JS router's fetch is undici under a ProxyAgent dispatcher
// (utils/proxyFetch.js getDispatcher → originalFetch(url, {dispatcher})),
// and undici caps the chain at twenty: `if (request.redirectCount === 20)
// return … makeNetworkError('redirect count exceeded')` —
// node_modules/undici/lib/web/fetch/index.js:1250-1255 (the WHATWG fetch
// algorithm). net/http's own default checkRedirect cap is 10, which would
// NOT be parity — an upstream legitimately redirecting 11-20 times succeeds
// in the JS router and must succeed here.
const MaxRedirects = 20

// IdleConnTimeout closes a pooled upstream conn after this much idle time,
// on EVERY transport this process builds. Two reasons, one parity and one
// hardening:
//
//   - Parity: the JS router's fetches run on undici dispatchers whose
//     keepAliveTimeout default is 4 s (node_modules/undici/lib/dispatcher/
//     client.js:252 `keepAliveTimeout == null ? 4e3 : keepAliveTimeout`;
//     utils/proxyFetch.js getDispatcher builds its ProxyAgent without
//     overriding it, and Node's global dispatcher carries the same client
//     defaults), so idle keep-alive conns are dropped after 4 s in the JS
//     router too.
//   - Hardening: net/http registers NO finalizer for pooled conns. A conn
//     still busy when a generation prune calls CloseIdleConnections returns
//     to the idle pool afterwards and, without an idle timeout, sits there
//     forever — pinning its fd and readLoop goroutine for the life of the
//     process. The transport arms an idle timer whenever a conn re-enters
//     the pool (net/http tryPutIdleConn), so this is the only backstop that
//     reaches conns that outlive their client.
const IdleConnTimeout = 4 * time.Second

// Server defaults.
const (
	DefaultPort     = "8090"
	DefaultLogLevel = "info"
)

// http.Server timing bounds (cmd/server/main.go). Go-side hardening with no
// JS counterpart to mirror: the JS server rides Node/undici platform
// defaults, which bound header reads and idle keep-alives implicitly; Go's
// http.Server defaults to NO timeouts, so a slow client can pin a goroutine
// and a connection indefinitely — bounded only by the shutdown grace. The two
// omissions (ReadTimeout/WriteTimeout) are deliberate and documented at the
// construction site in cmd/server/main.go: both would cut long-lived SSE
// streams mid-flight.
const (
	// HeaderReadTimeout bounds reading a request's headers only — per Go
	// docs the connection's read deadline is reset after the headers, so a
	// slow-but-legitimate streaming request body is never touched by it. A
	// real client's header block is under a kilobyte sent in one burst;
	// 10 s covers any sane connect→send latency (dozens of round trips)
	// while a slowloris drip that would otherwise hold a connection
	// forever is cut after 10 s. Also comfortably below the 55 s shutdown
	// grace, so header-stalled connections pre-dating a signal die inside
	// the drain window regardless.
	HeaderReadTimeout = 10 * time.Second

	// IdleTimeout bounds a keep-alive connection that sits IDLE between
	// requests. It never applies mid-response: net/http arms the idle read
	// deadline only after a response completes and clears it the moment the
	// next request's first bytes arrive (GOROOT src/net/http/server.go,
	// conn.serve — verified for go1.26), so a long-lived SSE stream cannot
	// be truncated by it at any chunk cadence. 120 s sits above common
	// front-proxy/LB idle windows (ALB 60 s, nginx 75 s) so healthy client
	// keep-alive reuse is not churned by us first, while a connection
	// abandoned by a vanished client is reclaimed in 2 minutes instead of
	// living until process shutdown; closing an idle keep-alive is
	// transparent — the next request opens a fresh connection.
	IdleTimeout = 120 * time.Second
)

// Upstream-error evidence bounds (internal/upstream/evidence.go). These cap
// the observational forensics layer — a hostile upstream (or a hostile error
// body) must never be able to grow log-line or recorder memory without limit.
// The row cap is sized for the default budget (3 egresses × one row each,
// plus scheduling skips); an operator-raised fallback budget can outgrow it,
// and overflow then lands in the dropped counter surfaced on the last event —
// bounded by design, visible when it happens.
const (
	// EvidenceMaxRows bounds the rows one request's recorder keeps; beyond it
	// appends are counted in a dropped counter instead of stored.
	EvidenceMaxRows = 16
	// EvidenceMessageBytes clamps a sanitized upstream/transport error
	// message inside one row.
	EvidenceMessageBytes = 512
	// EvidencePeekBytes clamps the raw error-body peek (the same capped slice
	// parseUpstreamError already read — never a second body read).
	EvidencePeekBytes = 256
	// EvidenceRateLimitEntries bounds how many rate-limit-ish response
	// headers one row records; EvidenceRateLimitValueBytes clamps each
	// header value (and the Retry-After echo).
	EvidenceRateLimitEntries    = 8
	EvidenceRateLimitValueBytes = 64
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
const DefaultShutdownGrace = 55 * time.Second

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
