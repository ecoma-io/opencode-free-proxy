// Package upstream ports the retrying upstream HTTP call
// (executors/base.js execute + config/runtimeConfig.js retry matrix) and the
// client-facing error envelope (utils/error.js).
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
)

// maxErrorBodyBytes caps how much of a terminal error response is read for
// the client-facing message (parseUpstreamError) and how much of a rejected
// response is drained before a retry (drainAndClose). Go-side transport
// hygiene, NOT an open-sse constant — JS reads error bodies unbounded
// (utils/error.js:61 `await response.text()`); the cap exists because Go owns
// the process memory the JS runtime would happily commit to a hostile
// upstream. It lives here rather than in internal/config for the same reason
// as connect.go's maxConnectHeaderBytes.
const maxErrorBodyBytes = 1 << 20

// Client performs the retrying upstream call (executors/base.js execute, the
// single-URL shape this proxy uses).
//
// Redirects: HTTP is left with CheckRedirect unset, so http.Client follows up
// to 10 redirects — faithful parity with the JS source, which passes no
// `redirect:` option anywhere in the executor path (base.js:144-149 sends
// only method/headers/body/signal through proxyFetch.js, which forwards the
// options object untouched to native fetch — utils/proxyFetch.js:203-257 —
// whose default is `redirect: "follow"`; the single `redirect: "manual"` in
// open-sse is the image fetch, translator/concerns/image.js:97-98, a
// different boundary). Accepted consequence: a 307/308 re-POSTs the body —
// including `Authorization: Bearer public` and the x-opencode-* headers — to
// the redirect target, exactly as the JS router would (pinned by
// TestRedirectFollowedLikeJSFetch).
type Client struct {
	// HTTP is the main transport: direct for unproxied and socks5 egresses,
	// Go's absolute-form proxy path for HTTP origins behind an HTTP(S)
	// proxy. tunneled (http/https proxy egresses only) carries HTTPS origins
	// through the hand-rolled CONNECT boundary — attempt picks by scheme.
	HTTP     *http.Client
	tunneled *http.Client
	Sleep    func(time.Duration)
	Now      func() time.Time

	// TLSConfig holds the ORIGIN TLS settings for the tunneled CONNECT
	// transport (and, cloned in, the proxy hop when the proxy endpoint is
	// itself https). Tests inject a root pool for self-signed fixtures;
	// production leaves it nil (system roots). Set before first use.
	TLSConfig *tls.Config
}

// NewClient wires an http.Client whose response-header wait is the connect
// timeout (FETCH_CONNECT_TIMEOUT_MS semantics: time to first response byte
// headers; the SSE body itself streams unbounded). The direct transport: no
// proxy, no credentials.
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: config.ConnectTimeout,
				ForceAttemptHTTP2:     true,
			},
		},
		Sleep: time.Sleep,
		Now:   time.Now,
	}
}

// UpstreamError is the client-facing failure after parsing an upstream error
// response (parseUpstreamError + createErrorResult).
type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("[%d]: %s", e.Status, e.Message)
}

// Do POSTs bodyJSON to url with headers, applying the retry matrix. On
// success the caller receives the live response with a streaming body; on
// error statuses the body is drained, parsed (parseUpstreamError), and the
// connection released. buildHeaders is invoked at the TOP of every attempt —
// including the first — mirroring base.js:127-130, where transformRequest and
// buildHeaders re-run inside the retry loop, so per-attempt variance (a fresh
// x-opencode-request id) happens on each try. The JS body re-transform on the
// same line is an in-place no-op on the already-transformed body, so the
// already-serialized bodyJSON is re-sent unchanged.
func (c *Client) Do(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte) (*http.Response, *UpstreamError) {
	resp, uerr, _ := c.DoClassified(ctx, url, buildHeaders, bodyJSON)
	return resp, uerr
}

// DoClassified is Do plus the failure taxonomy (failure.go) the executor
// needs for fallback/health decisions. The retry matrix is identical — the
// classification only sharpens the terminal branches: transport errors get
// their real class instead of a 502 guess, and 429 keeps its own class so
// the executor can fall back without poisoning health.
//
// Classification PRECEDES the retry decision (issue #6): a typed proxy-auth
// failure returns after ONE attempt without touching the shared budget — the
// proxy will refuse the same credentials identically on every retry, so the
// generic matrix would only add deterministic no-hope dials. The executor
// still sees FallbackAllowed() and moves to the next egress. Every other
// transport class keeps the base.js retry semantics.
func (c *Client) DoClassified(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte) (*http.Response, *UpstreamError, Class) {
	// base.js:104 `const retryAttemptsByUrl = {}` — ONE counter per URL shared
	// by every retryable status and network errors alike. tryRetry checks
	// `retryAttemptsByUrl[urlIndex] >= attempts` where `attempts` is the CAP OF
	// THE RULE THAT FIRED (base.js:113,121), so 502 and 503 draws deplete the
	// same budget: alternating 502/503 gives up after the first rule's cap on
	// combined attempts, not after each status's own cap.
	used := 0
	for {
		headers := buildHeaders()
		resp, netErr := c.attempt(ctx, url, headers, bodyJSON)
		if netErr != nil {
			// Network/fetch exceptions map to the 502 retry rule
			// (base.js:173 tryRetry(urlIndex, BAD_GATEWAY, `network …`)). The
			// [502]: prefix is applied at write time like every other error.
			// The class is the REAL class (proxy-auth vs timeout vs
			// connection vs ctx) — the 502 status is only the client-facing
			// envelope. classifyNetErrFor reads typed transport-boundary
			// errors only: a proxy CONNECT refusal carries a proxyAuthError
			// marker from connect.go/socks5.go, so no error-text probing is
			// involved.
			class := classifyNetErrFor(ctx, netErr)
			if class == ClassProxyAuthError {
				// Deliberate divergence from the JS matrix (documented in
				// issue #6): proxy-auth is terminal for THIS egress — one
				// dial, no budget consumption, immediate executor fallback.
				return nil, &UpstreamError{Status: 502, Message: netErr.Error()}, class
			}
			if !c.tryRetry(ctx, &used, config.RetryRules[502]) {
				// Surfacing the raw transport text is PARITY, topology
				// exposure included: base.js:179 rethrows the raw fetch
				// error, chatCore.js:373-375 hands it to formatProviderError,
				// and utils/error.js:139-147 deliberately renders
				// `[502]: ${error.message}${cause}` with the comment "Expose
				// low-level cause (e.g. UND_ERR_SOCKET, ECONNRESET, ETIMEDOUT)
				// for diagnosing fetch failures" — so the JS client envelope
				// carries the dial address exactly as this message carries the
				// (credential-redacted) egress host:port. Accepted parity.
				return nil, &UpstreamError{Status: 502, Message: netErr.Error()}, class
			}
			continue
		}
		if rule, retryable := config.RetryRules[resp.StatusCode]; retryable && rule.Attempts > 0 {
			// Unconfigured statuses resolve to attempts 0 (resolveRetryEntry
			// returns {attempts:0} for a missing key) and never retry.
			if c.tryRetry(ctx, &used, rule) {
				drainAndClose(resp)
				continue
			}
		}
		// Non-retryable (or retries exhausted): parse the final response the
		// way parseUpstreamError does. The read is CAPPED — a deliberate
		// divergence from JS (utils/error.js:61 `await response.text()` is
		// unbounded): a hostile upstream/proxy streaming an endless 4xx body
		// must not grow this process without limit before the text becomes the
		// client-facing envelope. Semantics are unchanged for any body under
		// the cap (every real upstream error body is); an over-cap body
		// surfaces truncated, and since the truncation is no longer valid JSON
		// parseUpstreamError falls back to the raw capped text — the envelope
		// stays bounded by construction. Same bound as drainAndClose.
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			_ = resp.Body.Close()
			return nil, parseUpstreamError(resp.StatusCode, raw), classifyStatusFor(resp.StatusCode)
		}
		return resp, nil, ClassSuccess
	}
}

// drainAndClose reads the rest of a rejected response so the connection can
// be reused, then closes it. Bounded by maxErrorBodyBytes like the terminal
// read: a hostile upstream must not be able to pin this goroutine's memory
// past the drain either.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
}

// CloseIdleConnections closes the idle pooled conns of every transport the
// client owns. The router calls it when a generation prune evicts the client:
// without it an evicted transport's idle conns linger until GC finalizes
// them (up to ~2 minutes between forced collections), making the FD
// accounting of an egress swap nondeterministic.
func (c *Client) CloseIdleConnections() {
	c.HTTP.CloseIdleConnections()
	if c.tunneled != nil {
		c.tunneled.CloseIdleConnections()
	}
}

// attempt performs one POST without touching the response body; netErr
// distinguishes transport failures. The transport is picked by TARGET scheme:
// https through an HTTP(S) proxy rides the tunneled CONNECT boundary, http
// rides the absolute-form proxy transport (or the single direct/socks5 one).
func (c *Client) attempt(ctx context.Context, url string, headers map[string]string, bodyJSON []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if req.URL.Scheme == "https" && c.tunneled != nil {
		client = c.tunneled
	}
	return client.Do(req)
}

// tryRetry consumes one draw from the shared per-URL budget against this
// rule's cap; sleeps when retrying. base.js:111-125: the counter is shared,
// the cap comes from the rule that fired
// (`if (attempts <= 0 || retryAttemptsByUrl[urlIndex] >= attempts) return
// false; retryAttemptsByUrl[url]++`).
func (c *Client) tryRetry(ctx context.Context, used *int, rule config.RetryRule) bool {
	// The ctx check comes FIRST: a dead request draws no budget it can never
	// spend. (The call terminates on this path either way — used is local to
	// one DoClassified — so this is ordering hygiene, not semantics.)
	if ctx.Err() != nil {
		return false
	}
	if rule.Attempts <= 0 || *used >= rule.Attempts {
		return false
	}
	*used++
	c.Sleep(rule.Delay)
	return true
}

// parseUpstreamError ports utils/error.js parseUpstreamError: extract the
// message from the upstream body, then re-type it for clients
// (buildErrorBody's status→type/code map is applied at write time).
func parseUpstreamError(status int, body []byte) *UpstreamError {
	text := string(body)
	var message string
	var haveMessage bool
	if json.Valid(body) {
		var parsed map[string]any
		if json.Unmarshal(body, &parsed) == nil {
			// JS: json.error?.message || json.message || json.error || bodyText —
			// each operand is only consulted when the previous was falsy, so an
			// error object without .message falls through to json.message first.
			if errObj := jsonx.AsObj(parsed["error"]); errObj != nil {
				if m := jsonx.AsStr(errObj["message"]); m != "" {
					message, haveMessage = m, true
				}
			}
			if !haveMessage {
				if m := jsonx.AsStr(parsed["message"]); m != "" {
					message, haveMessage = m, true
				}
			}
			// The bare-error operand only counts when JS-truthy: {"error":false}
			// / 0 / "" fall through to bodyText, they are not stringified.
			if !haveMessage && jsonx.Truthy(parsed["error"]) {
				if s, is := parsed["error"].(string); is {
					// Truthy already excludes "" — the string is the message.
					message, haveMessage = s, true
				} else {
					// Non-string truthy error (object/number) → JSON.stringify.
					b, _ := json.Marshal(parsed["error"])
					message, haveMessage = string(b), true
				}
			}
		}
	}
	if !haveMessage {
		message = text
	}
	if message == "" {
		if def, has := config.DefaultErrorMessages[status]; has {
			message = def
		} else {
			message = fmt.Sprintf("Upstream error: %d", status)
		}
	}
	return &UpstreamError{Status: status, Message: message}
}

// BuildErrorBody ports buildErrorBody: the OpenAI-compatible error envelope.
// Statuses MISSING from config.ErrorTypes — 407-as-response-status, 426, 418,
// … — are PARITY with the JS table (config/errorConfig.js ERROR_TYPES has no
// rows for them either): error.js:10-13 falls back to
// `{type:"server_error",code:"internal_server_error"}` for >= 500 and
// `{type:"invalid_request_error",code:""}` otherwise. That is how a 407 that
// arrives as a response status surfaces typed invalid_request_error with an
// empty code — deliberate, verified parity, not an omission.
func BuildErrorBody(statusCode int, message string) map[string]any {
	info, known := config.ErrorTypes[statusCode]
	if !known {
		if statusCode >= 500 {
			info = config.ErrorInfo{Type: "server_error", Code: "internal_server_error"}
		} else {
			info = config.ErrorInfo{Type: "invalid_request_error", Code: ""}
		}
	}
	if message == "" {
		if def, has := config.DefaultErrorMessages[statusCode]; has {
			message = def
		} else {
			message = "An error occurred"
		}
	}
	return jsonx.ObjOf("error", jsonx.ObjOf(
		"message", message,
		"type", info.Type,
		"code", info.Code,
	))
}

// ScanLines consumes an SSE body line by line, delivering each line without
// its trailing newline (stream.js buffer.split("\n") semantics — a \r from
// CRLF upstreams survives, exactly as in JS). An unterminated final segment
// (upstream closed mid-line) is delivered through onTail instead of fn: JS
// passthrough forwards that residual buffer raw in its flush (only the
// "data:"-prefix fix), while translate mode re-parses it — the relays decide.
// The stall deadline resets on every line read (STREAM_STALL_TIMEOUT_MS).
// When fn returns an error or the stall fires, the caller must cancel ctx (or
// close the body) to unblock the reader goroutine.
func ScanLines(ctx context.Context, body io.Reader, stall time.Duration, fn func(line string) error, onTail func(line string) error) error {
	lines := make(chan string, 64)
	tails := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(tails)
		reader := bufio.NewReaderSize(body, 64*1024)
		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF && line != "" {
				// Unterminated final segment — not a complete line.
				select {
				case tails <- line:
				case <-ctx.Done():
				}
				return
			}
			if line != "" {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case readErr <- err:
					default:
					}
				}
				return
			}
		}
	}()
	timer := time.NewTimer(stall)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// All complete lines delivered; the residual tail (if any)
				// goes through onTail, then a pending read error surfaces.
				select {
				case tail := <-tails:
					if onTail != nil {
						if err := onTail(tail); err != nil {
							return err
						}
					}
				default:
				}
				select {
				case err := <-readErr:
					return err
				default:
					return nil
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(stall)
			if err := fn(strings.TrimRight(line, "\n")); err != nil {
				return err
			}
		case <-timer.C:
			return fmt.Errorf("stream stalled: no SSE data for %s", stall)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
