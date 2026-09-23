package upstream

// Evidence-capture tests for DoClassifiedObserved: every failed interaction
// leaves exactly ONE row carrying the facts the verdict actually had
// (status/headers/body/rate limits BEFORE classification reduces them), and
// successful dials leave no row. One Client.Do is one logical upstream call
// (issue #53), so "one row per failed interaction" and "one row per attempt"
// are the same statement here.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode-free-proxy/internal/config"
)

// doObserved runs one observed call against a test server.
func doObserved(c *Client, url string, rec *Recorder) (*http.Response, *UpstreamError, Failure) {
	return c.DoClassifiedObserved(context.Background(), url, staticHeaders(), []byte("{}"), rec)
}

// testClient is the plain direct client the evidence cases dial with. It used
// to stub the retry sleep; there is no sleep seam any more, so it is NewClient.
func testClient() *Client { return NewClient() }

func TestEvidence429CarriesRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-RateLimit-Limit", "100")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1735689600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	rec := NewRecorder()
	_, uerr, failure := doObserved(testClient(), srv.URL, rec)
	if uerr == nil || uerr.Status != 429 || failure.Class != ClassUpstream429 {
		t.Fatalf("uerr=%v class=%s", uerr, failure.Class)
	}
	// A provider verdict: the request was received and answered, so nothing
	// here may ever authorise a re-send (the recovery contract's whole point).
	if failure.Origin != OriginUpstream || failure.RequestState != RequestStateResponseStarted || failure.ReplaySafe() {
		t.Fatalf("429 provenance = %+v, want an upstream response_started verdict", failure)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (one call, one row)", len(rows))
	}
	row := rows[0]
	if row.Phase != PhaseResponse || row.Status != 429 || row.Class != ClassUpstream429.String() {
		t.Fatalf("row = %+v", row)
	}
	if row.RateLimit == nil || row.RateLimit.RetryAfter != "17" || len(row.RateLimit.Entries) != 3 {
		t.Fatalf("rate limits lost: %+v", row.RateLimit)
	}
	// The executor, not the client, owns the two decisions; before it runs
	// they are unset — a row never claims a decision before one exists.
	if row.HealthDecision != "" || row.FallbackDecision != "" {
		t.Fatalf("client-layer row carries decisions: %+v", row)
	}
	if row.ErrType != "rate_limit_error" {
		t.Fatalf("error type lost: %q", row.ErrType)
	}
	if row.BodyPeek == "" || row.Truncated {
		t.Fatalf("peek = %q truncated=%t, want the small body verbatim", row.BodyPeek, row.Truncated)
	}
	if row.Fingerprint == "" {
		t.Fatal("fingerprint missing")
	}
}

func TestEvidenceStatusCoverage(t *testing.T) {
	cases := []struct {
		status int
		class  Class
	}{
		{400, ClassClientError},
		{401, ClassClientError},
		{403, ClassClientError},
		{404, ClassClientError},
		{413, ClassClientError},
		{429, ClassUpstream429},
		{500, ClassUpstream5xx},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			}))
			defer srv.Close()

			rec := NewRecorder()
			_, uerr, failure := doObserved(testClient(), srv.URL, rec)
			if uerr == nil || uerr.Status != tc.status {
				t.Fatalf("uerr = %v", uerr)
			}
			if failure.Class != tc.class {
				t.Fatalf("class = %s, want %s", failure.Class, tc.class)
			}
			rows := rec.Rows()
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if rows[0].Status != tc.status || rows[0].Class != tc.class.String() || rows[0].RateLimit != nil {
				t.Fatalf("row = %+v", rows[0])
			}
		})
	}
}

// TestEvidence502IsTerminal: a 502 is the provider's answer and the end of
// the request. Exactly one row, carrying the body it answered with, its rate
// limits, and the provenance that forbids replaying it anywhere.
func TestEvidence502IsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1735689600")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
	}))
	defer srv.Close()

	rec := NewRecorder()
	_, uerr, failure := doObserved(testClient(), srv.URL, rec)
	if uerr == nil || uerr.Status != 502 || failure.Class != ClassUpstream5xx {
		t.Fatalf("uerr=%v class=%s", uerr, failure.Class)
	}
	if failure.ReplaySafe() {
		t.Fatalf("502 provenance claims replay safety: %+v", failure)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Status != 502 || row.Class != ClassUpstream5xx.String() {
		t.Fatalf("row = %+v", row)
	}
	if row.BodyPeek == "" || row.Message != "bad gateway" {
		t.Fatalf("terminal row lost the body: %+v", row)
	}
	if row.RateLimit == nil {
		t.Fatalf("response headers lost: %+v", row)
	}
}

func TestEvidenceTransportErrors(t *testing.T) {
	t.Run("connection refused is one row", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		rec := NewRecorder()
		_, uerr, failure := doObserved(testClient(), url, rec)
		if uerr == nil || failure.Class != ClassConnectionError {
			t.Fatalf("uerr=%v class=%s", uerr, failure.Class)
		}
		// Nothing was listening, so no request byte ever existed: this is the
		// one shape that authorises an egress move.
		if !failure.ReplaySafe() {
			t.Fatalf("refused dial is not replay-safe: %+v", failure)
		}
		rows := rec.Rows()
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want exactly 1", len(rows))
		}
		row := rows[0]
		if row.Phase != PhaseTransport || row.Status != 0 || row.Class != ClassConnectionError.String() {
			t.Fatalf("row = %+v", row)
		}
		if row.Message == "" || row.Fingerprint == "" {
			t.Fatalf("row missing message/fingerprint: %+v", row)
		}
		if !strings.Contains(row.Message, "connection refused") {
			t.Fatalf("message = %q, want the transport text", row.Message)
		}
		if row.Origin != OriginTransport.String() || row.RequestState != RequestStateNotSent.String() {
			t.Fatalf("provenance lost on the row: %+v", row)
		}
	})
	t.Run("fingerprint is address-independent", func(t *testing.T) {
		// Two different dead addresses, same logical refusal → same fingerprint.
		addrs := make([]string, 2)
		for i := 0; i < 2; i++ {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addrs[i] = l.Addr().String()
			_ = l.Close()
		}
		fps := make([]string, 2)
		for i, addr := range addrs {
			rec := NewRecorder()
			_, _, _ = doObserved(testClient(), "http://"+addr, rec)
			rows := rec.Rows()
			if len(rows) == 0 {
				t.Fatal("no transport rows")
			}
			fps[i] = rows[len(rows)-1].Fingerprint
		}
		if fps[0] == "" || fps[0] != fps[1] {
			t.Fatalf("fingerprints differ across addresses: %q vs %q", fps[0], fps[1])
		}
	})
}

func TestEvidenceLargeBodyIsPeekedNotCopied(t *testing.T) {
	big := strings.Repeat("x", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"`+big+`"}}`)
	}))
	defer srv.Close()

	rec := NewRecorder()
	_, _, _ = doObserved(testClient(), srv.URL, rec)
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if len(rows[0].BodyPeek) > config.EvidencePeekBytes {
		t.Fatalf("peek = %d bytes, cap is %d", len(rows[0].BodyPeek), config.EvidencePeekBytes)
	}
	if !rows[0].Truncated {
		t.Fatal("truncation flag missing for an over-peek body")
	}
	if rows[0].BodyBytes < 5000 {
		t.Fatalf("response_bytes = %d, want the real body size", rows[0].BodyBytes)
	}
}

func TestEvidenceStructuredErrorFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad model","type":"invalid_request_error","code":"model_not_found"}}`)
	}))
	defer srv.Close()

	rec := NewRecorder()
	_, _, _ = doObserved(testClient(), srv.URL, rec)
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ErrType != "invalid_request_error" || rows[0].ErrCode != "model_not_found" || rows[0].Message != "bad model" {
		t.Fatalf("structured fields lost: %+v", rows[0])
	}
}

func TestEvidenceSuccessEmitsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	rec := NewRecorder()
	resp, uerr, _ := doObserved(testClient(), srv.URL, rec)
	if uerr != nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	if rec.Len() != 0 {
		t.Fatalf("rows = %d, want 0 (the completion line owns success)", rec.Len())
	}
}
