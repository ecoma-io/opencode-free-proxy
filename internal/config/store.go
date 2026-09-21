package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"opencode-free-proxy/internal/logging"
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
//
// One read per cycle, and the hash travels with the bytes: every load path
// reads the file exactly once, hashes those bytes, and parses THOSE bytes
// (LoadBytes). Parsing can therefore never disagree with the stamp — a write
// landing between two reads of the old read-then-re-read flow could stamp a
// snapshot with another generation's hash and make every later "unchanged"
// decision lie.
type Store struct {
	path     string
	interval time.Duration
	cur      atomic.Pointer[Runtime]
	log      zerolog.Logger
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
func NewStore(path string, interval time.Duration, logger any) (*Store, error) {
	log := logging.Resolve(logger)
	// One read for both the snapshot and the poller's compare seed: the hash
	// is stamped from the same bytes that were parsed (readStamped + LoadBytes,
	// never LoadFile's own read).
	raw, sum, err := readStamped(path)
	if err != nil {
		return nil, err
	}
	rt, err := LoadBytes(raw)
	if err != nil {
		return nil, err
	}
	s := &Store{
		path:     path,
		interval: interval,
		log:      log,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		initHash: sum,
	}
	rt.Generation = s.gen.Add(1)
	s.cur.Store(rt)
	if interval > 0 {
		go s.poll()
	}
	return s, nil
}

// NewDefault returns a store holding the synthetic direct-egress runtime and
// no poller (used when OCFP_CONFIG is unset).
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

// LoadFile reads, interpolates, parses, and resolves the config at path. It
// reads the file exactly once and hands those bytes to LoadBytes — the same
// path the poller uses, so a snapshot and its content stamp can never come
// from different file generations.
func LoadFile(path string) (*Runtime, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(raw)
}

// LoadBytes interpolates, parses, validates, and resolves an ALREADY-READ
// config document. It is the whole load pipeline after the read, and the
// single entry point every snapshot is built through: because the caller
// supplies the bytes, any content hash computed over them provably describes
// the snapshot that comes back (the store's TOCTOU fix — see Store).
func LoadBytes(raw []byte) (*Runtime, error) {
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

// readStamped reads the config file once and returns its bytes plus the
// SHA-256 of THOSE bytes. NewStore and the poller both stamp from it, so the
// hash that gates "unchanged" always describes the bytes a snapshot would be
// parsed from.
func readStamped(path string) (raw []byte, sum string, err error) {
	raw, err = os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
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
// the error (a valid config is never replaced by an invalid one). The read is
// the cycle's ONLY one: the hash and the bytes handed to LoadBytes come from
// it together, so a swap can never pair one generation's content with
// another's stamp. A write landing after the read is simply picked up by the
// next tick.
// ParseZerologLevel maps the config document's validated level to zerolog.
// Trace and fatal are deliberately not config values, matching the sibling
// gateway's process-wide logging contract.
func ParseZerologLevel(level string) zerolog.Level {
	switch level {
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

func (s *Store) tick(lastHash *string) {
	raw, hash, err := readStamped(s.path)
	if err != nil {
		s.log.Warn().Err(err).Msg("config reload: read failed, keeping previous config")
		return
	}
	if hash == *lastHash {
		return
	}
	rt, err := LoadBytes(raw)
	if err != nil {
		s.log.Warn().Err(err).Msg("config reload: rejected, keeping previous config")
		return
	}
	*lastHash = hash
	rt.Generation = s.gen.Add(1)
	s.cur.Store(rt)
	// The poll goroutine is the sole runtime writer of zerolog's global level.
	// Every event consults it when emitted, so this takes effect atomically.
	zerolog.SetGlobalLevel(ParseZerologLevel(rt.LogLevel()))
	s.log.Info().Uint64("generation", rt.Generation).Int("egresses", len(rt.File.Egress)).Int("routes", len(rt.File.Routes)).
		Msgf("config reload: swapped to new config (generation %d, %d egresses, %d routes)",
			rt.Generation, len(rt.File.Egress), len(rt.File.Routes))
}
