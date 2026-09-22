package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"

	"opencode-free-proxy/internal/config"
)

// Origin TLS identity (issue #48): every origin handshake this package
// performs speaks the OFFICIAL opencode client's ClientHello, so the free-tier
// upstream cannot distinguish this proxy from a genuine client on the TLS
// layer.
//
// The reference is a field-by-field capture of the official CLI v1.18.31
// (2026-09-22, local CONNECT capture rig — 11 connections to
// models.opencode.ai:443 and opencode.ai:443, all identical in shape): the
// CLI is a Bun v1.3.14 binary, and its TLS is Bun's BoringSSL in the DEFAULT
// embedder configuration — NOT Chrome-shaped (no GREASE anywhere, no
// post-quantum key share) and ALPN offers http/1.1 ONLY (Bun's fetch does not
// enable HTTP/2). Two consequences the rest of the transport layer is pinned
// to:
//
//   - ALPN parity is ["http/1.1"], so HTTP/2 is never negotiated and stdlib
//     *http.Transport keeps speaking HTTP/1.1 over the returned *utls.UConn —
//     exactly what the official client does against the same upstream. (The
//     pre-parity code offered ["h2", "http/1.1"]; retiring that mismatch is
//     the point of this file.)
//   - stdlib never upgrades a non-*tls.Conn returned from DialTLSContext to
//     its bundled HTTP/2 (go1.26.4 net/http dialConn sets tlsState only
//     inside the *tls.Conn type assertion and the TLSNextProto dispatch
//     reads it) — irrelevant under http/1.1-only ALPN, and the reason
//     ForceAttemptHTTP2 is dropped from the transports that use this dialer:
//     it would be dead config, and dead config lies.
//
// Like connect.go's tlsMinVersion (issue #18), the spec below is
// transport-layer identity, NOT an open-sse constant — the 9router runtime it
// ports is Node/undici whose hello differs from both sides of this change —
// so it lives here with its evidence rather than in internal/config.

// officialClientHelloSpec is the captured official-client ClientHello:
//
//	JA3 c8fae8189ffed5fd35dbfd98aa3e7b77
//	legacy_version 0303, session_id 32 random bytes
//	ciphers (17, ordered):
//	  1301 1302 1303 c02b c02f c02c c030 cca9 cca8
//	  c009 c013 c00a c014 009c 009d 002f 0035
//	extensions (14, ordered):
//	  server_name, extended_master_secret, renegotiation_info(00),
//	  supported_groups[001d 0017 0018], ec_point_formats[00],
//	  session_ticket(empty), alpn["http/1.1"], status_request(0100000000),
//	  signature_algorithms[0403 0804 0401 0503 0805 0501 0806 0601 0201],
//	  signed_certificate_timestamp(empty), key_share[x25519],
//	  psk_key_exchange_modes[01], supported_versions[0304 0303],
//	  padding(BoringSSL style: handshake message padded up to 512 bytes)
//
// The spec is rebuilt PER HANDSHAKE, not shared: utls writes marshaling-time
// state into the extension structs themselves (UtlsPaddingExtension stores
// its computed PaddingLen, key shares are generated into the spec's
// KeyShareExtension), so one shared spec value would be a data race between
// concurrent dials. The builder is cheap — plain literals.
func officialClientHelloSpec() utls.ClientHelloSpec {
	return utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		// 0 = compressionNone / pointFormatUncompressed — unexported in
		// utls, and the captured hello offers exactly one of each.
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{
				Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384},
			},
			&utls.SupportedPointsExtension{
				SupportedPoints: []byte{0},
			},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{
				SupportedSignatureAlgorithms: []utls.SignatureScheme{
					utls.ECDSAWithP256AndSHA256,
					utls.PSSWithSHA256,
					utls.PKCS1WithSHA256,
					utls.ECDSAWithP384AndSHA384,
					utls.PSSWithSHA384,
					utls.PKCS1WithSHA384,
					utls.PSSWithSHA512,
					utls.PKCS1WithSHA512,
					utls.PKCS1WithSHA1,
				},
			},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{
				Modes: []uint8{utls.PskModeDHE},
			},
			&utls.SupportedVersionsExtension{
				Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12},
			},
			// BoringPaddingStyle is utls' port of BoringSSL's own padding
			// rule (handshake message padded up to 512 bytes; the captured
			// official hello carries it as the final extension with 237
			// zero bytes for the "opencode.ai" SNI).
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// originTLSConfig builds the ORIGIN-hop TLS settings for the forged hello:
// ServerName pinned to the dialed host unless the injected config names one
// (the transport cannot know the host — from its view these are direct
// connections), verification and root pool taken from the injected client
// config (tests inject a pool for self-signed fixtures; production leaves
// nil for system roots), the issue #18 floor stated on the literal and on
// every injected clone. ALPN is identity, NOT a knob: the official client's
// offer is http/1.1 only, so an injected NextProtos (nothing sets one today)
// is overwritten rather than honored — the spec's ALPNExtension drives the
// wire offer regardless, and this keeps the negotiated-protocol bookkeeping
// consistent with it.
func originTLSConfig(base *tls.Config, addr string) *tls.Config {
	cfg := &tls.Config{MinVersion: tlsMinVersion}
	if base != nil {
		cfg = base.Clone()
		floorTLS(cfg)
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			cfg.ServerName = host
		}
	}
	cfg.NextProtos = []string{"http/1.1"}
	return cfg
}

// utlsConfigFrom converts the stdlib-shaped origin config into utls' own
// Config (utls forks crypto/tls.Config rather than aliasing it). Only the
// fields this package's seam can carry are copied — ServerName for SNI,
// RootCAs/InsecureSkipVerify for verification, the version bounds, and the
// ALPN offer (informational under a custom spec: the spec's ALPNExtension is
// what goes on the wire; NextProtos still feeds negotiation bookkeeping).
func utlsConfigFrom(cfg *tls.Config) *utls.Config {
	u := &utls.Config{}
	if cfg != nil {
		u.ServerName = cfg.ServerName
		u.RootCAs = cfg.RootCAs
		u.InsecureSkipVerify = cfg.InsecureSkipVerify
		u.MinVersion = cfg.MinVersion
		u.MaxVersion = cfg.MaxVersion
		u.NextProtos = cfg.NextProtos
	}
	return u
}

// handshakeOrigin performs the origin TLS handshake over an established raw
// conn, speaking the official client's hello. It does NOT arm a deadline or a
// ctx watcher — every caller already owns both for its whole dial (connect.go
// spans dial+CONNECT+origin TLS under one deadline with one AfterFunc
// watcher; originTLSDialer below arms its own for the post-dial handshake),
// so this only handshakes and hands the conn back, closed on error.
func handshakeOrigin(ctx context.Context, conn net.Conn, cfg *tls.Config) (*utls.UConn, error) {
	uconn := utls.UClient(conn, utlsConfigFrom(cfg), utls.HelloCustom)
	spec := officialClientHelloSpec()
	if err := uconn.ApplyPreset(&spec); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("apply hello spec: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return uconn, nil
}

// originTLSDialer wraps a raw dialer (direct TCP or the SOCKS5 tunnel) into
// an origin TLS dialer speaking the forged hello. The raw dialer owns its own
// bounds (DialTimeout for direct TCP, the SOCKS handshake deadline in
// socks5.go); the TLS handshake then gets its own single conn deadline —
// deadlineFrom(ctx, ConnectTimeout), the caller's deadline winning when
// earlier — plus the same context.AfterFunc teardown discipline as
// connect.go/socks5.go: AfterFunc arms ONLY on ctx cancellation and stop()
// disarms it deterministically on the success path, so a completed handshake
// can never race its own teardown, and the raw conn is captured AT SPAWN
// because conn is reassigned to the TLS wrapper inside handshakeOrigin.
func originTLSDialer(
	dialRaw func(ctx context.Context, network, addr string) (net.Conn, error),
	tlsConfig func() *tls.Config,
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialRaw(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(deadlineFrom(ctx, config.ConnectTimeout))
		rawConn := conn
		stopWatcher := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
		defer stopWatcher()

		var base *tls.Config
		if tlsConfig != nil {
			base = tlsConfig()
		}
		uconn, err := handshakeOrigin(ctx, conn, originTLSConfig(base, addr))
		if err != nil {
			return nil, fmt.Errorf("origin tls %s: %w", addr, err)
		}
		_ = conn.SetDeadline(time.Time{}) // caller owns the conn from here
		return uconn, nil
	}
}
