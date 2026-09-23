package upstream

// Tests for the upstream call (executors/base.js execute) and the
// client-facing error envelope (utils/error.js parseUpstreamError /
// buildErrorBody).
//
// The retry-matrix tests that used to live here are gone with the matrix
// itself (issue #53): one Client.Do is one logical upstream call, and the
// status table is now asserted by terminal_test.go — the request either
// reaches a provider and is relayed, or it fails at the transport boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

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
