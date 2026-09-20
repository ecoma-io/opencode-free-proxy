package config

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Store owns the immutable runtime configuration snapshot and its hot-reload
// poller. Requests call Get() once and hold the returned *Runtime for their
// whole lifetime; a reload swaps the pointer atomically, so an in-flight
// request never observes a mutation and never mixes two config generations.
//
// The poller mirrors the rotation-proxy-gateway design: fixed-interval file
// read → SHA-256 content compare → only parse+validate + swap on change.
// Filesystem events are never the only mechanism; repeated writes between
// ticks coalesce into one parse of the final content.
type Store struct {
	path     string
	interval time.Duration
	cur      atomic.Pointer[Runtime]
	logf     func(format string, args ...any)
	// gen stamps Runtime.Generation on every successful load; it is the
	// store-side monotonic config version (first load 1, each swap +1) so
	// logs and tests can name a request's exact snapshot. Generation 0 is
	// reserved for the built-in default runtime (never stamped here).
	gen atomic.Uint64

	// initHash is the content hash at startup; the poller seeds its compare
	// from it so an unchanged file is never reparsed on the first tick.
	initHash string
	stopCh   chan struct{}
	done     chan struct{}
	// stopOnce guards close(stopCh): two concurrent Stops must not
	// double-close (the second blocks on once.Do until the first's
	// close + <-done completes — correct serialization, no panic).
	stopOnce sync.Once
}

// NewStore loads the config file at path and starts the poller. interval 0
// disables polling (single load). A load failure is fatal here — at startup
// there is no valid previous snapshot to fall back on.
func NewStore(path string, interval time.Duration, logf func(string, ...any)) (*Store, error) {
	rt, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = log.Printf
	}
	s := &Store{
		path:     path,
		interval: interval,
		logf:     logf,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	rt.Generation = s.gen.Add(1)
	s.cur.Store(rt)
	if interval > 0 {
		if raw, err := os.ReadFile(path); err == nil {
			sum := sha256.Sum256(raw)
			s.initHash = hex.EncodeToString(sum[:])
		}
		go s.poll()
	}
	return s, nil
}

// NewDefault returns a store holding the synthetic direct-egress runtime and
// no poller (used when OFP_CONFIG is unset).
func NewDefault() *Store {
	s := &Store{done: make(chan struct{})}
	s.cur.Store(DefaultRuntime())
	return s
}

// Get returns the current immutable snapshot.
func (s *Store) Get() *Runtime { return s.cur.Load() }

// Stop halts the poller and waits for it to exit. Safe to call any number of
// times and from concurrent goroutines. Stores without a poller (NewDefault;
// NewStore with interval 0) have no ticker to stop and return immediately.
func (s *Store) Stop() {
	if s.stopCh == nil || s.interval <= 0 {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.done
	})
}

// LoadFile reads, interpolates, parses, and resolves the config at path.
func LoadFile(path string) (*Runtime, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	interpolated, err := Interpolate(raw)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(interpolated, &f); err != nil {
		return nil, err
	}
	return f.Resolve()
}
func (s *Store) poll() {
	defer close(s.done)
	lastHash := s.initHash
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick(&lastHash)
		}
	}
}

// tick is one poll cycle. A content change is detected by SHA-256 before any
// parse work; parse/validation failure keeps the previous snapshot and logs
// the error (a valid config is never replaced by an invalid one).
func (s *Store) tick(lastHash *string) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.logf("config reload: read failed, keeping previous config: %v", err)
		return
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if hash == *lastHash {
		return
	}
	rt, err := LoadFile(s.path)
	if err != nil {
		s.logf("config reload: rejected, keeping previous config: %v", err)
		return
	}
	// Re-check the hash after the successful parse: if the file changed
	// between our read and LoadFile's re-read, the hash we compared is stale
	// and a second tick will converge on the newer content.
	*lastHash = hash
	rt.Generation = s.gen.Add(1)
	s.cur.Store(rt)
	s.logf("config reload: swapped to new config (generation %d, %d egresses, %d routes)",
		rt.Generation, len(rt.File.Egress), len(rt.File.Routes))
}
