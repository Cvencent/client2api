package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// A relay edge hiccup answers with a status that reads like a refusal and a
// body that carries no vendor envelope at all -- hcnsec intermittently returns
// 404/498 + the bare word "openai_error" for a model it served seconds earlier.
// The body cannot match any marker table, so the old status-only branches put a
// transient upstream fault into the caller-shaped buckets (404 -> kindNotFound,
// other 4xx -> kindClient), both terminal in the decision table: the request
// died with a 404 while the sibling platform sat healthy next in the chain.
func TestBareOpenaiErrorBodyIsAnUpstreamFault(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantModel bool
	}{
		{name: "a bare 404 body is an upstream fault, not a missing model", status: http.StatusNotFound},
		{name: "the non-standard 498 is an upstream fault", status: 498},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`openai_error`))
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
			f, ok := core.AsFailure(err)
			if !ok {
				t.Fatalf("err = %v, want a classified *core.Failure", err)
			}
			if f.Kind != core.FailureUpstream {
				t.Fatalf("kind = %q, want upstream: a non-envelope body is a relay fault, not a caller refusal", f.Kind)
			}
			if !core.Retryable(f.Kind) {
				t.Fatalf("kind %q must rotate; the same relay serves the next turn", f.Kind)
			}
			// Retryable is also what the routing loop uses to move the request
			// to the next candidate platform in a multi-candidate chain, so a
			// hiccup never ends the chain while a sibling sits next in line.
			if f.Status != tc.status {
				t.Fatalf("status = %d, want the vendor's %d preserved", f.Status, tc.status)
			}
		})
	}
}

// A structured 404 naming a missing model is the relay saying "this id does not
// exist here".  The credential is fine and every account on the relay would
// answer the same, so the verdict must be FailureModelRefused -- the same one
// trae/openrouter/minimaxcode hand down -- which moves the request to the next
// platform without parking anything.
func TestAStructured404IsAModelRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found: gpt-oss-120b"}}`))
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
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureModelRefused {
		t.Fatalf("kind = %q, want model_refused: the model is missing on this relay, not the request", f.Kind)
	}
	if rec, _ := c.pool.byID("groq"); rec.failures != 0 {
		t.Fatalf("failures = %d, want 0: a missing model says nothing about the credential", rec.failures)
	}
	if !core.NextPlatformOn(f.Kind) {
		t.Fatal("a model refusal must move the request to the next platform")
	}
}

// One same-provider retry for an upstream-classified fault: the relay that
// hiccuped on this request serves the next one, so the retry rescues a caller
// whose chain has no other platform to move to.  The verdict must not be
// recorded when the retry serves -- one blip is not a park.
func TestAnUpstreamFaultIsRetriedOnceOnTheSameProvider(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := newProviderTestClient(t, srv, "groq")
	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })

	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat = %v, want the retry to serve", err)
	}
	if _, _, _, _, _, _ = drainStream(stream); atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("hits = %d, want 2 (the hiccup, then the serving retry)", atomic.LoadInt64(&hits))
	}
	if rec, _ := c.pool.byID("groq"); rec.failures != 0 {
		t.Fatalf("failures = %d, want 0: a blip the retry recovered from is not a verdict", rec.failures)
	}
}

// Two consecutive upstream faults exhaust the one same-provider retry and the
// rotation budget with it, so the request ends as the last vendor verdict
// instead of looping.
func TestTwoUpstreamFaultsEndTheRequest(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
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
		t.Fatal("Chat reported success against a dead relay")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2: the hiccup and its single retry, nothing more", got)
	}
	f, ok := core.AsFailure(err)
	if !ok || f.Kind != core.FailureUpstream {
		t.Fatalf("err = %v, want an upstream-classified failure", err)
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
