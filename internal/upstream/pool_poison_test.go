package upstream

// TestScanLinesPoolPoison: the reader buffer is pooled across requests and
// returned WITHOUT clearing — stale bytes are intentionally left in place,
// and the only reuse boundary is `buf[:n]` (a reader goroutine slices the
// buffer to exactly the bytes its own Read returned, never to capacity).
// These tests force that surface to fail: every Read plants a rare token
// (with embedded newlines) past `n`, so any implementation that scanned the
// buffer beyond `n` would emit the token as bogus lines or tail.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// poisonReader serves its payload like a normal reader, then fills the rest
// of the 32 KiB read buffer — past its own `n` — with a distinctive token
// containing newlines. Under the `buf[:n]` discipline that poison is never
// appended to `pending`, so neither lines nor tail may ever contain it.
type poisonReader struct {
	payload []byte
	off     int
	token   string
}

func (r *poisonReader) Read(buf []byte) (int, error) {
	if r.off >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(buf, r.payload[r.off:])
	r.off += n
	fill := bytes.Repeat([]byte(r.token+"\n"), (len(buf)-n)/(len(r.token)+1)+1)
	copy(buf[n:], fill)
	return n, nil
}

// TestScanLinesPoolPoisonNoLeak runs two sequential ScanLines calls — the
// second reuses the pooled buffer the first returned — each with its own
// payload and its own rare token. Neither call may surface its own token or
// the other call's token in any line or in the tail.
func TestScanLinesPoolPoisonNoLeak(t *testing.T) {
	collect := func(t *testing.T, payload, token string) ([]string, string) {
		t.Helper()
		reader := &poisonReader{payload: []byte(payload), token: token}
		var got []string
		var tail string
		err := ScanLines(context.Background(), reader, time.Second, func(line string) error {
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

	// Request 1 plants ZXQ-POISON-ONE past its own n and returns the buffer
	// to the pool uncleared. Its own lines and tail must be pure payload.
	got, tail := collect(t, "data: one\nline2\ntail-one", "ZXQ-POISON-ONE")
	if len(got) != 2 || got[0] != "data: one" || got[1] != "line2" {
		t.Fatalf("request 1 lines = %q, want [data: one line2]", got)
	}
	if tail != "tail-one" {
		t.Fatalf("request 1 tail = %q, want tail-one", tail)
	}
	for _, s := range append(got, tail) {
		if strings.Contains(s, "ZXQ-POISON-ONE") {
			t.Fatalf("request 1 surfaced its own pooled-buffer poison: %q", s)
		}
	}

	// Request 2 reuses the pooled buffer. It must see neither request 1's
	// stale token nor its own freshly planted one.
	got, tail = collect(t, "data: two\ntail-two", "ZXQ-POISON-TWO")
	if len(got) != 1 || got[0] != "data: two" {
		t.Fatalf("request 2 lines = %q, want [data: two]", got)
	}
	if tail != "tail-two" {
		t.Fatalf("request 2 tail = %q, want tail-two", tail)
	}
	for _, s := range append(got, tail) {
		if strings.Contains(s, "ZXQ-POISON-ONE") {
			t.Fatalf("request 2 leaked request 1's pooled-buffer bytes: %q", s)
		}
		if strings.Contains(s, "ZXQ-POISON-TWO") {
			t.Fatalf("request 2 surfaced its own pooled-buffer poison: %q", s)
		}
	}
}
