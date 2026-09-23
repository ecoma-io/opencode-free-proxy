// evidence_log.go renders one request's upstream evidence rows as zerolog
// events — the single emit boundary for the forensics layer (B4 in the
// evidence design): internal/upstream only COLLECTS rows, this file is the
// only place they become log lines.
//
// Level policy: every failed upstream interaction (dial/stream/forced) is one
// warn `upstream_error` event — failure evidence must survive the default
// info level; skip rows are scheduling diagnostics at debug. Successful
// requests emit nothing here (the completion line owns success telemetry), so
// the layer adds zero log volume to the happy path.
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"opencode-free-proxy/internal/upstream"

	"github.com/rs/zerolog"
)

// evidenceLog couples a request's recorder with the request-scoped facts only
// the router knows (correlation ids, model/endpoint/session pseudonym, body
// hash) and renders rows through them. Constructed once per request in relay,
// before the executor runs; Emit is called once the executor's decisions are
// complete, and the stream-phase helpers are called by the relay paths when a
// post-header phase dies. Not safe for concurrent use — a request's evidence
// is written and read on that request's goroutine.
type evidenceLog struct {
	log zerolog.Logger
	rec *upstream.Recorder

	requestID    string
	generation   uint64
	route        string
	model        string
	endpoint     string
	streaming    bool
	sessionFP    string
	upstreamHost string
	maxAttempts  int

	requestBytes int
	// bodyJSON is the already-serialized request slice relay holds anyway;
	// its sha256 is computed lazily — only when at least one row is emitted —
	// so the happy path never pays for hashing.
	bodyJSON []byte
	bodySHA  string

	// emitted counts the rows already rendered. Emit renders only rows
	// stored since the last pass, so a post-header abort (which appends a
	// row and emits again) never re-renders what the first pass already
	// logged: one row is one event, ever.
	emitted int
}

// newEvidenceLog builds the per-request renderer. sessionFP is a sha256
// pseudonym of the resolved upstream session id, NOT the id itself: session
// ids can name users/projects/conversations, so they never travel raw into
// logs. The pseudonym is stable per session for correlation while remaining
// opaque — it is a correlator, not anonymization (the operator could brute
// force known ids), and the docs say so.
func newEvidenceLog(log zerolog.Logger, rec *upstream.Recorder, requestID string, generation uint64, route, model, endpoint string, streaming bool, session string, upstreamBase string, maxAttempts int, bodyBytes int, bodyJSON []byte) *evidenceLog {
	sum := sha256.Sum256([]byte(session))
	host := ""
	if u, err := url.Parse(upstreamBase); err == nil {
		host = u.Host
	}
	return &evidenceLog{
		log:          log,
		rec:          rec,
		requestID:    requestID,
		generation:   generation,
		route:        route,
		model:        model,
		endpoint:     endpoint,
		streaming:    streaming,
		sessionFP:    hex.EncodeToString(sum[:])[:16],
		upstreamHost: host,
		maxAttempts:  maxAttempts,
		requestBytes: bodyBytes,
		bodyJSON:     bodyJSON,
	}
}

// attemptID derives the attempt correlation identity: request_id/N for egress
// attempt N (request abc → abc/1, abc/2, …). One attempt is one logical
// upstream call, so there is no dial suffix. Skips have no dial and therefore
// no attempt id — the egress field identifies them.
func (e *evidenceLog) attemptID(row upstream.Row) string {
	if row.Attempt < 1 {
		return ""
	}
	return fmt.Sprintf("%s/%d", e.requestID, row.Attempt)
}

// bodyHash lazily computes the request-body sha256 prefix. Hashing happens
// over bytes the request already holds (no extra retention — the slice is
// relay's own and outlives the log anyway) and only when evidence is emitted,
// so clean traffic never pays it.
func (e *evidenceLog) bodyHash() string {
	if e.bodySHA == "" && e.bodyJSON != nil {
		sum := sha256.Sum256(e.bodyJSON)
		e.bodySHA = hex.EncodeToString(sum[:])[:16]
	}
	return e.bodySHA
}

// Emit renders the rows collected so far and not yet rendered: dial and
// skip rows once the executor's decisions are complete, and — when a
// post-header phase later appends more (StreamAbort/ForcedAbort emit through
// this same cursor) — just those new rows. Dial/stream/forced rows render as
// warn upstream_error events, skip rows as debug egress_skipped diagnostics.
// The dropped counter (rows past the cap) rides the LAST event of the pass —
// whichever kind it is — so truncation is visible exactly where it happened.
func (e *evidenceLog) Emit() {
	rows := e.rec.Rows()
	dropped := e.rec.Dropped()
	for i := e.emitted; i < len(rows); i++ {
		var d int
		if i == len(rows)-1 {
			d = dropped
		}
		if rows[i].Phase == upstream.PhaseSkip {
			e.emitSkip(rows[i], d)
		} else {
			e.emitError(rows[i], d)
		}
	}
	e.emitted = len(rows)
}

// emitError renders one failed upstream interaction. Every field carrying
// text is already sanitized upstream of here; the rendered Msgf quotes all
// free text (%q) per the logforge discipline so nothing can forge a log line
// (CWE-117).
func (e *evidenceLog) emitError(row upstream.Row, dropped int) {
	evt := e.log.Warn()
	evt.Str("request_id", e.requestID)
	if id := e.attemptID(row); id != "" {
		evt.Str("attempt_id", id)
	}
	if row.Attempt > 0 {
		evt.Int("attempt", row.Attempt).Int("max_attempts", e.maxAttempts)
	}
	evt.Uint64("generation", e.generation).
		Str("route", e.route).
		Str("model", e.model).
		Str("endpoint", e.endpoint).
		Bool("streaming", e.streaming).
		Str("session_fp", e.sessionFP).
		Int("request_bytes", e.requestBytes).
		Str("request_body_sha256", e.bodyHash()).
		Str("upstream_host", e.upstreamHost).
		Str("phase", row.Phase).
		Str("egress", row.Egress)
	if row.EgressType != "" {
		evt.Str("egress_type", row.EgressType)
	}
	// The intent the attempt's egress was selected under (issue #56): the
	// request asked for `normal` (first attempt) or `new-egress` (a
	// replacement after a replay-safe failure). Recorded, never inferred from
	// the status, and never taken from the wire.
	if row.Intent != "" {
		evt.Str("egress_intent", row.Intent)
	}
	if row.Status > 0 {
		evt.Int("status", row.Status)
	}
	evt.Str("class", row.Class)
	// Failure provenance (issue #51): which side of the wire failed, at which
	// step of the egress path, and what the transport can prove about whether
	// the request went out. Rendered under distinct keys — `phase` is already
	// the ROW's lifecycle phase (response|transport|skip|stream|forced), a
	// different axis. Absent when there is nothing to attribute.
	if row.Origin != "" {
		evt.Str("origin", row.Origin)
	}
	if row.FailurePhase != "" {
		evt.Str("failure_phase", row.FailurePhase)
	}
	if row.RequestState != "" {
		evt.Str("request_state", row.RequestState)
	}
	if row.Reason != "" {
		evt.Str("reason", row.Reason)
	}
	if row.ErrType != "" {
		evt.Str("error_type", row.ErrType)
	}
	if row.ErrCode != "" {
		evt.Str("error_code", row.ErrCode)
	}
	if row.Message != "" {
		evt.Str("message", row.Message)
	}
	if row.Fingerprint != "" {
		evt.Str("error_fingerprint", row.Fingerprint)
	}
	if row.BodyPeek != "" {
		evt.Str("body_peek", row.BodyPeek)
	}
	if row.BodyBytes > 0 {
		evt.Int("response_bytes", row.BodyBytes)
		if row.Truncated {
			evt.Bool("response_truncated", true)
		}
	}
	if rl := row.RateLimit; rl != nil {
		if rl.RetryAfter != "" {
			evt.Str("retry_after", rl.RetryAfter)
		}
		for _, ent := range rl.Entries {
			evt.Str(strings.ToLower(ent.Name), ent.Value)
		}
	}
	evt.Int64("duration_ms", row.DurationMS)
	if row.InFlight > 0 {
		evt.Int("in_flight", row.InFlight)
	}
	if row.HealthDecision != "" {
		evt.Str("health_decision", row.HealthDecision)
	}
	if row.FallbackDecision != "" {
		evt.Str("fallback_decision", row.FallbackDecision)
	}
	if dropped > 0 {
		evt.Int("evidence_dropped", dropped)
	}
	evt.Msgf("upstream_error request_id=%s attempt=%q egress=%q phase=%s status=%d class=%s fallback_decision=%s message=%q",
		e.requestID, e.attemptID(row), row.Egress, row.Phase, row.Status, row.Class, row.FallbackDecision, row.Message)
}

// emitSkip renders a pass-over as a compact debug diagnostic — a skip is a
// scheduling fact (slot full between plan and dial, transport build failure,
// a plan entry that left the snapshot), not an upstream error, and slot-full
// skips can be frequent under load: debug keeps them out of the info stream
// while remaining available when an investigation raises the level.
func (e *evidenceLog) emitSkip(row upstream.Row, dropped int) {
	evt := e.log.Debug().
		Str("request_id", e.requestID).
		Uint64("generation", e.generation).
		Str("route", e.route).
		Str("egress", row.Egress)
	if row.EgressType != "" {
		evt.Str("egress_type", row.EgressType)
	}
	if row.Intent != "" {
		evt.Str("egress_intent", row.Intent)
	}
	evt.Str("reason", row.Reason)
	if dropped > 0 {
		evt.Int("evidence_dropped", dropped)
	}
	evt.Msgf("egress_skipped request_id=%s egress=%q reason=%s", e.requestID, row.Egress, row.Reason)
}

// StreamAbort records a post-header stream death — the phase where a live
// response was already delivered downstream and commitment has long since
// been made. The row's class is response_started (the reserved taxonomy
// boundary, produced HERE and only here): it is a logging classification of
// the failure phase, never an HTTP verdict about the dial — no health
// observation, no fallback, and the already-delivered status is recorded as
// status, never rewritten into a 4xx/5xx.
func (e *evidenceLog) StreamAbort(egress string, deliveredStatus int, origin upstream.Origin, reason string, durMS int64) {
	e.appendAndEmit(upstream.Row{
		Phase:        upstream.PhaseStream,
		Egress:       egress,
		Status:       deliveredStatus,
		Class:        upstream.ClassResponseStarted.String(),
		Origin:       origin.String(),
		FailurePhase: upstream.FailurePhaseResponseBody.String(),
		RequestState: upstream.RequestStateResponseStarted.String(),
		Reason:       reason,
		DurationMS:   durMS,
	})
}

// ForcedAbort records a failed forced SSE→JSON conversion (stall/size cap/
// client cancel/read error/malformed stream). upstreamStatus is the status
// the response STARTED with — the client keeps seeing the generic 502, but a
// phase row records what the upstream did, never the synthesized client
// verdict; the reason travels only here.
func (e *evidenceLog) ForcedAbort(egress string, upstreamStatus int, origin upstream.Origin, reason string, durMS int64) {
	e.appendAndEmit(upstream.Row{
		Phase:        upstream.PhaseForced,
		Egress:       egress,
		Status:       upstreamStatus,
		Class:        upstream.ClassResponseStarted.String(),
		Origin:       origin.String(),
		FailurePhase: upstream.FailurePhaseResponseBody.String(),
		RequestState: upstream.RequestStateResponseStarted.String(),
		Reason:       reason,
		DurationMS:   durMS,
	})
}

// appendAndEmit stores one post-header row and renders it — and only it —
// immediately: the rows the executor collected were already rendered by the
// first Emit, and re-rendering them would duplicate every event (one row is
// one event, ever — the layer's emit contract). When the recorder refuses
// the row (cap full) it still renders — the interaction happened and must
// not vanish; the transient row is bounded like any other, and
// evidence_dropped on the event says the recorder overflowed.
func (e *evidenceLog) appendAndEmit(row upstream.Row) {
	e.rec.Append(row)
	e.emitted = e.rec.Len()
	e.emitError(row, e.rec.Dropped())
}
