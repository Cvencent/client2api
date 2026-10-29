package raccoon

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// The 413 is about the body, so every account would refuse it identically and
// the walk stops at the first one.  The headline must say that much: "every
// usable account on this platform failed" reads as a pool-wide health problem
// and sends the operator after accounts that were never even asked.
func TestAPayloadRefusalNamesTheSingleAttempt(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), twoAccountConfig(ts.URL), ts.Client())

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "sn-deepseek-v4-1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat: an upstream 413 must fail the request")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1: a payload refusal is not account-specific", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "after 1 account") {
		t.Fatalf("error must name the single attempt: %q", msg)
	}
	if !strings.Contains(msg, "not account-specific") {
		t.Fatalf("error must say why the other accounts were not tried: %q", msg)
	}
	if strings.Contains(msg, "every usable account") {
		t.Fatalf("error claims the whole pool was tried: %q", msg)
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted so the gateway moves on", err)
	}
}

// The other half of the count: an account-scoped refusal (a 429) does walk the
// pool, and the headline has to say so.  "after 2 accounts" and "after 1
// account" are different diagnoses, and the operator acts on them differently.
func TestAnAccountRefusalReportsTheWholePool(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":42900,"message":"slow down"}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), twoAccountConfig(ts.URL), ts.Client())

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "sn-deepseek-v4-1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat: an upstream 429 must fail the request")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2: a 429 is account-scoped and must rotate", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "after 2 accounts") {
		t.Fatalf("error must report how far the walk went: %q", msg)
	}
	if strings.Contains(msg, "not account-specific") {
		t.Fatalf("a throttling refusal is account-specific, yet the error says otherwise: %q", msg)
	}
}

// twoAccountConfig points two credentials at the same test server, so a test
// can tell "stopped at the first account" from "walked the pool".
func twoAccountConfig(baseURL string) string {
	return fmt.Sprintf(`{"base_url":%q,"accounts":[{"label":"a","access_token":"tok-a","user_id":"u1"},{"label":"b","access_token":"tok-b","user_id":"u2"}]}`, baseURL)
}
