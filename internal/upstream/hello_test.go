package upstream

// Wire-shape and round-trip tests for the forged origin hello (hello.go,
// issue #48). The assertion tables below are transcribed from the CAPTURED
// official opencode CLI v1.18.31 ClientHello (Bun v1.3.14, BoringSSL default
// embedder config — 11 identical captures against models.opencode.ai:443 and
// opencode.ai:443, 2026-09-22; capture method in hello.go's doc). They are
// deliberately written as literals INDEPENDENT of officialClientHelloSpec: if
// the spec builder drifts from the capture, these fail, not rubber-stamp.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

// capturedCiphers / capturedSigAlgs / capturedExtOrder mirror the capture.
var (
	capturedCiphers = []uint16{
		0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
		0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
	}
	capturedSigAlgs = []uint16{
		0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
	}
	capturedExtOrder = []int{
		0, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43, 21,
	}
	capturedGroups    = []uint16{0x001d, 0x0017, 0x0018}
	capturedVersions  = []uint16{0x0304, 0x0303}
	capturedKeyShares = []uint16{0x001d}
)

// isGREASEValue matches the RFC 8701 GREASE codepoints (any 0x?a?a).
func isGREASEValue(v uint16) bool {
	return v&0x0f0f == 0x0a0a
}

// readFirstRecord reads exactly one full TLS record (the ClientHello flight
// fits in one record at this size — the padded official hello is 512 bytes of
// handshake message) and returns its payload.
func readFirstRecord(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 4*1024)
	for len(buf) < 5 {
		n, err := conn.Read(chunk)
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk[:n]...)
	}
	if buf[0] != 0x16 {
		return nil, io.EOF // not a handshake record — nothing to assert
	}
	recLen := int(buf[3])<<8 | int(buf[4])
	for len(buf) < 5+recLen {
		n, err := conn.Read(chunk)
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk[:n]...)
	}
	return buf[5 : 5+recLen], nil
}

// assertOfficialHello parses a ClientHello handshake message and asserts it
// field-by-field against the captured official client.
func assertOfficialHello(t *testing.T, msg []byte) {
	t.Helper()
	if len(msg) < 4 || msg[0] != 0x01 {
		t.Fatalf("not a ClientHello handshake message: %#v", msg[:4])
	}
	hsLen := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
	if 4+hsLen != len(msg) {
		t.Fatalf("handshake length %d does not cover the %d-byte message", hsLen, len(msg))
	}
	// BoringSSL pads the ClientHello message up to 512 bytes (237 zero bytes
	// of ext-21 padding for the captured "opencode.ai" SNI) — the official
	// hello is exactly 512 with a same-magnitude SNI.
	if len(msg) != 512 {
		t.Fatalf("handshake message = %d bytes, want 512 (BoringSSL padding style, per capture)", len(msg))
	}

	b := msg[4:]
	cur := func(n int) []byte {
		if len(b) < n {
			t.Fatalf("ClientHello truncated: want %d more bytes, have %d", n, len(b))
		}
		r := b[:n]
		b = b[n:]
		return r
	}
	u16 := func() uint16 { r := cur(2); return uint16(r[0])<<8 | uint16(r[1]) }

	if v := u16(); v != 0x0303 {
		t.Fatalf("legacy_version = %04x, want 0303 (compat version, real versions in ext 43)", v)
	}
	cur(32) // random
	if sid := cur(1)[0]; sid != 32 {
		t.Fatalf("session_id length = %d, want 32 (TLS 1.3 compatibility session id)", sid)
	}
	cur(32)

	nCiphers := int(u16()) / 2
	if nCiphers != len(capturedCiphers) {
		t.Fatalf("cipher count = %d, want %d", nCiphers, len(capturedCiphers))
	}
	for i := 0; i < nCiphers; i++ {
		if c := u16(); c != capturedCiphers[i] {
			t.Fatalf("cipher[%d] = %04x, want %04x (order is part of the fingerprint)", i, c, capturedCiphers[i])
		}
	}
	if cm := cur(1)[0]; cm != 1 || cur(1)[0] != 0 {
		t.Fatal("compression methods = want exactly [null]")
	}

	extsLen := int(u16())
	exts := cur(extsLen)
	var order []int
	extPayload := map[int][]byte{}
	for len(exts) >= 4 {
		code := int(exts[0])<<8 | int(exts[1])
		elen := int(exts[2])<<8 | int(exts[3])
		exts = exts[4:]
		if len(exts) < elen {
			t.Fatalf("extension %d declares %d bytes but only %d remain", code, elen, len(exts))
		}
		if isGREASEValue(uint16(code)) {
			t.Fatalf("extension %04x is GREASE — the official client's BoringSSL embedder config sends none", code)
		}
		order = append(order, code)
		extPayload[code] = exts[:elen]
		exts = exts[elen:]
	}
	if len(order) != len(capturedExtOrder) {
		t.Fatalf("extension count = %d (%v), want %d", len(order), order, len(capturedExtOrder))
	}
	for i, code := range capturedExtOrder {
		if order[i] != code {
			t.Fatalf("extension[%d] = %d, want %d (full order: %v)", i, order[i], code, order)
		}
	}

	// server_name (0): the dialed hostname.
	sni := extPayload[0]
	if len(sni) < 5 || sni[2] != 0x00 {
		t.Fatalf("server_name payload = %#v, want an SNI host_name entry", sni)
	}
	nameLen := int(sni[3])<<8 | int(sni[4])
	if name := string(sni[5 : 5+nameLen]); name != "origin.example" {
		t.Fatalf("SNI = %q, want origin.example", name)
	}
	// extended_master_secret (23) and SCT (18): empty payloads.
	if len(extPayload[23]) != 0 || len(extPayload[18]) != 0 {
		t.Fatal("extended_master_secret/SCT must carry empty payloads, per capture")
	}
	// renegotiation_info (65281): the empty renegotiated_connection field.
	if reneg := extPayload[65281]; len(reneg) != 1 || reneg[0] != 0x00 {
		t.Fatalf("renegotiation_info = %#v, want [00]", reneg)
	}
	// supported_groups (10).
	groups := u16List(t, extPayload[10])
	assertU16s(t, "supported_groups", groups, capturedGroups)
	// ec_point_formats (11): exactly [uncompressed].
	if pts := extPayload[11]; len(pts) != 2 || pts[0] != 1 || pts[1] != 0 {
		t.Fatalf("ec_point_formats = %#v, want [00]", pts)
	}
	// session_ticket (35): empty (no ticket yet).
	if len(extPayload[35]) != 0 {
		t.Fatal("session_ticket must be empty on a fresh handshake, per capture")
	}
	// alpn (16): exactly ["http/1.1"] — 2-byte list len 9 = 1 length byte + 8
	// chars.
	if alpn := extPayload[16]; string(alpn) != "\x00\x09\x08http/1.1" {
		t.Fatalf("alpn = %#v, want exactly the http/1.1 offer", alpn)
	}
	// status_request (5): OCSP, empty request context and extensions.
	if ocsp := extPayload[5]; string(ocsp) != "\x01\x00\x00\x00\x00" {
		t.Fatalf("status_request = %#v, want 0100000000", ocsp)
	}
	// signature_algorithms (13).
	assertU16s(t, "signature_algorithms", u16List(t, extPayload[13]), capturedSigAlgs)
	// key_share (51): a 2-byte list length, then exactly the captured
	// groups, each x25519 = 32 bytes.
	ks := extPayload[51]
	if len(ks) < 2 {
		t.Fatalf("key_share = %#v, too short for the list length", ks)
	}
	listLen := int(ks[0])<<8 | int(ks[1])
	ks = ks[2:]
	if listLen != len(ks) {
		t.Fatalf("key_share list length %d does not cover the %d-byte payload", listLen, len(ks))
	}
	var ksGroups []uint16
	for len(ks) >= 4 {
		g := uint16(ks[0])<<8 | uint16(ks[1])
		klen := int(ks[2])<<8 | int(ks[3])
		ks = ks[4:]
		if len(ks) < klen {
			t.Fatalf("key_share group %04x declares %d key bytes but only %d remain", g, klen, len(ks))
		}
		if g == 0x001d && klen != 32 {
			t.Fatalf("x25519 key share = %d bytes, want 32", klen)
		}
		ksGroups = append(ksGroups, g)
		ks = ks[klen:]
	}
	assertU16s(t, "key_share groups", ksGroups, capturedKeyShares)
	// psk_key_exchange_modes (45): 1-byte count + [psk_dhe_ke].
	if psk := extPayload[45]; string(psk) != "\x01\x01" {
		t.Fatalf("psk_key_exchange_modes = %#v, want count 1 + mode 01", psk)
	}
	// supported_versions (43).
	sv := extPayload[43]
	var versions []uint16
	if len(sv) < 1 || int(sv[0]) != len(sv)-1 {
		t.Fatalf("supported_versions = %#v, malformed list", sv)
	}
	for i := 1; i+1 < len(sv); i += 2 {
		versions = append(versions, uint16(sv[i])<<8|uint16(sv[i+1]))
	}
	assertU16s(t, "supported_versions", versions, capturedVersions)
	// padding (21): presence asserted by the order table; its length is the
	// 512-byte message target above.
}

func u16List(t *testing.T, data []byte) []uint16 {
	t.Helper()
	if len(data) < 2 {
		t.Fatalf("u16 list = %#v, too short", data)
	}
	l := int(data[0])<<8 | int(data[1])
	if l != len(data)-2 || l%2 != 0 {
		t.Fatalf("u16 list = %#v, length %d does not cover the payload", data, l)
	}
	var out []uint16
	for i := 2; i+1 < len(data); i += 2 {
		v := uint16(data[i])<<8 | uint16(data[i+1])
		if isGREASEValue(v) {
			t.Fatalf("u16 list contains GREASE value %04x — the official client sends none", v)
		}
		out = append(out, v)
	}
	return out
}

func assertU16s(t *testing.T, what string, got, want []uint16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %04x, want %04x", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %04x, want %04x (full: %04x)", what, i, got[i], want[i], got)
		}
	}
}

// TestOriginHelloMatchesOfficialCapture is the offline wire proof: the bytes
// originTLSDialer puts on the wire, parsed at the TCP layer, match the
// captured official client field-for-field — cipher list AND order,
// extension list AND order, every key extension's payload, no GREASE
// anywhere, and the BoringSSL 512-byte padding. The capture server never
// answers, so the handshake fails — the hello itself is the test subject.
func TestOriginHelloMatchesOfficialCapture(t *testing.T) {
	helloCh := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		msg, err := readFirstRecord(conn)
		if err != nil {
			return
		}
		helloCh <- msg
	}()

	// The addr stays a HOSTNAME (the SNI + padding assertions depend on it);
	// dialRaw routes the TCP conn to the local capture listener.
	dial := originTLSDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := dial(context.Background(), "tcp", "origin.example:443")
		errCh <- err
	}()
	select {
	case msg := <-helloCh:
		assertOfficialHello(t, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the capture listener never saw a ClientHello")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("the capture listener closes after reading — the handshake must fail, not succeed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial against the closed capture listener never returned")
	}
}

// TestOriginHandshakeRoundTripDirect proves the direct-mode wiring end to
// end: NewClientFor(nil) + the Client.TLSConfig root-pool seam (so the
// utls.Config conversion carried RootCAs), a real TLS 1.3 handshake against
// a stdlib server in the official hello, HTTP/1.1 negotiated over it, and a
// served SSE round-trip. The server side ALSO pins what the wire carried:
// the offered ALPN, cipher list, and versions exactly as captured.
func TestOriginHandshakeRoundTripDirect(t *testing.T) {
	type serverSeen struct {
		protos     []string
		versions   []uint16
		ciphers    []uint16
		negotiated string
	}
	seen := make(chan serverSeen, 4)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		negotiated := ""
		if r.TLS != nil {
			negotiated = r.TLS.NegotiatedProtocol
		}
		seen <- serverSeen{negotiated: negotiated}
		_, _ = io.WriteString(w, "data: official-hello\n\n")
	}))
	srv.TLS = &tls.Config{
		NextProtos: []string{"http/1.1"},
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			seen <- serverSeen{protos: chi.SupportedProtos, versions: chi.SupportedVersions, ciphers: chi.CipherSuites}
			return nil, nil
		},
	}
	srv.StartTLS()
	defer srv.Close()

	c := noSleepClient(NewClientFor(nil))
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.TLSConfig = &tls.Config{RootCAs: pool}

	resp, uerr := c.Do(context.Background(), srv.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if uerr != nil {
		t.Fatalf("the forged origin handshake must serve a real round-trip, got %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "data: official-hello") {
		t.Fatalf("status=%d body=%q, want the origin's 200 SSE", resp.StatusCode, body)
	}
	// Negotiation is asserted SERVER-side: stdlib's transport only populates
	// Response.TLS off a *tls.Conn (dialConn's type assertion — see hello.go),
	// so a UConn-backed response carries no client-visible ConnectionState.
	// The server's r.TLS is the wire truth.
	for range 2 {
		select {
		case s := <-seen:
			switch {
			case s.protos != nil:
				if len(s.protos) != 1 || s.protos[0] != "http/1.1" {
					t.Fatalf("server saw ALPN offer %v, want [http/1.1]", s.protos)
				}
				assertU16s(t, "server-side cipher offer", s.ciphers, capturedCiphers)
				assertU16s(t, "server-side version offer", s.versions, capturedVersions)
			default:
				if s.negotiated != "http/1.1" {
					t.Fatalf("server negotiated %q, want http/1.1 (the only ALPN offer, per capture)", s.negotiated)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the server never saw the ClientHello or the request")
		}
	}
}

// TestOriginHandshakeRoundTripSocks proves the socks-mode wiring: the SOCKS5
// transport's DialTLSContext rides the SAME socks dialer as plain http
// origins and performs the official origin handshake through the tunnel.
// (The CONNECT mode's equivalent proof is TestConnect200WithDeclaredBodyStillTunnels
// in transport_hardening_test.go — it now tunnels through handshakeOrigin.)
func TestOriginHandshakeRoundTripSocks(t *testing.T) {
	negotiated := make(chan string, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if r.TLS != nil {
			negotiated <- r.TLS.NegotiatedProtocol // the server-side wire truth (see the direct test)
		}
		_, _ = io.WriteString(w, "data: socks-official-hello\n\n")
	}))
	srv.TLS = &tls.Config{NextProtos: []string{"http/1.1"}}
	srv.StartTLS()
	defer srv.Close()

	f := newSocks5Fake(t, 0x00, 0x00, 0x00) // no-auth, tunnels everything
	c := noSleepClient(NewClientFor(&config.Proxy{Type: config.ProxySOCKS5, URL: f.url}))
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.TLSConfig = &tls.Config{RootCAs: pool}

	resp, uerr, class := c.DoClassified(context.Background(), srv.URL+"/zen/v1/chat/completions", staticHeaders(), []byte("{}"))
	if uerr != nil || class != ClassSuccess {
		t.Fatalf("uerr=%v class=%s, want a served round-trip through the socks tunnel in the official hello", uerr, class)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: socks-official-hello") {
		t.Fatalf("tunneled body = %q, want the origin's SSE chunk", body)
	}
	select {
	case proto := <-negotiated:
		if proto != "http/1.1" {
			t.Fatalf("server negotiated %q, want http/1.1 through the socks tunnel", proto)
		}
	default:
		t.Fatal("the origin never saw a request")
	}
}

// TestOriginTLSStallBoundedByContext: an origin that accepts TCP and never
// answers the handshake must not strand the dial — the same contract
// socks5.go and connect.go already honor, proven here for originTLSDialer's
// own conn deadline + AfterFunc teardown (the transport detaches dial-ctx
// cancellation, so without this the handshake would run to the deadline).
func TestOriginTLSStallBoundedByContext(t *testing.T) {
	s := newStallProxy(t, func(net.Conn) error { return nil })
	dial := originTLSDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", s.addr)
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := dial(ctx, "tcp", "origin.example:443")
		errCh <- err
	}()
	select {
	case <-s.greeted:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture flaw: the origin never received the TCP dial")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a stalled handshake must fail once the ctx deadline fires")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled handshake outlived its 250ms deadline by seconds — the origin TLS handshake is unbounded")
	}
	select {
	case <-s.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("the ctx deadline did not CLOSE the stalled conn")
	}
}
