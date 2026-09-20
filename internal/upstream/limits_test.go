package upstream

// Concurrency-slot hardening: the release ordering of slotReleaseBody (body
// closed BEFORE the slot frees) and the Peek/Acquire/Release race.

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// onceCloser records the FIRST Close only — the streaming relay closes bodies
// twice, and the ordering assertion cares about the first.
type onceCloser struct {
	io.ReadCloser
	once   sync.Once
	closed func()
}

func (c *onceCloser) Close() error {
	c.once.Do(c.closed)
	return c.ReadCloser.Close()
}

// TestSlotReleaseBodyClosesBodyBeforeFree pins the release ordering: a
// replacement request must never be admitted past the concurrency cap while
// this attempt's connection is still open, so Close must tear the body down
// before freeing the slot — and free it exactly once across the relay's
// double close.
func TestSlotReleaseBodyClosesBodyBeforeFree(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(ev string) {
		mu.Lock()
		order = append(order, ev)
		mu.Unlock()
	}

	b := &slotReleaseBody{
		ReadCloser: &onceCloser{
			ReadCloser: io.NopCloser(strings.NewReader("x")),
			closed:     func() { record("body-close") },
		},
		free: func() { record("slot-free") },
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := b.Close(); err != nil { // stream.go closes twice
		t.Fatalf("second close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "body-close" || order[1] != "slot-free" {
		t.Fatalf("close order = %v, want [body-close slot-free …] — the slot must not free while the body is open", order)
	}
	frees := 0
	for _, ev := range order {
		if ev == "slot-free" {
			frees++
		}
	}
	if frees != 1 {
		t.Fatalf("slot freed %d times, want exactly once across the double close", frees)
	}
}

// TestPeekAcquireRace hammers the Limiter from many goroutines: the cap must
// hold as a true upper bound on in-flight attempts, Peek/Acquire/Release must
// converge (no deadlock), and the limiter must return to a clean state. Run
// under -race.
func TestPeekAcquireRace(t *testing.T) {
	const (
		capacity = 4
		workers  = 16
		iters    = 250
	)
	l := NewLimiter()
	var (
		wg        sync.WaitGroup
		inflight  atomic.Int64
		overCap   atomic.Int64
		peekTrue  atomic.Int64
		peekFalse atomic.Int64
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iters; j++ {
				// Peek is advisory (no reservation); it must never block or
				// panic, whatever the racing Release/Acquire churn is.
				if l.Peek("e", capacity) {
					peekTrue.Add(1)
				} else {
					peekFalse.Add(1)
				}
				if l.Acquire("e", capacity) {
					// The counted window must sit strictly INSIDE the slot-
					// holding interval [Acquire-success, Release): a
					// goroutine is counted exactly while it holds a slot.
					// (Counting past the Release — even by one instruction —
					// lets a replacement's Add race this goroutine's
					// decrement and produce phantom over-cap reads.)
					if inflight.Add(1) > capacity {
						overCap.Add(1)
					}
					inflight.Add(-1)
					l.Release("e")
				}
			}
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: concurrent Peek/Acquire/Release did not converge")
	}
	if overCap.Load() != 0 {
		t.Fatalf("in-flight exceeded the cap %d times — Acquire/Release are unsound", overCap.Load())
	}
	if inflight.Load() != 0 {
		t.Fatalf("in-flight = %d after every Release, want 0", inflight.Load())
	}
	if !l.Acquire("e", capacity) {
		t.Fatal("all slots must be free after the storm")
	}

	// A saturated limiter must never over-advertise: Peek reports false while
	// every slot is held (deterministic — the storm above only asserts that
	// Peek liveness holds, not its verdicts).
	for i := 1; i < capacity; i++ {
		if !l.Acquire("e", capacity) {
			t.Fatalf("Acquire %d/%d failed", i, capacity)
		}
	}
	if l.Peek("e", capacity) {
		t.Fatal("Peek advertised a free slot at full capacity")
	}
	l.Release("e")
	if !l.Peek("e", capacity) {
		t.Fatal("Peek must see a freed slot")
	}

	// Unbounded (max <= 0) always admits, held or not.
	if !l.Peek("unbounded", 0) || !l.Acquire("unbounded", 0) {
		t.Fatal("max <= 0 means unlimited")
	}
}
