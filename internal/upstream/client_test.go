package upstream

// Tests for the retrying upstream call (executors/base.js execute +
// config/runtimeConfig.js DEFAULT_RETRY_CONFIG) and the client-facing error
// envelope (utils/error.js parseUpstreamError / buildErrorBody).
//
// Sleep is injected so the retry matrix is verified by recording the delays
// the JS loop would `await new Promise(resolve => setTimeout(resolve, waitMs))`
// on — no wall-clock waiting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// recordingSleeper stands in for time.Sleep and records every delay the
// retry loop would have waited (base.js tryRetry → setTimeout).
type recordingSleeper struct {
	mu    sync.Mutex
	calls []time.Duration
}

func (r *recordingSleeper) sleep(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, d)
}

func (r *recordingSleeper) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.calls...)
}

func newTestClient(sleeper *recordingSleeper) *Client {
	c := NewClient()
	c.Sleep = sleeper.sleep
	return c
}

// TestRetryMatrix ports DEFAULT_RETRY_CONFIG (runtimeConfig.js):
//
//	429: { attempts: 0 }, 502: { attempts: 3, delayMs: 3000 },
//	503: { attempts: 3, delayMs: 2000 }, 504: { attempts: 2, delayMs: 3000 }
//
// plus the base.js network-error rule (`tryRetry(urlIndex,
// HTTP_STATUS.BAD_GATEWAY, 'network …')`).
func TestRetryMatrix(t *testing.T) {
	t.Run("502 twice then 200 succeeds after 2 retries with 3s sleeps", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {}\n\n")
		}))
		defer srv.Close()

		sleeper := &recordingSleeper{}
		resp, uerr := newTestClient(sleeper).Do(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
		if uerr != nil {
			t.Fatalf("expected success after retries, got %+v", uerr)
		}
		defer func() { _ = resp.Body.Close() }()

		mu.Lock()
		defer mu.Unlock()
		if hits != 3 {
			t.Fatalf("expected 3 upstream hits (2×502 + 1×200), got %d", hits)
		}
		delays := sleeper.recorded()
		if len(delays) != 2 {
			t.Fatalf("expected 2 sleeps, got %d (%v)", len(delays), delays)
		}
		for _, d := range delays {
			if d != config.RetryRules[502].Delay {
				t.Fatalf("expected 502 delay %s, got %s", config.RetryRules[502].Delay, d)
			}
		}
	})

	t.Run("503 retries 3 times with 2s delays then surfaces the parsed error", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
		}))
		defer srv.Close()

		sleeper := &recordingSleeper{}
		uerr, _ := doExpectError(newTestClient(sleeper), srv.URL)
		if uerr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d (%s)", uerr.Status, uerr.Message)
		}
		if uerr.Message != "overloaded" {
			t.Fatalf("expected the final 503 body to be parsed, got %q", uerr.Message)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 4 { // 1 initial + 3 retries
			t.Fatalf("expected 4 upstream hits, got %d", hits)
		}
		delays := sleeper.recorded()
		if len(delays) != 3 {
			t.Fatalf("expected 3 sleeps, got %d (%v)", len(delays), delays)
		}
		for _, d := range delays {
			if d != config.RetryRules[503].Delay {
				t.Fatalf("expected 503 delay %s, got %s", config.RetryRules[503].Delay, d)
			}
		}
	})

	t.Run("504 retries 2 times with 3s delays", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, `{"error":{"message":"gateway timeout"}}`)
		}))
		defer srv.Close()

		sleeper := &recordingSleeper{}
		uerr, _ := doExpectError(newTestClient(sleeper), srv.URL)
		if uerr.Status != http.StatusGatewayTimeout {
			t.Fatalf("expected status 504, got %d", uerr.Status)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 3 { // 1 initial + 2 retries
			t.Fatalf("expected 3 upstream hits, got %d", hits)
		}
		delays := sleeper.recorded()
		if len(delays) != 2 {
			t.Fatalf("expected 2 sleeps, got %d (%v)", len(delays), delays)
		}
		for _, d := range delays {
			if d != config.RetryRules[504].Delay {
				t.Fatalf("expected 504 delay %s, got %s", config.RetryRules[504].Delay, d)
			}
		}
	})

	t.Run("429 is never retried (free tier fails fast)", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limited"}}`)
		}))
		defer srv.Close()

		sleeper := &recordingSleeper{}
		uerr, _ := doExpectError(newTestClient(sleeper), srv.URL)
		if uerr.Status != http.StatusTooManyRequests {
			t.Fatalf("expected status 429, got %d", uerr.Status)
		}
		if uerr.Message != "Rate limited" {
			t.Fatalf("expected parsed 429 message, got %q", uerr.Message)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 1 {
			t.Fatalf("expected exactly 1 upstream hit (no retries), got %d", hits)
		}
		if delays := sleeper.recorded(); len(delays) != 0 {
			t.Fatalf("expected no sleeps for 429, got %v", delays)
		}
	})

	t.Run("500 is outside the retry matrix", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
		}))
		defer srv.Close()

		sleeper := &recordingSleeper{}
		uerr, _ := doExpectError(newTestClient(sleeper), srv.URL)
		if uerr.Status != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d", uerr.Status)
		}
		// utils/error.js parseUpstreamError: non-JSON body → raw bodyText.
		if uerr.Message != "boom" {
			t.Fatalf("expected raw non-JSON body as message, got %q", uerr.Message)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 1 {
			t.Fatalf("expected exactly 1 upstream hit, got %d", hits)
		}
		if delays := sleeper.recorded(); len(delays) != 0 {
			t.Fatalf("expected no sleeps for 500, got %v", delays)
		}
	})

	t.Run("network error retries as 502 then reports UpstreamError 502", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close() // every dial now fails — a transport-level error

		sleeper := &recordingSleeper{}
		resp, uerr := newTestClient(sleeper).Do(context.Background(), url, staticHeaders(), []byte("{}"))
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatal("expected no response for a dead upstream")
		}
		if uerr == nil {
			t.Fatal("expected an UpstreamError")
		}
		// base.js maps fetch exceptions onto the BAD_GATEWAY retry rule; chatCore
		// then formats them as `[502]: …` (formatProviderError).
		if uerr.Status != http.StatusBadGateway {
			t.Fatalf("expected status 502 for a network error, got %d", uerr.Status)
		}
		if uerr.Message == "" {
			t.Fatal("expected the transport error text as message")
		}
		delays := sleeper.recorded()
		if len(delays) != 3 {
			t.Fatalf("expected 3 sleeps for the 502 rule, got %d (%v)", len(delays), delays)
		}
		for _, d := range delays {
			if d != config.RetryRules[502].Delay {
				t.Fatalf("expected 502 delay %s, got %s", config.RetryRules[502].Delay, d)
			}
		}
	})
}

// TestBuildHeadersInvokedPerAttempt pins base.js:127-130: buildHeaders runs
// at the TOP of every retry-loop iteration (line 130, re-executed on each
// `urlIndex--; continue`), so every attempt carries a freshly built map — a
// forged x-opencode-request id is regenerated per try, not frozen from the
// first attempt.
func TestBuildHeadersInvokedPerAttempt(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	seq := 0
	buildHeaders := func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		calls++
		seq++
		return map[string]string{"x-opencode-request": fmt.Sprintf("msg_%d", seq)}
	}

	var seenIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenIDs = append(seenIDs, r.Header.Get("x-opencode-request"))
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer srv.Close()

	sleeper := &recordingSleeper{}
	uerr, _ := doExpectHeaders(newTestClient(sleeper), srv.URL, buildHeaders)
	if uerr == nil {
		t.Fatal("expected the final 503 to surface as an UpstreamError")
	}
	if uerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected the final 503 to surface, got %d", uerr.Status)
	}

	wantHits := 1 + config.RetryRules[503].Attempts // 1 initial + 3 retries
	mu.Lock()
	defer mu.Unlock()
	if len(seenIDs) != wantHits {
		t.Fatalf("expected %d upstream hits, got %d (%q)", wantHits, len(seenIDs), seenIDs)
	}
	if calls != wantHits {
		t.Fatalf("buildHeaders invoked %d times, want once per attempt (%d)", calls, wantHits)
	}
	// Each attempt saw a different (fresh) header map.
	for i, got := range seenIDs {
		if want := fmt.Sprintf("msg_%d", i+1); got != want {
			t.Fatalf("attempt %d carried x-opencode-request %q, want %q (maps must be rebuilt per attempt)", i+1, got, want)
		}
	}
}

// TestSharedRetryBudgetAcrossStatuses pins base.js:104,113,121:
// retryAttemptsByUrl is ONE counter per URL shared by every retryable status —
// the cap checked is the fired rule's own attempts. Alternating 502/503 must
// therefore exhaust after min(RetryRules[502].Attempts,
// RetryRules[503].Attempts) retries on combined attempts; per-status counters
// (the old "status-%d" keys) would have allowed twice as many.
func TestSharedRetryBudgetAcrossStatuses(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n%2 == 1 { // 502, 503, 502, 503, …
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer srv.Close()

	sleeper := &recordingSleeper{}
	resp, uerr := newTestClient(sleeper).Do(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if uerr == nil {
		t.Fatal("expected the shared budget to exhaust")
	}

	// The budget empties when the shared counter reaches the first rule cap;
	// the response that arrives then is not retried and is parsed instead.
	wantHits := min(config.RetryRules[502].Attempts, config.RetryRules[503].Attempts) + 1
	mu.Lock()
	defer mu.Unlock()
	if hits != wantHits {
		t.Fatalf("expected %d combined attempts (shared budget) before giving up, got %d", wantHits, hits)
	}
	// That final response is the wantHits-th hit — an even ordinal → the 503.
	if uerr.Status != http.StatusServiceUnavailable || uerr.Message != "overloaded" {
		t.Fatalf("expected the un-retried 503 to be parsed, got [%d]: %s", uerr.Status, uerr.Message)
	}
	if delays := sleeper.recorded(); len(delays) != wantHits-1 {
		t.Fatalf("expected %d sleeps, got %d (%v)", wantHits-1, len(delays), delays)
	}
}

// TestNetworkAndStatusShareRetryBudget pins base.js:173: a fetch exception
// draws from the same retryAttemptsByUrl budget as a status retry
// (tryRetry(urlIndex, HTTP_STATUS.BAD_GATEWAY, …)), so network errors and 502s
// alternate-deplete one pool capped by the 502 rule.
func TestNetworkAndStatusShareRetryBudget(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n%2 == 1 { // odd attempts die with no response — the fetch-exception shape
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response is not a Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close() // no HTTP response at all → transport error
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
	}))
	defer srv.Close()

	sleeper := &recordingSleeper{}
	resp, uerr := newTestClient(sleeper).Do(context.Background(), srv.URL, staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if uerr == nil {
		t.Fatal("expected the shared budget to exhaust")
	}

	// net,502,net,502: the second 502 arrives with the shared counter already
	// at the 502 rule's cap, so it is parsed instead of retried.
	wantHits := config.RetryRules[502].Attempts + 1
	mu.Lock()
	defer mu.Unlock()
	if hits != wantHits {
		t.Fatalf("expected %d combined attempts (shared budget) before giving up, got %d", wantHits, hits)
	}
	if uerr.Status != http.StatusBadGateway {
		t.Fatalf("expected the final 502 body to be parsed, got [%d]: %s", uerr.Status, uerr.Message)
	}
	if uerr.Message != "bad gateway" {
		t.Fatalf("expected the parsed 502 body message, got %q", uerr.Message)
	}
	if delays := sleeper.recorded(); len(delays) != wantHits-1 {
		t.Fatalf("expected %d sleeps, got %d (%v)", wantHits-1, len(delays), delays)
	}
}

func doExpectHeaders(c *Client, url string, buildHeaders func() map[string]string) (*UpstreamError, int) {
	resp, uerr := c.Do(context.Background(), url, buildHeaders, []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	return uerr, 0
}

func doExpectError(c *Client, url string) (*UpstreamError, int) {
	resp, uerr := c.Do(context.Background(), url, staticHeaders(), []byte("{}"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	return uerr, 0
}

// staticHeaders is the no-op buildHeaders closure for tests that don't care
// about per-attempt header variance.
func staticHeaders() func() map[string]string {
	return func() map[string]string { return nil }
}

// TestParseUpstreamError ports utils/error.js parseUpstreamError:
//
//	message = json.error?.message || json.message || json.error || bodyText
func TestParseUpstreamError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{
			name:    "error.message object shape",
			status:  403,
			body:    `{"error":{"message":"You exceeded your current quota"}}`,
			wantMsg: "You exceeded your current quota",
		},
		{
			name:    "top-level message shape",
			status:  500,
			body:    `{"message":"plain message"}`,
			wantMsg: "plain message",
		},
		{
			name:    "error object without message is stringified",
			status:  429,
			body:    `{"error":{"code":"quota_exceeded"}}`,
			wantMsg: `{"code":"quota_exceeded"}`,
		},
		{
			name:    "non-JSON body kept verbatim",
			status:  502,
			body:    "upstream is sad",
			wantMsg: "upstream is sad",
		},
		{
			name:    "JSON object without any message falls back to raw text",
			status:  500,
			body:    `{}`,
			wantMsg: `{}`,
		},
		{
			name:    "empty body uses the status default",
			status:  403,
			body:    "",
			wantMsg: config.DefaultErrorMessages[403],
		},
		{
			name:    "unknown status with empty body uses the generic default",
			status:  599,
			body:    "",
			wantMsg: "Upstream error: 599",
		},
		// The bare `json.error` operand is gated on JS truthiness
		// (error.js:80 `|| json.error || bodyText`): falsy errors fall
		// through to the raw body instead of being stringified.
		{
			name:    "error false is falsy and falls through to raw body",
			status:  502,
			body:    `{"error":false}`,
			wantMsg: `{"error":false}`,
		},
		{
			name:    "error zero is falsy and falls through to raw body",
			status:  500,
			body:    `{"error":0}`,
			wantMsg: `{"error":0}`,
		},
		{
			name:    "error empty string is falsy and falls through to raw body",
			status:  500,
			body:    `{"error":""}`,
			wantMsg: `{"error":""}`,
		},
		{
			// A truthy string error is used verbatim — error.js:80 selects it
			// directly (`json.error?.message` on a string is undefined).
			name:    "error string is the message",
			status:  403,
			body:    `{"error":"You exceeded your current quota"}`,
			wantMsg: "You exceeded your current quota",
		},
		{
			name:    "error number is stringified",
			status:  500,
			body:    `{"error":5}`,
			wantMsg: "5",
		},
		{
			name:    "error true is stringified",
			status:  500,
			body:    `{"error":true}`,
			wantMsg: "true",
		},
		{
			// Empty containers are truthy in JS → JSON.stringify({}) = "{}".
			name:    "error empty object is stringified",
			status:  500,
			body:    `{"error":{}}`,
			wantMsg: "{}",
		},
		{
			name:    "error empty array is stringified",
			status:  500,
			body:    `{"error":[]}`,
			wantMsg: "[]",
		},
		{
			// error.message wins only when present; the error OBJECT without a
			// message is skipped in favor of the top-level message (the object
			// itself is not re-consulted once json.message is truthy).
			name:    "error object without message falls to top-level message",
			status:  500,
			body:    `{"error":{"other":1},"message":"m2"}`,
			wantMsg: "m2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUpstreamError(tc.status, []byte(tc.body))
			if got.Status != tc.status {
				t.Fatalf("status = %d, want %d", got.Status, tc.status)
			}
			if got.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", got.Message, tc.wantMsg)
			}
			// The client-facing prefix `[status]: message` is applied once at
			// write time (chatCore formatProviderError → createErrorResult).
			if got.Error() != fmt.Sprintf("[%d]: %s", tc.status, tc.wantMsg) {
				t.Fatalf("Error() = %q, want the [status]: message form", got.Error())
			}
		})
	}
}

// TestBuildErrorBody ports buildErrorBody (utils/error.js) against
// config/errorConfig.js ERROR_TYPES / DEFAULT_ERROR_MESSAGES.
func TestBuildErrorBody(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		message  string
		wantMsg  string
		wantType string
		wantCode string
	}{
		{name: "400", status: 400, message: "m400", wantMsg: "m400", wantType: "invalid_request_error", wantCode: "bad_request"},
		{name: "401", status: 401, message: "m401", wantMsg: "m401", wantType: "authentication_error", wantCode: "invalid_api_key"},
		{name: "403", status: 403, message: "quota", wantMsg: "quota", wantType: "permission_error", wantCode: "insufficient_quota"},
		{name: "404", status: 404, message: "missing", wantMsg: "missing", wantType: "invalid_request_error", wantCode: "model_not_found"},
		{name: "429", status: 429, message: "slow down", wantMsg: "slow down", wantType: "rate_limit_error", wantCode: "rate_limit_exceeded"},
		{name: "500", status: 500, message: "boom", wantMsg: "boom", wantType: "server_error", wantCode: "internal_server_error"},
		{name: "502", status: 502, message: "bg", wantMsg: "bg", wantType: "server_error", wantCode: "bad_gateway"},
		{name: "503", status: 503, message: "unavail", wantMsg: "unavail", wantType: "server_error", wantCode: "service_unavailable"},
		{name: "504", status: 504, message: "timeout", wantMsg: "timeout", wantType: "server_error", wantCode: "gateway_timeout"},
		{
			// Unknown 4xx: ERROR_TYPES[418] is undefined →
			// { type: "invalid_request_error", code: "" }.
			name: "unknown 418", status: 418, message: "teapot", wantMsg: "teapot",
			wantType: "invalid_request_error", wantCode: "",
		},
		{
			// Unknown 5xx: { type: "server_error", code: "internal_server_error" }.
			name: "unknown 599", status: 599, message: "weird", wantMsg: "weird",
			wantType: "server_error", wantCode: "internal_server_error",
		},
		{
			// message || DEFAULT_ERROR_MESSAGES[statusCode] || "An error occurred".
			name: "empty message uses the status default", status: 503, message: "",
			wantMsg: config.DefaultErrorMessages[503], wantType: "server_error", wantCode: "service_unavailable",
		},
		{
			name: "empty message unknown status uses the generic default", status: 599, message: "",
			wantMsg: "An error occurred", wantType: "server_error", wantCode: "internal_server_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := BuildErrorBody(tc.status, tc.message)
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("body missing error object: %#v", body)
			}
			if errObj["message"] != tc.wantMsg {
				t.Fatalf("message = %v, want %q", errObj["message"], tc.wantMsg)
			}
			if errObj["type"] != tc.wantType {
				t.Fatalf("type = %v, want %q", errObj["type"], tc.wantType)
			}
			if errObj["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %q", errObj["code"], tc.wantCode)
			}
		})
	}
}

// blockingReader returns its buffered data once, then blocks until Close —
// the "upstream went quiet mid-stream" shape for the stall test.
type blockingReader struct {
	data   []byte
	off    int
	closed chan struct{}
	once   sync.Once
}

func newBlockingReader(data string) *blockingReader {
	return &blockingReader{data: []byte(data), closed: make(chan struct{})}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.off < len(b.data) {
		n := copy(p, b.data[b.off:])
		b.off += n
		return n, nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReader) Close() { b.once.Do(func() { close(b.closed) }) }

// TestScanLines covers stream.js buffer.split("\n") line delivery: the
// trailing newline is stripped (a \r from CRLF upstreams survives), a partial
// final line is delivered at EOF through onTail (the relays decide what a
// residual buffer means), the stall watchdog fires, and fn errors / context
// cancellation unwind.
func TestScanLines(t *testing.T) {
	collect := func(t *testing.T, input string) ([]string, string) {
		t.Helper()
		var got []string
		var tail string
		err := ScanLines(context.Background(), strings.NewReader(input), time.Second, func(line string) error {
			got = append(got, line)
			return nil
		}, func(line string) error {
			tail = line
			return nil
		})
		if err != nil {
			t.Fatalf("ScanLines: %v", err)
		}
		return got, tail
	}

	t.Run("delivers newline separated lines", func(t *testing.T) {
		got, _ := collect(t, "a\n\nb\n")
		want := []string{"a", "", "b"} // blank separators are delivered as empty lines
		if len(got) != len(want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("lines = %q, want %q", got, want)
			}
		}
	})

	t.Run("trailing partial line is delivered at EOF", func(t *testing.T) {
		got, tail := collect(t, "l1\npar")
		// The unterminated final segment is NOT a complete line: it goes to
		// onTail, not fn.
		if len(got) != 1 || got[0] != "l1" {
			t.Fatalf("lines = %q, want [l1]", got)
		}
		if tail != "par" {
			t.Fatalf("tail = %q, want par", tail)
		}
	})

	t.Run("CRLF keeps the carriage return (JS split semantics)", func(t *testing.T) {
		got, _ := collect(t, "l1\r\nl2\n")
		if len(got) != 2 || got[0] != "l1\r" || got[1] != "l2" {
			t.Fatalf("lines = %q, want [l1\\r l2]", got)
		}
	})

	t.Run("stall watchdog fires when the body goes quiet", func(t *testing.T) {
		br := newBlockingReader("first\n")
		defer br.Close()
		start := time.Now()
		err := ScanLines(context.Background(), br, 50*time.Millisecond, func(string) error { return nil }, nil)
		if err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("expected a stall error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Fatalf("stall fired after %s — earlier than the 50ms deadline", elapsed)
		}
	})

	t.Run("fn error propagates", func(t *testing.T) {
		boom := errors.New("boom")
		err := ScanLines(context.Background(), strings.NewReader("a\nb\n"), time.Second, func(line string) error {
			if line == "a" {
				return boom
			}
			return nil
		}, nil)
		if !errors.Is(err, boom) {
			t.Fatalf("expected the fn error to propagate, got %v", err)
		}
	})

	t.Run("context cancel unblocks a quiet body", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		br := newBlockingReader("x\n")
		defer br.Close()
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		err := ScanLines(ctx, br, time.Second, func(string) error { return nil }, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

// TestScanLinesDeliversJSON keeps the SSE fixture shape honest: an upstream
// chat chunk survives the reader untouched.
func TestScanLinesDeliversJSON(t *testing.T) {
	chunk := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hi"}, "finish_reason": nil}},
	}
	raw, _ := json.Marshal(chunk)
	var got []string
	err := ScanLines(context.Background(), strings.NewReader("data: "+string(raw)+"\n\n"), time.Second, func(line string) error {
		got = append(got, line)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ScanLines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected data line + blank separator, got %q", got)
	}
	if !strings.HasPrefix(got[0], "data: {") {
		t.Fatalf("data line malformed: %q", got[0])
	}
}

// trickleReader delivers its payload five bytes at a time on a delay — an
// upstream slowly assembling ONE long event with no newline until it is
// done, then a clean EOF.
type trickleReader struct {
	data  []byte
	off   int
	pause time.Duration
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	time.Sleep(r.pause)
	end := r.off + 5
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.off:end])
	r.off += n
	return n, nil
}

// TestScanLinesTrickleIsNotStall: the stall watchdog must reset on ANY read
// progress, not only complete lines (streamHandler.js:179,185-186 "Any
// upstream chunk resets the timer"; :229-239 armStall per chunk). Eleven
// bytes at 20 ms apart under a 50 ms stall carry NO newline until the final
// byte, so a per-LINE reset stalls this body at 50 ms while the trickling
// upstream is still live — the exact false stall the JS watchdog was changed
// to avoid ("measuring stall on the transform output caused false stalls").
func TestScanLinesTrickleIsNotStall(t *testing.T) {
	tr := &trickleReader{data: []byte("data: hello"), pause: 20 * time.Millisecond}
	var got []string
	var tail string
	err := ScanLines(context.Background(), tr, 50*time.Millisecond, func(line string) error {
		got = append(got, line)
		return nil
	}, func(line string) error {
		tail = line
		return nil
	})
	if err != nil {
		t.Fatalf("ScanLines: %v — a trickling body is live traffic, not a stall", err)
	}
	if len(got) != 0 {
		t.Fatalf("fn lines = %q; the trickle never terminated, it is one segment for onTail", got)
	}
	if tail != "data: hello" {
		t.Fatalf("tail = %q, want the whole trickled segment", tail)
	}
}

// errAfterReader returns its data once, then a non-EOF error — a transport
// death mid-body.
type errAfterReader struct {
	data []byte
	off  int
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// TestScanLinesMidLineErrorDropsResidual: an errored stream never flushes
// in JS (streamHandler.js:161-163 controller.error; stream.js:515-530
// forward of the residual buffer lives in flush(), which runs only on a
// clean end), so a partial segment followed by a non-EOF read error must be
// DROPPED — not delivered as a complete line (the old behavior), not
// tailed. The error itself is the whole story.
func TestScanLinesMidLineErrorDropsResidual(t *testing.T) {
	boom := errors.New("connection reset")
	r := &errAfterReader{data: []byte("l1\ndata: partial"), err: boom}
	var got []string
	tailCalled := false
	err := ScanLines(context.Background(), r, time.Second, func(line string) error {
		got = append(got, line)
		return nil
	}, func(string) error {
		tailCalled = true
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ScanLines error = %v, want the read error", err)
	}
	if len(got) != 1 || got[0] != "l1" {
		t.Fatalf("fn lines = %q, want only the complete line [l1] — the partial segment is not a line", got)
	}
	if tailCalled {
		t.Fatal("onTail ran for a mid-line transport error — an errored stream never flushes (stream.js:515-530 is clean-end only)")
	}
}
