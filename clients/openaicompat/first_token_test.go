package openaicompat

import (
	"errors"
	"io"
	"net/http"
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
	s := newChatStream(c, resp, ProviderConfig{}, "acct", func() {})
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
	}, ProviderConfig{}, "acct", func() {})
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
