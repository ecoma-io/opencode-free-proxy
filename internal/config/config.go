// Package config holds all runtime constants. Mirrors open-sse/config —
// values are never hardcoded outside this package.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Upstream (the OpenCode inference gateway, free tier).
const (
	UpstreamBase = "https://opencode.ai"

	// Upstream gate (measured live 2026-09-18, re-measured 2026-10-15 against
	// the inference backend, see docs/recon-free-tier-gate.md):
	// /inference/v1/chat/completions and /inference/v1/responses with
	// `Authorization: Bearer public` reject requests that do not look like the
	// official OpenCode agentic client. `stream` must be true; `tools` must
	// carry the fingerprint quartet WITH real parameter schemas; the session id
	// must travel under BOTH x-opencode-session and x-opencode-session-id; the
	// v1-era `x-opencode-request` header is refused outright (any value);
	// User-Agent must carry `opencode/` with a version >= 1.18.0.
	//
	// These const names keep the `Zen*` prefix they had when the paths were
	// `/zen/v1/*`; the VALUE moved on 2026-10-15 when upstream retired /zen/*
	// (it now answers 403 FreeTierError for every identity, the official
	// client's included) and pointed the client at `/inference/v1`. They are
	// request suffixes on the CONFIGURED upstream base (the OCFP_CONFIG
	// upstream.base, default https://opencode.ai); request URLs are never
	// absolute outside this const set (upstream.BuildURL, models.go, cloak).
	ZenChatPath      = "/inference/v1/chat/completions"
	ZenResponsesPath = "/inference/v1/responses"
	ZenModelsPath    = "/inference/v1/models"
	PublicBearer     = "public"

	// Free-model selection (mirrors src/app/api/providers/suggested-models/filters.js).
	// "big-pickle" is free without the -free suffix; deepseek-v4-flash-free
	// returns "Model is unavailable" upstream (2026-09-02).
	KnownFreeModels = "big-pickle"
	DeadFreeModels  = "deepseek-v4-flash-free"
)

// UAStyle selects which User-Agent WIRE FORM the request path stamps. The
// two forms are the two client generations the free-tier gate has admitted:
//
//   - V1 (the compound triple, opencode/<v> ai-sdk/… runtime/bun/…): what
//     this proxy has always forged; still admitted (measured 2026-10-15).
//   - V2 (opencode/<channel>/<version>/<name>): what the official 2.x CLI
//     sends since the inference backend (packages/core/src/app.ts at
//     v2.1.0: `opencode/${app.channel}/${app.version}/${app.name}`; captured
//     on the wire 2026-10-13 as opencode/latest/2.0.27/cli).
//
// Both are synced in the same background round by identity.UserAgentCache
// (each form degrading independently on failure — never a mixed identity);
// Style is the runtime CHOICE between them — a config knob read per request,
// so a gate change is answered by a reload, not a rebuild.
// Default V1: the long-measured identity. Empty YAML value = V1.
type UAStyle string

const (
	UAStyleV1 UAStyle = "v1"
	UAStyleV2 UAStyle = "v2"
)

// OpenCode client identity for the free-tier gate.
const (
	// Used when the GitHub release lookup fails or the cache is cold.
	ClientFallbackVersion = "1.18.31"
	// Cold-cache fallback for the v2 wire form (user_agent.style: v2). The
	// 2.x channel form (opencode/latest/<v>/cli) has never existed in the
	// 1.x generation, so this must be a 2.x era tag — a v1 number here would
	// fabricate the cross-generation identity opencode/latest/1.18.31/cli,
	// which no real client ever sent (measured 2026-10-13:
	// opencode/latest/2.0.27/cli) and which a form/version-consistent gate
	// would reject. Earliest stable v2 release in the repo's tag history.
	ClientFallbackVersionV2 = "2.0.1"
	// Upstream rejects User-Agent versions below opencode/1.18.0.
	//
	// MEASURED live 2026-10-15 on the /inference/v1 gate (and again on
	// /zen/v1 on 2026-10-13): `opencode/1.17.x` is answered 426
	// UpgradeRequired ("OpenCode 1.18.0 or newer is required to use the free
	// tier"), 1.18.0 and up are admitted, and the version gate is evaluated
	// BEFORE the free-tier gate (a 1.17 UA with a bad session id returns 403
	// under the old endpoint, but a legal one returns 426). A UA with no
	// numeric version at all (`opencode/latest`, `python-requests/2.0`) is a
	// 403, not a 426 — a different rejection path, worth knowing when reading
	// logs. Raised from 17 to 18 so a 1.17-era opencode client FORGES the
	// current identity instead of having its too-old UA forwarded verbatim and
	// the upstream 426 relayed as-is. See docs/recon-free-tier-gate.md.
	ClientMinMajor    = 1
	ClientMinMinor    = 18
	GitHubReleasesURL = "https://api.github.com/repos/anomalyco/opencode/releases/latest"
	// GitHubTagsListURL lists the repo's git tags (newest first) and backs
	// the v2 wire-form sync. NOT the releases list: verified by a full
	// pagination sweep 2026-10-20, every release in the repo carries a 1.x
	// tag (zero v2 releases exist) while stable v2 tags exist only as git
	// tags — a releases scan would miss them entirely.
	GitHubTagsListURL = "https://api.github.com/repos/anomalyco/opencode/tags?per_page=100"
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

// Muse Spark free models are served by the Responses endpoint (OpenAI Responses
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
//
// MEASURED 2026-10-15 against the /inference/v1 gate (docs/recon-free-tier-gate.md):
// the quartet is still four tools, but two facts changed with opencode v2.
//
//   - The command tool is named `shell`, not `bash`. Either name is accepted
//     today (`bash` still passes), but `shell` is what the official v2 client
//     sends, so it is what we forge.
//   - The gate now reads the tool SCHEMAS: four declarations with empty
//     `properties` are refused with 403 FreeTierError, and dropping the
//     command tool from the quartet is refused too. Descriptions are not
//     read (blanking every description still passes). FingerprintToolSchemas
//     below carries the real declarations.
var FingerprintTools = []string{"shell", "glob", "grep", "read"}

// FingerprintTool is one official OpenCode client tool declaration, reduced to
// the fields that reach the wire: the real description and the real parameter
// schema. Required names the properties the official client marks required.
type FingerprintTool struct {
	Description string
	Properties  map[string]any
	Required    []string
}

// FingerprintToolSchemas are the REAL tool declarations of the official client
// at opencode v2.0.29, read from packages/core/src/tool/plugin/{shell,glob,
// grep,read}.ts (`name`, the `description` literal and the `Input` struct
// annotations) with the field types from packages/core/src/filesystem.ts
// (GlobInput/GrepInput) and tool/read-filesystem.ts (PageInput).
//
// KNOWN DRIFT: these track an upstream release and change with it. The gate
// reads schemas, so a release that reshapes these can start refusing requests
// that were accepted before — re-read the sources at the new tag and update
// here. The field types are spelled as JSON Schema; the client derives them
// from Effect Schema at serialize time.
var FingerprintToolSchemas = map[string]FingerprintTool{
	"shell": {
		Description: "Execute a shell command and return its output. " +
			"Quote file paths containing spaces or special characters. " +
			"Prefer dedicated tools over shell commands when possible. " +
			"When output is large, the full result is saved to a file and a truncated preview is returned. " +
			"Rely on automatic truncation unless filtering the output is more useful. " +
			"Commands accept an optional timeout, background commands have no timeout by default. " +
			"Background commands return immediately, and you will be notified when they complete.",
		Properties: map[string]any{
			"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
			"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
			"timeout":    map[string]any{"type": "integer", "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
			"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
		},
		Required: []string{"command"},
	},
	"glob": {
		Description: `Search file paths using a glob pattern (examples: "**/*.ts", "src/**/*.tsx").`,
		Properties: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern to match files against"},
			"path":    map[string]any{"type": "string", "description": "Directory to search. Defaults to the current working directory."},
			"hidden":  map[string]any{"type": "boolean", "description": "Include hidden files and directories (default: false)."},
			"limit":   map[string]any{"type": "integer", "description": "Maximum number of matching files to return (default: 100)"},
		},
		Required: []string{"pattern"},
	},
	"grep": {
		Description: "Search file contents using ripgrep's regular expression syntax or literal text matching. " +
			"Use it to locate specific code, symbols, or text patterns, and narrow searches with `path` or `include`. " +
			"Returns matching file paths, line numbers, and line previews.",
		Properties: map[string]any{
			"pattern":       map[string]any{"type": "string", "description": "Regular expression or literal text to match in file contents."},
			"path":          map[string]any{"type": "string", "description": "File or directory to search. Defaults to the current working directory."},
			"include":       map[string]any{"type": "string", "description": `Glob pattern to filter files (for example, "*.js" or "*.{ts,tsx}")`},
			"literal":       map[string]any{"type": "boolean", "description": "Treat `pattern` as exact text instead of a regular expression (default: false)."},
			"caseSensitive": map[string]any{"type": "boolean", "description": "Use case-sensitive matching (default: true)."},
			"limit":         map[string]any{"type": "integer", "description": "Maximum number of matching lines to return (default: 100)"},
		},
		Required: []string{"pattern"},
	},
	"read": {
		Description: "Read the contents of a file or directory. Supports text files, images, and PDFs. " +
			"Images and PDFs are presented directly to the model. " +
			"Each text line is prefixed by its 1-based line number as <line>: <content>. " +
			"The prefix is for reference and is not part of the file content. " +
			"Directory entries are returned one per line. " +
			"Use offset and limit to read large files or directories in sections. " +
			"Prefer one larger read over many small slices, and use grep to find specific content in large files.",
		Properties: map[string]any{
			"path":   map[string]any{"type": "string", "description": "File or directory to read"},
			"offset": map[string]any{"type": "integer", "description": "The line or directory entry to start reading from (1-based)"},
			"limit":  map[string]any{"type": "integer", "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
		},
		Required: []string{"path"},
	},
}

// Wire limits (Responses API).
const (
	MaxToolNameLen     = 128
	MaxResponsesCallID = 64
	MaxSessionLength   = 256
	MinMaxOutputTokens = 16 // upstream 400s below this ("The number must be `>= 16`")
)

// Caller correlation id (issue #83). ONE fixed, always-on inbound header, read
// and LOGGED as a separate `caller_request_id` field beside this process's own
// `request_id` — never derived from it, never substituted for it, never a
// decision input and never written to the response. It is a diagnostic, not
// policy, so it carries no config knob: a configurable header name would be a
// hot-reloadable knob with exactly one correct value.
//
// The header is deliberately NOT an X-OFP-* name (issue #77 retired that
// namespace) and NOT one of the names the session resolver consumes
// (identity/sessionresolver.go) or the executor forges
// (upstream/executor.go x-opencode-request-id). It is not in captureDownstream's
// forwarding allow-list either, so it is never sent upstream and cannot reach
// session stickiness, prompt caching or the session_fp pseudonym.
const (
	// CallerRequestIDHeader is the single inbound header read for a
	// caller-supplied correlation id.
	CallerRequestIDHeader = "X-Request-Id"
	// CallerRequestIDPattern is the accept-or-ignore charset: 1..128 bytes of
	// [A-Za-z0-9._:-], anchored. It excludes CR, LF, NUL, quote and backslash
	// by construction, so an ACCEPTED value can never forge a log line, and it
	// bounds both cardinality and length. A value outside it is DROPPED, never
	// folded or truncated: a rewritten join key fails silently, whereas a
	// missing field leaves this process's own request_id to degrade the join to
	// "same service, same time" rather than to a wrong id.
	CallerRequestIDPattern = `^[A-Za-z0-9._:-]{1,128}$`
	// MaxCallerRequestIDLen documents the bound the pattern already encodes; it
	// is the number a caller is told about, and the length a test asserts.
	MaxCallerRequestIDLen = 128
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
// A duration variable that is present but malformed is an error, never a
// silent default (issue #86): the caller must not start a process on a value
// the operator never chose. Errors are joined so one boot reports every
// malformed variable, and they name the key and the value's LENGTH only — an
// env var can carry a secret and neither ParseDuration's error text nor this
// function's ever quote it.
type Config struct {
	Port string
	// ConfigPath is OCFP_CONFIG: the routing + service config file. Empty =
	// the built-in default runtime.
	ConfigPath string
	// ShutdownGrace is OCFP_SHUTDOWN_GRACE: how long draining waits for
	// active requests/streams before forced close. Accepts milliseconds
	// ("55000") or a duration literal ("55s", "350s").
	ShutdownGrace time.Duration
	// ConfigPoll is OCFP_CONFIG_POLL_MS: the hot-reload poll interval.
	// Accepts milliseconds ("1000") or a duration literal ("1s").
	ConfigPoll time.Duration
}

// DefaultShutdownGrace is used when OCFP_SHUTDOWN_GRACE is unset.
const DefaultShutdownGrace = 55 * time.Second

// DefaultConfigPoll is the hot-reload poll interval (the rotation-proxy
// gateway design polled at 1 s; repeated writes coalesce).
const DefaultConfigPoll = time.Second

func FromEnv() (*Config, error) {
	grace, graceErr := envDuration("OCFP_SHUTDOWN_GRACE", DefaultShutdownGrace)
	poll, pollErr := envDuration("OCFP_CONFIG_POLL_MS", DefaultConfigPoll)
	return &Config{
		Port:       envOr("OCFP_PORT", DefaultPort),
		ConfigPath: os.Getenv("OCFP_CONFIG"),
		// OCFP_SHUTDOWN_GRACE and OCFP_CONFIG_POLL_MS each accept EITHER
		// form envDuration understands: a bare integer in milliseconds (the
		// documented contract, and what the shipped compose file and every doc
		// table use), or a Go duration literal itself. A value matching
		// neither is a startup error, not a silent fallback — see
		// envDuration and issue #86.
		ShutdownGrace: grace,
		ConfigPoll:    poll,
	}, errors.Join(graceErr, pollErr)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration reads one duration-valued environment variable in EITHER of
// the two forms operators actually write:
//
//   - a bare positive integer, in MILLISECONDS — the long-documented contract
//     for every OCFP_ ms sibling (OCFP_CONFIG_POLL_MS, and OCFP_SHUTDOWN_GRACE
//     as shipped in compose.yaml and every doc table);
//   - a Go duration literal ("350s", "1m30s", "500ms") — the natural spelling
//     for a variable named *_GRACE, and the only form the sister service
//     openai-compatible-injector accepts for its identically named
//     OAICR_SHUTDOWN_GRACE. Two org services reading the same-shaped variable
//     under two different syntaxes is itself a defect (issue #86).
//
// The integer form is tried FIRST, so every value that parsed under the old
// envMs parses to exactly the same duration now; nothing that worked before
// changes.
//
// Neither form matching, or matching with a non-positive result, is a
// STARTUP ERROR, never a silent fallback to def. The old envMs returned def
// on anything unparseable with no signal at all, which is what let a
// production OCFP_SHUTDOWN_GRACE=350s run on the 55s default for every start
// (issue #86). The failure is one-directional and always toward the shorter,
// more dangerous value: an operator who asked for a long drain got a short
// one and force-closed live streams, believing the opposite. Silent fallback
// on a malformed value is the defect; refusing to boot is the fix.
//
// The value is NEVER echoed, only its length: an env var can carry a secret
// and time.ParseDuration's own error text quotes its input verbatim, so the
// message carries the length and an example spelling instead. Errors are
// returned, not logged, so FromEnv stays a pure reader and cmd/server decides
// how to fail (log.Fatal, matching every other bootstrap failure).
func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Millisecond, nil
		}
		return def, fmt.Errorf("%s is not a positive number of milliseconds (%d characters)", key, len(raw))
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d > 0 {
			return d, nil
		}
		return def, fmt.Errorf("%s is not a positive duration (%d characters)", key, len(raw))
	}
	return def, fmt.Errorf("%s is not a positive millisecond integer and not a positive duration (%d characters; e.g. 55000, 350s, 1m30s)", key, len(raw))
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

	// SecondaryReadTimeout bounds the total time one SECONDARY body read may
	// spend reading an already-received response: the terminal error-envelope
	// read, the redirect drain, and the non-SSE guard. Each is a fixed-size
	// read off a response whose headers have already arrived — the transport's
	// response-HEADER timeout (ConnectTimeout) is spent and bounds nothing
	// further, so a peer that streams bytes forever below the byte cap would
	// pin the goroutine outright. This is a TOTAL bound, not a progress-reset
	// stall: these reads are bounded at maxErrorBodyBytes/maxNonSSEBodyBytes
	// anyway and the bytes are secondary (an error envelope, a discarded
	// redirect body, a short HTML page), so a peer impossible to finish within
	// the window buys nothing more. Distinct from the SSE product read
	// (ScanLines), which deliberately resets its stall on ANY progress — a
	// slow-but-live stream is the product there. Go-side hardening with no JS
	// counterpart (utils/error.js:61 reads error bodies unbounded). 10 s is a
	// dial-peer cadence: a real secondary body is a handful of KiB read in
	// milliseconds, so this fires only against a stalled or hostile peer.
	SecondaryReadTimeout = 10 * time.Second
)
