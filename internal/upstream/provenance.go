package upstream

import (
	"context"
	"errors"
	"net"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
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
//   - The state belongs to the LOGICAL CALL, not to a hop (issue #60). One
//     intent to obtain one model response may take several network
//     interactions — a redirect hop, a fresh connection — and it ends exactly
//     once (docs/recovery-semantics.md, Vocabulary). A hop that dials after an
//     earlier hop already transmitted cannot claim the request never went out;
//     the call's transmission record is monotonic and outlives every hop.
//   - not_sent may be claimed ONLY from a boundary that can prove it: the
//     dial / TLS / proxy-protocol phases this package performs itself, every
//     one of which completes before net/http is handed a connection — and
//     only while NO hop of the call has handed a request byte to one.
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
	// never wrote a byte through it — AND no earlier hop of the same logical
	// call wrote one either (the claim is about the call, issue #60).
	RequestStateNotSent
	// RequestStateResponseStarted: a response arrived. The response is terminal
	// for this layer; the request is not replayable under any circumstance.
	// On a transport failure this is the state a later hop inherits from an
	// earlier one that was already answered — the redirect that made it a
	// multi-hop call proves a response existed on this logical call.
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

// callTrace is the logical upstream call's delivery record: the facts that
// hold for the whole call however many hops it takes. Both are monotonic —
// once a byte or a response byte has existed, no later hop may un-know it
// (issue #60).
//
// Both flags are written from goroutines the caller does not own (the dial
// goroutine, the transport's write loop) and read once the hop has returned,
// so they are atomic rather than plain bools.
type callTrace struct {
	// transmitted: a request byte was handed to a connection on SOME hop of
	// this call. Set by the per-hop write hook.
	transmitted atomic.Bool
	// responded: a response was received on SOME hop — for a multi-hop call,
	// the redirect that made it multi-hop.
	responded atomic.Bool
}

// noteResponse records that a hop received a response. Only a hop whose
// response was a followed redirect leaves the call running, so this is the
// fact a LATER hop's failure inherits.
func (c *callTrace) noteResponse() {
	if c != nil {
		c.responded.Store(true)
	}
}

// state is the request state a hop's failure inherits from the call's history
// when that history forbids not_sent: a call that was already answered reports
// response_started, and one that transmitted without being answered (or whose
// answer this process never saw) reports unknown. Neither authorises a replay.
func (c *callTrace) state() RequestState {
	if c != nil && c.responded.Load() {
		return RequestStateResponseStarted
	}
	return RequestStateUnknown
}

// dialTrace is ONE HOP's record of what the egress path did. It is written by
// the dialers this package owns, by the request-write hook, and read once,
// after the hop returns. Every hop of a logical call gets its own: a hop's
// dial facts describe that hop only, and the call-level monotonic facts live
// in callTrace.
//
// Why the split exists at all (issue #60): the phase must come from the code
// that spoke the protocol, but classification happens at the call site that
// only sees an error value — and a redirect chain has several dials and
// several writes under ONE logical call. A single trace reused across hops
// both loses the call's history (hop 1's write is invisible to hop 2's dial
// failure, so hop 2 claims not_sent and the request is re-sent) and corrupts
// the hop's own facts (hop 1's successful dial masks hop 2's failed one).
// Per-hop records plus a monotonic call record is the shape that cannot say
// either of those things.
//
// The trace reaches the dialers through the request context (net/http's dial
// context retains context VALUES — net/http/transport.go getConn builds it
// with context.WithoutCancel — so a custom DialContext/DialTLSContext sees
// it), and it is the dialer, not the classifier, that knows whether a dial
// happened and how far it got.
//
// The mutex is not decoration: a transport may dial on its own goroutine, and
// the write hook fires on the transport's write loop. Channel hand-off makes
// the common path happen-before, but the lock removes the question entirely at
// the cost of one uncontended acquisition per phase.
//
// A hop's trace may also outlive the hop: a dial whose request was abandoned
// (a context cancel, a per-hop timeout) keeps running to completion in the
// transport, and writes into ITS OWN hop's trace. That is exactly why the
// record is per hop — a late writer can never pollute the next hop's record.
type dialTrace struct {
	call   *callTrace
	mu     sync.Mutex
	dialed bool // a dial was STARTED in this hop (not necessarily finished)
	dialOK bool // ... and finished successfully
	wrote  bool // the transport handed request bytes to a conn during this hop
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

// markWrite records that this hop handed request bytes to a connection. It is
// called from the request-write hook on the transport's write loop and is
// deliberately conservative in two ways:
//
//   - it is set for an ATTEMPTED write (the hook fires whether or not the
//     write returned an error): a write that failed partway may still have put
//     a prefix on the wire, and no evidence here can tell those apart. The
//     cost of being wrong in that direction is a failover that did not happen;
//     the cost of being wrong the other way is a duplicate POST.
//   - it is set on the hop AND on the call. The hop flag is this hop's
//     pre-transmission proof; the call flag is monotonic and survives into
//     every later hop of the same logical call (issue #60).
func (t *dialTrace) markWrite() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.wrote = true
	t.mu.Unlock()
	if t.call != nil {
		t.call.transmitted.Store(true)
	}
}

// writeHook is the httptrace hook that feeds markWrite. It is per REQUEST, not
// per connection — which is what makes a write over a POOLED connection
// observable at all: the previous instrument wrapped the conns this call
// dialed, so a hop served by an already-idle connection transmitted invisibly
// (issue #60). It also keeps non-request traffic out of the record: a TLS
// ClientHello is not a request byte and never reaches this hook.
func (t *dialTrace) writeHook() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { t.markWrite() },
	}
}

// provenance reduces the hop's record, the call's history, and the error's own
// type to the phase and request state the failure can be attributed. See
// FailurePhase/RequestState and postDialPhase.
func (t *dialTrace) provenance(err error) (FailurePhase, RequestState) {
	if t == nil {
		return FailurePhaseNone, RequestStateUnknown
	}
	t.mu.Lock()
	dialed, dialOK, wrote, phase := t.dialed, t.dialOK, t.wrote, t.phase
	t.mu.Unlock()

	var fp FailurePhase
	switch {
	case !dialed:
		// A pooled connection: the transport reused a live conn, so this hop
		// performed no dial at all. Nothing is attributable and nothing is
		// proven — unknown, never not_sent.
		return FailurePhaseNone, RequestStateUnknown
	case !dialOK:
		// The dial itself failed: a phase this package performed, entirely
		// before net/http owned the connection.
		fp = phase
	case !wrote:
		// The connection came up and the transport never handed it a request
		// byte during this hop. The recorded phase completed successfully,
		// hence no attribution.
		fp = FailurePhaseNone
	default:
		// Request bytes were handed to a conn during this hop. Whatever went
		// wrong after that is not provably pre-transmission.
		return postDialPhase(err), RequestStateUnknown
	}
	// fp is a phase this hop provably reached without writing. That is enough
	// to prove the hop unsent — but not enough to prove the CALL unsent: a
	// logical call that already transmitted on an earlier hop cannot claim
	// not_sent for a later hop's dial failure (issue #60). The hop's phase is
	// still the honest attribution of where THIS hop failed.
	if t.call != nil && t.call.transmitted.Load() {
		return fp, t.call.state()
	}
	return fp, RequestStateNotSent
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

// traceKey is the context key for the current HOP's dial trace. An unexported
// struct type, so no other package can collide with it.
type traceKey struct{}

// withHopTrace opens one hop of a logical call: a fresh per-hop record bound to
// the call's monotonic record, plus the request-write hook that feeds both.
// attempt calls it once per hop, so every dial and every write below it is
// attributed to the hop it belongs to (issue #60).
func withHopTrace(ctx context.Context, call *callTrace) (context.Context, *dialTrace) {
	hop := &dialTrace{call: call}
	// Both values must ride the request context: the trace for the dialers,
	// which read it from net/http's dial context, and the hook for net/http
	// itself, which reads it from the request context in Request.write.
	ctx = context.WithValue(ctx, traceKey{}, hop)
	return httptrace.WithClientTrace(ctx, hop.writeHook()), hop
}

// traceOf returns the current hop's dial trace, or nil when the context
// carries none (a client driven directly, or a test exercising a dialer on its
// own). Every method on *dialTrace is nil-safe, and recordingDialer skips the
// wrapper entirely when there is nothing to record into.
func traceOf(ctx context.Context) *dialTrace {
	t, _ := ctx.Value(traceKey{}).(*dialTrace)
	return t
}

// recordingDialer wraps a dial function so the hop's trace learns about it. It
// brackets the dial: the trace learns a dial started (with base as the baseline
// phase) and, when the dial returns, whether it completed. It no longer wraps
// the returned conn — request bytes are observed by the write hook, which sees
// every request including the ones that ride a pooled connection.
//
// With no trace in the context — every direct caller and every test that
// exercises a dialer on its own — the wrapper is transparent, so no caller can
// observe the instrumentation.
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
		return conn, nil
	}
}
