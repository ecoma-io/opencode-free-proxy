// User-Agent gate + compound-UA triple sync tests — ported from 9router
// tests/unit/opencode-client-version.test.js and
// open-sse/executors/opencode.js hasValidOpencodeVersion /
// open-sse/utils/opencodeClientVersion.js (extended for the full
// opencode + provider-utils + bun triple synced from GitHub, see
// docs/recon-opencode-ua.md).
package identity

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// TestHasValidVersion pins the free-tier version gate: opencode >= 1.17.0
// passes, older/bare/non-opencode UAs fail. The regex is unanchored and
// case-insensitive, the patch segment optional (JS /opencode\/(\d+)\.(\d+)
// (?:\.(\d+))?/i).
func TestHasValidVersion(t *testing.T) {
	cases := []struct {
		ua   string
		want bool
		note string
	}{
		{"", false, "empty UA"},
		{"opencode/1.16.0", false, "below the 1.18 gate"},
		{"opencode/1.15.0", false, "golden outdated version"},
		{"opencode/1.16.99", false, "just below the older gate"},
		{"opencode/0.9.4", false, "ancient"},
		{"opencode/1.17.0", false, "below the 1.18 gate (426 upstream)"},
		{"opencode/1.17", false, "patch segment optional, still below"},
		{"opencode/1.17.99", false, "just below the gate"},
		{"opencode/1.18.0", true, "exactly at the gate"},
		{"opencode/1.18", true, "patch segment optional"},
		{"opencode/1.18.31", true, "pinned fallback version"},
		{"opencode/1.19.0", true, "newer 1.x"},
		{"opencode/2.0.1", true, "future major"},
		{"opencode", false, "bare opencode UA has no version"},
		{"Mozilla/5.0", false, "non-opencode UA"},
		{"Claude-Code/1.0", false, "other client"},
		{"OpenCode/1.19.0", true, "case-insensitive product token"},
		{"opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14", true, "golden full opencode UA"},
		{"Mozilla/5.0 (Macintosh) opencode/1.18.0 something", true, "regex is unanchored"},
		{"xopencode/9.9.9", true, "JS regex has no word boundary before opencode"},
		{"opencode/1", false, "minor segment missing → no match"},
	}
	for _, tc := range cases {
		if got := HasValidVersion(tc.ua); got != tc.want {
			t.Errorf("HasValidVersion(%q) [%s] = %v, want %v", tc.ua, tc.note, got, tc.want)
		}
	}
}

// The compiled-in default triple must match the official CLI byte for byte
// (captured live from the v1.18.31 binary, docs/recon-opencode-ua.md):
// "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14".
func TestFallbackUA(t *testing.T) {
	want := "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if got := FallbackUA(); got != want {
		t.Errorf("FallbackUA() = %q, want %q", got, want)
	}
	if got := FallbackTriple().Compose(); got != want {
		t.Errorf("FallbackTriple().Compose() = %q, want %q", got, want)
	}
	if got := BuildUA(UATriple{Opencode: "2.0.1", ProviderUtils: "5.5.5", Bun: "9.9.9"}); got != "opencode/2.0.1 ai-sdk/provider-utils/5.5.5 runtime/bun/9.9.9" {
		t.Errorf("BuildUA(triple) = %q", got)
	}
}

// roundTripFunc is a stub transport so the probe never touches the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// syncStub fixtures: the three sources a Warm fetches for opencode/2.0.1.
const (
	stubLock = `{
  "packages": {
    "@ai-sdk/openai-compatible": ["@ai-sdk/openai-compatible@2.0.41", "", { "dependencies": { "@ai-sdk/provider-utils": "4.0.21" } }, "x"],
    "opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils": ["@ai-sdk/provider-utils@5.5.5", "", { "dependencies": {} }, "y"],
    "opencode/@ai-sdk/openai/@ai-sdk/provider-utils": ["@ai-sdk/provider-utils@7.7.7", "", { "dependencies": {} }, "z"]
  }
}`
	stubPkgJSON = `{"name":"opencode","packageManager":"bun@9.9.9+sha512.abcdef"}`
)

// stubLockRenamed is the same shape without the exact "opencode/" workspace
// prefix — the puReAny fallback must still resolve the openai-compatible copy.
var stubLockRenamed = strings.Replace(stubLock,
	"opencode/@ai-sdk/openai-compatible/@ai-sdk/provider-utils",
	"cli/@ai-sdk/openai-compatible/@ai-sdk/provider-utils", 1)

// uaSyncTransport serves the releases API + raw tag files. Handlers are
// swappable per-test to drive failure modes.
type uaSyncTransport struct {
	mu         sync.Mutex
	release    string // body for the releases/latest API
	tagsList   string // body for the tags list API backing v2 sync
	pkgJSON    string
	lock       string
	pkgStatus  int
	lockStatus int
	requests   []string
}

func newUASyncTransport() *uaSyncTransport {
	return &uaSyncTransport{
		release:    `{"tag_name":"v2.0.1"}`,
		tagsList:   `[{"name":"v2.0.1"},{"name":"v1.19.3"}]`,
		pkgJSON:    stubPkgJSON,
		lock:       stubLock,
		pkgStatus:  http.StatusOK,
		lockStatus: http.StatusOK,
	}
}

func (s *uaSyncTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.URL.String())
	switch r.URL.Host {
	case "api.github.com":
		if r.URL.String() == config.GitHubTagsListURL {
			return jsonResponse(http.StatusOK, s.tagsList), nil
		}
		return jsonResponse(http.StatusOK, s.release), nil
	default: // raw.githubusercontent.com
		if strings.HasSuffix(r.URL.Path, "/package.json") {
			return jsonResponse(s.pkgStatus, s.pkgJSON), nil
		}
		return jsonResponse(s.lockStatus, s.lock), nil
	}
}

func (s *uaSyncTransport) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

const stubUA = "opencode/2.0.1 ai-sdk/provider-utils/5.5.5 runtime/bun/9.9.9"

// The full sync chain resolves the v1 triple from the right URLs — and
// fetches the v2 form in the SAME pass (the dual contract: both wire forms
// are synced, never one without the other).
func TestWarmFetchesFullTriple(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}
	c := NewUserAgentCache()

	if got := c.Warm(client, true); got != stubUA {
		t.Errorf("Warm() = %q, want %q", got, stubUA)
	}
	if got := c.Get(); got != stubUA {
		t.Errorf("Get() = %q, want cached %q", got, stubUA)
	}
	wantURLs := []string{
		config.GitHubReleasesURL,
		config.GitHubRawBase + "/v2.0.1/" + config.RootPackageJSONPath,
		config.GitHubRawBase + "/v2.0.1/" + config.LockfilePath,
		config.GitHubTagsListURL,
	}
	if tr.calls() != len(wantURLs) {
		t.Fatalf("requests = %v, want exactly %v", tr.requests, wantURLs)
	}
	for i, want := range wantURLs {
		if tr.requests[i] != want {
			t.Errorf("request[%d] = %q, want %q", i, tr.requests[i], want)
		}
	}
	// The v2 form from the same pass must be cached beside the v1 triple:
	// select v2 and read (Get never I/Os).
	c.SetStyleProvider(func() UAStyle { return UAStyleV2 })
	if got, want := c.Get(), "opencode/latest/2.0.1/cli"; got != want {
		t.Errorf("v2 Get() after warm = %q, want %q", got, want)
	}
	c.SetStyleProvider(func() UAStyle { return UAStyleV1 })
	if got, want := FallbackV2(), "opencode/latest/"+config.ClientFallbackVersionV2+"/cli"; got != want {
		t.Errorf("FallbackV2() = %q, want %q", got, want)
	}
}

// Extraction units: the exact bun.lock key wins over the @ai-sdk/openai copy;
// a renamed workspace falls back to any openai-compatible-scoped resolution;
// the packageManager pin tolerates a +sha512 suffix; malformed inputs error.
func TestUASourceExtraction(t *testing.T) {
	if m := puReExact.FindStringSubmatch(stubLock); m == nil || m[1] != "5.5.5" {
		t.Errorf("puReExact on stub lock = %v, want 5.5.5", m)
	}
	if m := puReExact.FindStringSubmatch(stubLockRenamed); m != nil {
		t.Errorf("puReExact must not match a renamed workspace, got %v", m)
	}
	if m := puReAny.FindStringSubmatch(stubLockRenamed); m == nil || m[1] != "5.5.5" {
		t.Errorf("puReAny on renamed lock = %v, want 5.5.5", m)
	}
	if m := packageManagerRe.FindStringSubmatch(stubPkgJSON); m == nil || m[1] != "9.9.9" {
		t.Errorf("packageManagerRe = %v, want 9.9.9", m)
	}
	if m := packageManagerRe.FindStringSubmatch(`{"packageManager": "bun@1.3.14"}`); m == nil || m[1] != "1.3.14" {
		t.Errorf("packageManagerRe (no hash) = %v, want 1.3.14", m)
	}
	if packageManagerRe.FindStringSubmatch(`{"packageManager":"pnpm@9.0.0"}`) != nil {
		t.Error("packageManagerRe must reject non-bun pins")
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"no":"pin"}`), nil
	})}
	if _, err := fetchBunVersion(client, "2.0.1"); err == nil {
		t.Error("package.json without a bun pin must error")
	}
	client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"packages":{}}`), nil
	})}
	if _, err := fetchProviderUtilsVersion(client, "2.0.1"); err == nil {
		t.Error("bun.lock without the chain must error")
	}
}

// All-or-nothing WITHIN the v1 triple: a failure in ANY segment keeps the
// previously cached triple intact — never a mix of old and new versions
// across releases.
func TestWarmAllOrNothing(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}
	c := NewUserAgentCache()
	c.Warm(client, true)

	// New release resolves, but bun.lock fails → the old triple must stay.
	tr.mu.Lock()
	tr.release = `{"tag_name":"v3.0.0"}`
	tr.lockStatus = http.StatusInternalServerError
	tr.mu.Unlock()
	if got := c.Warm(client, true); got != stubUA {
		t.Errorf("Warm() with broken lock = %q, want previous %q", got, stubUA)
	}

	// Recovered sync picks up the new release end to end.
	tr.mu.Lock()
	tr.lockStatus = http.StatusOK
	tr.pkgJSON = strings.Replace(stubPkgJSON, "9.9.9", "10.0.0", 1)
	tr.lock = strings.Replace(stubLock, "5.5.5", "6.0.0", 1)
	tr.mu.Unlock()
	want := "opencode/3.0.0 ai-sdk/provider-utils/6.0.0 runtime/bun/10.0.0"
	if got := c.Warm(client, true); got != want {
		t.Errorf("Warm() after recovery = %q, want %q", got, want)
	}
}

// Per-form degradation (adversarial review F2/F3, pinned): one form's
// failure must never freeze the other form's refresh. v1 rides
// /releases/latest + raw files; v2 rides the tags list — different
// endpoints, independent failure modes.
func TestWarmFormsDegradeIndependently(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}
	c := NewUserAgentCache()
	c.Warm(client, true)
	// Both forms synced from the first pass.
	if got := c.Get(); got != stubUA {
		t.Fatalf("v1 Get() = %q, want %q", got, stubUA)
	}

	// v2 side fails (tags list down), the v1 side advances a release.
	tr.mu.Lock()
	tr.release = `{"tag_name":"v3.0.0"}`
	tr.pkgJSON = strings.Replace(stubPkgJSON, "9.9.9", "10.0.0", 1)
	tr.lock = strings.Replace(stubLock, "5.5.5", "6.0.0", 1)
	wantV1 := "opencode/3.0.0 ai-sdk/provider-utils/6.0.0 runtime/bun/10.0.0"
	// A v2 tag that fails the anchored scan: a prerelease-style suffix.
	tr.tagsList = `[{"name":"v2.9.0-rc1"},{"name":"v1.19.3"}]`
	tr.mu.Unlock()
	if got := c.Warm(client, true); got != wantV1 {
		t.Errorf("Warm() with broken v2 = %q, want the NEW v1 %q (v1 must not freeze)", got, wantV1)
	}
	// New v1 (stub v3 value) must be cached while v2 stays at the OLD value.
	c.SetStyleProvider(func() UAStyle { return UAStyleV2 })
	if got, want := c.Get(), "opencode/latest/2.0.1/cli"; got != want {
		t.Errorf("v2 Get() after v2 failure = %q, want the previous %q (stale, never mixed)", got, want)
	}
	c.SetStyleProvider(func() UAStyle { return UAStyleV1 })

	// Mirror: v1 side fails (bun.lock down), the v2 side still advances.
	tr.mu.Lock()
	tr.lockStatus = http.StatusInternalServerError
	tr.tagsList = `[{"name":"v2.5.0"},{"name":"v1.19.3"}]`
	tr.mu.Unlock()
	if got := c.Warm(client, true); got != wantV1 {
		t.Errorf("Warm() with broken v1 = %q, want previous v1 %q (held)", got, wantV1)
	}
	c.SetStyleProvider(func() UAStyle { return UAStyleV2 })
	if got, want := c.Get(), "opencode/latest/2.5.0/cli"; got != want {
		t.Errorf("v2 Get() after v1 failure = %q, want the NEW %q (v2 must not freeze)", got, want)
	}
	c.SetStyleProvider(func() UAStyle { return UAStyleV1 })
}

// Cold + failing → the compiled-in fallback (fail-open).
func TestWarmFailsOpenCold(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusServiceUnavailable, "nope"), nil
	})}
	c := NewUserAgentCache()
	if got := c.Warm(client, true); got != FallbackUA() {
		t.Errorf("Warm() on 503 = %q, want fallback %q", got, FallbackUA())
	}
	if got := c.Get(); got != FallbackUA() {
		t.Errorf("Get() after failure = %q, want fallback %q", got, FallbackUA())
	}
}

// Non-forced Warm throttles inside the TTL to zero source calls; force
// bypasses the TTL (the sync ticker relies on that).
func TestWarmTTLAndForce(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}
	c := NewUserAgentCache()
	base := time.Now()
	now := base
	c.now = func() time.Time { return now }

	c.Warm(client, false)
	if got := tr.calls(); got != 4 {
		t.Fatalf("first warm requests = %d, want 4 (v1 triple + v2 list)", got)
	}
	now = base.Add(config.VersionCacheTTL - time.Minute)
	c.Warm(client, false)
	if got := tr.calls(); got != 4 {
		t.Errorf("warm inside TTL requests = %d, want 4 (throttled)", got)
	}
	now = base.Add(config.VersionCacheTTL + time.Minute)
	if got := c.Warm(client, false); got != stubUA {
		t.Errorf("Warm() after TTL = %q, want %q", got, stubUA)
	}
	if got := tr.calls(); got != 8 {
		t.Errorf("post-TTL requests = %d, want 8", got)
	}
	c.Warm(client, true) // force: inside nothing, refreshes immediately
	if got := tr.calls(); got != 12 {
		t.Errorf("forced warm requests = %d, want 12", got)
	}
}

// Concurrent warms are deduplicated (single-flight): exactly one source pass.
func TestWarmSingleFlight(t *testing.T) {
	release := make(chan struct{})
	tr := newUASyncTransport()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-release // hold every probe open so the others must hit the inflight gate
		return tr.RoundTrip(r)
	})}
	c := NewUserAgentCache()

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.Warm(client, true)
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let every goroutine reach Warm
	close(release)
	wg.Wait()

	if got := tr.calls(); got != 4 {
		t.Errorf("source requests = %d, want 4 (v1 triple + v2 list)", got)
	}
	// Waiters behind the inflight gate return immediately (fail-open): the
	// cold-cache fallback. Exactly one goroutine — the winner — returns the
	// synced triple. Get() must serve the synced triple afterwards.
	synced := 0
	for i, r := range results {
		switch r {
		case stubUA:
			synced++
		case FallbackUA():
		default:
			t.Errorf("Warm() goroutine %d = %q, want %q or %q", i, r, stubUA, FallbackUA())
		}
	}
	if synced != 1 {
		t.Errorf("synced results = %d, want exactly 1 (single-flight winner)", synced)
	}
	if got := c.Get(); got != stubUA {
		t.Errorf("Get() after the sync pass = %q, want %q", got, stubUA)
	}
}

// Warm must never deadlock the cache mutex (regression: the old version
// called c.Get() while holding c.mu on the inflight path).
func TestWarmNoDeadlock(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"tag_name":"v2.0.1"}`), nil
	})}
	c := NewUserAgentCache()
	done := make(chan string, 1)
	go func() { done <- c.Warm(client, true) }()
	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("Warm() deadlocked")
	}
}

// The background ticker syncs repeatedly until stopped.
func TestStartSyncTicker(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}
	c := NewUserAgentCache()

	stop := c.StartSync(client, func() time.Duration { return 25 * time.Millisecond })
	deadline := time.After(2 * time.Second)
	for tr.calls() < 6 { // ≥2 full sync passes
		select {
		case <-deadline:
			t.Fatalf("sync ticker stalled after %d requests", tr.calls())
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	time.Sleep(60 * time.Millisecond)
	if got := c.Get(); got != stubUA {
		t.Errorf("Get() after sync = %q, want %q", got, stubUA)
	}
}

// parseReleaseTagName: tag names like "v1.18.31" / "1.20.0" yield a semver,
// anything else is rejected (golden vectors).
func TestParseReleaseTagName(t *testing.T) {
	cases := []struct {
		tag  string
		want string
	}{
		{"v1.18.31", "1.18.31"},
		{"1.20.0", "1.20.0"},
		{"v2.0.1", "2.0.1"},
		{"bad", ""},
		{"", ""},
		{"v1.2", ""},
	}
	for _, tc := range cases {
		m := TagRe.FindStringSubmatch(tc.tag)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != tc.want {
			t.Errorf("TagRe(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
}

// fetchLatestRelease returns the parsed semver for a 200 GitHub payload.
func TestFetchLatestReleaseParsesTag(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != config.GitHubReleasesURL {
			t.Errorf("probe URL = %q, want %q", r.URL.String(), config.GitHubReleasesURL)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		return jsonResponse(http.StatusOK, `{"tag_name":"v1.18.31"}`), nil
	})}
	version, err := fetchLatestRelease(client)
	if err != nil {
		t.Fatalf("fetchLatestRelease() error = %v", err)
	}
	if version != "1.18.31" {
		t.Errorf("fetchLatestRelease() = %q, want %q", version, "1.18.31")
	}
}

// A release tag without a semver and a non-200 response are both probe
// failures (the caller stays fail-open).
func TestFetchLatestReleaseFailureModes(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"tag_name":"bad"}`), nil
	})}
	if _, err := fetchLatestRelease(client); !errors.Is(err, errNoSemver) {
		t.Errorf("tag without semver: err = %v, want errNoSemver", err)
	}

	client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusServiceUnavailable, "nope"), nil
	})}
	if _, err := fetchLatestRelease(client); err == nil {
		t.Error("503 response must be a probe error")
	}

	client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	if _, err := fetchLatestRelease(client); err == nil {
		t.Error("transport error must surface")
	}
}

// fetchV2Version: direct unit tests (adversarial review gap 7).
func TestFetchV2Version(t *testing.T) {
	t.Run("no v2 tag → errNoV2Tag", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `[{"name":"v1.19.3"},{"name":"v1.18.31"}]`), nil
		})}
		if _, err := fetchV2Version(client); !errors.Is(err, errNoV2Tag) {
			t.Fatalf("err = %v, want errNoV2Tag", err)
		}
	})
	t.Run("prerelease suffix rejected (not truncated)", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `[{"name":"v2.1.0-rc1"},{"name":"v1.19.3"}]`), nil
		})}
		if _, err := fetchV2Version(client); !errors.Is(err, errNoV2Tag) {
			t.Fatalf("prerelease-suffixed tag must be skipped, err = %v", err)
		}
	})
	t.Run("first stable v2 tag wins (newest first)", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `[{"name":"v2.1.0"},{"name":"v2.0.32"},{"name":"v1.19.3"}]`), nil
		})}
		got, err := fetchV2Version(client)
		if err != nil {
			t.Fatal(err)
		}
		if got != "opencode/latest/2.1.0/cli" {
			t.Errorf("got = %q, want opencode/latest/2.1.0/cli", got)
		}
	})
	t.Run("picks lower stable when newer is prerelease-suffixed", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `[{"name":"v2.9.0-rc1"},{"name":"v2.0.32"},{"name":"v1.19.3"}]`), nil
		})}
		got, err := fetchV2Version(client)
		if err != nil {
			t.Fatal(err)
		}
		if got != "opencode/latest/2.0.32/cli" {
			t.Errorf("got = %q, want opencode/latest/2.0.32/cli", got)
		}
	})
	t.Run("malformed body → decode error", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"not":"a list"}`), nil
		})}
		if _, err := fetchV2Version(client); err == nil {
			t.Error("non-list JSON must error")
		}
	})
	t.Run("requests the tags URL with the GitHub Accept header", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != config.GitHubTagsListURL {
				t.Errorf("probe URL = %q, want %q", r.URL.String(), config.GitHubTagsListURL)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("Accept header = %q", got)
			}
			return jsonResponse(http.StatusOK, `[{"name":"v2.0.1"}]`), nil
		})}
		got, err := fetchV2Version(client)
		if err != nil {
			t.Fatal(err)
		}
		if got != "opencode/latest/2.0.1/cli" {
			t.Errorf("got = %q", got)
		}
	})
}

// The typed probe errors render human-readable statuses/messages (fail-open
// logging surfaces Error()).
func TestProbeErrorMessages(t *testing.T) {
	if got := (&statusError{code: http.StatusServiceUnavailable}).Error(); got != http.StatusText(http.StatusServiceUnavailable) {
		t.Errorf("statusError(503).Error() = %q, want %q", got, http.StatusText(http.StatusServiceUnavailable))
	}
	// fetchRaw returns a typed statusError for any non-200 raw fetch.
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, "denied"), nil
	})}
	_, err := fetchRaw(client, "2.0.1", config.RootPackageJSONPath)
	var se *statusError
	if !errors.As(err, &se) {
		t.Fatalf("fetchRaw(403) err = %v, want a *statusError", err)
	}
	if got := err.Error(); got != http.StatusText(http.StatusForbidden) {
		t.Errorf("fetchRaw(403).Error() = %q, want %q", got, http.StatusText(http.StatusForbidden))
	}
	// The oversize bound carries a descriptive errString.
	if got := errString("boom").Error(); got != "boom" {
		t.Errorf("errString.Error() = %q, want %q", got, "boom")
	}
	if got := errNoSemver.Error(); got != "github release tag carried no semver" {
		t.Errorf("errNoSemver.Error() = %q, want the fixed message", got)
	}
}

// Get before any warm returns the pinned fallback (cold cache).
func TestUserAgentCacheColdGet(t *testing.T) {
	if got := NewUserAgentCache().Get(); got != FallbackUA() {
		t.Errorf("cold Get() = %q, want %q", got, FallbackUA())
	}
}

// The style provider selects which wire form Get renders. Both forms are in
// the cache after one warm (the dual contract); the provider flips the form
// without re-syncing, and a nil-provider cache stays v1.
func TestStyleSelectsWireForm(t *testing.T) {
	tr := newUASyncTransport()
	client := &http.Client{Transport: tr}

	t.Run("default v1", func(t *testing.T) {
		c := NewUserAgentCache()
		if got := c.Style(); got != UAStyleV1 {
			t.Errorf("Style() = %q, want v1", got)
		}
		c.Warm(client, true)
		if got := c.Get(); got != stubUA {
			t.Errorf("Get() = %q, want v1 %q", got, stubUA)
		}
	})

	t.Run("v2 after set", func(t *testing.T) {
		tr2 := newUASyncTransport()
		client2 := &http.Client{Transport: tr2}
		c := NewUserAgentCache()
		c.SetStyleProvider(func() UAStyle { return UAStyleV2 })
		if got := c.Style(); got != UAStyleV2 {
			t.Errorf("Style() = %q, want v2", got)
		}
		// Cold with v2 selected → the compiled-in v2 fallback, never "".
		if got := c.Get(); got != FallbackV2() {
			t.Errorf("cold v2 Get() = %q, want %q", got, FallbackV2())
		}
		c.Warm(client2, true)
		// The synced v2 form (the transport's releases list carries a v2 tag).
		want := "opencode/latest/2.0.1/cli"
		if got := c.Get(); got != want {
			t.Errorf("v2 Get() = %q, want %q", got, want)
		}
		// One warm fetched BOTH forms: no extra source pass afterwards.
		if got := tr2.calls(); got != 4 {
			t.Errorf("requests after one warm = %d, want 4", got)
		}
		// Flip back to v1: no re-sync needed, the cached v1 triple serves.
		c.SetStyleProvider(func() UAStyle { return UAStyleV1 })
		if got := c.Get(); got != stubUA {
			t.Errorf("v1 Get() after flip = %q, want %q", got, stubUA)
		}
		if got := tr2.calls(); got != 4 {
			t.Errorf("requests after style flip = %d, want still 4", got)
		}
	})

	t.Run("nil provider resets to v1", func(t *testing.T) {
		c := NewUserAgentCache()
		c.SetStyleProvider(func() UAStyle { return UAStyleV2 })
		c.SetStyleProvider(nil)
		if got := c.Style(); got != UAStyleV1 {
			t.Errorf("Style() after nil = %q, want v1", got)
		}
	})
}
