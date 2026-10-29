package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A 413 is this provider's own request-size ceiling, not a property of the
// request: providers differ by orders of magnitude, so a sibling with a bigger
// limit can still serve the caller.  It used to land on kindClient ->
// FailureOther, which neither rotates nor fails over, so the caller got a hard
// error while a roomier platform sat next in the chain.
func TestA413LetsTheRequestFailOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"request body too large"}}`))
	}))
	defer srv.Close()

	c := newProviderTestClient(t, srv, "groq")
	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: strings.Repeat("x", 64<<10)}},
		Stream:   true,
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })

	_, err := c.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat reported success on a 413")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	// Retryable is what makes the gateway's routing loop call
	// serveRemainingCandidates, i.e. move to the next platform in the chain.
	if !core.Retryable(f.Kind) {
		t.Fatalf("kind = %q: a 413 must let the request fail over to another platform", f.Kind)
	}
	if f.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want the vendor's 413 preserved for a caller that exhausts every candidate", f.Status)
	}
}

// A 400 is a request-shape refusal: every provider would reject the same body,
// so it must stay terminal.  This is the deliberate opposite of the 413 case.
func TestA400StaysTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid parameter: tools"}}`))
	}))
	defer srv.Close()

	c := newProviderTestClient(t, srv, "groq")
	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })

	_, err := c.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat reported success on a 400")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if core.Retryable(f.Kind) || core.NextPlatformOn(f.Kind) {
		t.Fatalf("kind = %q: a 400 must not rotate or fail over", f.Kind)
	}
}

// A key that keeps failing must stop being chosen.  Before the pool tracked
// health, a dead provider was reset to "ready" the moment its request ended, so
// every later request walked straight back into it.
func TestAProviderIsParkedAfterConsecutiveFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	c := newProviderTestClient(t, srv, "groq")
	model := "groq/openai/gpt-oss-120b"

	attempt := func() error {
		req := &core.ChatRequest{
			Model:    model,
			Messages: []core.Message{{Role: "user", Content: "hi"}},
			Stream:   true,
		}
		req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
		_, err := c.Chat(context.Background(), req)
		return err
	}

	for i := 0; i < providerFailureThreshold; i++ {
		if err := attempt(); err == nil {
			t.Fatalf("attempt %d reported success against a 401", i+1)
		}
	}
	if !c.pool.isParked(c.now(), "groq") {
		t.Fatalf("after %d consecutive failures the provider is still selectable", providerFailureThreshold)
	}

	// A parked provider answers ErrPlatformExhausted so the gateway moves the
	// request to a sibling platform instead of retrying into the same wall.
	err := attempt()
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted once the provider is parked", err)
	}
	if got := c.pool.ready(c.now()); got != 0 {
		t.Fatalf("ready count = %d, want 0 while the only provider is parked", got)
	}
}

// A success clears the record, so one blip followed by a served request does
// not accumulate into a park.
func TestASuccessClearsTheFailureRecord(t *testing.T) {
	failing := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := newProviderTestClient(t, srv, "groq")
	model := "groq/openai/gpt-oss-120b"
	attempt := func() {
		req := &core.ChatRequest{
			Model:    model,
			Messages: []core.Message{{Role: "user", Content: "hi"}},
			Stream:   true,
		}
		req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
		stream, err := c.Chat(context.Background(), req)
		if err == nil {
			_, _, _, _, _, _ = drainStream(stream)
		}
	}

	attempt() // one 500
	if rec, _ := c.pool.byID("groq"); rec.failures != 1 {
		t.Fatalf("failures = %d, want 1 after one failed round", rec.failures)
	}

	failing = false
	attempt() // served
	if rec, _ := c.pool.byID("groq"); rec.failures != 0 {
		t.Fatalf("failures = %d, want 0 after the provider served a request", rec.failures)
	}
}

// newProviderTestClient builds a client whose single provider points at srv.
func newProviderTestClient(t *testing.T, srv *httptest.Server, providerID string) *Client {
	t.Helper()
	raw, _ := New(core.Deps{HTTPClient: srv.Client()})
	c := raw.(*Client)
	c.ensure()
	c.cfg = testConfig(t)
	for i := range c.cfg.Providers {
		if c.cfg.Providers[i].ID == providerID {
			c.cfg.Providers[i].BaseURL = srv.URL + "/" + providerID
		} else {
			// Keep exactly one provider eligible so the assertions are about
			// this provider's health, not about which one was picked.
			c.cfg.Providers[i].Disabled = true
		}
	}
	c.pool.reload(c.buildCredentials())
	return c
}
