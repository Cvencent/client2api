package loomy

import (
	"context"
	"errors"
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
