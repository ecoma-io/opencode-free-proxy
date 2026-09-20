package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"

	"opencode-free-proxy/internal/config"
)

// socks5Dialer implements a minimal SOCKS5 CONNECT dialer (RFC 1928 §3-4)
// with username/password auth (RFC 1929). Hostnames resolve LOCALLY: CONNECT
// carries the resolved IP — matching the config-level rejection of socks5h,
// whose remote-DNS semantics are deliberately out of scope. No x/net dep:
// the protocol is small enough to hand-roll with the stdlib.
type socks5Dialer struct {
	proxy  *url.URL
	dialer net.Dialer
}

func newSocks5Dialer(proxy *url.URL) *socks5Dialer {
	return &socks5Dialer{proxy: proxy, dialer: net.Dialer{Timeout: config.ConnectTimeout}}
}

// DialContext establishes the tunnel for addr ("host:port" of the UPSTREAM —
// resolved locally). The returned conn is ready for the caller's traffic.
func (d *socks5Dialer) DialContext(ctx context.Context, _ string, addr string) (net.Conn, error) {
	conn, err := d.dialer.DialContext(ctx, "tcp", proxyDialAddr(d.proxy))
	if err != nil {
		return nil, fmt.Errorf("socks5: dial proxy: %w", err)
	}
	// Bound the WHOLE handshake: the net.Dialer timeout covers only the TCP
	// connect, and a proxy that accepts then never answers would otherwise
	// strand the dial goroutine past any caller deadline (the HTTPS
	// transport's ResponseHeaderTimeout never starts — Client.Do has not
	// returned yet). Reads below have no ctx of their own, hence the
	// ctx-abort: cancellation mid-handshake closes the conn, aborting the
	// in-flight ReadFull. The deadline clears once the tunnel is established.
	_ = conn.SetDeadline(time.Now().Add(config.ConnectTimeout))
	// context.AfterFunc, not a select-watcher goroutine: a watcher selecting
	// between ctx.Done() and a handshake-done channel may pick ctx.Done() —
	// and Close() the just-returned LIVE tunnel — when both channels are
	// ready simultaneously (Go's select picks randomly). AfterFunc arms only
	// on ctx cancellation and stop() disarms it deterministically, so a
	// completed handshake can never race its own teardown; stop() runs on
	// every return below, so there is no watcher leak either.
	stopWatcher := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatcher()
	if err := d.negotiate(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := d.connect(ctx, conn, addr); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{}) // caller owns the conn from here
	return conn, nil
}

// negotiate picks the auth method (RFC 1928 §3) and, if the proxy demands
// it, authenticates (RFC 1929 §2). Credentials come from the proxy URL
// userinfo; nothing here ever formats them into an error.
func (d *socks5Dialer) negotiate(ctx context.Context, conn net.Conn) error {
	hasAuth := d.proxy.User != nil
	methods := []byte{0x00} // no-auth is always offered
	if hasAuth {
		methods = append(methods, 0x02) // username/password
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if err := writeAll(ctx, conn, greet); err != nil {
		return err
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if buf[0] != 0x05 {
		return fmt.Errorf("socks5: proxy replied with version %d", buf[0])
	}
	switch buf[1] {
	case 0x00:
		return nil
	case 0x02:
		if !hasAuth {
			return &proxyAuthError{msg: "socks5: proxy demanded auth but none configured"}
		}
		user := d.proxy.User.Username()
		pass, _ := d.proxy.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			return fmt.Errorf("socks5: credentials exceed RFC 1929 length limit")
		}
		p := []byte{0x01, byte(len(user))}
		p = append(p, user...)
		p = append(p, byte(len(pass)))
		p = append(p, pass...)
		if err := writeAll(ctx, conn, p); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		// RFC 1929 §2: version 0x01 + status 0x00 is success; anything else
		// is typed proxyAuthError. HONEST CAVEAT: a MALFORMED reply (a wrong
		// version byte, garbage status — a hostile or broken proxy) is not
		// strictly a credential verdict, yet it lands typed anyway because
		// this 2-byte reply cannot distinguish rejection from corruption.
		// The conservative typing is kept deliberately: both classes mark
		// health and allow fallback identically, so only the label differs —
		// and only this boundary code may produce the type either way.
		if buf[0] != 0x01 || buf[1] != 0x00 {
			return &proxyAuthError{msg: "socks5: proxy authentication failed"}
		}
		return nil
	case 0xff:
		// NO ACCEPTABLE METHODS (RFC 1928 §3): the proxy rejected every
		// method we offered — if we offered only 0x00, it demanded auth we
		// never sent; with 0x02 offered it refused our credential mechanism.
		// Same proxyAuthError contract as the 0x02 branch above (failure.go:
		// "a proxy that demanded auth we never sent").
		return &proxyAuthError{msg: "socks5: no acceptable authentication method"}
	default:
		return fmt.Errorf("socks5: proxy chose unknown method %d", buf[1])
	}
}

// connect sends CONNECT for addr with the host resolved LOCALLY and validates
// the reply, consuming the variable-length bind address (RFC 1928 §4, §6).
func (d *socks5Dialer) connect(ctx context.Context, conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5: bad target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("socks5: bad target port %q: %w", portStr, err)
	}
	// LOCAL resolution: the proxy never sees the hostname. This is the
	// socks5 contract — socks5h would flip DNS to the proxy side.
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		if err == nil {
			err = fmt.Errorf("no addresses")
		}
		return fmt.Errorf("socks5: resolve %q locally: %w", host, err)
	}
	ip := addrs[0].IP
	var atyp byte
	var addrBytes []byte
	if v4 := ip.To4(); v4 != nil {
		atyp, addrBytes = 0x01, v4
	} else {
		atyp, addrBytes = 0x04, ip.To16()
	}
	req := []byte{0x05, 0x01, 0x00, atyp}
	req = append(req, addrBytes...)
	req = append(req, byte(port>>8), byte(port))
	if err := writeAll(ctx, conn, req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5: bad reply version %d", head[0])
	}
	if head[1] != 0x00 {
		return replyErr(head[1])
	}
	var bound int
	switch head[3] {
	case 0x01:
		bound = 4
	case 0x04:
		bound = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		bound = int(l[0])
	default:
		return fmt.Errorf("socks5: bad bind address type %d", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(bound+2)); err != nil {
		return err
	}
	return nil
}

func replyErr(rep byte) error {
	switch rep {
	case 0x00:
		return nil
	case 0x01:
		return fmt.Errorf("socks5: general failure")
	case 0x02:
		// RFC 1928 §6: "connection not allowed by ruleset" — a policy
		// refusal, NOT a credential failure. Plain error on purpose so it
		// lands in ClassConnectionError (same fallback+health behavior,
		// correct label); only RFC 1929 auth failures are ClassProxyAuthError.
		return fmt.Errorf("socks5: connection not allowed by ruleset")
	case 0x03:
		return fmt.Errorf("socks5: network unreachable")
	case 0x04:
		return fmt.Errorf("socks5: host unreachable")
	case 0x05:
		return fmt.Errorf("socks5: connection refused")
	case 0x06:
		return fmt.Errorf("socks5: ttl expired")
	case 0x07:
		return fmt.Errorf("socks5: command not supported")
	case 0x08:
		return fmt.Errorf("socks5: address type not supported")
	default:
		return fmt.Errorf("socks5: unknown reply %d", rep)
	}
}

func writeAll(ctx context.Context, conn net.Conn, b []byte) error {
	for len(b) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := conn.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
