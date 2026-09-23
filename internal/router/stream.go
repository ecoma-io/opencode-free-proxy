package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"opencode-free-proxy/internal/cloak"
	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/relay"
	"opencode-free-proxy/internal/upstream"
)

// lineRelay is the per-request SSE relay contract. ProcessTail receives an
// unterminated final segment (upstream closed mid-line): passthrough forwards
// it raw (stream.js flush), translate re-parses it like any line.
type lineRelay interface {
	ProcessLine(line string) error
	ProcessTail(line string) error
	Flush() error
}

// htmlTitleRe pulls the <title> out of an upstream HTML error page.
var htmlTitleRe = regexp.MustCompile(`(?i)<title>([^<]+)</title>`)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// maxNonSSEBodyBytes bounds the non-SSE guard's body read. JS reads the whole
// body unbounded (`await providerResponse.text()`, streamingHandler.js:64);
// the guard only ever uses the <title> or a raw body under 200 bytes, both
// far below this bound (Go divergence: no unbounded reads of upstream bytes).
const maxNonSSEBodyBytes = 1 << 20

// stream dispatches a streaming client response: relay selection follows
// streamingHandler.js buildTransformStream — translate when the client and
// upstream formats differ, passthrough otherwise. Responses passthrough
// synthesizes response.failed + [DONE] when the stream aborts or stalls
// before a terminal event (buildAbortedResponsesTerminalBytes).
//
// ev/egID exist for the evidence layer: a stream that dies after headers is
// recorded as a response_started phase row (never an HTTP verdict —
// commitment was made when the executor returned the live response; no
// fallback, no health mark).
func (s *Server) stream(w http.ResponseWriter, r *http.Request, resp *http.Response, cancelUpstream context.CancelFunc, ev *evidenceLog, egID string, sourceFormat, targetFormat relay.Format, body map[string]any, upstreamModel string, customToolNames map[string]bool, intent *cloak.ThinkingCfg) {
	defer cancelUpstream()
	// Failover/error/forced paths close explicitly; this covers the relay paths
	// (base.js consumes or cancels the body either way).
	defer func() { _ = resp.Body.Close() }()
	started := time.Now()

	// Non-SSE upstream body (Cloudflare 5xx HTML page): return a clean JSON
	// error instead of piping garbage through the SSE path
	// (streamingHandler.js:62-80). JS reads the WHOLE body, and this one site
	// hand-rolls the error body `{error:{message}}` instead of using
	// errorResponse — no type/code envelope, only the formatProviderError-style
	// `[status]: ` prefix (:75-78).
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "application/json") {
		bodyText, _ := io.ReadAll(io.LimitReader(resp.Body, maxNonSSEBodyBytes))
		short := shortHTMLMessage(string(bodyText), ct)
		ev.StreamAbort(egID, resp.StatusCode, upstream.OriginUpstream, "non_sse_body", time.Since(started).Milliseconds())
		writeBareStreamError(w, resp.StatusCode, fmt.Sprintf("[%d]: %s", resp.StatusCode, short))
		return
	}

	corsHeaders(w.Header(), true)
	flusher, _ := w.(http.Flusher)
	out := &flushWriter{w: w, f: flusher}

	var streamRelay lineRelay
	if sourceFormat != targetFormat {
		tr := relay.NewTranslateRelay(out, body, upstreamModel, sourceFormat, "", intent)
		tr.SetCustomToolNames(customToolNames)
		streamRelay = tr
	} else {
		streamRelay = relay.NewPassthroughRelay(out, body, upstreamModel, sourceFormat, intent)
	}
	isResponsesPassthrough := sourceFormat == relay.FormatResponses && targetFormat == relay.FormatResponses

	// ctx cancellation releases the upstream connection on stall/teardown.
	lineErr := upstream.ScanLines(r.Context(), resp.Body, config.StreamStall, streamRelay.ProcessLine, streamRelay.ProcessTail)
	if lineErr != nil {
		origin, reason := streamAbortReason(r.Context(), lineErr)
		ev.StreamAbort(egID, resp.StatusCode, origin, reason, time.Since(started).Milliseconds())
		// Stall, transport failure, or client disconnect mid-stream: a
		// Responses passthrough client still needs a parseable terminal.
		if isResponsesPassthrough {
			_, _ = out.WriteString(relay.FormatIncompleteResponsesFailure())
			_, _ = out.WriteString("data: [DONE]\n\n")
		}
		_ = resp.Body.Close()
		return
	}
	if err := streamRelay.Flush(); err != nil {
		_ = resp.Body.Close()
	}
}

// streamAbortReason reduces a ScanLines failure to the side it belongs to and
// its bounded phase label: the request context died (client disconnected), the
// stall watchdog fired (ScanLines' typed ErrStreamStalled sentinel), or the
// body read itself errored. A label, never a message — the raw error text can
// carry topology.
//
// The origin is returned alongside because a mid-stream death is normally the
// provider's (OriginUpstream) but a client disconnect is the CALLER's — and
// the evidence row must not blame the egress for the caller leaving.
func streamAbortReason(ctx context.Context, err error) (upstream.Origin, string) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return upstream.OriginClient, "client_disconnect"
	}
	// Typed, not text-matched: ScanLines wraps its own sentinel
	// (upstream.ErrStreamStalled), so the watchdog is recognisable without
	// probing a message string (issue #6 discipline).
	if errors.Is(err, upstream.ErrStreamStalled) {
		return upstream.OriginUpstream, "stall"
	}
	return upstream.OriginUpstream, "read_error"
}

// shortHTMLMessage sanitizes an upstream HTML error page into a short
// client-safe message (streamingHandler.js:65-68). The <title> wins; failing
// that, a RAW body under 200 bytes is used stripped of tags (:68 tests
// `bodyText.length`, the raw text — not the stripped text; Go measures bytes
// where JS measures UTF-16 units, identical for the ASCII error pages this
// guards). collapse also squeezes internal whitespace runs where JS only
// trims the ends — same output for real error pages. Divergence: a body that
// strips to empty keeps the generic fallback; the JS `sanitizedTitle || …`
// chain would fall through to an empty message (`[status]: `), useless to any
// client.
func shortHTMLMessage(bodyText, ct string) string {
	collapse := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	if m := htmlTitleRe.FindStringSubmatch(bodyText); m != nil {
		sanitized := collapse(htmlTagRe.ReplaceAllString(m[1], ""))
		if sanitized != "" {
			return clamp160(sanitized)
		}
	}
	clean := collapse(htmlTagRe.ReplaceAllString(bodyText, ""))
	if clean != "" && len(bodyText) < 200 {
		return clamp160(clean)
	}
	return fmt.Sprintf("Upstream returned non-SSE response (%s)", ct)
}

// writeBareStreamError emits the non-SSE guard's hand-rolled error body —
// `{error:{message}}` with NO type/code. This one JS site bypasses
// errorResponse (utils/error.js) and inlines the envelope
// (streamingHandler.js:75-78), so unlike writeError it adds nothing the JS
// body does not carry.
func writeBareStreamError(w http.ResponseWriter, status int, message string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	w.Header().Set("Content-Type", "application/json")
	corsHeaders(w.Header(), false)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func clamp160(s string) string {
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

// flushWriter is an io.StringWriter that flushes after every write so SSE
// frames reach the client immediately.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) WriteString(p string) (int, error) {
	n, err := fw.w.Write([]byte(p))
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}
