package upstream

// Logical-call request state (issue #60). The request state and the replay
// permission are properties of the WHOLE logical upstream call — one intent to
// obtain one model response, which "may involve several network interactions
// (a redirect hop, a new egress, a fresh connection)" and "ends exactly once"
// (docs/recovery-semantics.md, Vocabulary). They are NOT properties of the hop
// that happened to fail last.
//
// The load-bearing direction is the negative one, as everywhere in this
// package: a wrong not_sent re-sends a request the provider may already have
// executed. These tests drive the failure sequences that used to produce one.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"opencode-free-proxy/internal/config"
	"opencode-free-proxy/internal/routing"
)

// TestRedirectHopDialFailureNeverReplaysATransmittedRequest drives the exact
// sequence the contract has to survive:
//
//	POST hop-1 -> the request is on the wire -> 307 -> hop-2 dial failure
//
// Hop 1 is served by a POOLED connection (warmed up through the same client
// beforehand), so the attempt performs no dial of its own while it does
// transmit. That is the shape in which a per-hop dial trace sees "a dial
// failed, nothing was written" for a request that has already left the
// process — and therefore claims not_sent, marks the egress unhealthy and
// re-sends the POST elsewhere.
func TestRedirectHopDialFailureNeverReplaysATransmittedRequest(t *testing.T) {
	dead := closedAddr(t)
	var redirect bool
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		if !redirect {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		// A DIFFERENT host:port that refuses connections — hop 2's dial fails
		// before any request byte can exist for it.
		w.Header().Set("Location", "http://"+dead+"/zen/v1/chat/completions")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	url := srv.URL + "/zen/v1/chat/completions"

	// followRedirects: attempt walks the chain hop by hop, re-picking the
	// transport per hop (the egress-client shape).
	c, err := NewClientFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()

	// Warm-up: one answered request through the same client, so hop 1 of the
	// request under test reuses its pooled connection instead of dialing.
	warm, uerr, _ := c.DoClassified(context.Background(), url, staticHeaders(), []byte("{}"))
	if uerr != nil {
		t.Fatalf("warm-up: %v", uerr)
	}
	// The body must be READ to the end before Close: net/http only returns a
	// connection to the idle pool when its response was fully consumed, and a
	// half-read body is exactly the case that closes it instead.
	if _, err := io.Copy(io.Discard, warm.Body); err != nil {
		t.Fatalf("warm-up body: %v", err)
	}
	_ = warm.Body.Close()
	if got := posts.Load(); got != 1 {
		t.Fatalf("warm-up requests = %d, want 1", got)
	}
	redirect = true

	// The executor's view: two eligible egresses and a budget of three, so a
	// failure it considers replay-safe WOULD move the request to egress b.
	exec := NewExecutor(func(*config.Egress) (*Client, bool) { return c, true }, nil, nil)
	egA, egB := &config.Egress{ID: "a"}, &config.Egress{ID: "b"}
	plan := routing.RoutePlan{
		RouteID:  "r",
		Attempts: []string{"a", "b"},
		Egresses: []*config.Egress{egA, egB},
	}

	resp, id, attempts, failure, uerr := exec.Execute(
		context.Background(), url, staticHeaders(), []byte("{}"), plan, policy(true, 3))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a transport failure must not be delivered as a response")
	}
	if uerr == nil {
		t.Fatal("want the hop-2 dial failure surfaced")
	}

	// Observable behaviour first: one attempt, one POST for the whole logical
	// call, and never a second egress.
	if attempts != 1 || id != "a" {
		t.Fatalf("attempts=%d id=%q, want a single attempt on a (no egress move after transmission)", attempts, id)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("upstream POSTs = %d, want 2 (warm-up + the transmitted hop 1) — a duplicate POST was sent", got)
	}

	// Then the record itself: the logical call transmitted a request, so the
	// state it reports can never be not_sent.
	if failure.RequestState == RequestStateNotSent {
		t.Fatalf("request state = not_sent after hop 1 transmitted the POST: %+v", failure)
	}
	if failure.ReplaySafe() {
		t.Fatalf("failure reported replay-safe after hop 1 transmitted the POST: %+v", failure)
	}
}
