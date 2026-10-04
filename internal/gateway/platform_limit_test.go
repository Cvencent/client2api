package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// platform_limit_test.go pins the operator's per-platform in-flight ceiling.
//
// The account pools cap concurrency per account; this ceiling is one level up,
// on the platform as a whole, because some vendors rate-limit per session or
// per process before their quota is touched (Tabbit answers 429 with its
// marketing page).  A saturated platform must answer 429 rather than queueing
// the request or rotating accounts.

func TestPlatformLimiterRejectsTheExtraSlot(t *testing.T) {
	l := &platformLimiter{max: 1}
	if !l.acquire() {
		t.Fatal("first acquire failed")
	}
	if l.acquire() {
		t.Fatal("second acquire succeeded while the only slot was busy")
	}
	l.release()
	if !l.acquire() {
		t.Fatal("acquire failed after release")
	}
}

func TestPlatformLimiterZeroMeansNoCeiling(t *testing.T) {
	l := &platformLimiter{max: 0}
	for i := 0; i < 10; i++ {
		if !l.acquire() {
			t.Fatalf("max=0 refused acquire %d; zero must mean no ceiling", i)
		}
	}
}

// blockingClient holds its stream open until release is closed, so the gateway
// test can observe what a second concurrent request sees while the first is in
// flight.
type blockingClient struct {
	mu      sync.Mutex
	opened  int
	release chan struct{}
}

func (c *blockingClient) Name() string { return "tabbit" }

func (c *blockingClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1"}}, nil
}

func (c *blockingClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	c.mu.Lock()
	c.opened++
	c.mu.Unlock()
	return &blockingStream{release: c.release}, nil
}

func (c *blockingClient) Status(context.Context) core.Status {
	return core.Status{Name: "tabbit", Ready: true}
}

type blockingStream struct {
	release chan struct{}
	done    bool
}

func (s *blockingStream) Recv() (core.Event, error) {
	if s.done {
		return core.Event{}, io.EOF
	}
	<-s.release
	s.done = true
	return core.Event{Type: core.EventDone, Finish: "stop"}, nil
}

func (s *blockingStream) Close() error { return nil }

func blockingChat(srv *http.Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// TestPlatformCeilingRefusesTheSecondChat drives the whole HTTP path: with a
// ceiling of 1, the first chat holds the slot and the second must come back 429.
func TestPlatformCeilingRefusesTheSecondChat(t *testing.T) {
	c := &blockingClient{release: make(chan struct{})}
	reg := core.NewRegistry()
	reg.Add(c)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"tabbit": {MaxInFlight: 1}})
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- blockingChat(srv, streamBody) }()

	// Wait until the module has been entered, i.e. the slot is taken.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		opened := c.opened
		c.mu.Unlock()
		if opened > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first chat never reached the module")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := blockingChat(srv, bufferedBody)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 while the only slot is busy", second.Code)
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Errorf("a saturated platform must carry Retry-After, got %q", got)
	}
	close(c.release)
	<-firstDone
}

// accountClient picks one fixed account and takes the gateway's per-account
// slot before producing its stream.  It is the smallest module that exercises
// the per-account ceiling through the whole HTTP path.
type accountClient struct {
	account string
	release chan struct{}

	mu      sync.Mutex
	opened  int
	refused int
}

func (c *accountClient) Name() string { return "tabbit" }

func (c *accountClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1"}}, nil
}

func (c *accountClient) Chat(_ context.Context, req *core.ChatRequest) (core.Stream, error) {
	if err := req.AcquireAccountSlot(c.account); err != nil {
		c.mu.Lock()
		c.refused++
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.opened++
	n := c.opened
	c.mu.Unlock()
	if n == 1 {
		return &blockingStream{release: c.release}, nil
	}
	// Only the first stream stays open.  Later streams finish at once so a
	// regression that lets the second request through does not hang the test
	// waiting on the first stream's release channel.
	done := make(chan struct{})
	close(done)
	return &blockingStream{release: done}, nil
}

func (c *accountClient) Status(context.Context) core.Status {
	return core.Status{Name: "tabbit", Ready: true}
}

// A platform may cap each account independently of the platform-wide ceiling.
// The first request holds the only slot of the account; the second is refused
// with 429 (the module cannot rotate: it has one account), and after the first
// stream closes the slot is returned and traffic is admitted again.
func TestPerAccountCeilingRefusesUntilTheStreamCloses(t *testing.T) {
	c := &accountClient{account: "acct-1", release: make(chan struct{})}
	reg := core.NewRegistry()
	reg.Add(c)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"tabbit": {MaxInFlightPerAccount: 1}})
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- blockingChat(srv, streamBody) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		opened := c.opened
		c.mu.Unlock()
		if opened > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first chat never reached the module")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := blockingChat(srv, bufferedBody)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 while the account slot is busy", second.Code)
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Errorf("a saturated account must carry Retry-After, got %q", got)
	}

	close(c.release)
	<-firstDone

	// The closed release channel makes the next stream end at once, so this
	// request both proves the slot was returned and finishes without help.
	third := blockingChat(srv, bufferedBody)
	if third.Code != http.StatusOK {
		t.Fatalf("third request status = %d, want 200 after the slot was released", third.Code)
	}
}
