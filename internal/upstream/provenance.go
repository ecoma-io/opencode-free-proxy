package upstream

import (
	"context"
	"errors"
	"net"
	"sync"
)

// provenance.go — WHERE an upstream failure happened, and HOW MUCH of the
// request can have reached the provider.
//
// Class (failure.go) answers "what kind of failure is this" for the decisions
// this build makes today. It cannot answer the question recovery turns on:
// did the failure happen BEFORE the request was transmitted? A proxy-connect
// refusal and a response-header timeout are both
// ClassTimeout/ClassConnectionError, yet only the first may be replayed on
// another egress. This file carries that second axis as a SEPARATE value
// rather than widening the enum, so `Class` keeps the shape the JS taxonomy
// gave it while `ReplaySafe()` is what the health mark and the egress move
// both read (issue #53, docs/recovery-semantics.md).
//
// The rules, stated once:
//
//   - not_sent may be claimed ONLY from a boundary that can prove it: the
//     dial / TLS / proxy-protocol phases this package performs itself, every
//     one of which completes before net/http is handed a connection.
//   - Everything after that hand-over (request write, response headers) is
//     UNKNOWN. This layer cannot prove what left the process, so it must not
//     guess — unknown authorises nothing.
//   - An attempt that reused a POOLED connection dialed nothing at all, so it
//     has no phase and degrades to unknown. That is the case a naive "the
//     error came out of DialContext" reading gets wrong.
//   - Nothing here is ever inferred from error TEXT (issue #6). Phases are
//     recorded by the code that spoke the protocol; the one post-dial
//     inference is typed (net.Error.Timeout, *net.OpError.Op).
//
// Deliberate Go-side addition, per AGENTS.md porting discipline #4: the ported
// open-sse router runs one egress behind one fetch and has no notion of which
// phase of its own egress failed, so there is no JS source to cite. This is
// transport-boundary bookkeeping with no open-sse counterpart.

// Origin names the side of the wire a failure came from.
type Origin int

const (
	// OriginNone: no failure to attribute — a success, or a row that has no
	// origin (a skip is a scheduling fact, not a failure).
	OriginNone Origin = iota
	// OriginUpstream: an HTTP response existed. Any status, including the
	// 4xx/5xx verdicts — the provider answered, so the request was received
	// and the response is the provider's.
	OriginUpstream
	// OriginTransport: the egress path itself failed and no HTTP response
	// exists. Only this origin can ever be a replay candidate, and only when
	// RequestState proves the request never went out.
	OriginTransport
	// OriginClient: the downstream caller went away. Nothing to deliver to,
	// nothing to replay.
	OriginClient
)

func (o Origin) String() string {
	switch o {
	case OriginUpstream:
		return "upstream"
	case OriginTransport:
		return "transport"
	case OriginClient:
		return "client"
	default:
		return ""
	}
}

// FailurePhase names the step of the egress path that was in progress when a
// transport failure was observed. Phases before the request is written are
// recorded by the dialer that spoke that protocol; phases after it are
// attributed only when Go's own error type says so.
//
// FailurePhaseNone is the zero value and means "not attributable" — an honest
// gap, never a guess. It appears for a pooled-connection failure (no dial ran,
// so no phase exists) and for a post-dial failure whose type carries no
// direction.
type FailurePhase int

const (
	FailurePhaseNone FailurePhase = iota
	// FailurePhaseProxyConnect: the TCP connection to the proxy endpoint.
	FailurePhaseProxyConnect
	// FailurePhaseProxyTLS: the TLS hop to an https proxy endpoint, before
	// any CONNECT is spoken.
	FailurePhaseProxyTLS
	// FailurePhaseProxyAuth: the proxy refused credentials — a typed
	// *proxyAuthError from the 407 CONNECT reply (connect.go) or from the
	// SOCKS5 method/auth exchange (socks5.go).
	FailurePhaseProxyAuth
	// FailurePhaseSocks5Greeting: RFC 1928 §3 version/method negotiation.
	FailurePhaseSocks5Greeting
	// FailurePhaseSocks5Auth: the RFC 1929 §2 username/password exchange.
	FailurePhaseSocks5Auth
	// FailurePhaseSocks5Connect: the RFC 1928 §4 CONNECT request, its reply,
	// and (for socks5://) the local target resolution that precedes it.
	FailurePhaseSocks5Connect
	// FailurePhaseConnectWrite / FailurePhaseConnectRead: the HTTP CONNECT
	// request written to an HTTP(S) proxy, and the reply read back.
	FailurePhaseConnectWrite
	FailurePhaseConnectRead
	// FailurePhaseTargetConnect: the TCP connection to the origin (the direct
	// path) — the only dial on an unproxied egress.
	FailurePhaseTargetConnect
	// FailurePhaseOriginTLS: the origin TLS handshake, in the official
	// client's ClientHello (hello.go).
	FailurePhaseOriginTLS
	// FailurePhaseRequestWrite: the HTTP request write failed. NOT safe to
	// replay — a failed write may have transmitted a prefix.
	FailurePhaseRequestWrite
	// FailurePhaseResponseHeaders: the response-header wait failed, timed out,
	// or was reset — the request is known to have been written. NOT safe.
	FailurePhaseResponseHeaders
	// FailurePhaseResponseBody: the response body died after headers. Produced
	// by the router's post-header stream phases, never by the transport
	// classifier: by then commitment has long been made.
	FailurePhaseResponseBody
)

func (p FailurePhase) String() string {
	switch p {
	case FailurePhaseProxyConnect:
		return "proxy_connect"
	case FailurePhaseProxyTLS:
		return "proxy_tls"
	case FailurePhaseProxyAuth:
		return "proxy_auth"
	case FailurePhaseSocks5Greeting:
		return "socks5_greeting"
	case FailurePhaseSocks5Auth:
		return "socks5_auth"
	case FailurePhaseSocks5Connect:
		return "socks5_connect"
	case FailurePhaseConnectWrite:
		return "connect_write"
	case FailurePhaseConnectRead:
		return "connect_read"
	case FailurePhaseTargetConnect:
		return "target_connect"
	case FailurePhaseOriginTLS:
		return "origin_tls"
	case FailurePhaseRequestWrite:
		return "request_write"
	case FailurePhaseResponseHeaders:
		return "response_headers"
	case FailurePhaseResponseBody:
		return "response_body"
	default:
		return ""
	}
}

// RequestState is what this process can PROVE about whether the request
// reached the provider. The zero value is RequestStateUnknown deliberately: an
// unrecorded attempt must never default into the state that authorises a
// replay.
type RequestState int

const (
	// RequestStateUnknown: nothing is proven. A pooled-connection failure, a
	// post-dial failure, a client cancel. Authorises nothing.
	RequestStateUnknown RequestState = iota
	// RequestStateNotSent: the request provably never left this process — the dial,
	// the proxy protocol, or the origin TLS handshake failed before net/http
	// was given a connection, or the connection came up and the transport
	// never wrote a byte through it.
	RequestStateNotSent
	// RequestStateResponseStarted: the provider answered. The response is terminal
	// for this layer; the request is not replayable under any circumstance.
	RequestStateResponseStarted
)

func (s RequestState) String() string {
	switch s {
	case RequestStateNotSent:
		return "not_sent"
	case RequestStateResponseStarted:
		return "response_started"
	default:
		return "unknown"
	}
}

// Failure is one failed upstream interaction's full provenance: the class the
// existing decision tables read, plus where it happened and what is proven
// about the request. It is a value, not a status — it is never stored in a
// Client or a registry.
type Failure struct {
	Class        Class
	Origin       Origin
	Phase        FailurePhase
	RequestState RequestState
}

// ReplaySafe reports whether this failure provably happened before the request
// was transmitted — the single condition under which re-sending the request on
// another egress cannot duplicate provider work.
//
// Deliberately narrow: transport-origin AND proven-not-sent. An HTTP verdict
// is the provider's own answer (OriginUpstream) and is never replayable here;
// an unproven state (unknown) is never replayable either.
//
// It is the ONE predicate the recovery policy reads: the executor gates the
// egress move and the health mark on it (fallback.go), so the two can never
// disagree about whether the egress was at fault. It is also what the router
// attributes a response with on the wire — the phase and request state are
// only meaningful to a caller because this definition is the contract's
// (provenance_header.go, docs/recovery-semantics.md).
func (f Failure) ReplaySafe() bool {
	return f.Origin == OriginTransport && f.RequestState == RequestStateNotSent
}

// statusFailure is the provenance of an HTTP verdict: a response exists, so
// the provider received the request and answered it. The phase is
// response_headers because that is the moment a response first exists.
func statusFailure(status int) Failure {
	return Failure{
		Class:        classifyStatusFor(status),
		Origin:       OriginUpstream,
		Phase:        FailurePhaseResponseHeaders,
		RequestState: RequestStateResponseStarted,
	}
}

// classifyTransportFailure is the provenance of a transport error: the class
// classifyNetErrFor already produces, plus the phase and request state the
// attempt's dial trace can prove.
//
// The context check comes first and matches classifyNetErrFor's: a canceled
// client context is attributed to the caller, and its request state is left
// unknown because nothing downstream can act on it either way.
func classifyTransportFailure(ctx context.Context, err error, trace *dialTrace) Failure {
	class := classifyNetErrFor(ctx, err)
	f := Failure{Class: class, Origin: OriginTransport}
	if class == ClassContextCanceled {
		f.Origin = OriginClient
		return f
	}
	f.Phase, f.RequestState = trace.provenance(err)
	return f
}

// dialTrace is one attempt's record of what the egress path did. It is written
// by the dialers this package owns and read once, after the attempt returns.
//
// Why it exists at all: the phase must come from the code that spoke the
// protocol, but classification happens at the call site that only sees an
// error value. The trace is carried in the request context (net/http's dial
// context retains context VALUES — net/http/transport.go getConn builds it
// with context.WithoutCancel — so a custom DialContext/DialTLSContext sees it),
// and it is the dialer, not the classifier, that knows whether a dial happened
// and how far it got.
//
// The mutex is not decoration: a transport may dial on its own goroutine, and
// the write flag is set from within that dial's conn while the classifier
// reads the trace on the request goroutine. Channel hand-off makes the common
// path happen-before, but the trace outlives the dial and a redirect chain
// reuses one trace across hops — the lock removes the question entirely at the
// cost of one uncontended acquisition per phase.
type dialTrace struct {
	mu     sync.Mutex
	dialed bool // a dial was STARTED in this attempt (not necessarily finished)
	dialOK bool // ... and finished successfully
	wrote  bool // the transport handed request bytes to the returned conn
	phase  FailurePhase
}

// begin marks a dial entering phase p. Called by the outermost dial wrapper, so
// p is the dial's baseline phase — the protocol dialers then refine it as they
// advance.
func (t *dialTrace) begin(p FailurePhase) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.dialed = true
	t.phase = p
	t.mu.Unlock()
}

// enter refines the phase to the step now in progress. The LAST phase entered
// when a dial fails is the phase that failed.
func (t *dialTrace) enter(p FailurePhase) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.phase = p
	t.mu.Unlock()
}

// succeeded marks the dial complete. A dial that returned no error proves the
// phase it recorded is behind us, so the recorded phase stops being a failure
// attribution (see provenance).
func (t *dialTrace) succeeded() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.dialOK = true
	t.mu.Unlock()
}

// markWrite records that the transport attempted a write through the conn it
// was handed. Set on ENTRY to the write, before the underlying write runs, and
// deliberately conservative: a write that fails partway may still have put a
// prefix on the wire, so an attempted write can never be followed by a
// not_sent claim.
func (t *dialTrace) markWrite() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.wrote = true
	t.mu.Unlock()
}

// provenance reduces the trace plus the error's own type to the phase and
// request state the failure can be attributed. See FailurePhase/RequestState
// and postDialPhase.
func (t *dialTrace) provenance(err error) (FailurePhase, RequestState) {
	if t == nil {
		return FailurePhaseNone, RequestStateUnknown
	}
	t.mu.Lock()
	dialed, dialOK, wrote, phase := t.dialed, t.dialOK, t.wrote, t.phase
	t.mu.Unlock()

	switch {
	case !dialed:
		// A pooled connection: the transport reused a live conn, so this
		// attempt performed no dial at all. Nothing is attributable and
		// nothing is proven — unknown, never not_sent.
		return FailurePhaseNone, RequestStateUnknown
	case !dialOK:
		// The dial itself failed: a phase this package performed, entirely
		// before net/http owned the connection. Provably unsent.
		return phase, RequestStateNotSent
	case !wrote:
		// The connection came up and the transport never wrote through it, so
		// nothing from this attempt reached the provider either. The recorded
		// phase completed successfully, hence no attribution.
		return FailurePhaseNone, RequestStateNotSent
	default:
		// Request bytes were handed to the wire. Whatever went wrong after
		// that is not provably pre-transmission.
		return postDialPhase(err), RequestStateUnknown
	}
}

// postDialPhase attributes a failure that happened after the connection was
// established, using Go's own error typing only — never error text (issue #6):
//
//   - net.Error.Timeout: this package arms no conn write deadline for the
//     request itself, so between the request write and the response headers
//     the only deadline in play is the transport's ResponseHeaderTimeout. A
//     post-dial timeout is therefore the response-header wait.
//   - *net.OpError.Op: "write" is the request write, "read" is the header wait.
//
// Anything else stays FailurePhaseNone. Both attributed phases are unsafe to
// replay — the distinction is forensic, not a permission.
func postDialPhase(err error) FailurePhase {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return FailurePhaseResponseHeaders
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "write":
			return FailurePhaseRequestWrite
		case "read":
			return FailurePhaseResponseHeaders
		}
	}
	return FailurePhaseNone
}

// traceKey is the context key for the per-attempt dial trace. An unexported
// struct type, so no other package can collide with it.
type traceKey struct{}

// withDialTrace attaches a fresh trace to one attempt's context.
func withDialTrace(ctx context.Context) (context.Context, *dialTrace) {
	t := &dialTrace{}
	return context.WithValue(ctx, traceKey{}, t), t
}

// traceOf returns the attempt's dial trace, or nil when the context carries
// none (a client driven directly, or a test exercising a dialer on its own).
// Every method on *dialTrace is nil-safe, and recordingDialer skips the
// wrapper entirely when there is nothing to record into.
func traceOf(ctx context.Context) *dialTrace {
	t, _ := ctx.Value(traceKey{}).(*dialTrace)
	return t
}

// recordingConn records that the transport wrote through the connection it was
// handed. It wraps the FINAL conn of a dial — the one net/http speaks the
// request into — so the flag means "request bytes were handed to the wire",
// never "the TLS handshake started" (the handshake happens on the conn this
// one wraps, inside the dialer).
type recordingConn struct {
	net.Conn
	trace *dialTrace
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.trace.markWrite()
	return c.Conn.Write(b)
}

// recordingDialer wraps a dial function so its result is traceable. It brackets
// the dial: the trace learns a dial started (with base as the baseline phase)
// and, when the dial returns, whether it completed.
//
// With no trace in the context — every direct caller and every pre-migration
// test — the wrapper is transparent: the conn is returned untouched, so no
// caller can observe the instrumentation.
func recordingDialer(base FailurePhase, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		t := traceOf(ctx)
		if t == nil {
			return dial(ctx, network, addr)
		}
		t.begin(base)
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		t.succeeded()
		return &recordingConn{Conn: conn, trace: t}, nil
	}
}
