package loomy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// A header stall is the vendor accepting the connection and answering nothing.
// The module must not walk the rest of the pool on it: every credential would
// sit in the same silent queue, and the old walk turned one platform outage
// into minutes of added latency for a single request.  The stall budget is two
// opens (one same-account retry), after which the request is handed back with
// ErrPlatformExhausted so the gateway moves to the next platform.
func TestChatStopsRotationAfterTwoHeaderStalls(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	rt := &stubTransport{}
	// The plain stub cannot hold a connection open, so install a transport that
	// answers the business endpoints normally and wedges the chat endpoint past
	// the first-byte deadline.
	wedge := &stallTransport{inner: rt}
	c := newTestClient(t, `{"first_byte_timeout":"30ms"}`, wedge)
	seedAccount(t, c, "loomy-1", testToken, 0)
	seedAccount(t, c, "loomy-2", testTokenTwo, 0)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success against a wedged platform")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted so the gateway fails over", err)
	}
	// The pool was never the problem, so the reason the gateway prints must not
	// claim it was: this sentinel is what turns the row into "the upstream never
	// answered" instead of "no usable account".
	if !errors.Is(err, core.ErrUpstreamSilent) {
		t.Fatalf("err = %v, want ErrUpstreamSilent so the row names the stall", err)
	}
	if !strings.Contains(err.Error(), "no response within") {
		t.Fatalf("err = %v, want the first-byte stall report", err)
	}
	// Two opens on the first account (one retry), then stop: the second
	// credential is never queued into the same wedge.
	if got := atomic.LoadInt32(&wedge.chatOpens); got != 2 {
		t.Fatalf("chat opens = %d, want 2 (one open plus one same-account retry, then stop)", got)
	}
	// The credential did nothing wrong, so the cooldown ladder is untouched.
	for _, id := range []string{"loomy-1", "loomy-2"} {
		acc, ok := c.store.lookup(id)
		if !ok {
			t.Fatalf("%s vanished from the store", id)
		}
		if acc.failures != 0 || acc.cooldownStage != 0 {
			t.Fatalf("the stall was charged to %s: failures=%d stage=%d", id, acc.failures, acc.cooldownStage)
		}
	}
}

// A large prompt widens the header deadline instead of stalling on the fixed
// floor: the vendor needs minutes of prefill before a response header can
// exist.  A widened deadline is not retried -- one full prefill was already
// paid, and a second identical wait only delays the platform failover.
//
// 2026-11-20: a 613k-token conversation was reported as "the platform has no
// usable account" while 33 accounts were ready, because the fixed 60 s gate
// fired twice inside what was really a long prefill.
func TestChatDoesNotRetryAWidenedHeaderStall(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	rt := &stubTransport{}
	wedge := &stallTransport{inner: rt}
	c := newTestClient(t, `{"first_byte_timeout":"30ms"}`, wedge)
	seedAccount(t, c, "loomy-1", testToken, 0)
	seedAccount(t, c, "loomy-2", testTokenTwo, 0)

	// ~12k bytes is ~3.5k tokens, so the deadline is 30 ms plus a full second
	// of prefill budget: clearly wider than the floor the config asks for.
	big := strings.Repeat("x", 12_000)
	start := time.Now()
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: big}},
	})
	if err == nil {
		t.Fatal("Chat reported success against a wedged platform")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) || !errors.Is(err, core.ErrUpstreamSilent) {
		t.Fatalf("err = %v, want platform-exhausted joined with upstream-silent", err)
	}
	// The widened gate really was waited out...
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("stall returned after %s, want the widened deadline (>= 1s)", elapsed)
	}
	// ...and paid for exactly once.
	if got := atomic.LoadInt32(&wedge.chatOpens); got != 1 {
		t.Fatalf("chat opens = %d, want 1 (a widened stall is not retried)", got)
	}
}

// stallTransport wedges the chat endpoint until the caller's context is done,
// and forwards everything else to inner.
type stallTransport struct {
	inner     http.RoundTripper
	chatOpens int32
}

func (s *stallTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		atomic.AddInt32(&s.chatOpens, 1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return s.inner.RoundTrip(r)
}

// A connection-level failure is not the credential's fault, and this module's
// cooldown ladder ends in a PERMANENT abnormal park.  The 2026-10-29 loomy
// outage parked 24 healthy sessions that way: every one of them recorded
// `chat: Post "...": EOF` and reached the fourth rung.
func TestTransportFailureDoesNotParkTheAccounts(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	rt := failingTransport{err: io.ErrUnexpectedEOF}
	c := newTestClient(t, `{"first_byte_timeout":"30s"}`, rt)
	seedAccount(t, c, "loomy-1", testToken, 0)
	seedAccount(t, c, "loomy-2", testTokenTwo, 0)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success against a broken connection")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted so the gateway moves to the next platform", err)
	}
	for _, id := range []string{"loomy-1", "loomy-2"} {
		acc, ok := c.store.lookup(id)
		if !ok {
			t.Fatalf("%s vanished from the store", id)
		}
		if acc.failures != 0 || acc.cooldownStage != 0 || acc.abnormal {
			t.Fatalf("%s was charged for a platform outage: failures=%d stage=%d abnormal=%v",
				id, acc.failures, acc.cooldownStage, acc.abnormal)
		}
	}
}
