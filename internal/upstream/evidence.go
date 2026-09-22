// evidence.go is the strictly-observational forensics layer for upstream
// failures: a request-scoped Recorder collecting one bounded Row per failed
// dial, skipped plan entry, and post-header stream phase. NOTHING in this file
// influences routing, retry, fallback, health, or streaming behavior — rows
// are written where the facts become known (client.go verdict boundary,
// fallback.go decision boundary, router stream phases) and read only by the
// router's log renderer. Per AGENTS.md rule 7 the recorder is request-scoped
// process state: nothing survives a reload, nothing resets, only the owning
// request reads it.
package upstream

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
)

// Row phases. A phase names WHERE in the lifecycle the failure was observed —
// never a re-classification: a stream row records a failure that happened
// AFTER a live response was already delivered downstream, so it can never be
// an HTTP 4xx/5xx verdict about the dial.
const (
	PhaseResponse  = "response"  // an upstream HTTP response with status >= 400
	PhaseTransport = "transport" // dial/TLS/proxy-protocol failure, no HTTP response exists
	PhaseSkip      = "skip"      // a plan entry was passed over without dialing
	PhaseStream    = "stream"    // live response died after headers (commitment already made)
	PhaseForced    = "forced"    // forced SSE→JSON conversion failed after headers
)

// Skip reasons (fallback.go pass-over branches). A skip is a scheduling fact,
// never a failure: no health observation, no retry-budget draw.
const (
	SkipSlotFull       = "slot_full"
	SkipTransportBuild = "transport_build"
	SkipUnknownEgress  = "unknown_egress"
)

// RetryDecision names what happened after the dial this row describes.
const (
	RetryRetrySameEgress = "retry_same_egress" // the per-egress matrix retried
	RetryFallback        = "fallback"          // the executor moved to the next egress
	RetryStop            = "stop"              // terminal: returned to the caller
)

// HealthDecision names the health observation this dial produced. There is
// deliberately no "reset" value: a successful dial gets NO row (the completion
// line owns success telemetry), so every row is a failure observation and only
// marked (poisoned the streak) or neutral (429/4xx/cancel — verdicts about the
// request, not the egress) can appear.
const (
	HealthMarked  = "marked"
	HealthNeutral = "neutral"
)

// Row is one upstream interaction's forensic record. Every string field that
// can carry upstream- or attacker-controlled text passes through
// sanitizeEvidence before it lands here; secrets are structurally excluded —
// request bodies, authorization/cookie headers, and session ids are never
// inputs to any field (rates/identity travel as bounded header names, parsed
// error type/code, and fingerprints).
type Row struct {
	Phase string
	// Egress id + proxy type, annotated by the executor (the client layer
	// dials a transport and does not know the egress it belongs to).
	Egress     string
	EgressType string // direct|http|https|socks5, from the snapshot's config.Egress
	// Attempt is the 1-based cross-egress attempt number; Dial is the 1-based
	// dial index inside that attempt (> 1 only when the retry matrix retried).
	// The renderer derives attempt_id = request_id/Attempt[.Dial].
	Attempt int
	Dial    int
	// Reason carries the skip reason or the stream/forced failure phase
	// (stall/read_error/client_disconnect/too_large/…); empty for dial rows.
	Reason string
	// Status is the upstream HTTP status; 0 when no HTTP response exists
	// (transport/skip phases) and the delivered status for stream rows.
	Status int
	Class  string // Class.String(); stream rows use ClassResponseStarted

	ErrType  string // parsed error.type from a structured upstream error body
	ErrCode  string // parsed error.code
	Message  string // sanitized upstream or transport message
	BodyPeek string // sanitized prefix of the already-read capped error body
	// BodyBytes is how much of the error body was read (bounded by
	// maxErrorBodyBytes); Truncated marks that the peek had to be clamped.
	BodyBytes int
	Truncated bool

	Fingerprint string
	RateLimit   *RateLimit

	// MatrixDraws is the per-egress shared retry budget consumed through
	// this dial; Retried marks that THIS dial was retried (set only after
	// tryRetry decided — a row never claims a retry before the matrix made
	// it) and RetryDelayMS the delay that followed.
	MatrixDraws  int
	Retried      bool
	RetryDelayMS int64

	DurationMS int64
	// InFlight is the egress's concurrency occupancy at dial time; 0 when
	// the egress is uncapped (the limiter does not track those).
	InFlight int

	HealthDecision string
	RetryDecision  string
}

// RateLimit is the safe extraction of a response's rate-limit headers.
// Present-only semantics: a header the response did not carry is simply an
// absent field — missing information is never converted to a zero value.
type RateLimit struct {
	RetryAfter string           // bounded echo of Retry-After when present
	Entries    []RateLimitEntry // every other header whose name matches rate-limit-ish
}

// RateLimitEntry is one rate-limit header observation. Name is the canonical
// header name (operator/upstream controlled, safe); Value is clamped and
// control-stripped.
type RateLimitEntry struct {
	Name  string
	Value string
}

// Recorder accumulates one request's rows. Nil-safe: every method accepts a
// nil receiver so decision paths never branch on whether forensics is wired.
// Single-writer by construction (the executor loop and the stream phases run
// sequentially within one request); the mutex exists so Rows() snapshots stay
// safe regardless.
type Recorder struct {
	mu      sync.Mutex
	rows    []Row
	dropped int
}

// NewRecorder returns an empty per-request recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Append stores one row, or — past EvidenceMaxRows — counts it as dropped so
// a hostile upstream cannot grow recorder memory without bound.
func (r *Recorder) Append(row Row) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) >= config.EvidenceMaxRows {
		r.dropped++
		return
	}
	r.rows = append(r.rows, row)
}

// Len reports the stored row count (0 for a nil recorder).
func (r *Recorder) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// Annotate applies f to rows[from:] — the executor's completion pass over one
// egress attempt's rows, filling egress/attempt identity and the health +
// retry decisions it just made. from is a Len() captured before the attempt
// dialed; out-of-range indices are clamped, so a stale index can never panic.
func (r *Recorder) Annotate(from int, f func(*Row)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from < 0 {
		from = 0
	}
	if from > len(r.rows) {
		return
	}
	for i := from; i < len(r.rows); i++ {
		f(&r.rows[i])
	}
}

// Rows returns a snapshot copy.
func (r *Recorder) Rows() []Row {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Row, len(r.rows))
	copy(out, r.rows)
	return out
}

// Dropped reports how many rows past the cap were discarded.
func (r *Recorder) Dropped() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// sanitizeEvidence makes attacker-controlled text safe for a log field:
// control bytes (< 0x20 and DEL — the CRLF set log forging needs, CWE-117)
// become spaces, whitespace runs collapse to one space, and the result is
// clamped to limit bytes without splitting a UTF-8 sequence (backing off to a
// rune boundary, then trimming a trailing partial escape-free prefix).
func sanitizeEvidence(s string, limit int) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			c = ' '
		}
		if c == ' ' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteByte(c)
	}
	out := b.String()
	if len(out) > limit {
		out = out[:limit]
		// Never emit a partial rune: back off over invalid trailing encodings
		// (dangling continuation bytes of a cut sequence AND a leading byte
		// left without its sequence — DecodeLastRuneInString flags both as
		// RuneError with size 1). A mid-rune cut would otherwise smuggle
		// invalid UTF-8 into the JSON log.
		for len(out) > 0 {
			r, size := utf8.DecodeLastRuneInString(out)
			if r != utf8.RuneError || size > 1 {
				break
			}
			out = out[:len(out)-1]
		}
	}
	return strings.TrimSpace(out)
}

// isAddressByte is the byte set an address-shaped token may contain once digit
// runs have folded to '#' (hostname letters/digits, separators, the folded
// digit marker, and IPv6 brackets/at).
func isAddressByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '#' || c == '[' || c == ']' || c == '@'
}

// looksLikeAddressToken reports whether a (digit-folded, lowercased) token
// looks like a host address the fingerprint must not depend on: a dotted
// hostname or IPv4 literal, optionally with a folded :# port, or an IPv6-ish
// token (brackets / '::'). Words like "connect:" or "tcp" never match — no
// dotted host part and no '::'.
func looksLikeAddressToken(tok string) bool {
	if strings.Contains(tok, "::") {
		return true
	}
	host, port, hasPort := strings.Cut(tok, ":")
	for i := 0; i < len(host); i++ {
		if !isAddressByte(host[i]) {
			return false
		}
	}
	if !strings.Contains(host, ".") {
		return false
	}
	if !hasPort {
		return true
	}
	// The port must have folded to '#' runs ("host:#+").
	for i := 0; i < len(port); i++ {
		if port[i] != '#' {
			return false
		}
	}
	return true
}

// NormalizeMessage reduces a message to its stable semantic core for
// fingerprinting: control-stripped, whitespace-collapsed, lowercased, digit
// runs folded to '#' ("retry in 17 seconds" ≡ "retry in 31 seconds"), and
// address-shaped tokens (IP literals, host:port, dotted hostnames) removed so
// the SAME logical transport error yields the SAME fingerprint regardless of
// which egress dialed it. Bounded to 256 bytes (after the fold; the caller
// already sanitized the raw text).
func NormalizeMessage(s string) string {
	s = strings.ToLower(sanitizeEvidence(s, 512))
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			// Fold the whole run to a single '#'.
			b.WriteByte('#')
			for i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	folded := b.String()
	fields := strings.Fields(folded)
	kept := make([]string, 0, len(fields))
	for _, tok := range fields {
		if looksLikeAddressToken(tok) {
			continue
		}
		kept = append(kept, tok)
	}
	out := strings.Join(kept, " ")
	if len(out) > 256 {
		out = out[:256]
	}
	return out
}

// Fingerprint is the deterministic identity of a logical upstream error:
// FNV-1a 64 over status|type|code|NormalizeMessage(message). Non-cryptographic
// by design — its only job is equality grouping across attempts/requests —
// and NEVER an input to behavior. status is 0 for transport errors, whose
// fingerprint additionally folds the class in (errType) so a timeout and a
// connection refusal never collide.
func Fingerprint(status int, errType, errCode, message string) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s|%s|%s", status, errType, errCode, NormalizeMessage(message))
	return fmt.Sprintf("%016x", h.Sum64())
}

// rateLimitNameFragments are the substrings (lowercased) a header name must
// contain to count as rate-limit evidence. Deliberately broader than any one
// vendor's scheme (x-ratelimit-*, ratelimit-*, x-rate-limit-*) because the
// upstream's naming is unknown and must not be assumed; equally deliberately
// NOT a catch-all: authorization/cookie/set-cookie names can never match, so
// credential-bearing headers are structurally unreachable.
var rateLimitNameFragments = []string{"ratelimit", "rate-limit"}

// ExtractRateLimit collects the rate-limit evidence a response carries. The
// returned RateLimit is nil when the response has none (present/absent
// semantics — absence is the observation). Entries are sorted by name because
// http.Header map iteration is randomized and the log output must be stable.
func ExtractRateLimit(h http.Header) *RateLimit {
	if h == nil {
		return nil
	}
	rl := &RateLimit{}
	if v := h.Get("Retry-After"); v != "" {
		rl.RetryAfter = sanitizeEvidence(v, config.EvidenceRateLimitValueBytes)
	}
	for name, vals := range h {
		lower := strings.ToLower(name)
		if lower == "retry-after" {
			continue
		}
		matched := false
		for _, frag := range rateLimitNameFragments {
			if strings.Contains(lower, frag) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, v := range vals {
			rl.Entries = append(rl.Entries, RateLimitEntry{
				Name:  sanitizeEvidence(name, config.EvidenceRateLimitValueBytes),
				Value: sanitizeEvidence(v, config.EvidenceRateLimitValueBytes),
			})
		}
	}
	if rl.RetryAfter == "" && len(rl.Entries) == 0 {
		return nil
	}
	sort.Slice(rl.Entries, func(i, j int) bool { return rl.Entries[i].Name < rl.Entries[j].Name })
	if len(rl.Entries) > config.EvidenceRateLimitEntries {
		rl.Entries = rl.Entries[:config.EvidenceRateLimitEntries]
	}
	return rl
}

// appendTransportRow records a failed dial that produced no HTTP response
// (connection/timeout/proxy-auth/context). The transport fingerprint folds the
// class into the errType slot so a timeout and a connection refusal never
// collide, and NormalizeMessage strips the dial address so the same logical
// failure through a different egress fingerprints identically. retried is set
// only by callers that already saw tryRetry succeed.
func appendTransportRow(rec *Recorder, dial int, dur time.Duration, draws int, retried bool, delay time.Duration, class Class, err error) {
	if rec == nil {
		return
	}
	decision := ""
	if retried {
		decision = RetryRetrySameEgress
	}
	rec.Append(Row{
		Phase:         PhaseTransport,
		Dial:          dial,
		Class:         class.String(),
		Message:       sanitizeEvidence(err.Error(), config.EvidenceMessageBytes),
		Fingerprint:   Fingerprint(0, class.String(), "", err.Error()),
		DurationMS:    dur.Milliseconds(),
		MatrixDraws:   draws,
		Retried:       retried,
		RetryDelayMS:  delay.Milliseconds(),
		RetryDecision: decision,
	})
}

// appendResponseRow records an upstream HTTP error verdict. raw is the SAME
// capped slice the caller already read for parseUpstreamError — never a
// second body read; it is nil for a retried verdict whose body went straight
// to the drain (headers-only row, no message/peek), with uerr nil alongside.
func appendResponseRow(rec *Recorder, dial int, dur time.Duration, draws int, retried bool, delay time.Duration, status int, h http.Header, raw []byte, uerr *UpstreamError, class Class) {
	if rec == nil {
		return
	}
	msg := ""
	if uerr != nil {
		msg = sanitizeEvidence(uerr.Message, config.EvidenceMessageBytes)
	}
	et, ec := "", ""
	if raw != nil {
		et, ec = parseErrorFields(raw)
	}
	decision := ""
	if retried {
		decision = RetryRetrySameEgress
	}
	row := Row{
		Phase:         PhaseResponse,
		Dial:          dial,
		Status:        status,
		Class:         class.String(),
		ErrType:       sanitizeEvidence(et, 64),
		ErrCode:       sanitizeEvidence(ec, 64),
		Message:       msg,
		BodyBytes:     len(raw),
		Truncated:     raw != nil && len(raw) > config.EvidencePeekBytes,
		Fingerprint:   Fingerprint(status, et, ec, msg),
		RateLimit:     ExtractRateLimit(h),
		DurationMS:    dur.Milliseconds(),
		MatrixDraws:   draws,
		Retried:       retried,
		RetryDelayMS:  delay.Milliseconds(),
		RetryDecision: decision,
	}
	if !retried {
		row.BodyPeek = sanitizeEvidence(string(raw), config.EvidencePeekBytes)
	}
	rec.Append(row)
}

// parseErrorFields extracts error.type/error.code from a structured upstream
// error body. A sibling of parseUpstreamError — it consumes the SAME capped
// raw slice the parser already read (never a second body read) and mirrors
// its jsonx semantics without touching the ported parser's behavior. Missing
// or unstructured bodies return "" for both, which the renderer omits.
func parseErrorFields(raw []byte) (errType, errCode string) {
	if !json.Valid(raw) {
		return "", ""
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		return "", ""
	}
	errObj := jsonx.AsObj(parsed["error"])
	if errObj == nil {
		return "", ""
	}
	return jsonx.AsStr(errObj["type"]), jsonx.AsStr(errObj["code"])
}
