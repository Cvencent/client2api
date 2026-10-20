package raccoon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// The vendor's HTTP layer refuses oversized bodies with a bare 413 and no
// usable number. The error the gateway records has to carry the two facts the
// operator needs: how large the request was and what to shrink.
func TestChatRequestTooLargeCarriesSizeAndAdvice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		`{"base_url":"`+ts.URL+`","access_token":"tok","user_id":"u1"}`, ts.Client())

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "sn-deepseek-v4-1-flash",
		Messages: []core.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}},
	})
	if err == nil {
		t.Fatal("Chat: an upstream 413 must fail the request")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTP 413") {
		t.Fatalf("error must keep the status: %q", msg)
	}
	if !strings.Contains(msg, "request body too large") {
		t.Fatalf("error must name the problem: %q", msg)
	}
	if !strings.Contains(msg, " MB") {
		t.Fatalf("error must quote the encoded size: %q", msg)
	}
	if !strings.Contains(msg, "shorten the conversation") {
		t.Fatalf("error must say what to shrink: %q", msg)
	}
}

// A credential-level or transient refusal must keep the vendor's own message:
// the size annotation is only for the one status where size is the whole story.
func TestChatNon413KeepsVendorMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":40001,"message":"model not served"}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		`{"base_url":"`+ts.URL+`","access_token":"tok","user_id":"u1"}`, ts.Client())

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "sn-deepseek-v4-1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat: an upstream 400 must fail the request")
	}
	if !strings.Contains(err.Error(), "model not served") {
		t.Fatalf("error must keep the vendor message: %q", err.Error())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{
		512:      "512 B",
		20 << 10: "20 KB",
		3 << 20:  "3.0 MB",
		2730:     "3 KB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
