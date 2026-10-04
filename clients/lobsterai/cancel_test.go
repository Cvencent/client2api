package lobsterai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// These tests pin the guard in openChat that keeps a caller's cancellation from
// being read as evidence about the credential.  Without it classifyErrFor folds
// context.Canceled into kindServer, and softErrorThreshold of those in a row park
// an account that is perfectly healthy -- which is exactly what happened live
// when the panel's Stop button aborted a chat.

const chatPath = "/api/proxy/v1/chat/completions"

func cancelTestRequest() *core.ChatRequest {
	return &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
}

// sideHandler answers everything except the chat endpoint, so the only request
// this test exercises is the one under test.
func sideHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

// waitForChat blocks until the fixture has seen the chat request, so the test
// cancels the caller while the request really is in flight.
func waitForChat(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.rec.all() {
			if c.path == chatPath {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no request reached %s within 5s", chatPath)
}

func TestACancelledCallerDoesNotCountAgainstTheAccount(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			sideHandler(w, r)
			return
		}
		<-r.Context().Done()
	})
	addAccount(t, f.client, "access-1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.client.Chat(ctx, cancelTestRequest())
		done <- err
	}()

	waitForChat(t, f)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat after the caller cancelled: err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Chat did not return after the caller was cancelled")
	}

	acc := f.client.pool.byID(accountID("777"))
	if acc == nil {
		t.Fatal("the account left the pool")
	}
	if acc.ErrCount != 0 {
		t.Errorf("a cancelled caller counted %d failures against the account", acc.ErrCount)
	}
	if acc.CooldownUntil != "" {
		t.Errorf("a cancelled caller cooled the account down until %s", acc.CooldownUntil)
	}
	if got := f.client.pool.ready(f.client.now(), 10); got != 1 {
		t.Errorf("ready = %d, want 1: the credential must stay in the pool", got)
	}
}

func TestATransportFailureStillCountsAgainstTheAccount(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatPath {
			sideHandler(w, r)
			return
		}
		// Abort the connection: the client sees a transport failure, not a
		// status code, which is the shape the guard must keep counting.
		panic(http.ErrAbortHandler)
	})
	addAccount(t, f.client, "access-1")

	if _, err := f.client.Chat(context.Background(), cancelTestRequest()); err == nil {
		t.Fatal("Chat succeeded against an aborted connection")
	}
	acc := f.client.pool.byID(accountID("777"))
	if acc == nil {
		t.Fatal("the account left the pool")
	}
	if acc.ErrCount == 0 {
		t.Error("a broken network path was not recorded against the account")
	}

	// Enough of them in a row must still park it.
	for i := 0; i < softErrorThreshold*2 && acc.CooldownUntil == ""; i++ {
		_, _ = f.client.Chat(context.Background(), cancelTestRequest())
		acc = f.client.pool.byID(accountID("777"))
	}
	if acc == nil {
		t.Fatal("the account left the pool")
	}
	if acc.CooldownUntil == "" {
		t.Errorf("the account was never parked after %d transport failures", softErrorThreshold)
	}
	if got := f.client.pool.ready(f.client.now(), 10); got != 0 {
		t.Errorf("ready = %d, want 0: a parked credential must not be handed out", got)
	}
}
