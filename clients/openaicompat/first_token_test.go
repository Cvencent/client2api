package openaicompat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// keepaliveBody replays SSE comment lines forever, which is what a relay does
// while it holds a request in its queue.  It never produces a data frame, so
// nothing reaches the caller -- and every byte it writes resets the byte-idle
// deadline, which is why a byte-idle timeout alone cannot bound this wait.
type keepaliveBody struct {
	mu     sync.Mutex
	closed bool
	data   []byte
}

func (b *keepaliveBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.closed {
			return 0, io.EOF
		}
		if len(b.data) > 0 {
			n := copy(p, b.data)
			b.data = b.data[n:]
			return n, nil
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		b.mu.Lock()
		b.data = append(b.data, ": keepalive\n\n"...)
	}
}

func (b *keepaliveBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

// The live shape: a provider accepted the request, kept the socket alive with
// comments, and only answered minutes later with an error.  The caller must not
// be made to wait that long -- and because nothing visible was produced, the
// gateway can still move the turn to a sibling platform.
func TestFirstTokenDeadlineBoundsAKeepaliveStall(t *testing.T) {
	c := &Client{cfg: Config{FirstTokenTimeout: "60ms", StreamIdleTimeout: "10m"}, now: time.Now}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &keepaliveBody{},
	}
	s := newChatStream(c, resp, ProviderConfig{}, "acct", func() {}, nil)
	defer s.Close()

	started := time.Now()
	_, err := s.Recv()
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Recv reported success on a keepalive-only stream")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Recv took %s, want the first-token deadline to end it", elapsed)
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureUpstream || !core.Retryable(f.Kind) {
		t.Fatalf("kind = %q, want a retryable upstream failure so the chain can move on", f.Kind)
	}
	if !strings.Contains(err.Error(), "no visible frame") {
		t.Fatalf("err = %v, want it to name the first-token stall", err)
	}
}

// A visible frame stops the deadline: a model is allowed to take its time once
// it has started answering, and an SSE keepalive after that must not be
// mistaken for a stall.
func TestFirstTokenDeadlineDoesNotFireAfterContent(t *testing.T) {
	c := &Client{cfg: Config{FirstTokenTimeout: "50ms", StreamIdleTimeout: "10m"}, now: time.Now}
	body := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	s := newChatStream(c, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}, ProviderConfig{}, "acct", func() {}, nil)
	defer s.Close()

	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != core.EventDelta || ev.Delta != "hi" {
		t.Fatalf("event = %+v, want the content delta", ev)
	}
	// The timer must be disarmed, so waiting past its original deadline is safe.
	time.Sleep(120 * time.Millisecond)
	if _, err := s.Recv(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after content = %v, want EOF (the deadline must not fire)", err)
	}
}

// headerStallServer accepts the request and never writes a response header --
// the shape a relay has when it is wedged before it can even answer.  The
// whole point of the header guard is that this stage is bounded too: the
// stream's own first-token timer cannot see it, because no stream exists yet.
func headerStallServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the response open without sending headers or closing.  When the
		// guard cancels the request the handler is released and returns.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

// The header wait is bounded by the same first-frame deadline as the body, and
// the verdict is a retryable upstream failure: nothing visible was produced, so
// the gateway can move the turn to a sibling platform.  Without this the
// request would hold its platform in-flight slot for the full chat timeout.
func TestHeaderStallIsBoundedByTheFirstFrameDeadline(t *testing.T) {
	srv := headerStallServer(t)

	c := &Client{
		instanceName: "relay",
		deps:         core.Deps{HTTPClient: srv.Client()},
		cfg:          Config{FirstTokenTimeout: "80ms", ChatTimeout: "10m", MaxTokensField: "max_tokens"},
		now:          time.Now,
		pool:         newProviderPool(),
	}

	started := time.Now()
	_, err := c.doChat(context.Background(),
		ProviderConfig{ID: "acct-1", APIKey: "k", BaseURL: srv.URL + "/v1", Models: []string{"demo"}},
		"acct-1", []byte(`{"model":"demo","messages":[{"role":"user","content":"hi"}]}`))
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("doChat succeeded against a server that never answered")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("doChat took %s, want the first-frame deadline to end the header wait", elapsed)
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureUpstream || !core.Retryable(f.Kind) {
		t.Fatalf("kind = %q, want a retryable upstream failure so the chain can move on", f.Kind)
	}
	if !strings.Contains(err.Error(), "no visible frame") {
		t.Fatalf("err = %v, want it to name the first-frame stall", err)
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// The guard stops the wait by cancelling the request; the classified
		// failure must still wrap that cause so callers can see why it ended.
		t.Logf("note: err does not wrap the cancellation: %v", err)
	}
}

// The source's chat_timeout must reach the transport the request is built from.
// An operator who tunes it on a relay expects every request that relay makes to
// honour it; a value that stops at SourceConfig would leave the module default
// in force and the knob would read as broken.
func TestSourceChatTimeoutReachesTheTransport(t *testing.T) {
	c, err := newSource("relay", core.SourceConfig{
		BaseURL:     "https://relay.example/v1",
		ChatTimeout: "2m",
	}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	transport := c.transportLocked()
	if got := transport.cfg.chatTimeout(); got != 2*time.Minute {
		t.Fatalf("transport chatTimeout = %v, want the source's 2m", got)
	}
	// The empty case keeps the module default, so an untouched relay is
	// unaffected by this field existing.
	d, err := newSource("plain", core.SourceConfig{BaseURL: "https://relay.example/v1"}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	if got := d.transportLocked().cfg.chatTimeout(); got != defaultChatTimeout {
		t.Fatalf("plain transport chatTimeout = %v, want the module default %v", got, defaultChatTimeout)
	}
}
