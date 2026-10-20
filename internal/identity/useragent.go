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
// for the free-tier gate (>= 1.18 — below that upstream answers 426).
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

// UAStyle and its constants alias the config-owned type (AGENTS.md rule 3:
// runtime constants live only in internal/config); identity renders the
// forms, config names them.
type UAStyle = config.UAStyle

const (
	UAStyleV1 = config.UAStyleV1
	UAStyleV2 = config.UAStyleV2
)

// FallbackV2 is the compiled-in v2 wire form. The version segment is the
// v2-declared fallback tag (config.ClientFallbackVersionV2), NOT the v1
// triple's tag: the 2.x channel form never existed in the 1.x generation, so
// pairing "opencode/latest" with a v1 version would be an identity no real
// client ever sent (F1, adversarial review 2026-10-20) — the same
// never-mixed rule the UADual contract enforces on synced values.
func FallbackV2() string {
	return "opencode/latest/" + config.ClientFallbackVersionV2 + "/cli"
}

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
// BOTH wire forms atomically (UADual — v1 never mixes across releases, and
// v2 lands only together with v1); any failure keeps the previous value —
// or the compiled-in defaults while cold — and advances the cache clock so
// a broken network doesn't hammer GitHub.
//
// Sync is DECOUPLED from the request hot path: Warm runs once at startup and
// then on a ticker (StartSync). Get is a pure read. This deliberately
// diverges from opencodeClientVersion.js (9router warms lazily per request,
// executors/opencode.js:376) — with the ticker at config.UASyncInterval the
// cache is at most one interval stale, so requests never block on GitHub.
type UserAgentCache struct {
	mu sync.Mutex
	// style is read LIVE per request so a config reload can flip the wire
	// form without touching the cache (or vice versa).
	style    func() UAStyle
	inflight bool
	dual     UADual // zero until the first successful sync
	cachedAt time.Time
	now      func() time.Time
}

// UADual is one sync round's result, form by form: the v1 compound triple
// and the v2 channel/version/name string. The pointers make per-form
// degradation explicit — nil = that form's lookup failed this round and the
// caller keeps its previous value; the form that succeeded still advances.
type UADual struct {
	V1 *UATriple
	V2 *string
}

func NewUserAgentCache() *UserAgentCache {
	return &UserAgentCache{now: time.Now, style: func() UAStyle { return UAStyleV1 }}
}

// SetStyleProvider installs the live UAStyle source (typically
// reading config.Runtime.UAStyle()). A nil provider keeps v1. Hot-reload
// friendly: the func is called on every Get(), so a config swap flips the
// wire form without touching the cache.
//
// The provider is invoked while the cache mutex is held (from Get/Style/Warm
// via uaLocked) — it must NOT call back into the cache (Get/Style/Warm
// deadlock: sync.Mutex is not reentrant). The store-backed closure main.go
// installs only reads an atomic pointer and is safe.
func (c *UserAgentCache) SetStyleProvider(fn func() UAStyle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		c.style = func() UAStyle { return UAStyleV1 }
		return
	}
	c.style = fn
}

// Style returns the currently selected wire form (v1 or v2).
func (c *UserAgentCache) Style() UAStyle {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.style == nil {
		return UAStyleV1
	}
	return c.style()
}

// FallbackUA returns the pinned identity used while the cache is cold.
func FallbackUA() string { return FallbackTriple().Compose() }

// Get returns the current best User-Agent without triggering network I/O —
// in the STYLE the live provider selects (v1 compound or v2 channel form).
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
// (time.NewTicker would panic). The interval is re-read on EVERY cycle — a
// reload may have changed the configured OCFP_CONFIG user_agent.sync_interval
// — so the loop follows the live store snapshot instead of a captured
// cadence. Re-arm is a read, never
// a second goroutine: a second StartSync would still run a second loop, which
// is why main.go calls StartSync exactly once. A non-positive read at any
// cycle idles on a 1 s re-check that never fetches (the compile-in fallback
// stays), and a later positive read resumes the fetch cadence in place — no
// supervisor, no stop/start race. Startup itself warms once before this loop.
func (c *UserAgentCache) StartSync(client *http.Client, interval func() time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			want := interval()
			if want <= 0 {
				// Disabled sync: idle on a 1 s re-check (one wakeup/second,
				// no network, no fetch) so a reload that re-enables sync is
				// picked up by the NEXT cycle — the loop follows the
				// snapshot, never a supervisor.
				select {
				case <-done:
					return
				case <-time.After(time.Second):
				}
				continue
			}
			timer := time.NewTimer(want)
			select {
			case <-done:
				timer.Stop()
				return
			case <-timer.C:
				ua := c.Warm(client, true) // force: the sync loop bypasses the TTL gate
				_ = ua                     // Warm logs nothing; failure is fail-open by design
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Warm syncs the UA identity from upstream sources, BOTH wire forms in one
// round, each form degrading independently on failure (see fetchDual):
//
//  1. GitHub releases API → opencode version for the v1 triple
//     (opencodeClientVersion.js:41-59); GitHub tags API → the newest v2.*
//     tag for the v2 form. Either lookup failing keeps ONLY that form's
//     previous value — v1 and v2 never propagate each other's failures
//     (adversarial review F2/F3 2026-10-20).
//  2. raw.githubusercontent.com at the v1 tag, root package.json →
//     `packageManager` Bun pin → runtime segment.
//  3. bun.lock at the v1 tag → the provider-utils resolution of
//     packages/opencode's @ai-sdk/openai-compatible chain → ai-sdk segment.
//
// force skips the TTL gate (the sync ticker passes true). Concurrent callers
// are deduplicated (single-flight). Fail-open per form: on that form's error
// the previously cached value (or the compiled-in default) stays and
// cachedAt advances — a form never regresses to an older successful value,
// and one form's failure never resets the other's clock.
func (c *UserAgentCache) Warm(client *http.Client, force bool) string {
	c.mu.Lock()
	now := c.now()
	// The TTL gate keys on V1 only, and that leans on the per-form
	// degradation invariant: V2 is populated iff V1 is (both are written
	// only by a Warm that ran fetchDual, past the fetches, under this same
	// mutex) — so a V2-only check would be redundant today. If the invariant
	// is ever broken (per-form sync cadences, for example), this gate needs
	// a V2 branch of its own. (Adversarial review F3, 2026-10-20.)
	if !force && c.dual.V1 != nil && now.Sub(c.cachedAt) < config.VersionCacheTTL {
		c.mu.Unlock()
		return c.uaLocked()
	}
	if c.inflight {
		c.mu.Unlock()
		return c.Get()
	}
	c.inflight = true
	c.mu.Unlock()

	dual := fetchDual(client)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight = false
	c.cachedAt = c.now()
	if dual.V1 != nil {
		c.dual.V1 = dual.V1
	}
	if dual.V2 != nil {
		c.dual.V2 = dual.V2
	}
	return c.uaLocked()
}

// uaLocked renders the full User-Agent in the configured STYLE; caller must
// hold c.mu. (Not c.Get() — sync.Mutex is not reentrant, and c.style() is
// invoked under this lock: the installed provider must never call back into
// the cache. SetStyleProvider documents the same condition.) Each form has
// its own compiled-in fallback so a selection is never an empty string even
// before the first successful sync of that form.
func (c *UserAgentCache) uaLocked() string {
	style := UAStyleV1
	if c.style != nil {
		style = c.style()
	}
	if style == UAStyleV2 {
		if c.dual.V2 != nil {
			return *c.dual.V2
		}
		return FallbackV2()
	}
	if c.dual.V1 != nil {
		return c.dual.V1.Compose()
	}
	return FallbackUA()
}

// fetchDual resolves BOTH wire forms in one probe round, each form DEGRADING
// SEPARATELY: a nil form means that form's lookup failed and Warm keeps its
// previous value (or the compiled-in default while cold) while the other
// form still advances. The two lookups ride different endpoints with
// different failure modes (the v2 tags probe failing must never freeze the
// v1 refresh — adversarial review F2/F3 2026-10-20), while WITHIN each form
// the never-mixed rule holds: the v1 triple is all-or-nothing across its
// three segments, and the v2 string is a single fresh value. A form pointer
// is non-nil only on that form's complete success, so a stored dual can
// never mix values across sync rounds within a form either.
func fetchDual(client *http.Client) (dual UADual) {
	if t, err := fetchTriple(client); err == nil {
		dual.V1 = &t
	}
	if v, err := fetchV2Version(client); err == nil {
		dual.V2 = &v
	}
	return dual
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

// v2TagRe pulls a semver from a v2.* tag ("v2.1.0" → "2.1.0"). Anchored at
// the end so a prerelease tag "v2.0.1-rc1" is rejected whole (adversarial
// review F4) rather than silently truncated to 2.0.1 — the tags API carries
// no prerelease flag to filter on.
var v2TagRe = regexp.MustCompile(`^v?(2\.\d+\.\d+)$`)

// fetchV2Version resolves the newest v2.* tag and renders the wire form the
// official 2.x CLI sends: `opencode/<channel>/<version>/<name>`
// (packages/core/src/app.ts at v2.1.0: useragent(app) joins channel,
// version and name with "/"). The CLI's release install reports channel
// "latest" and name "cli" — the values captured on the wire 2026-10-13
// (opencode/latest/2.0.27/cli).
//
// The probe reads the TAGS list, not the releases list
// (verified directly 2026-10-20): the repo's releases all carry 1.x tags —
// zero v2 releases exist anywhere in the releases pagination — while stable
// v2 tags (v2.0.0 … v2.1.0) exist only as git tags. A releases-list scan
// would fail with errNoV2Tag on every warm; /releases/latest handshakes are
// v1-only too. Tag names are ordered newest-first by the API, so the scan
// takes the FIRST v2-shaped match. There is no prerelease flag on tags, so
// the fully-anchored pattern is the only filter — a "v2.1.0-rc1" tag would
// be skipped whole, never truncated.
func fetchV2Version(client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodGet, config.GitHubTagsListURL, nil)
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
	var body []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return "", err
	}
	for _, r := range body {
		if m := v2TagRe.FindStringSubmatch(r.Name); m != nil {
			return "opencode/latest/" + m[1] + "/cli", nil
		}
	}
	return "", errNoV2Tag
}

var errNoV2Tag = errString("no stable v2 tag in the GitHub tags list")

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

var errNoSemver = errString("github release tag carried no semver")

type errString string

func (e errString) Error() string { return string(e) }
