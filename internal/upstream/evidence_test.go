package upstream

// Unit tests for the strictly-observational forensics layer (evidence.go):
// sanitizer, message normalization + fingerprint determinism, rate-limit
// extraction, and the bounded/nil-safe recorder.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"opencode-free-proxy/internal/config"
)

func TestSanitizeEvidence(t *testing.T) {
	t.Run("control bytes become spaces and whitespace collapses", func(t *testing.T) {
		got := sanitizeEvidence("a\r\nb\t c\x00d\x7fe  f", 512)
		if got != "a b c d e f" {
			t.Fatalf("sanitizeEvidence = %q, want %q", got, "a b c d e f")
		}
	})
	t.Run("truncation is rune-safe", func(t *testing.T) {
		// 6 bytes of CJK text; a 4-byte clamp would split the 2nd rune.
		got := sanitizeEvidence("日本語です", 4)
		if got != "日" {
			t.Fatalf("clamp = %q, want %q (no partial rune)", got, "日")
		}
		// The input-side byte clamp can leave the same split — a limit that
		// lands mid-rune must still back off to a rune boundary.
		if got := sanitizeEvidence("日本語", 7); got != "日本" {
			t.Fatalf("input-clamped sanitize = %q, want %q", got, "日本")
		}
	})
	t.Run("leading and trailing whitespace is trimmed", func(t *testing.T) {
		if got := sanitizeEvidence("  x  ", 512); got != "x" {
			t.Fatalf("trim = %q, want x", got)
		}
	})
	t.Run("empty stays empty", func(t *testing.T) {
		if got := sanitizeEvidence("", 512); got != "" {
			t.Fatalf("empty = %q", got)
		}
	})
}

func TestNormalizeMessage(t *testing.T) {
	t.Run("digit runs fold so volatile numbers converge", func(t *testing.T) {
		a := NormalizeMessage("Rate limited — retry in 17 seconds")
		b := NormalizeMessage("Rate limited — retry in 31 seconds")
		if a != b {
			t.Fatalf("normalized %q != %q: the same logical error must fold", a, b)
		}
	})
	t.Run("distinct messages stay distinct", func(t *testing.T) {
		if NormalizeMessage("quota exceeded") == NormalizeMessage("model overloaded") {
			t.Fatal("semantically different messages must not collide")
		}
	})
	t.Run("IPv4 host:port tokens are stripped", func(t *testing.T) {
		a := NormalizeMessage("dial tcp 10.1.2.3:443: connect: connection refused")
		b := NormalizeMessage("dial tcp 192.168.0.9:8443: connect: connection refused")
		if a != b {
			t.Fatalf("address-dependent fingerprints: %q != %q", a, b)
		}
		if strings.Contains(a, "10.") || strings.Contains(a, "192.") {
			t.Fatalf("address survived normalization: %q", a)
		}
	})
	t.Run("IPv6 tokens are stripped", func(t *testing.T) {
		a := NormalizeMessage("dial tcp [2001:db8::1]:443: timeout")
		b := NormalizeMessage("dial tcp [::1]:8443: timeout")
		if a != b {
			t.Fatalf("IPv6 address survived normalization: %q vs %q", a, b)
		}
	})
	t.Run("non-address words survive", func(t *testing.T) {
		got := NormalizeMessage("connect: connection refused by api.example.com:443")
		// "connect:" has no numeric port; the host:port (the shape that varies
		// across egresses) IS stripped.
		if !strings.Contains(got, "connect:") {
			t.Fatalf("plain word folded away: %q", got)
		}
		if strings.Contains(got, "example.com") {
			t.Fatalf("host:port survived: %q", got)
		}
	})
	t.Run("dotted identifiers are not addresses", func(t *testing.T) {
		// "config.yaml" / "secrets.env" are filenames, not hosts: stripping
		// them would merge two different logical errors into one fingerprint.
		if NormalizeMessage("failed parsing config.yaml") == NormalizeMessage("failed parsing secrets.env") {
			t.Fatal("dotted identifiers must keep distinguishing fingerprints")
		}
		// Dotted hosts WITH a numeric segment are still addresses.
		if strings.Contains(NormalizeMessage("dial tcp 10.0.0.1 refused"), "10.") {
			t.Fatal("dotted numeric host survived")
		}
	})
}

func TestFingerprint(t *testing.T) {
	t.Run("deterministic and normalization-aware", func(t *testing.T) {
		a := Fingerprint(429, "rate_limit", "too_many", "retry in 17 seconds")
		b := Fingerprint(429, "rate_limit", "too_many", "retry in 31 seconds")
		if a != b {
			t.Fatal("volatile-digit messages must fingerprint identically")
		}
		if len(a) != 16 {
			t.Fatalf("fingerprint = %q, want 16 hex chars", a)
		}
	})
	t.Run("status, type and code are distinct inputs", func(t *testing.T) {
		base := Fingerprint(429, "t", "c", "m")
		if Fingerprint(500, "t", "c", "m") == base {
			t.Fatal("status must change the fingerprint")
		}
		if Fingerprint(429, "t2", "c", "m") == base {
			t.Fatal("error type must change the fingerprint")
		}
		if Fingerprint(429, "t", "c2", "m") == base {
			t.Fatal("error code must change the fingerprint")
		}
	})
	t.Run("transport classes never collide", func(t *testing.T) {
		// Transport rows fold the class into the errType slot (status 0), so a
		// timeout and a refusal on the same address text stay distinguishable.
		timeout := Fingerprint(0, ClassTimeout.String(), "", "dial tcp 1.2.3.4:443: boom")
		refusal := Fingerprint(0, ClassConnectionError.String(), "", "dial tcp 1.2.3.4:443: boom")
		if timeout == refusal {
			t.Fatal("timeout and connection_error must not collide")
		}
	})
	t.Run("address independence across egresses", func(t *testing.T) {
		a := Fingerprint(0, ClassConnectionError.String(), "", "dial tcp 1.2.3.4:443: connection refused")
		b := Fingerprint(0, ClassConnectionError.String(), "", "dial tcp 9.8.7.6:1080: connection refused")
		if a != b {
			t.Fatal("the same logical failure through different addresses must match")
		}
	})
}

func TestExtractRateLimit(t *testing.T) {
	t.Run("absent headers yield nil, not a zero struct", func(t *testing.T) {
		h := http.Header{"Content-Type": []string{"application/json"}}
		if rl := ExtractRateLimit(h); rl != nil {
			t.Fatalf("ExtractRateLimit = %+v, want nil (present/absent semantics)", rl)
		}
		if ExtractRateLimit(nil) != nil {
			t.Fatal("nil header must yield nil")
		}
	})
	t.Run("Retry-After plus vendor rate-limit headers are captured sorted", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "17")
		h.Set("X-RateLimit-Limit", "100")
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset", "1735689600")
		h.Set("X-Rate-Limit-Policy", "100 per minute") // dash spelling also matches
		h.Set("Content-Type", "application/json")
		h.Set("Set-Cookie", "session=e2e-secret") // must never match

		rl := ExtractRateLimit(h)
		if rl == nil {
			t.Fatal("rate-limit headers present, got nil")
		}
		if rl.RetryAfter != "17" {
			t.Fatalf("RetryAfter = %q", rl.RetryAfter)
		}
		want := []string{
			"X-Rate-Limit-Policy",
			"X-Ratelimit-Limit",
			"X-Ratelimit-Remaining",
			"X-Ratelimit-Reset",
		}
		if len(rl.Entries) != len(want) {
			t.Fatalf("entries = %+v, want %v", rl.Entries, want)
		}
		for i, name := range want {
			if rl.Entries[i].Name != name {
				t.Fatalf("entry %d = %s, want %s (sorted, canonical)", i, rl.Entries[i].Name, name)
			}
		}
		for _, ent := range rl.Entries {
			if strings.Contains(strings.ToLower(ent.Name), "cookie") || strings.Contains(ent.Value, "e2e-secret") {
				t.Fatalf("credential header leaked: %+v", ent)
			}
		}
	})
	t.Run("values are sanitized and clamped", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "17\r\nFAKE-LOG-LINE")
		h.Set("X-RateLimit-Reset", strings.Repeat("9", config.EvidenceRateLimitValueBytes+50))
		rl := ExtractRateLimit(h)
		if rl == nil {
			t.Fatal("got nil")
		}
		if strings.ContainsRune(rl.RetryAfter, '\n') || strings.ContainsRune(rl.RetryAfter, '\r') {
			t.Fatalf("CRLF survived into retry_after: %q", rl.RetryAfter)
		}
		if len(rl.Entries) != 1 || len(rl.Entries[0].Value) > config.EvidenceRateLimitValueBytes {
			t.Fatalf("value clamp failed: %+v", rl.Entries)
		}
	})
	t.Run("entry list is capped", func(t *testing.T) {
		h := http.Header{}
		for i := 0; i < config.EvidenceRateLimitEntries+5; i++ {
			h.Set(fmt.Sprintf("X-RateLimit-More-%d", i), "1")
		}
		rl := ExtractRateLimit(h)
		if rl == nil || len(rl.Entries) != config.EvidenceRateLimitEntries {
			t.Fatalf("entries = %d, want capped at %d", len(rl.Entries), config.EvidenceRateLimitEntries)
		}
	})
	t.Run("a header carried multiple times merges into one entry", func(t *testing.T) {
		// One canonical name must render as ONE JSON key — duplicate keys in
		// a single JSON object would let last-wins parsers drop evidence.
		h := http.Header{}
		h.Add("X-RateLimit-Remaining", "42")
		h.Add("X-RateLimit-Remaining", "7")
		rl := ExtractRateLimit(h)
		if rl == nil || len(rl.Entries) != 1 {
			t.Fatalf("entries = %+v, want a single merged entry", rl)
		}
		// Go canonicalizes the name (X-RateLimit → X-Ratelimit); values join
		// in the sorted order the extractor pinned for determinism.
		if rl.Entries[0].Name != "X-Ratelimit-Remaining" || rl.Entries[0].Value != "42, 7" {
			t.Fatalf("merged entry = %+v, want %q: %q", rl.Entries[0], "X-Ratelimit-Remaining", "42, 7")
		}
	})
}

func TestRecorderBoundedAndNilSafe(t *testing.T) {
	t.Run("nil recorder is a no-op everywhere", func(t *testing.T) {
		var rec *Recorder
		rec.Append(Row{Phase: PhaseSkip})
		if rec.Len() != 0 || rec.Dropped() != 0 || rec.Rows() != nil {
			t.Fatal("nil recorder must stay inert")
		}
		rec.Annotate(0, func(*Row) { t.Fatal("must not run on nil") })
	})
	t.Run("rows past the cap are counted, not stored", func(t *testing.T) {
		rec := NewRecorder()
		for i := 0; i < config.EvidenceMaxRows; i++ {
			rec.Append(Row{Phase: PhaseSkip, Egress: fmt.Sprint(i)})
		}
		rec.Append(Row{Phase: PhaseSkip, Egress: "overflow"})
		if rec.Len() != config.EvidenceMaxRows || rec.Dropped() != 1 {
			t.Fatalf("len=%d dropped=%d, want %d/1", rec.Len(), rec.Dropped(), config.EvidenceMaxRows)
		}
		for _, row := range rec.Rows() {
			if row.Egress == "overflow" {
				t.Fatal("overflow row must not be stored")
			}
		}
	})
	t.Run("Rows returns a defensive copy", func(t *testing.T) {
		rec := NewRecorder()
		rec.Append(Row{Phase: PhaseSkip, Egress: "a"})
		rows := rec.Rows()
		rows[0].Egress = "mutated"
		if rec.Rows()[0].Egress != "a" {
			t.Fatal("Rows() must be a snapshot copy")
		}
	})
	t.Run("Annotate clamps out-of-range indices", func(t *testing.T) {
		rec := NewRecorder()
		rec.Append(Row{Phase: PhaseSkip, Egress: "a"})
		rec.Annotate(-5, func(row *Row) { row.Egress = "touched" })
		rec.Annotate(9, func(row *Row) { row.Egress = "touched-late" })
		if got := rec.Rows()[0].Egress; got != "touched" {
			t.Fatalf("clamped-negative annotate = %q, want touched", got)
		}
	})
	t.Run("AnnotateAt touches exactly one row", func(t *testing.T) {
		var nilRec *Recorder
		nilRec.AnnotateAt(0, func(*Row) { t.Fatal("must not run on nil") })

		rec := NewRecorder()
		rec.Append(Row{Phase: PhaseSkip, Egress: "a"})
		rec.Append(Row{Phase: PhaseResponse, Egress: "b"})
		rec.AnnotateAt(1, func(row *Row) { row.RetryDecision = RetryStop })
		rec.AnnotateAt(99, func(row *Row) { row.Message = "boom" }) // out of range: no-op
		rows := rec.Rows()
		if rows[0].RetryDecision != "" || rows[0].Message != "" {
			t.Fatalf("AnnotateAt leaked past its row: %+v", rows[0])
		}
		if rows[1].RetryDecision != RetryStop || rows[1].Message != "" {
			t.Fatalf("AnnotateAt missed its row: %+v", rows[1])
		}
	})
}

func TestParseErrorFields(t *testing.T) {
	t.Run("structured type and code are extracted", func(t *testing.T) {
		et, ec := parseErrorFields([]byte(`{"error":{"message":"m","type":"rate_limit_error","code":"429"}}`))
		if et != "rate_limit_error" || ec != "429" {
			t.Fatalf("type=%q code=%q", et, ec)
		}
	})
	t.Run("missing or unstructured bodies yield empty strings", func(t *testing.T) {
		for _, raw := range [][]byte{
			nil,
			[]byte("plain text"),
			[]byte(`{"error":"a string, not an object"}`),
			[]byte(`{"no_error":true}`),
		} {
			if et, ec := parseErrorFields(raw); et != "" || ec != "" {
				t.Fatalf("parseErrorFields(%q) = %q/%q, want empty", raw, et, ec)
			}
		}
	})
}

// TestSuccessProducesNoRow pins the layer's level policy at its source: the
// happy path is the completion line's job, so nothing but failures and skips
// ever become rows. appendResponseRow/appendTransportRow are only invoked on
// failed dials by construction; a nil recorder must also stay silent.
func TestSuccessProducesNoRow(t *testing.T) {
	var rec *Recorder
	appendTransportRow(rec, 1, 0, 0, false, 0, ClassSuccess, nil)
	appendResponseRow(rec, 1, 0, 0, false, 0, 200, nil, nil, nil, ClassSuccess)
	if rec.Len() != 0 {
		t.Fatal("nil recorder must remain inert")
	}
}
