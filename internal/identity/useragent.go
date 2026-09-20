package identity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"opencode-free-proxy/internal/config"
)

// UARe extracts `opencode/<major>.<minor>[.patch]` from a User-Agent.
var UARe = regexp.MustCompile(`(?i)opencode/(\d+)\.(\d+)(?:\.(\d+))?`)

// TagRe pulls a semver from a GitHub release tag like "v1.19.2".
var TagRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)`)

// HasValidVersion reports whether ua identifies an opencode client new enough
// for the free-tier gate (>= 1.17 — below that upstream answers 426/403).
// A bare "opencode" UA has no version and fails the check.
func HasValidVersion(ua string) bool {
	m := UARe.FindStringSubmatch(ua)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > config.ClientMinMajor ||
		(major == config.ClientMinMajor && minor >= config.ClientMinMinor)
}

// UATriple is the set of versions the official CLI compounds into its
// User-Agent (docs/recon-opencode-ua.md has the full source trace):
//
//		opencode/<Opencode> ai-sdk/provider-utils/<ProviderUtils> runtime/bun/<Bun>
//
//	  - Opencode: release version — session/llm/request.ts:18
//	    (`opencode/${InstallationVersion}`), attached to every request on a
//	    provider whose id starts with "opencode" (request.ts:187-204).
//	  - ProviderUtils: @ai-sdk/provider-utils as resolved for packages/opencode's
//	    @ai-sdk/openai-compatible dependency — that package performs the zen
//	    fetch, and its provider-utils copy stamps the `ai-sdk/provider-utils/<v>`
//	    suffix inside postJsonToApi.
//	  - Bun: the bundled Bun build runtime = the repo's root package.json
//	    `packageManager` pin at the same tag.
type UATriple struct {
	Opencode      string
	ProviderUtils string
	Bun           string
}

// Compose renders the wire form. The CLI appends the ai-sdk suffix to the
// base UA with single spaces (withUserAgentSuffix joins the existing
// user-agent header and the suffix with " ").
func (t UATriple) Compose() string {
	return fmt.Sprintf("opencode/%s ai-sdk/provider-utils/%s runtime/bun/%s",
		t.Opencode, t.ProviderUtils, t.Bun)
}

// FallbackTriple is the compiled-in default triple — the chosen default tag
// (config documents the source of each segment). Used until the first
// successful sync and whenever sync fails while the cache is cold (fail-open,
// mirroring opencodeClientVersion.js fallbackUserAgent).
func FallbackTriple() UATriple {
	return UATriple{
		Opencode:      config.ClientFallbackVersion,
		ProviderUtils: config.ClientFallbackProviderUtils,
		Bun:           config.ClientFallbackBun,
	}
}

// BuildUA renders a UA from an explicit triple (tests, warm logging).
func BuildUA(t UATriple) string { return t.Compose() }

// bun.lock resolution extraction. The lockfile keys install paths relative to
// the root node_modules; the zen fetch chain is
// `opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils` (captured live:
// the official v1.18.31 binary sends provider-utils 4.0.23 and this is the
// only chain matching it). bun.lock is JSONC-flavored, so extraction is
// regex-based rather than a full JSON parse.
var (
	// Primary: the exact opencode-workspace chain.
	puReExact = regexp.MustCompile(`"opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils":\s*\[\s*"@ai-sdk/provider-utils@(\d+\.\d+\.\d+)"`)
	// Fallback: any scoped copy of the same chain (survives a workspace
	// rename). Multiple matches with different versions resolve to the first.
	puReAny = regexp.MustCompile(`"[^"]*@ai-sdk/openai-compatible/@ai-sdk/provider-utils":\s*\[\s*"@ai-sdk/provider-utils@(\d+\.\d+\.\d+)"`)
)

// packageManagerRe extracts the Bun pin from the root package.json
// (`"packageManager": "bun@1.3.14"` — may carry a +sha512 suffix).
var packageManagerRe = regexp.MustCompile(`"packageManager"\s*:\s*"bun@(\d+\.\d+\.\d+)`)

// UserAgentCache is the fail-open identity probe. One successful sync stores
// the full triple atomically (never a mixed triple); any failure keeps the
// previous value — or the compiled-in default while cold — and advances the
// cache clock so a broken network doesn't hammer GitHub.
//
// Sync is DECOUPLED from the request hot path: Warm runs once at startup and
// then on a ticker (StartSync). Get is a pure read. This deliberately
// diverges from opencodeClientVersion.js (9router warms lazily per request,
// executors/opencode.js:376) — with the ticker at config.UASyncInterval the
// cache is at most one interval stale, so requests never block on GitHub.
type UserAgentCache struct {
	mu       sync.Mutex
	inflight bool
	triple   UATriple // zero until the first successful sync
	cachedAt time.Time
	now      func() time.Time
}

func NewUserAgentCache() *UserAgentCache {
	return &UserAgentCache{now: time.Now}
}

// FallbackUA returns the pinned identity used while the cache is cold.
func FallbackUA() string { return FallbackTriple().Compose() }

// Get returns the current best User-Agent without triggering network I/O.
func (c *UserAgentCache) Get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uaLocked()
}

// StartSync launches the background refresh loop (one Warm per interval,
// forced). The loop is LAZY and explicit: constructing the cache spawns
// nothing — the goroutine exists only between StartSync and its stop, so a
// cache that is never started (tests, the healthcheck subcommand) has zero
// sync goroutines. interval <= 0 starts nothing and returns a no-op stop
// (time.NewTicker would panic) — a misconfigured cadence degrades to the
// compiled-in fallback triple instead of crashing. The returned stop is
// idempotent and safe to call any number of times, before any sync has run
// or after the loop already exited; a second StartSync on the same cache
// runs a second loop (the single-flight Warm dedupe keeps them from
// stampeding GitHub), so callers should start once and keep the stop.
// Startup itself warms once before/alongside this loop.
func (c *UserAgentCache) StartSync(client *http.Client, interval time.Duration) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ua := c.Warm(client, true)
				_ = ua // Warm logs nothing; failure is fail-open by design
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Warm syncs the UA triple from upstream sources:
//
//  1. GitHub releases API → opencode version (opencodeClientVersion.js:41-59).
//  2. raw.githubusercontent.com at that tag, root package.json → `packageManager`
//     Bun pin → runtime segment.
//  3. bun.lock at that tag → the provider-utils resolution of packages/opencode's
//     @ai-sdk/openai-compatible chain → ai-sdk segment.
//
// force skips the TTL gate (the sync ticker passes true). Concurrent callers
// are deduplicated (single-flight). Fail-open: on any error the previously
// cached triple (or the compiled-in default) stays and cachedAt advances.
func (c *UserAgentCache) Warm(client *http.Client, force bool) string {
	c.mu.Lock()
	now := c.now()
	if !force && c.triple.Opencode != "" && now.Sub(c.cachedAt) < config.VersionCacheTTL {
		c.mu.Unlock()
		return c.uaLocked()
	}
	if c.inflight {
		c.mu.Unlock()
		return c.Get()
	}
	c.inflight = true
	c.mu.Unlock()

	triple, err := fetchTriple(client)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight = false
	c.cachedAt = c.now()
	if err == nil {
		c.triple = triple
	}
	return c.uaLocked()
}

// uaLocked renders the full User-Agent; caller must hold c.mu. (Not c.Get() —
// sync.Mutex is not reentrant.)
func (c *UserAgentCache) uaLocked() string {
	if c.triple.Opencode != "" {
		return c.triple.Compose()
	}
	return FallbackUA()
}

// fetchTriple resolves all three segments; all-or-nothing so the cache never
// holds a triple mixed across releases.
func fetchTriple(client *http.Client) (UATriple, error) {
	version, err := fetchLatestRelease(client)
	if err != nil {
		return UATriple{}, err
	}
	bun, err := fetchBunVersion(client, version)
	if err != nil {
		return UATriple{}, err
	}
	pu, err := fetchProviderUtilsVersion(client, version)
	if err != nil {
		return UATriple{}, err
	}
	return UATriple{Opencode: version, ProviderUtils: pu, Bun: bun}, nil
}

// fetchBunVersion reads the root package.json at the probed tag and pulls the
// `packageManager` Bun pin (the version the release binaries bundle).
func fetchBunVersion(client *http.Client, tag string) (string, error) {
	raw, err := fetchRaw(client, tag, config.RootPackageJSONPath)
	if err != nil {
		return "", err
	}
	m := packageManagerRe.FindSubmatch(raw)
	if m == nil {
		return "", errString("root package.json at " + tag + " has no bun packageManager pin")
	}
	return string(m[1]), nil
}

// fetchProviderUtilsVersion reads bun.lock at the probed tag and resolves the
// provider-utils version of the openai-compatible chain (see puReExact).
func fetchProviderUtilsVersion(client *http.Client, tag string) (string, error) {
	raw, err := fetchRaw(client, tag, config.LockfilePath)
	if err != nil {
		return "", err
	}
	if m := puReExact.FindSubmatch(raw); m != nil {
		return string(m[1]), nil
	}
	if m := puReAny.FindSubmatch(raw); m != nil {
		return string(m[1]), nil
	}
	return "", errString("bun.lock at " + tag + " has no openai-compatible provider-utils resolution")
}

// fetchRaw GETs a file from the opencode repo at a tag ref.
func fetchRaw(client *http.Client, tag, path string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := config.GitHubRawBase + "/v" + tag + "/" + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "opencode-free-proxy-ua-sync")
	httpClient := *client
	httpClient.Timeout = 15 * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode}
	}
	// bun.lock is ~700KB, package.json a few KB — bound the read anyway.
	const maxRaw = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRaw+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRaw {
		return nil, errString(url + " exceeds the raw-fetch size bound")
	}
	return body, nil
}

func fetchLatestRelease(client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodGet, config.GitHubReleasesURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "opencode-free-proxy-version-probe")
	httpClient := *client
	httpClient.Timeout = 8 * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{code: resp.StatusCode}
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	m := TagRe.FindStringSubmatch(body.TagName)
	if m == nil {
		return "", errNoSemver
	}
	return m[1], nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

var errNoSemver = errString("github release tag carried no semver")

type errString string

func (e errString) Error() string { return string(e) }
