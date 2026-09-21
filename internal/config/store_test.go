package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const pollInterval = 20 * time.Millisecond

// writeConfig writes doc to a temp path and returns the path.
func writeConfig(t *testing.T, dir, name, doc string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewStoreLoadsValidFile(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a, proxy: {type: http, url: "http://h:1"}}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Stop()
	if s.Get() == nil || len(s.Get().File.Egress) != 1 {
		t.Fatal("snapshot not loaded")
	}
}

func TestNewStoreInvalidFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `egress: [this is not yaml`)
	if _, err := NewStore(p, 0, nil); err == nil {
		t.Fatal("invalid config must fail startup")
	}
}

func TestStoreReloadSwapsSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	var logs []string
	var mu sync.Mutex
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, strings.TrimSpace(format))
	}
	s, err := NewStore(p, pollInterval, logf)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	rt0 := s.Get()
	// Hold a reference (simulates an in-flight request) and reload.
	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, b]}]
`)
	deadline := time.Now().Add(2 * time.Second)
	for s.Get() == rt0 && time.Now().Before(deadline) {
		time.Sleep(2 * pollInterval)
	}
	if s.Get() == rt0 {
		t.Fatal("reload never swapped the snapshot")
	}
	// old snapshot still intact
	if len(rt0.File.Egress) != 1 {
		t.Fatalf("held snapshot mutated: %d egresses", len(rt0.File.Egress))
	}
	if len(s.Get().File.Egress) != 2 {
		t.Fatalf("new snapshot wrong: %d egresses", len(s.Get().File.Egress))
	}
	mu.Lock()
	swapped := 0
	for _, l := range logs {
		if strings.Contains(l, "swapped") {
			swapped++
		}
	}
	mu.Unlock()
	if swapped < 1 {
		t.Fatalf("no swap logged: %v", logs)
	}
}

func TestStoreInvalidReloadKeepsOld(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	var mu sync.Mutex
	var logs []string
	s, err := NewStore(p, pollInterval, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, strings.TrimSpace(format))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: a}]
routes: [{id: r, egress: [a]}]
`)
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, l := range logs {
			if strings.Contains(l, "rejected") {
				found = true
			}
		}
		mu.Unlock()
		if found && len(s.Get().File.Egress) == 1 {
			break
		}
		time.Sleep(2 * pollInterval)
	}
	if !found {
		t.Fatal("rejection never logged")
	}
	if len(s.Get().File.Egress) != 1 {
		t.Fatalf("old snapshot replaced by invalid config: %d egresses", len(s.Get().File.Egress))
	}
}

func TestStoreUnchangedContentNoReparse(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	var mu sync.Mutex
	var parsed int
	s, err := NewStore(p, pollInterval, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(format, "swapped") {
			parsed++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	time.Sleep(6 * pollInterval)
	mu.Lock()
	n := parsed
	mu.Unlock()
	if n != 0 {
		t.Fatalf("unchanged content re-parsed %d times", n)
	}
}

func TestStoreTemporaryReadFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Get().File.Egress) == 1 {
			break
		}
		time.Sleep(2 * pollInterval)
	}
	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, b]}]
`)
	deadline = time.Now().Add(2 * time.Second)
	for len(s.Get().File.Egress) != 2 && time.Now().Before(deadline) {
		time.Sleep(2 * pollInterval)
	}
	if len(s.Get().File.Egress) != 2 {
		t.Fatal("restored file never picked up")
	}
}

// TestStoreConcurrentGetStop exercises the atomic snapshot under -race:
// poller swaps while readers hold snapshots and Stop races the ticker.
func TestStoreConcurrentGetStop(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				rt := s.Get()
				_, _ = rt.MatchRoute(false, 0, "x")
				time.Sleep(time.Microsecond)
			}
		}()
	}
	// Reload in parallel with readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, b]}]
`)
			time.Sleep(pollInterval)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	wg.Wait()
}

func TestStoreStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s.Stop() // must not panic
}

func TestDefaultStoreNoPoller(t *testing.T) {
	s := NewDefault()
	defer s.Stop()
	if !s.Get().Direct || len(s.Get().File.Egress) != 1 {
		t.Fatal("default runtime wrong")
	}
}

// TestStoreNoPollerStopReturns: a NewStore with interval 0 never starts the
// poller, so its done channel never closes — Stop must return immediately
// instead of blocking forever (issue #3 review).
func TestStoreNoPollerStopReturns(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop on a poller-less store must return promptly")
	}
}

// TestStoreConcurrentStopNoPanic: concurrent Stops must not double-close
// stopCh (sync.Once serializes; the waiters block on the winner's close).
func TestStoreConcurrentStopNoPanic(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Stop()
		}()
	}
	wg.Wait()
	s.Stop() // must still be safe after the concurrent batch
}

// TestStoreGenerationStartsAtOne: the first loaded snapshot is generation 1
// (DefaultRuntime is 0); each swap bumps it. A held snapshot keeps its own
// generation forever — the immutability proof for in-flight requests.
func TestStoreGenerationStartsAtOne(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	rt0 := s.Get()
	if rt0.Generation != 1 {
		t.Fatalf("first load generation = %d, want 1", rt0.Generation)
	}

	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, b]}]
`)
	deadline := time.Now().Add(2 * time.Second)
	for s.Get().Generation == rt0.Generation && time.Now().Before(deadline) {
		time.Sleep(2 * pollInterval)
	}
	if s.Get().Generation != 2 {
		t.Fatalf("after first reload generation = %d, want 2", s.Get().Generation)
	}
	if rt0.Generation != 1 {
		t.Fatalf("held snapshot generation mutated: %d, want 1 (in-flight request sees the old snapshot)", rt0.Generation)
	}
}

// TestStoreInvalidReloadKeepsGeneration: a rejected config must neither swap
// the snapshot nor burn a generation — the id space stays contiguous, so a
// "generation" in logs is unambiguous.
func TestStoreInvalidReloadKeepsGeneration(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`)
	s, err := NewStore(p, pollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	g0 := s.Get().Generation

	// Duplicate egress id — must be rejected by Validate.
	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: a}]
routes: [{id: r, egress: [a]}]
`)
	// Give the poller a few cycles to attempt (and reject) the reload.
	time.Sleep(6 * pollInterval)
	if g := s.Get().Generation; g != g0 {
		t.Fatalf("generation changed on rejected reload: %d -> %d", g0, g)
	}
}

// reloadDocA / reloadDocB are two distinct VALID configs: one egress vs two,
// so a snapshot's content names which document it was parsed from.
const (
	reloadDocA = `
egress: [{id: a}]
routes: [{id: r, egress: [a]}]
`
	reloadDocB = `
egress: [{id: a}, {id: b}]
routes: [{id: r, egress: [a, b]}]
`
)

// TestLoadBytesMatchesLoadFile: LoadBytes is the pipeline behind LoadFile —
// same bytes in, same snapshot out (it is what lets the store stamp a hash
// over the bytes it actually parsed).
func TestLoadBytesMatchesLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", `
egress:
  - id: a
    proxy: {type: http, url: "http://a.example:1"}
    weight: 2
  - id: b
routes:
  - id: stream
    priority: 5
    match: {streaming: true}
    egress: [a, b]
  - id: default
    egress: [a, b]
fallback: {max_attempts: 2}
health: {failure_threshold: 4, cooldown: 12s}
`)
	fromFile, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fromBytes, err := LoadBytes(raw)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if fromFile.Generation != 0 || fromBytes.Generation != 0 {
		t.Fatal("a bare load must not stamp a generation (the store owns stamping)")
	}
	if len(fromFile.File.Egress) != len(fromBytes.File.Egress) || len(fromFile.Routes()) != len(fromBytes.Routes()) {
		t.Fatalf("LoadFile and LoadBytes disagree: %+v vs %+v", fromFile.File, fromBytes.File)
	}
	for i := range fromFile.File.Egress {
		a, b := fromFile.File.Egress[i], fromBytes.File.Egress[i]
		if a.ID != b.ID || a.TransportSignature() != b.TransportSignature() || a.EffectiveWeight() != b.EffectiveWeight() {
			t.Fatalf("egress %d differs: %+v vs %+v", i, a, b)
		}
	}
	if fromFile.HealthThreshold() != fromBytes.HealthThreshold() ||
		fromFile.HealthCooldown() != fromBytes.HealthCooldown() ||
		fromFile.Fallback.MaxAttempts != fromBytes.Fallback.MaxAttempts {
		t.Fatal("policies differ between LoadFile and LoadBytes")
	}

	// LoadBytes runs the full pipeline on its own account too: interpolation
	// and validation both gate a snapshot behind the caller's bytes.
	t.Setenv("OCFP_TEST_RELOAD_URL", "http://env.example:9")
	withEnv, err := LoadBytes([]byte(`
egress: [{id: a, proxy: {type: http, url: "${OCFP_TEST_RELOAD_URL}"}}]
routes: [{id: r, egress: [a]}]
`))
	if err != nil {
		t.Fatalf("interpolation through LoadBytes: %v", err)
	}
	if e, _ := withEnv.Egress("a"); e.Proxy.URL != "http://env.example:9" {
		t.Fatalf("env not interpolated through LoadBytes: %q", e.Proxy.URL)
	}
	if _, err := LoadBytes([]byte("egress: [{id: a, proxy: {type: http, url: \"http://${OCFP_TEST_UNSET_VAR}:1\"}}]\n" +
		"routes: [{id: r, egress: [a]}]\n")); err == nil || !strings.Contains(err.Error(), "OCFP_TEST_UNSET_VAR") {
		t.Fatalf("unset var must be a load error naming it, got %v", err)
	}
	if _, err := LoadBytes([]byte("egress: [{id: a}, {id: a}]\nroutes: [{id: r, egress: [a]}]\n")); err == nil {
		t.Fatal("invalid document must not resolve")
	}
}

// TestStampedLoadCoherentUnderMidLoadRewrite is the TOCTOU proof: the stamp
// and the snapshot come from ONE read. The file is rewritten BETWEEN the read
// and the load — the exact window the old read-then-re-read poller straddled,
// where a snapshot could end up stamped with another generation's hash and
// make every later "unchanged" decision lie.
func TestStampedLoadCoherentUnderMidLoadRewrite(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", reloadDocA)

	raw1, sum1, err := readStamped(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(raw1); sum1 != want {
		t.Fatalf("stamp %q is not the hash of the returned bytes (%q)", sum1, want)
	}

	// Mutate the file after the read, before the load.
	writeConfig(t, dir, "cfg.yaml", reloadDocB)
	rt, err := LoadBytes(raw1)
	if err != nil {
		t.Fatalf("load of the already-read bytes: %v", err)
	}
	if len(rt.File.Egress) != 1 {
		t.Fatalf("snapshot is a chimera: %d egresses, want the 1 from the read bytes", len(rt.File.Egress))
	}

	// A second stamped read of the rewritten file is a DIFFERENT stamp, and
	// loading its bytes yields the new generation's content.
	raw2, sum2, err := readStamped(p)
	if err != nil {
		t.Fatal(err)
	}
	if sum2 == sum1 {
		t.Fatal("rewritten content kept the same stamp")
	}
	rt2, err := LoadBytes(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt2.File.Egress) != 2 {
		t.Fatalf("second stamped load incoherent: %d egresses, want 2", len(rt2.File.Egress))
	}
}

// TestStoreInvalidThenValidReloadReverts: valid → invalid keeps the last good
// snapshot (and its generation); a later valid file is adopted — generation
// increments and the content swaps — while the held old snapshot stays put.
func TestStoreInvalidThenValidReloadReverts(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.yaml", reloadDocA)
	var mu sync.Mutex
	var logs []string
	s, err := NewStore(p, pollInterval, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, strings.TrimSpace(format))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	rt1 := s.Get()
	if rt1.Generation != 1 {
		t.Fatalf("first load generation = %d, want 1", rt1.Generation)
	}

	// Phase 1: valid → invalid. Rejected, last good kept, no generation burned.
	writeConfig(t, dir, "cfg.yaml", `
egress: [{id: a}, {id: a}]
routes: [{id: r, egress: [a]}]
`)
	deadline := time.Now().Add(2 * time.Second)
	rejected := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, l := range logs {
			if strings.Contains(l, "rejected") {
				rejected = true
			}
		}
		mu.Unlock()
		if rejected {
			break
		}
		time.Sleep(2 * pollInterval)
	}
	if !rejected {
		t.Fatal("invalid reload was never attempted (no rejection logged)")
	}
	if got := s.Get(); got != rt1 || got.Generation != 1 || len(got.File.Egress) != 1 {
		t.Fatalf("invalid reload disturbed the last good snapshot: %+v", got)
	}

	// Phase 2: invalid → valid-again. The revert adopts the new good config.
	writeConfig(t, dir, "cfg.yaml", reloadDocB)
	deadline = time.Now().Add(2 * time.Second)
	for s.Get().Generation == 1 && time.Now().Before(deadline) {
		time.Sleep(2 * pollInterval)
	}
	rt2 := s.Get()
	if rt2 == rt1 {
		t.Fatal("revert never swapped the snapshot")
	}
	if rt2.Generation != 2 {
		t.Fatalf("revert generation = %d, want 2", rt2.Generation)
	}
	if len(rt2.File.Egress) != 2 {
		t.Fatalf("revert adopted %d egresses, want the valid file's 2", len(rt2.File.Egress))
	}
	// The snapshot held across the whole episode is untouched.
	if rt1.Generation != 1 || len(rt1.File.Egress) != 1 {
		t.Fatalf("held snapshot mutated across invalid→valid: %+v", rt1)
	}
	mu.Lock()
	swapped := false
	for _, l := range logs {
		if strings.Contains(l, "swapped") {
			swapped = true
		}
	}
	mu.Unlock()
	if !swapped {
		t.Fatal("revert never logged a swap")
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
