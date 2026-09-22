package upstream

import "sync"

// Limiter is the per-egress concurrency gate. Acquire happens in the
// executor at dial time; Release pairs with it either on attempt failure or
// when the winning response body closes — the cap counts in-flight
// requests/streams, not dials. Peek is the scheduler's soft check: a slot
// may be taken between planning and dialing, so the executor re-checks with
// Acquire and SKIPS the egress without marking it failed (skip ≠ failure).
// Keyed by egress id, so it survives config swaps.
type Limiter struct {
	mu  sync.Mutex
	cur map[string]int
}

func NewLimiter() *Limiter { return &Limiter{cur: map[string]int{}} }

// Peek reports whether a slot is free now (0 = unlimited). No reservation.
func (l *Limiter) Peek(id string, max int) bool {
	if max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur[id] < max
}

// Acquire reserves one slot; false when at capacity (0 = unlimited).
func (l *Limiter) Acquire(id string, max int) bool {
	if max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur[id] >= max {
		return false
	}
	l.cur[id]++
	return true
}

// Release frees one slot. Safe to pair only with successful Acquires; the
// > 0 guard makes a stray call harmless instead of corrupting counts.
func (l *Limiter) Release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur[id] > 0 {
		l.cur[id]--
	}
}

// InFlight reports the current occupancy for id. Only meaningful for capped
// egresses — Acquire does not count uncapped (max <= 0) traffic — so the
// evidence layer records it solely when a cap is configured. Read-only
// snapshot under the same mutex; no reservation.
func (l *Limiter) InFlight(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur[id]
}
