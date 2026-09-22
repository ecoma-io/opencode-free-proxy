package upstream

// Evidence-capture tests for DoClassifiedObserved: every failed dial leaves a
// row carrying the facts the verdict actually had (status/headers/body/rate
// limits BEFORE classification reduces them), retried verdicts are captured
// before the drain destroys them, and successful dials leave no row. The
// retry/classification BEHAVIOR is already pinned by client_test.go — these
// tests pin only what the recorder observed.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"opencode-free-proxy/internal/config"
)

// doObserved runs one observed call against a test server.
func doObserved(c *Client, url string, rec *Recorder) (*http.Response, *UpstreamError, Class) {
	return c.DoClassifiedObserved(context.Background(), url, staticHeaders(), []byte("{}"), rec)
}

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
	_, uerr, class := doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
	if uerr == nil || uerr.Status != 429 || class != ClassUpstream429 {
		t.Fatalf("uerr=%v class=%s", uerr, class)
	}
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (429 never retries)", len(rows))
	}
	row := rows[0]
	if row.Phase != PhaseResponse || row.Status != 429 || row.Class != ClassUpstream429.String() {
		t.Fatalf("row = %+v", row)
	}
	if row.RateLimit == nil || row.RateLimit.RetryAfter != "17" || len(row.RateLimit.Entries) != 3 {
		t.Fatalf("rate limits lost: %+v", row.RateLimit)
	}
	if row.Retried || row.RetryDecision != "" {
		t.Fatalf("no retry happened, but row says retried=%t decision=%q", row.Retried, row.RetryDecision)
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
			_, uerr, class := doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
			if uerr == nil || uerr.Status != tc.status {
				t.Fatalf("uerr = %v", uerr)
			}
			if class != tc.class {
				t.Fatalf("class = %s, want %s", class, tc.class)
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

func TestEvidence502ExhaustedChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1735689600")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
	}))
	defer srv.Close()

	rec := NewRecorder()
	_, uerr, class := doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
	if uerr == nil || uerr.Status != 502 || class != ClassUpstream5xx {
		t.Fatalf("uerr=%v class=%s", uerr, class)
	}
	rows := rec.Rows()
	if len(rows) != 4 { // 3 matrix retries + the terminal dial
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	for i, row := range rows {
		if row.Dial != i+1 {
			t.Fatalf("row %d has dial %d", i, row.Dial)
		}
		if row.Status != 502 || row.Class != ClassUpstream5xx.String() {
			t.Fatalf("row %d = %+v", i, row)
		}
		if row.RateLimit == nil {
			t.Fatalf("row %d: retried rows keep headers-only evidence (rate limits)", i)
		}
		if i < 3 {
			// Retried verdicts are captured BEFORE drainAndClose: headers-only
			// (body unread), but the retry disposition is already decided.
			if !row.Retried || row.RetryDecision != RetryRetrySameEgress || row.RetryDelayMS != config.RetryRules[502].Delay.Milliseconds() {
				t.Fatalf("retried row %d = %+v", i, row)
			}
			if row.BodyPeek != "" || row.BodyBytes != 0 || row.Message != "" {
				t.Fatalf("retried row %d must be headers-only: %+v", i, row)
			}
		}
	}
	term := rows[3]
	if term.Retried || term.RetryDecision != "" || term.MatrixDraws != 3 {
		t.Fatalf("terminal row = %+v, want exhausted matrix (3 draws) and no client-level decision", term)
	}
	if term.BodyPeek == "" || term.Message != "bad gateway" {
		t.Fatalf("terminal row lost the body: %+v", term)
	}
}

func TestEvidence503RetryThenSuccessLeavesOneRow(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	rec := NewRecorder()
	resp, uerr, _ := doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
	if uerr != nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	rows := rec.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly the failed dial (success gets none)", len(rows))
	}
	if rows[0].Status != 503 || !rows[0].Retried || rows[0].RetryDelayMS != config.RetryRules[503].Delay.Milliseconds() {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestEvidenceTransportErrors(t *testing.T) {
	t.Run("connection refused chain", func(t *testing.T) {
		// A closed listener: every dial is refused, and the 502 rule retries
		// the transport 3 times before the terminal row.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		rec := NewRecorder()
		_, uerr, class := doObserved(newTestClient(&recordingSleeper{}), url, rec)
		if uerr == nil || class != ClassConnectionError {
			t.Fatalf("uerr=%v class=%s", uerr, class)
		}
		rows := rec.Rows()
		if len(rows) != 4 {
			t.Fatalf("rows = %d, want 3 retried + 1 terminal", len(rows))
		}
		for i, row := range rows {
			if row.Phase != PhaseTransport || row.Status != 0 || row.Class != ClassConnectionError.String() {
				t.Fatalf("row %d = %+v", i, row)
			}
			if row.Message == "" || row.Fingerprint == "" {
				t.Fatalf("row %d missing message/fingerprint", i)
			}
		}
		if !strings.Contains(rows[3].Message, "connection refused") {
			t.Fatalf("terminal message = %q, want the transport text", rows[3].Message)
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
			_, _, _ = doObserved(newTestClient(&recordingSleeper{}), "http://"+addr, rec)
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
	_, _, _ = doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
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
	_, _, _ = doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
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
	resp, uerr, _ := doObserved(newTestClient(&recordingSleeper{}), srv.URL, rec)
	if uerr != nil {
		t.Fatalf("uerr = %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	if rec.Len() != 0 {
		t.Fatalf("rows = %d, want 0 (the completion line owns success)", rec.Len())
	}
}
