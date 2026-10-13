// Session/request id generation tests — ported from the golden vitest vectors
// in 9router tests/unit/opencode-session.test.js and the algorithms in
// open-sse/executors/opencode.js (generateSessionId / generateRequestId /
// translateSessionId / OPENCODE_SESSION_RE).
package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testFixedMilli is a wall-clock millisecond used ONLY by the determinism
// tests in this file (no other test may pass it to the generators, so the
// package-global per-millisecond counter starts from a known state).
const testFixedMilli int64 = 1700000000000 // 2023-11-14T22:13:20Z

// jsTimeHex mirrors the opencode.js BigInt encoding on uint64:
//
//	value = ms*0x1000 + counter   (BigInt(timestamp) * 0x1000n + BigInt(counter))
//	if invert: value = ~value     (two's complement, sign-extended)
//	time   = bytes 5..0 of value, big-endian hex (6 bytes → 12 chars)
//
// On uint64 the low 48 bits of ^v equal the infinite-precision two's
// complement, so the byte extraction is identical.
func jsTimeHex(ms, counter int64, invert bool) string {
	v := uint64(ms)*0x1000 + uint64(counter)
	if invert {
		v = ^v
	}
	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x",
		byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Session IDs must match the OpenCode canonical shape: ses_ + 12 lowercase
// hex + 14 base62, 30 chars total (golden: 20 samples, we take 500).
func TestGenerateSessionIDFormat(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := GenerateSessionID(time.Now())
		if len(id) != 30 {
			t.Fatalf("GenerateSessionID() length = %d, want 30 (%q)", len(id), id)
		}
		if !SessionRE.MatchString(id) {
			t.Fatalf("GenerateSessionID() = %q, does not match OPENCODE_SESSION_RE", id)
		}
	}
}

// GenerateRequestID has the same layout with the msg_ prefix; its time value
// is always ms*0x1000 + 1 (no counter, no bitwise NOT).
func TestGenerateRequestIDFormat(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := GenerateRequestID(time.Now())
		if len(id) != 30 {
			t.Fatalf("GenerateRequestID() length = %d, want 30 (%q)", len(id), id)
		}
		if !RequestRE.MatchString(id) {
			t.Fatalf("GenerateRequestID() = %q, does not match the msg_ id regex", id)
		}
	}
}

// The id embeds the wall-clock millisecond and a per-millisecond counter,
// NOT'ed, in the 12 hex chars: fixed-clock calls must produce the exact JS
// byte sequence (counter 1, then 2 for a same-millisecond repeat).
func TestGenerateSessionIDTimeEncoding(t *testing.T) {
	now := time.UnixMilli(testFixedMilli)
	first := GenerateSessionID(now)
	second := GenerateSessionID(now) // same millisecond → per-ms counter 2

	wantFirst := "ses_" + jsTimeHex(testFixedMilli, 1, true)  // 4301a97ffffe
	wantSecond := "ses_" + jsTimeHex(testFixedMilli, 2, true) // 4301a97ffffd
	if first[:16] != wantFirst {
		t.Errorf("first GenerateSessionID(fixed) time prefix = %q, want %q (JS ~ms*0x1000+1)", first[:16], wantFirst)
	}
	if second[:16] != wantSecond {
		t.Errorf("second GenerateSessionID(fixed) time prefix = %q, want %q (JS ~ms*0x1000+2)", second[:16], wantSecond)
	}
	// The 14 random chars stay base62.
	for _, id := range []string{first, second} {
		for _, c := range id[16:] {
			if !strings.ContainsRune(base62Chars, c) {
				t.Errorf("id %q has non-base62 random char %q", id, c)
			}
		}
	}
	// Golden constants so a refactor of jsTimeHex cannot silently drift.
	if first[:16] != "ses_4301a97ffffe" {
		t.Errorf("golden vector: got %q, want %q", first[:16], "ses_4301a97ffffe")
	}
}

// Request ids carry NO bitwise NOT and NO counter: the same timestamp always
// yields the same hex time prefix (ms*0x1000 + 1).
func TestGenerateRequestIDTimeEncoding(t *testing.T) {
	now := time.UnixMilli(testFixedMilli)
	a := GenerateRequestID(now)
	b := GenerateRequestID(now)
	want := "msg_" + jsTimeHex(testFixedMilli, 1, false) // bcfe56800001
	if a[:16] != want || b[:16] != want {
		t.Errorf("GenerateRequestID(fixed) time prefix = %q / %q, want %q", a[:16], b[:16], want)
	}
	if a[:16] != "msg_bcfe56800001" {
		t.Errorf("golden vector: got %q, want %q", a[:16], "msg_bcfe56800001")
	}
}

// Session and request ids never share a prefix.
func TestSessionAndRequestPrefixesDiffer(t *testing.T) {
	now := time.Now()
	if GenerateSessionID(now)[:4] != "ses_" {
		t.Error("session id must start with ses_")
	}
	if GenerateRequestID(now)[:4] != "msg_" {
		t.Error("request id must start with msg_")
	}
}

// translateSessionId hashes opencode\0<tool|generic>\0<sessionId> with sha256,
// maps digest[6:20] → BASE62_CHARS[b % 62], and takes the 12-hex prefix from
// the digest's TOP 36 bits with the low 12 bits forced to the `~1` counter
// encoding (see TranslateSessionID — the upstream gate rejects a counter >= 64).
func TestTranslateSessionIDGolden(t *testing.T) {
	cases := []struct {
		key  string
		tool string
		want string
	}{
		{"conversation-a", "claude", "ses_9b6cb3f23ffe6eKUOqkP4rAtBT"},
		{"claude:550e8400-e29b-41d4-a716-446655440000", "claude", "ses_ca4d828f2ffeuulSOfo9t9Qrlf"},
		{"session-from-codex", "generic", "ses_af05d6254ffe2IHGwdH2GUk6Jk"},
		{"", "", "ses_a7f9702bdffe5CgHPQUAk1uIqH"}, // empty tool falls back to "generic"
	}
	for _, tc := range cases {
		got := TranslateSessionID(tc.key, tc.tool)
		if got != tc.want {
			t.Errorf("TranslateSessionID(%q, %q) = %q, want %q", tc.key, tc.tool, got, tc.want)
		}
		if !SessionRE.MatchString(got) {
			t.Errorf("TranslateSessionID(%q, %q) = %q does not match OPENCODE_SESSION_RE", tc.key, tc.tool, got)
		}
	}
}

// Cross-check the implementation shape against an in-test recomputation of the
// digest mapping: the top 36 bits are the sha256 prefix, the low 12 are forced
// to `~1`, and digest[6:20] → BASE62_CHARS[b % 62]. The JS original keeps the
// whole 48 bits from the digest, which the upstream gate now rejects.
func TestTranslateSessionIDShapeMatchesJSAlgorithm(t *testing.T) {
	sum := sha256.Sum256([]byte("opencode\x00claude\x00conversation-a"))
	var w [8]byte
	copy(w[2:], sum[:6])
	x := binary.BigEndian.Uint64(w[:])
	x = (x &^ 0xfff) | (^uint64(1) & 0xfff)
	want := "ses_" + fmt.Sprintf("%012x", x)
	for i := 6; i < 20; i++ {
		want += string(base62Chars[int(sum[i])%62])
	}
	if got := TranslateSessionID("conversation-a", "claude"); got != want {
		t.Errorf("TranslateSessionID = %q, want %q (sha256 mapping)", got, want)
	}
}

// The upstream free-tier gate decodes the 12 hex chars into ~(millis*0x1000 +
// counter) and rejects any id whose counter is >= 64 (measured live 2026-10-13,
// deterministic). This pins the invariant the fix exists to keep: the decoded
// counter of a translated id must stay in [0, 63].
func TestTranslateSessionIDCounterUnderUpstreamGateLimit(t *testing.T) {
	decodeCounter := func(s string) uint64 {
		raw, err := strconv.ParseUint(strings.TrimPrefix(s, "ses_")[:12], 16, 64)
		if err != nil {
			t.Fatalf("prefix of %q is not hex: %v", s, err)
		}
		return (^raw) & 0xfff
	}
	keys := []string{"", "conversation-a", "session-from-codex", "claude:550e8400", strings.Repeat("k", 200)}
	for _, key := range keys {
		for _, tool := range []string{"", "generic", "claude", "codex"} {
			got := TranslateSessionID(key, tool)
			if c := decodeCounter(got); c > 63 {
				t.Errorf("TranslateSessionID(%q, %q) = %q decodes to counter %d, want <= 63 "+
					"(upstream rejects >= 64 with 403 FreeTierError)", key, tool, got, c)
			}
		}
	}
}

// Deterministic per (key, tool) pair; different keys and different tools must
// not collide (golden: "isolates different conversations and tools").
func TestTranslateSessionIDDeterministicAndIsolated(t *testing.T) {
	a1 := TranslateSessionID("conversation-a", "claude")
	a2 := TranslateSessionID("conversation-a", "claude")
	if a1 != a2 {
		t.Errorf("same key+tool produced %q and %q", a1, a2)
	}
	if TranslateSessionID("conversation-a", "claude") == TranslateSessionID("conversation-b", "claude") {
		t.Error("different conversations must hash differently")
	}
	if TranslateSessionID("same", "claude") == TranslateSessionID("same", "codex") {
		t.Error("different tools must hash differently")
	}
}

// Already-valid ids pass through without re-hashing (golden: preserves
// "ses_f534dfae8ffeCy4Ee4tLWNygDc" and its whitespace-padded form).
func TestTranslateSessionIDPreservesValidIDs(t *testing.T) {
	valid := "ses_f534dfae8ffeCy4Ee4tLWNygDc"
	if got := TranslateSessionID(valid, ""); got != valid {
		t.Errorf("TranslateSessionID(valid) = %q, want unchanged %q", got, valid)
	}
	if got := TranslateSessionID("  "+valid+"  ", ""); got != valid {
		t.Errorf("TranslateSessionID(padded valid) = %q, want trimmed %q", got, valid)
	}
}

// Session shape validation: prefix, exact segment lengths, lowercase-only hex
// time part, base62-only random part (the JS regexes are case-sensitive and
// untrimmed).
func TestSessionIDShapeValidation(t *testing.T) {
	hex12 := "f534dfae8ffe"
	b62_14 := "Cy4Ee4tLWNygDc"
	cases := []struct {
		id    string
		valid bool
		note  string
	}{
		{"ses_" + hex12 + b62_14, true, "golden canonical id"},
		{"ses_" + hex12 + b62_14, true, "canonical"},
		{"ses_000000000000AAAAAAAAAAAAAA", true, "boundary chars still valid"},
		{"msg_" + hex12 + b62_14, false, "wrong prefix"},
		{"SES_" + hex12 + b62_14, false, "prefix is case-sensitive"},
		{"ses_" + hex12[:11] + b62_14, false, "11 hex chars"},
		{"ses_" + hex12 + "0" + b62_14, false, "13 hex chars"},
		{"ses_" + hex12 + b62_14[:13], false, "13 base62 chars"},
		{"ses_" + hex12 + b62_14 + "x", false, "15 base62 chars"},
		{"ses_" + "F534DFAE8FFE" + b62_14, false, "hex part must be lowercase"},
		{"ses_" + hex12[:11] + "g" + b62_14, false, "'g' is not a hex digit"},
		{"ses_" + hex12 + b62_14[:13] + "!", false, "'!' is not base62"},
		{"ses_" + hex12 + b62_14[:13] + "é", false, "non-ASCII is not base62"},
		{"ses_" + hex12 + " " + b62_14[:13], false, "space is not base62"},
		{"ses_" + hex12, false, "missing random part"},
		{"ses_", false, "prefix only"},
		{"", false, "empty"},
		{" ses_" + hex12 + b62_14, false, "untrimmed input must fail the regex"},
	}
	for _, tc := range cases {
		if got := SessionRE.MatchString(tc.id); got != tc.valid {
			t.Errorf("SessionRE.MatchString(%q) [%s] = %v, want %v", tc.id, tc.note, got, tc.valid)
		}
	}
}

// IsValidSession trims before matching (nativeSession semantics).
func TestIsValidSessionTrims(t *testing.T) {
	valid := "ses_f534dfae8ffeCy4Ee4tLWNygDc"
	if !IsValidSession("  " + valid + "  ") {
		t.Error("IsValidSession must trim surrounding whitespace")
	}
	if IsValidSession("") || IsValidSession("   ") {
		t.Error("empty/blank ids are not valid sessions")
	}
	if IsValidSession("ses_zzzz") {
		t.Error("truncated id must not validate")
	}
}
