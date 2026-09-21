//go:build e2e

// Inbound-listener hardening E2E: the served process must cut a slowloris
// client — one that opens a connection and stalls mid-header — within the
// configured header deadline, instead of pinning the connection (and its
// goroutine) until the shutdown grace. Driven over a raw TCP connection
// against the real binary; the field wiring itself is unit-asserted in
// cmd/server (TestNewHTTPServerTimeouts).

package e2e

import (
	"net"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// TestSlowHeaderClientIsCut: dial the shared suite proxy, write half a
// request line, then stall. On the header deadline net/http ends the
// connection; the observed stdlib shape (go1.26.4, verified standalone) for
// a stalled REQUEST LINE is "400 Bad Request" + "Connection: close" and
// close — other stalls (mid-header-block) may close bare. The test accepts
// either; the load-bearing assertion is that the connection ENDS within the
// deadline budget instead of hanging until the shutdown grace.
func TestSlowHeaderClientIsCut(t *testing.T) {
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyBase, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Half a request line, then silence: only the header deadline (or
	// process death) can end this connection — nothing legitimate replies.
	if _, err := conn.Write([]byte("GET /healthz HTT")); err != nil {
		t.Fatalf("write partial header: %v", err)
	}

	// Give the read a budget past the configured deadline; with no header
	// timeout at all this read blocks for the whole budget and fails.
	budget := config.HeaderReadTimeout + 5*time.Second
	if err := conn.SetReadDeadline(time.Now().Add(budget)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	start := time.Now()
	var reply strings.Builder
	for {
		buf := make([]byte, 128)
		n, rerr := conn.Read(buf)
		if n > 0 {
			reply.Write(buf[:n])
		}
		if rerr != nil {
			if ne, ok := rerr.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatalf("stalled-header connection still open after %v (deadline %v + 5 s slack) — header timeout not applied", time.Since(start), config.HeaderReadTimeout)
			}
			break // EOF/reset — the server cut the connection
		}
	}
	elapsed := time.Since(start)

	// The cut must come from the header deadline, not from something else
	// killing the connection early (a dead listener would fail the rest of
	// the suite, but assert the shape here too).
	if elapsed < config.HeaderReadTimeout/2 {
		t.Fatalf("connection cut after only %v — something other than the header deadline closed it", elapsed)
	}
	t.Logf("slowloris connection cut after %v; pre-close reply %q", elapsed, reply.String())
}
