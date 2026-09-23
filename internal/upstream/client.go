// Package upstream ports the retrying upstream HTTP call
// (executors/base.js execute + config/runtimeConfig.js retry matrix) and the
// client-facing error envelope (utils/error.js).
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/jsonx"
)

// ErrStreamStalled is ScanLines' stall sentinel: the upstream sent no bytes
// for the whole stall window. Exported and typed so a caller can recognise the
// watchdog without probing the message text — the phase label a stall earns in
// the evidence stream must come from the boundary that raised it, not from
// strings.HasPrefix on an error (issue #6 discipline).
var ErrStreamStalled = errors.New("stream stalled")

// maxErrorBodyBytes caps how much of a terminal error response is read for
// the client-facing message (parseUpstreamError) and how much of a rejected
// response is drained before the connection is returned (drainAndClose).
// Go-side transport
// hygiene, NOT an open-sse constant — JS reads error bodies unbounded
// (utils/error.js:61 `await response.text()`); the cap exists because Go owns
// the process memory the JS runtime would happily commit to a hostile
// upstream. It lives here rather than in internal/config for the same reason
// as connect.go's maxConnectHeaderBytes.
const maxErrorBodyBytes = 1 << 20

// ReadBoundedBody reads up to max bytes of an already-received response body,
// within config.SecondaryReadTimeout total — whichever bound hits first. The
// byte cap alone is not enough: once the response headers have arrived the
// transport's response-HEADER timeout is spent and bounds nothing further, so
// a peer that streams bytes forever below the cap would pin the caller
// indefinitely, and the caller never expected a stream — every site reads a
// fixed-size secondary body (an error envelope, a discarded redirect body, a
// non-SSE guard) that is truncated either way. Go-side hardening; JS reads
// error bodies unbounded (utils/error.js:61).
//
// On expiry the body is CLOSED (which unblocks the racing reader) and nil is
// returned: nil is what these paths already mean for an unusable body, so the
// terminal read falls into parseUpstreamError's raw-text fallback and the
// drain path aborts the connection. The returned body is the caller's to close
// on the normal path; the race's Close is idempotent, and http bodies' Close
// after Close is a no-op. The returned bytes are the caller's; on error (a
// mid-body transport failure, NOT a stall) whatever prefix arrived is what the
// unbounded byte cap would have truncated to anyway.
func ReadBoundedBody(ctx context.Context, body io.ReadCloser, max int) []byte {
	return readBoundedBody(ctx, body, max, config.SecondaryReadTimeout)
}

// readBoundedBody is ReadBoundedBody with an injectable total: the callers
// share the process-wide constant (internal/config owns every runtime
// constant), while the test below pins the watchdog with a short deadline a
// never-ending peer can be held against.
func readBoundedBody(ctx context.Context, body io.ReadCloser, max int, total time.Duration) []byte {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	done := make(chan []byte, 1)
	go func() {
		raw, _ := io.ReadAll(io.LimitReader(body, int64(max)))
		select {
		case done <- raw:
		case <-ctx.Done():
		}
	}()
	select {
	case raw := <-done:
		return raw
	case <-ctx.Done():
		// Close unblocks the reader; the caller closes the body on its own
		// path too, and a second Close is an idempotent no-op on http bodies.
		// The race's partial prefix is discarded: the expiry already decided
		// this body is unusable. Closing here is what makes the goroutine
		// finish (net/http bodies document that Close releases a pending
		// Read), so it is joined on the runner's own schedule; the buffered
		// send in the runner keeps that join from ever blocking this branch.
		_ = body.Close()
		return nil
	}
}

// Client performs ONE upstream call (executors/base.js execute, the
// single-URL shape this proxy uses): it dials, sends, reads headers, and
// returns whatever came back. There is no per-URL budget inside it — see
// DoClassified's note — so its only failure mode is one that produced no
// response, and only a failure that provably preceded the request byte is
// eligible for egress failover.
//
// Redirects: the JS router follows them by default — base.js:144-149 sends
// only method/headers/body/signal through proxyFetch.js (utils/
// proxyFetch.js:203-257), native fetch's default `redirect: "follow"`. WHO
// follows them differs by client:
//
//   - Egress clients (NewClientFor) set followRedirects: attempt follows each
//     hop ITSELF, re-picking the scheme-appropriate transport per hop, because
//     stdlib's redirect loop inside one http.Client cannot — an https request
//     redirected to an http Location would make the TUNNELED transport dial
//     the origin DIRECT (its Proxy is nil and DialContext unset → net/http's
//     zeroDialer), leaking the host IP past the egress, and an http request
//     redirected to https would make the ABSOLUTE-FORM transport speak CONNECT
//     itself, whose 407 refusals carry only the reason phrase (untyped →
//     retry storm, violating the issue #6 contract). undici never had the
//     problem: fetch's redirect handling re-dispatches every hop through the
//     same ProxyAgent dispatcher (undici/lib/web/fetch/index.js http-redirect
//     fetch → mainFetch → httpNetworkOrCacheFetch), so all hops stay on the
//     proxy. The hop cap is config.MaxRedirects — 20, undici's own limit
//     (undici/lib/web/fetch/index.js:1250 `request.redirectCount === 20` →
//     network error), NOT net/http's default of 10.
//   - The direct client (NewClient) leaves CheckRedirect unset: one transport
//     serves both schemes, so stdlib's follow is proxy-safe there.
//
// Accepted consequence (both paths, pinned by TestRedirectFollowedLikeJSFetch
// and the redirect tests): a 307/308 re-POSTs the body — including
// `Authorization: Bearer public` and the x-opencode-* headers — to the
// redirect target. Deliberate divergence, kept from the pre-manual-follow
// behavior: undici STRIPS Authorization on any cross-ORIGIN redirect
// (undici/lib/web/fetch/index.js:1313-1317), which this port does not
// replicate — the follow loop applies stdlib's coarser cross-DOMAIN rule
// instead (see attempt), which keeps credentials across port changes. The
// credentials here are the public bearer + opencode identity headers, all
// deliberately replayable to any free-tier endpoint.
type Client struct {
	// HTTP is the main transport: direct for unproxied and socks5 egresses,
	// Go's absolute-form proxy path for HTTP origins behind an HTTP(S)
	// proxy. tunneled (http/https proxy egresses only) carries HTTPS origins
	// through the hand-rolled CONNECT boundary — attempt picks by scheme.
	HTTP     *http.Client
	tunneled *http.Client
	// Now is the clock the evidence rows time a dial with. There is no Sleep
	// seam any more: it existed to stub the retry matrix's delays, and with the
	// matrix gone no code path in this package waits on purpose.
	Now func() time.Time

	// followRedirects: attempt owns the redirect loop (see the Client doc).
	// Set by NewClientFor; NewClient leaves it false so stdlib follows.
	followRedirects bool

	// TLSConfig holds the ORIGIN TLS settings for every origin handshake
	// (direct, SOCKS5-tunneled, and CONNECT-tunneled; cloned in, also the
	// proxy hop when the proxy endpoint is itself https). Tests inject a
	// root pool for self-signed fixtures; production leaves it nil (system
	// roots). Set before first use.
	TLSConfig *tls.Config
}

// NewClient wires an http.Client whose response-header wait is the connect
// timeout (FETCH_CONNECT_TIMEOUT_MS semantics: time to first response byte
// headers; the SSE body itself streams unbounded). The direct transport: no
// proxy, no credentials. IdleConnTimeout is on every transport for the reason
// config.IdleConnTimeout states (no GC finalizer ever reaps a pooled conn);
// DialTimeout bounds the phase ResponseHeaderTimeout cannot reach (see its
// doc in internal/config — the JS fetch bounds all phases with one abort
// signal), and the origin TLS handshake speaks the official client's hello
// under its own 60s budget (hello.go, issue #48) — stdlib never handshakes
// here, so TLSHandshakeTimeout/ForceAttemptHTTP2 would be dead config.
func NewClient() *Client {
	c := &Client{Now: time.Now}
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	c.HTTP = &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: config.ConnectTimeout,
			IdleConnTimeout:       config.IdleConnTimeout,
			// Same provenance wrapping as NewClientFor (transport.go): one
			// wrapper per dial function, at its outermost layer.
			DialContext:    recordingDialer(FailurePhaseTargetConnect, dialer.DialContext),
			DialTLSContext: recordingDialer(FailurePhaseTargetConnect, originTLSDialer(dialer.DialContext, func() *tls.Config { return c.TLSConfig })),
		},
	}
	return c
}

// UpstreamError is the client-facing failure after parsing an upstream error
// response (parseUpstreamError + createErrorResult).
type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("[%d]: %s", e.Status, e.Message)
}

// Do POSTs bodyJSON to url with headers, making exactly ONE logical upstream
// call. On success the caller receives the live response with a streaming
// body; on an error status the body is read capped, parsed
// (parseUpstreamError), and the connection released. buildHeaders is invoked
// at the TOP of the call, mirroring base.js:127-130, where buildHeaders runs
// inside the attempt loop and forges a fresh x-opencode-request id per
// attempt — here, per executor attempt (the executor is what re-dials now
// that the matrix is gone). The JS body re-transform on the same line is an
// in-place no-op on the already-transformed body, so the already-serialized
// bodyJSON is re-sent unchanged.
func (c *Client) Do(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte) (*http.Response, *UpstreamError) {
	resp, uerr, _ := c.DoClassified(ctx, url, buildHeaders, bodyJSON)
	return resp, uerr
}

// DoClassified is Do plus the failure taxonomy and provenance (failure.go,
// provenance.go) the executor needs for its fallback/health decision. It is
// the nil-recorder shape of DoClassifiedObserved — identical behavior, no
// evidence collection.
func (c *Client) DoClassified(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte) (*http.Response, *UpstreamError, Failure) {
	return c.DoClassifiedObserved(ctx, url, buildHeaders, bodyJSON, nil)
}

// DoClassifiedObserved is DoClassified plus the evidence recorder — rec is
// strictly observational (nil turns it off). It performs exactly ONE logical
// upstream call: one dial, one verdict, no loop. A provider HTTP response of
// ANY status is terminal here (the caller relays it; provider-level retry is
// the Injector's, docs/recovery-semantics.md), and a transport failure ends
// the call carrying its provenance so the executor can decide whether the
// failure provably happened before the request was transmitted.
//
// Row phasing inside the one dial, in order:
//
//  1. verdict — attempt() returns; the row's response/transport facts are
//     built from the verdict BEFORE classification reduces it (the terminal
//     >=400 branch extracts evidence from the same capped raw slice
//     parseUpstreamError reads);
//  2. classification plus provenance — the row receives the failure the
//     transport boundary reported: class, and where on the egress path it
//     happened (Failure).
//
// A row is appended only for a FAILED dial; the dial that serves the request
// produces no row (the router's completion line owns success telemetry).
//
// The failure is returned, not just its class: the class says what went
// wrong, and ONLY the provenance says whether re-sending could duplicate the
// provider's work. `Failure.ReplaySafe()` is the executor's fallback
// predicate; the health mark reads the narrower `Failure.MarksEgressHealth()`
// (issue #62), which is a question about the egress rather than the request.
func (c *Client) DoClassifiedObserved(ctx context.Context, url string, buildHeaders func() map[string]string, bodyJSON []byte, rec *Recorder) (*http.Response, *UpstreamError, Failure) {
	// buildHeaders runs at the TOP of the dial, including the first — mirroring
	// base.js:127-130, where transformRequest and buildHeaders re-run per
	// attempt, so a fresh x-opencode-request id is forged for each one.
	headers := buildHeaders()
	// One logical call, one monotonic delivery record; one trace per hop. The
	// hop trace reaches the dialers through the request context (net/http
	// builds its dial context with context.WithoutCancel, which retains VALUES,
	// so a custom DialContext/DialTLSContext sees it); the call record is what
	// keeps a later hop from re-claiming not_sent after an earlier hop already
	// transmitted (provenance.go, issue #60).
	call := &callTrace{}
	started := c.Now()
	resp, hop, path, netErr := c.attempt(ctx, call, url, headers, bodyJSON)
	dur := c.Now().Sub(started)
	if netErr != nil {
		// The [502] status is only the client-facing envelope — the class is
		// the REAL class (proxy-auth vs timeout vs connection vs ctx), and the
		// provenance is where the failure happened. classifyTransportFailure
		// reads typed transport-boundary errors only: a proxy CONNECT refusal
		// carries a proxyAuthError marker from connect.go/socks5.go, so no
		// error-text probing is involved (issue #6).
		//
		// Surfacing the raw transport text is PARITY, topology exposure
		// included: base.js:179 rethrows the raw fetch error,
		// chatCore.js:373-375 hands it to formatProviderError, and
		// utils/error.js:139-147 deliberately renders
		// `[502]: ${error.message}${cause}` with the comment "Expose low-level
		// cause (e.g. UND_ERR_SOCKET, ECONNRESET, ETIMEDOUT) for diagnosing
		// fetch failures" — so the JS client envelope carries the dial address
		// exactly as this message carries the (credential-redacted) egress
		// host:port. Accepted parity.
		failure := classifyTransportFailure(ctx, netErr, hop)
		appendTransportRow(rec, dur, failure, netErr)
		return nil, &UpstreamError{Status: 502, Message: netErr.Error()}, failure
	}
	if resp.StatusCode >= 400 {
		// The read is CAPPED — a deliberate divergence from JS
		// (utils/error.js:61 `await response.text()` is unbounded): a hostile
		// upstream/proxy streaming an endless 4xx body must not grow this
		// process without limit before the text becomes the client-facing
		// envelope. Semantics are unchanged for any body under the cap (every
		// real upstream error body is); an over-cap body surfaces truncated,
		// and since the truncation is no longer valid JSON parseUpstreamError
		// falls back to the raw capped text — the envelope stays bounded by
		// construction. Same bound as drainAndClose, plus the same total
		// deadline: a peer streaming forever below the cap must not pin this
		// goroutine either (issue #45 hardening, config.SecondaryReadTimeout).
		raw := ReadBoundedBody(ctx, resp.Body, maxErrorBodyBytes)
		_ = resp.Body.Close()
		// Evidence capture precedes classification: the row's facts are
		// extracted from the untouched verdict first, so classification (and
		// the error envelope reduction that follows) can never be the place
		// upstream information is lost.
		uerr := parseUpstreamError(resp.StatusCode, raw)
		// Authorship comes from the PATH, never from the status that arrived on
		// it (issue #63): an HTTP forward proxy is an answering peer for a
		// plain-http hop, so a 502/503/407/403 on that path may be its own
		// answer rather than the provider's, and nothing in the response
		// separates the two.
		failure := statusFailure(resp.StatusCode, path)
		appendResponseRow(rec, dur, resp.StatusCode, resp.Header, raw, uerr, failure)
		return nil, uerr, failure
	}
	// A live response is served, not failed, so its CLASS is ClassSuccess and
	// its client-facing envelope is untouched — but its AUTHORSHIP is still the
	// path's, for the same reason a failed one's is: this response is about to
	// be labelled on the wire and relayed as the provider's answer, and a hop an
	// HTTP intermediary carried cannot promise that (a captive portal's 200 is
	// the case that makes it visible — issue #63). The router reads this record
	// to label the response; nothing else about the success path changes.
	return resp, nil, Failure{Origin: path.responseOrigin(), RequestState: path.responseState()}
}

// drainAndClose reads the rest of a rejected response so the connection can
// be reused, then closes it. Bounded by maxErrorBodyBytes like the terminal
// read, and by the same total deadline: a hostile upstream must not be able to
// pin this goroutine's memory — or its attention — past the drain either
// (config.SecondaryReadTimeout).
func drainAndClose(ctx context.Context, resp *http.Response) {
	ReadBoundedBody(ctx, resp.Body, maxErrorBodyBytes)
	_ = resp.Body.Close()
}

// CloseIdleConnections closes the idle pooled conns of every transport the
// client owns. The router calls it when a generation prune evicts the client
// so an egress swap releases its fds promptly.
//
// This alone is NOT enough, and no GC backstop exists: net/http registers NO
// finalizer for pooled conns, so a conn still busy when the prune runs
// returns to its transport's idle pool afterwards and stays there forever.
// IdleConnTimeout (set on every transport this package builds; see
// config.IdleConnTimeout) is the backstop that eventually reaps it.
func (c *Client) CloseIdleConnections() {
	c.HTTP.CloseIdleConnections()
	if c.tunneled != nil {
		c.tunneled.CloseIdleConnections()
	}
}

// attempt performs one POST without touching the response body; netErr
// distinguishes transport failures. The transport is picked by TARGET scheme:
// https through an HTTP(S) proxy rides the tunneled CONNECT boundary, http
// rides the absolute-form proxy transport (or the single direct/socks5 one).
//
// For followRedirects clients the redirect chain is walked HERE, hop by hop,
// so every hop re-runs the scheme→transport pick (see the Client doc). The
// per-hop behavior mirrors the two engines this port sits between:
//
//   - undici (the JS router's fetch, undici/lib/web/fetch/index.js
//     http-redirect fetch): a 3xx WITHOUT Location is the final answer
//     (:1233-1237 `locationURL is null → return response`, same as
//     net/http/client.go:643-649); a Location that fails to parse or is not
//     http(s) is a network error (:1239-1247); the chain gives up after
//     config.MaxRedirects followed hops with a network-class error (:1250);
//     301/302 on POST and 303 on anything non-GET/HEAD switch the hop to GET
//     with no body (:1290-1306 — identical to net/http redirectBehavior,
//     client.go:512-539).
//   - net/http's client follow loop (client.go do()): headers are re-copied
//     from the ORIGINAL request on every hop (makeHeadersCopier), the
//     sensitive set (Authorization & friends) is stripped once the chain
//     leaves the initial DOMAIN — sticky thereafter (:688-693, 814-815) —
//     and body-describing headers are dropped once a hop dropped the body
//     (:817-821). NOT replicated: stdlib's Referer injection (:694-698) —
//     the JS executor sends no Referer on any hop, and undici adds none.
//
// The mid-chain responses are drained bounded (drainAndClose) and closed;
// their bodies are never surfaced.
//
// The last hop's dial trace and the last hop's PATH are returned alongside the
// verdict. The trace is the only place the hop's dial facts live, and the
// caller classifies the failure with them (provenance.go). The path is the only
// place the hop's authorship lives, and the caller labels the response with it
// (hopPath) — both belong to the delivering hop, never to an earlier one: the
// conflation that would make a redirect chain's tail inherit its head's facts
// is issue #60 for the trace and would be the same error for the path.
func (c *Client) attempt(ctx context.Context, call *callTrace, url string, headers map[string]string, bodyJSON []byte) (*http.Response, *dialTrace, hopPath, error) {
	var hop *dialTrace
	method := http.MethodPost
	// Sticky per-chain state, both mirroring net/http's do loop: once the
	// chain leaves the initial domain the sensitive headers stay stripped
	// even if a later hop returns to it, and once a hop dropped the body it
	// is never re-sent.
	stripSensitive := false
	droppedBody := false
	initialHost, initialHostname := "", ""
	hops := 0
	current := url
	for {
		var body io.Reader
		if !droppedBody && bodyJSON != nil {
			body = bytes.NewReader(bodyJSON)
		}
		// Each hop gets its own record and its own write hook; both hang off
		// the call's monotonic record, so a hop can prove what IT did and can
		// never claim a state the call has already moved past (issue #60).
		hopCtx, hopTrace := withHopTrace(ctx, call)
		hop = hopTrace
		req, err := http.NewRequestWithContext(hopCtx, method, current, body)
		if err != nil {
			return nil, hop, hopPath{}, err
		}
		// The path THIS hop takes, re-picked per hop like the transport below
		// (a redirect may cross schemes and land on a different kind of path —
		// the last assignment before a response is returned is the delivering
		// hop's).
		path := c.hopPathOf(req.URL.Scheme)
		if initialHost == "" {
			initialHost = req.URL.Host
			initialHostname = req.URL.Hostname()
		}
		for k, v := range headers {
			if stripSensitive && isSensitiveRedirectHeader(k) {
				continue
			}
			if droppedBody && isBodyRedirectHeader(k) {
				continue
			}
			req.Header.Set(k, v)
		}
		if !c.followRedirects {
			// The direct client (NewClient): stdlib follows, unchanged —
			// its one transport serves both schemes, so stdlib's loop
			// cannot bypass a proxy. (A tunneled transport only exists on
			// followRedirects clients; the scheme pick below is the
			// historical one, kept for parity.)
			client := c.HTTP
			if req.URL.Scheme == "https" && c.tunneled != nil {
				client = c.tunneled
			}
			resp, err := client.Do(req)
			return resp, hop, path, err
		}
		resp, err := c.roundTrip(req)
		if err != nil {
			return nil, hop, path, err
		}
		if !isRedirectStatus(resp.StatusCode) {
			return resp, hop, path, nil
		}
		// A redirect is a response this logical call received. Recorded before
		// anything else looks at it, because it is a fact about the CALL: the
		// hops that follow this one inherit it, and none of them may claim the
		// request was never sent (issue #60).
		call.noteResponse()
		loc := resp.Header.Get("Location")
		if loc == "" {
			// A 3xx without Location is the answer, not a hop — undici
			// returns it (fetch/index.js:1233-1237), net/http too
			// (client.go:643-649). The caller decides what a 3xx body means.
			return resp, hop, path, nil
		}
		next, err := req.URL.Parse(loc)
		if err != nil {
			drainAndClose(ctx, resp)
			return nil, hop, path, fmt.Errorf("redirect: parse Location %q: %w", loc, err)
		}
		if next.Scheme != "http" && next.Scheme != "https" {
			drainAndClose(ctx, resp)
			return nil, hop, path, fmt.Errorf("redirect: Location %q is not an HTTP(S) URL", loc)
		}
		// Redirect-host allow-list (GHSA-5472-vw5j-wjvg): a redirect may only
		// stay on the host the logical call STARTED on — the configured
		// upstream base (validated at config load), which is the one origin
		// this proxy is authorized to reach. A hostile 3xx therefore cannot
		// steer the request at a private network (169.254.169.254 metadata,
		// loopback services, internal names) through the trusted egress. The
		// comparison is on hostname, not host:port: same-host cross-port
		// redirects are legitimate (opencode.ai:443 → opencode.ai:80 for an
		// HTTP->HTTPS flip), and no additional trust is extended by allowing
		// them — the host is already the trusted origin. Differs from
		// undici/net-http, which follow cross-host redirects by default; this
		// is the deliberate hardening that closes the SSRF. Note the refusal
		// happens AFTER noteResponse above, so the failure the caller records
		// inherits response_started/unknown — never not_sent.
		if !sameRedirectHost(next.Hostname(), initialHostname) {
			drainAndClose(resp)
			return nil, hop, path, fmt.Errorf("redirect: Location %q leaves the request host (%s)", boundedLocation(loc), initialHostname)
		}
		hops++
		if hops > config.MaxRedirects {
			drainAndClose(ctx, resp)
			return nil, hop, path, fmt.Errorf("redirect count exceeded (%d)", config.MaxRedirects)
		}
		// 301/302 on POST, 303 on anything non-GET/HEAD: the next hop is a
		// body-less GET (undici fetch/index.js:1290-1306; net/http
		// redirectBehavior client.go:512-539). 307/308 keep method + body.
		if (resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && method == http.MethodPost ||
			resp.StatusCode == http.StatusSeeOther && method != http.MethodGet && method != http.MethodHead {
			method = http.MethodGet
			droppedBody = true
		}
		// net/http client.go:688-692: leaving the initial host only strips
		// the sensitive set when the destination is a DIFFERENT domain —
		// subdomains (and same-host port changes, Hostname() ignores the
		// port) keep credentials.
		if !stripSensitive && next.Host != initialHost && !sameDomainOrSub(next.Hostname(), initialHostname) {
			stripSensitive = true
		}
		drainAndClose(ctx, resp)
		current = next.String()
	}
}

// roundTrip sends req through the transport the SCHEME picks, calling
// RoundTrip directly — never http.Client.Do, whose internal follow loop is
// exactly what the manual chain above exists to replace (it would re-cross a
// scheme boundary on stdlib's terms, bypassing the per-hop transport pick).
// A nil Transport falls back to http.DefaultTransport like http.Client
// (net/http/client.go send).
func (c *Client) roundTrip(req *http.Request) (*http.Response, error) {
	return c.transportFor(req.URL.Scheme).RoundTrip(req)
}

// hopPathOf is the scheme→path table — the sibling of transportFor, asked the
// other question: not WHICH transport carries this hop, but WHO on the way can
// answer for it (provenance.go, hopPath).
//
// A plain-http hop on a client that HAS a tunneled transport is necessarily the
// absolute-form proxy path: transportFor sends https through the CONNECT
// boundary and everything else through c.HTTP, and c.HTTP is only a proxy
// transport when NewClientFor built it from an http/https proxy. That hop has
// an HTTP intermediary in it, so its responses are only ever attributable to
// "somewhere on the path". Every other hop is end-to-end: direct and SOCKS5
// hand the request bytes to the target, and the CONNECT tunnel carries https
// the proxy cannot read.
//
// The tunneled client's OWN responses (a 407 at CONNECT time) never reach here
// as a response at all — connect.go types them as a transport failure — which
// is why this table only has to answer for hops that produced one.
//
// Re-evaluated per hop by attempt, like the transport pick next to it: a
// redirect may cross the boundary (http→https leaves the proxy's reach), and
// the authorship of the final response belongs to the hop that delivered it.
func (c *Client) hopPathOf(scheme string) hopPath {
	return hopPath{intermediated: c.tunneled != nil && scheme == "http"}
}

// transportFor is the scheme→transport table: https rides the tunneled
// CONNECT boundary when one exists (http/https proxy egresses), everything
// else rides the main transport. Re-evaluated per hop by attempt.
func (c *Client) transportFor(scheme string) http.RoundTripper {
	if scheme == "https" && c.tunneled != nil {
		if t := c.tunneled.Transport; t != nil {
			return t
		}
		return http.DefaultTransport
	}
	if t := c.HTTP.Transport; t != nil {
		return t
	}
	return http.DefaultTransport
}

// isRedirectStatus mirrors undici's redirectStatusSet / net/http's
// redirectBehavior: exactly these statuses redirect. 300/304/305 and the rest
// are final answers.
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// isSensitiveRedirectHeader mirrors net/http's sensitive redirect set
// (client.go:814-815): credentials that must not follow a redirect onto a
// different domain.
func isSensitiveRedirectHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Authorization", "Www-Authenticate", "Cookie", "Cookie2",
		"Proxy-Authorization", "Proxy-Authenticate":
		return true
	}
	return false
}

// isBodyRedirectHeader mirrors net/http's body-header set (client.go:817-821):
// headers describing a request body a POST→GET redirect hop no longer sends
// (the fetch spec deletes the same names, undici fetch/index.js:1296-1302).
func isBodyRedirectHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Content-Encoding", "Content-Language", "Content-Location", "Content-Type":
		return true
	}
	return false
}

// sameDomainOrSub mirrors net/http isDomainOrSubdomain (client.go:1028-1039):
// dest equals parent, or dest is a dot-suffixed child ("api.example.com"
// under "example.com"); a dest containing ':' or '%' (IPv6 literals/zones)
// never suffix-matches. Deliberate divergence: stdlib normalizes both sides
// through IDNA first (idnaASCIIFromURL, transport.go:3024) — this port does
// not carry x/net/idna, so hosts differing only in A-label form count as
// different domains and strip the sensitive set slightly more often. The
// safe direction: over-stripping Authorization on an exotic redirect beats
// leaking it.
func sameDomainOrSub(dest, parent string) bool {
	if dest == parent {
		return true
	}
	if strings.ContainsAny(dest, ":%") {
		return false
	}
	return strings.HasSuffix(dest, "."+parent)
}

// sameRedirectHost is the redirect allow-list predicate
// (GHSA-5472-vw5j-wjvg): a redirect may only stay on the host the logical
// call started on. Exact hostname equality, case-insensitive (hostnames are
// case-insensitive); a trailing-dot FQDN form or an IDNA A-label counts as a
// DIFFERENT host and is refused — the safe direction (over-refusing an exotic
// redirect beats dialing a wrong host through the trusted egress).
func sameRedirectHost(dest, initial string) bool {
	return strings.EqualFold(dest, initial)
}

// boundedLocation limits the redirect-refusal error text: the Location header
// is attacker-controlled, and the error surfaces to clients. The header is
// already trimmed to a short cap; if it is longer the tail is dropped rather
// than echoed in full (CWE-117 hygiene: the value rides the JSON error body,
// never a log line, but a bounded echo keeps the message honest and small).
func boundedLocation(loc string) string {
	const max = 200
	if len(loc) > max {
		return loc[:max] + "…"
	}
	return loc
}

// There is deliberately no tryRetry here. base.js:104-125 spent a per-URL
// budget on retryable STATUSES inside one egress; that whole mechanism is the
// Injector's now (docs/recovery-semantics.md), and this package makes one
// logical call per attempt. See config.go for why the rule table is gone.

// parseUpstreamError ports utils/error.js parseUpstreamError: extract the
// message from the upstream body, then re-type it for clients
// (buildErrorBody's status→type/code map is applied at write time).
func parseUpstreamError(status int, body []byte) *UpstreamError {
	text := string(body)
	var message string
	var haveMessage bool
	if json.Valid(body) {
		var parsed map[string]any
		if json.Unmarshal(body, &parsed) == nil {
			// JS: json.error?.message || json.message || json.error || bodyText —
			// each operand is only consulted when the previous was falsy, so an
			// error object without .message falls through to json.message first.
			if errObj := jsonx.AsObj(parsed["error"]); errObj != nil {
				if m := jsonx.AsStr(errObj["message"]); m != "" {
					message, haveMessage = m, true
				}
			}
			if !haveMessage {
				if m := jsonx.AsStr(parsed["message"]); m != "" {
					message, haveMessage = m, true
				}
			}
			// The bare-error operand only counts when JS-truthy: {"error":false}
			// / 0 / "" fall through to bodyText, they are not stringified.
			if !haveMessage && jsonx.Truthy(parsed["error"]) {
				if s, is := parsed["error"].(string); is {
					// Truthy already excludes "" — the string is the message.
					message, haveMessage = s, true
				} else {
					// Non-string truthy error (object/number) → JSON.stringify.
					b, _ := json.Marshal(parsed["error"])
					message, haveMessage = string(b), true
				}
			}
		}
	}
	if !haveMessage {
		message = text
	}
	if message == "" {
		if def, has := config.DefaultErrorMessages[status]; has {
			message = def
		} else {
			message = fmt.Sprintf("Upstream error: %d", status)
		}
	}
	return &UpstreamError{Status: status, Message: message}
}

// BuildErrorBody ports buildErrorBody: the OpenAI-compatible error envelope.
// Statuses MISSING from config.ErrorTypes — 407-as-response-status, 426, 418,
// … — are PARITY with the JS table (config/errorConfig.js ERROR_TYPES has no
// rows for them either): error.js:10-13 falls back to
// `{type:"server_error",code:"internal_server_error"}` for >= 500 and
// `{type:"invalid_request_error",code:""}` otherwise. That is how a 407 that
// arrives as a response status surfaces typed invalid_request_error with an
// empty code — deliberate, verified parity, not an omission.
func BuildErrorBody(statusCode int, message string) map[string]any {
	info, known := config.ErrorTypes[statusCode]
	if !known {
		if statusCode >= 500 {
			info = config.ErrorInfo{Type: "server_error", Code: "internal_server_error"}
		} else {
			info = config.ErrorInfo{Type: "invalid_request_error", Code: ""}
		}
	}
	if message == "" {
		if def, has := config.DefaultErrorMessages[statusCode]; has {
			message = def
		} else {
			message = "An error occurred"
		}
	}
	return jsonx.ObjOf("error", jsonx.ObjOf(
		"message", message,
		"type", info.Type,
		"code", info.Code,
	))
}

// ScanLines consumes an SSE body line by line, delivering each line without
// its trailing newline (stream.js:243-247 buffer.split("\n") semantics — a
// \r from CRLF upstreams survives, exactly as in JS). An unterminated final
// segment at a CLEAN EOF is delivered through onTail instead of fn:
// stream.js:515-530 forwards the residual buffer only in flush(), and a
// transform's flush runs only when the stream ends cleanly — an ERRORED
// stream never flushes (streamHandler.js:161-163 controller.error), so a
// partial segment followed by a non-EOF read error is DROPPED: fn gets
// nothing, onTail gets nothing, the error itself is the whole story. The
// stall deadline resets on ANY read progress, not only complete lines
// (streamHandler.js:179,185-186 "Any upstream chunk resets the timer";
// :229-239 armStall per chunk): a slow event trickled across many chunks
// with no newline for stretches past the stall window is live traffic, and
// JS does not call it a stall. When fn returns an error or the stall fires,
// the caller must cancel ctx (or close the body) to unblock the reader
// goroutine.
func ScanLines(ctx context.Context, body io.Reader, stall time.Duration, fn func(line string) error, onTail func(line string) error) error {
	lines := make(chan string, 64)
	tails := make(chan string, 1)
	readErr := make(chan error, 1)
	// progress pings on every read that returned BYTES — complete line or
	// not — so the consumer can reset the stall deadline per chunk like
	// streamHandler.js does. Cap 1 with a non-blocking send: pings coalesce,
	// and coalescing can never under-reset the timer (one observed ping
	// between two timer fires is a full reset).
	progress := make(chan struct{}, 1)
	go func() {
		defer close(lines)
		defer close(tails)
		buf := make([]byte, 32*1024)
		// pending is the unterminated segment so far. It grows without
		// bound for a line that never ends — deliberate parity:
		// stream.js:243-247 `buffer += text` is unbounded the same way
		// (SSE carries no line-length cap anywhere in the JS router), and
		// an injected cap would fabricate line splits upstream never sent.
		var pending []byte
		for {
			n, err := body.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				select {
				case progress <- struct{}{}:
				default:
				}
				for {
					i := bytes.IndexByte(pending, '\n')
					if i < 0 {
						break
					}
					line := string(pending[:i+1]) // copy: pending reuses its array
					pending = pending[i+1:]
					select {
					case lines <- line:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				if err == io.EOF {
					// Clean end: the residual is the unterminated final
					// segment, delivered through onTail exactly like
					// stream.js:515-530 flush() forwards it.
					if len(pending) > 0 {
						select {
						case tails <- string(pending):
						case <-ctx.Done():
						}
					}
					return
				}
				// Non-EOF error: no line, no tail — the errored stream never
				// flushes in JS, so the partial segment dies with it.
				select {
				case readErr <- err:
				default:
				}
				return
			}
		}
	}()
	timer := time.NewTimer(stall)
	defer timer.Stop()
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(stall)
	}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// All complete lines delivered; the residual tail (if any)
				// goes through onTail, then a pending read error surfaces.
				// The ok check matters: tails is CLOSED by the reader
				// goroutine, and a receive from a closed empty channel
				// succeeds with the zero value — without it onTail ran with
				// "" on EVERY clean end (and, before finding 7, on every
				// errored end too).
				select {
				case tail, ok := <-tails:
					if ok && onTail != nil {
						if err := onTail(tail); err != nil {
							return err
						}
					}
				default:
				}
				select {
				case err := <-readErr:
					return err
				default:
					return nil
				}
			}
			reset()
			if err := fn(strings.TrimRight(line, "\n")); err != nil {
				return err
			}
		case <-progress:
			// The watchdog tracks raw upstream chunks, not parsed lines
			// (streamHandler.js:179,185-186): reset on byte progress alone.
			reset()
		case <-timer.C:
			// %w keeps the sentence byte-identical while making it typed.
			return fmt.Errorf("%w: no SSE data for %s", ErrStreamStalled, stall)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
