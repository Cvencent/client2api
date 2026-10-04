package codearts

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// holdUntilCancelled is a chat handler that keeps the request open until the
// caller walks away, which is exactly the shape of the operator pressing Stop
// in the panel or closing the tab mid-answer.
func holdUntilCancelled(seen chan<- struct{}) func(http.ResponseWriter, *http.Request, recordedRequest, int) {
	var once bool
	return func(w http.ResponseWriter, r *http.Request, _ recordedRequest, _ int) {
		if !once {
			once = true
			close(seen)
		}
		<-r.Context().Done()
	}
}

// TestACancelledCallerDoesNotCoolTheAccountDown pins the guard in Chat's retry
// loop.  classifyErr maps a cancellation onto kindTransient, and kindTransient
// is on cooldownFor's list, so without the guard one interrupted request parks
// a perfectly healthy credential for default_short_cooldown and the panel shows
// it as "cooling".
func TestACancelledCallerDoesNotCoolTheAccountDown(t *testing.T) {
	f := newChatFixture(t)
	seen := make(chan struct{})
	f.onCha = holdUntilCancelled(seen)
	c := f.client(t, `,"max_attempts":3`)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, &core.ChatRequest{
			Model:    "deepseek-v4-flash",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		errc <- err
	}()

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the module never sent a chat request")
	}
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("a cancelled Chat returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after the caller cancelled")
	}

	if !c.pool.ready() {
		t.Errorf("a cancelled caller parked the account; a cancellation says nothing about the credential")
	}
	for _, e := range c.pool.all() {
		if e.fails != 0 {
			t.Errorf("account %s counted %d failures for a cancelled caller, want 0", e.id(), e.fails)
		}
	}
}

// TestARealTransientFailureStillCoolsTheAccountDown is the control for the test
// above: it proves the guard narrows the cooldown to cancellations instead of
// switching the transient path off.  An upstream 500 really is evidence that
// the credential should step aside for a moment.
func TestARealTransientFailureStillCoolsTheAccountDown(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeJSONBody(w, http.StatusInternalServerError, map[string]any{
			"error_code": "InferHub.500",
			"error_msg":  "upstream is having a moment",
		})
	}
	c := f.client(t, `,"max_attempts":3`)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat against a 500 returned no error")
	}

	if c.pool.ready() {
		t.Error("a real upstream 500 left the account ready; transient failures must still cool it down")
	}
}
