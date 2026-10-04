package loomy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// These tests run against a real net/http server over a real loopback socket.
// The RoundTripper stubs used everywhere else never exercise the HTTP client
// itself -- URL joining, header writing, chunked transfer, the actual SSE
// framing -- so this file covers that gap.

// recorder collects what the server saw, safely: the handler runs on the
// server's own goroutine and may still be finishing while the test reads.
type recorder struct {
	mu     sync.Mutex
	auth   string
	token  string
	accept string
	body   string
	path   string
	hits   int
}

func (r *recorder) note(req *http.Request, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	r.path = req.URL.Path
	r.auth = req.Header.Get("Authorization")
	r.token = req.Header.Get("token")
	r.accept = req.Header.Get("Accept")
	r.body = body
}

func (r *recorder) snapshot() recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recorder{auth: r.auth, token: r.token, accept: r.accept, body: r.body, path: r.path, hits: r.hits}
}

// sseServer answers a chat completion with a real event stream.
func sseServer(t *testing.T, rec *recorder, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		rec.note(req, string(raw))

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("the test server cannot flush; the stream test would be meaningless")
			return
		}
		for _, frame := range frames {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatStreamsFromARealHTTPServer(t *testing.T) {
	rec := &recorder{}
	srv := sseServer(t, rec, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	})

	c := newTestClient(t, `{"api_base":"`+srv.URL+`/api/v1"}`, nil)
	seedAccount(t, c, "acc", testToken, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	sawUsage := false
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
		case core.EventUsage:
			if ev.Usage == nil || ev.Usage.TotalTokens != 9 {
				t.Fatalf("usage = %+v", ev.Usage)
			}
			sawUsage = true
		}
	}
	if got := text.String(); got != "Hello" {
		t.Fatalf("streamed text = %q, want Hello", got)
	}
	if !sawUsage {
		t.Fatal("the usage frame was dropped")
	}

	seen := rec.snapshot()
	if seen.path != "/api/v1/chat/completions" {
		t.Fatalf("path = %q", seen.path)
	}
	if seen.auth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want a bearer token", seen.auth)
	}
	if seen.token != testToken {
		t.Fatalf("token header = %q", seen.token)
	}
	if seen.accept != "text/event-stream" {
		t.Fatalf("Accept = %q", seen.accept)
	}
	if !strings.Contains(seen.body, `"stream":true`) {
		t.Fatalf("body = %q, want a streaming request", seen.body)
	}
	if !strings.Contains(seen.body, `"model":"deepseek-v4-flash-0731"`) {
		t.Fatalf("body = %q, want the resolved model id", seen.body)
	}
}

func TestChatReportsAnErrorFrameFromARealServer(t *testing.T) {
	// The vendor can answer a stream request with HTTP 200 and a JSON envelope.
	// That must surface as an error, not as a silently empty stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, failureEnvelope(loomyAuthErrorCode, "登录状态失效"))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"api_base":"`+srv.URL+`/api/v1"}`, nil)
	seedAccount(t, c, "acc", testToken, 0)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("a JSON envelope answered a stream request without an error")
	}

	acc, ok := c.store.lookup("acc")
	if !ok || !acc.dead {
		t.Fatal("the rejected session was not parked")
	}
}

func TestModelsFetchesFromARealHTTPServer(t *testing.T) {
	var (
		mu    sync.Mutex
		token string
		auth  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		token = req.Header.Get("token")
		auth = req.Header.Get("Authorization")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okEnvelope(`[
			{"id":"m-one","name":"M One （x1.5）","type":"chat","context_length":123456},
			{"id":"m-image","name":"M Image · x0.1","type":"image"},
			{"id":"m-two","name":"M Two","type":"chat"}
		]`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"api_base":"`+srv.URL+`/api/v1"}`, nil)
	seedAccount(t, c, "acc", testToken, 0)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want the two chat rows: %+v", len(models), models)
	}
	if models[0].ID != "m-one" || models[1].ID != "m-two" {
		t.Fatalf("ids = %q, %q", models[0].ID, models[1].ID)
	}
	if got := models[0].Extra["display_name"]; got != "M One · x1.5" {
		t.Fatalf("display_name = %v, want the normalised multiplier", got)
	}
	if got := models[0].Extra["context_length"]; got != 123456 {
		t.Fatalf("context_length = %v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if token != testToken {
		t.Fatalf("token header = %q", token)
	}
	if auth != "" {
		t.Fatalf("Authorization = %q: /models rejects it", auth)
	}
}
