// Package identity reproduces the OpenCode desktop client identity the
// free-tier upstream gate validates: session/request id generation that
// bit-for-bit matches the official client, plus the User-Agent version probe.
//
// Ported from 9router open-sse/executors/opencode.js and
// open-sse/utils/opencodeClientVersion.js — keep them in lockstep.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SessionRE is the upstream session shape: ses_ + 12 hex + 14 base62.
var SessionRE = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// RequestRE is the request-id shape: msg_ + 12 hex + 14 base62.
var RequestRE = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// idClock serializes the timestamp/counter pair the way the single-threaded
// Node module-global does: same-ms callers get counter 1, 2, 3, …
type idClock struct {
	mu        sync.Mutex
	lastMilli int64
	counter   int64
}

var clock idClock

// unstableRandom mirrors the upstream generator: 14 random bytes, each taken
// modulo 62 (modulo bias included on purpose — bit-for-bit parity).
func unstableRandom() string {
	b := make([]byte, 14)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never fails on Linux; degrade to a fixed filler rather
		// than panic mid-request.
		for i := range b {
			b[i] = byte(i)
		}
	}
	out := make([]byte, 14)
	for i, c := range b {
		out[i] = base62Chars[int(c)%62]
	}
	return string(out)
}

// timeBytes renders (millis*0x1000+counter) as the 6 low bytes of the NOT'ed
// value — exactly what the JS BigInt path emits:
//
//	value = ~(millis*0x1000 + counter)   // two's complement, sign-extended
//	time  = bytes 5..0 of value, hex     // (value >> (40-8i)) & 0xff
//
// On uint64, ^v has the same low 48 bits as the infinite-precision two's
// complement, so extracting (x >> shift) & 0xff for shift 40,32,…,0 is
// byte-for-byte identical.
func timeBytes(v uint64) [6]byte {
	var out [6]byte
	for i := 0; i < 6; i++ {
		out[i] = byte((v >> (40 - 8*i)) & 0xff)
	}
	return out
}

func hexTime(v uint64) string {
	b := timeBytes(v)
	return hex.EncodeToString(b[:])
}

// GenerateSessionID mirrors upstream generateSessionId(): the id embeds the
// wall-clock millisecond and a per-millisecond counter, NOT'ed, plus 14
// base62 random chars.
func GenerateSessionID(now time.Time) string {
	clock.mu.Lock()
	if now.UnixMilli() != clock.lastMilli {
		clock.lastMilli = now.UnixMilli()
		clock.counter = 0
	}
	clock.counter++
	c := clock.counter
	clock.mu.Unlock()

	v := uint64(now.UnixMilli())*0x1000 + uint64(c)
	return "ses_" + hexTime(^v) + unstableRandom()
}

// GenerateRequestID mirrors upstream generateRequestId(): same layout as the
// session id but with the msg_ prefix, NO bitwise NOT, and no counter — the
// value is always millis*0x1000 + 1.
func GenerateRequestID(now time.Time) string {
	v := uint64(now.UnixMilli())*0x1000 + 1
	return "msg_" + hexTime(v) + unstableRandom()
}

// TranslateSessionID converts an arbitrary conversation key into a valid
// OpenCode session id. Values already matching the canonical shape are
// preserved (trimmed). Otherwise sha256("opencode\0<tool>\0<key>") is mapped
// into the same shape, making conversation stickiness deterministic.
func TranslateSessionID(sessionID, clientTool string) string {
	if trimmed := strings.TrimSpace(sessionID); SessionRE.MatchString(trimmed) {
		return trimmed
	}
	tool := clientTool
	if tool == "" {
		tool = "generic"
	}
	sum := sha256.Sum256([]byte("opencode\x00" + tool + "\x00" + sessionID))
	// DIVERGENCE (upstream change, 2026-10-13): opencode.ai's free-tier gate
	// no longer checks only the session id's SHAPE — it decodes the 12 hex
	// chars back into `~(millis*0x1000 + counter)` and rejects any id whose
	// 12-bit counter is >= 64 (measured live against both /zen/v1 endpoints,
	// deterministic; docs/recon-free-tier-gate.md). A raw sha256 prefix
	// carries random bits there, so ~63/64 of hash-derived sessions were
	// rejected with 403 FreeTierError on every model. The millisecond field
	// next to it is NOT validated (+/-365 days all pass).
	//
	// The JS source (open-sse/executors/opencode.js:97,
	// `digest.subarray(0, 6).toString("hex")`) has the same shape and is now
	// broken upstream too — an upstream behaviour change, not a porting error.
	//
	// Fix: keep the 36 high bits hash-derived (they carry the stickiness) and
	// force the 12 low bits to the legal `~1` counter encoding, so the id is
	// byte-identical to what generateSessionId emits for its first call in a
	// millisecond. Pinned by TestTranslateSessionIDCounterUnderUpstreamGateLimit.
	var w [8]byte
	copy(w[2:], sum[:6])
	x := binary.BigEndian.Uint64(w[:])
	x = (x &^ 0xfff) | (^uint64(1) & 0xfff)
	timeHex := fmt.Sprintf("%012x", x)
	random := make([]byte, 14)
	for i := 6; i < 20; i++ {
		random[i-6] = base62Chars[int(sum[i])%62]
	}
	return fmt.Sprintf("ses_%s%s", timeHex, string(random))
}

// IsValidSession reports whether s is already an upstream-shaped session id.
func IsValidSession(s string) bool {
	return SessionRE.MatchString(strings.TrimSpace(s))
}
