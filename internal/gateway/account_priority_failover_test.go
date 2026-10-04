package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// scriptedClient fails the first n calls, then succeeds.  It exists to pin the
// order the gateway uses when one platform has several accounts: a per-account
// failure must not be treated as a platform failure.
type scriptedClient struct {
	name      string
	mu        sync.Mutex
	calls     int
	failCalls int
	err       error
	servedBy  string
}

func (c *scriptedClient) Name() string { return c.name }

func (c *scriptedClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1", OwnedBy: c.name}}, nil
}

func (c *scriptedClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true, Models: []string{"m1"}}
}

func (c *scriptedClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n <= c.failCalls {
		return nil, c.err
	}
	core.NoteServedBy(req, c.servedBy)
	return &fakeStream{events: usageEvents(1, 1)}, nil
}

func (c *scriptedClient) callsMade() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// A platform that still has another account must be retried before the gateway
// moves to a lower-priority platform.
func TestGatewayStaysOnPlatformWhileItsAccountsRemain(t *testing.T) {
	alpha := &scriptedClient{
		name:      "alpha",
		failCalls: 1,
		err:       core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("account one failed")),
		servedBy:  "a2",
	}
	beta := &scriptedClient{name: "beta", servedBy: "b1"}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live:     livecfg.New(livecfg.Snapshot{MaxRotate: 1, RotateBackoffBase: time.Nanosecond}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if alpha.callsMade() != 2 {
		t.Fatalf("alpha calls = %d, want 2 (first account failed, second served)", alpha.callsMade())
	}
	if beta.callsMade() != 0 {
		t.Fatalf("beta calls = %d, want 0 while alpha still had an account", beta.callsMade())
	}
}

// Only after the module says its account pool is exhausted may the gateway
// switch platforms.
func TestGatewaySwitchesPlatformAfterPoolExhausted(t *testing.T) {
	alpha := &scriptedClient{
		name:      "alpha",
		failCalls: 10,
		err:       errors.Join(core.ErrPlatformExhausted, core.Fail("alpha", "a2", core.FailureUpstream, http.StatusBadGateway, errors.New("all accounts failed"))),
		servedBy:  "a3",
	}
	beta := &scriptedClient{name: "beta", servedBy: "b1"}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live:     livecfg.New(livecfg.Snapshot{MaxRotate: 5, RotateBackoffBase: time.Nanosecond}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if alpha.callsMade() != 1 {
		t.Fatalf("alpha calls = %d, want 1: the exhausted signal must stop same-platform retries", alpha.callsMade())
	}
	if beta.callsMade() != 1 {
		t.Fatalf("beta calls = %d, want 1 after alpha reported exhaustion", beta.callsMade())
	}
}
