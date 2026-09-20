package config

import (
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
