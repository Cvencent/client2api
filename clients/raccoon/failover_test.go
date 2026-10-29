package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A transport fault on this platform must not end the whole route.  The module
// used to hand the gateway a bare *url.Error, which carries no FailureKind:
// the shared decision table could not classify it, so the gateway answered 502
// and stopped instead of trying the next platform in the chain.  The pool is
// the module's own business, so its exhaustion must be reported explicitly.
func TestChatReportsPoolExhaustionSoTheGatewayFailsOver(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The vendor accepts the connection and then drops it: the live symptom
		// was `Post ".../chat/completions": EOF` in the gateway log.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijacking the connection: %v", err)
		}
		_ = conn.Close()
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-glm-5-3-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success against a dropped connection")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted so the gateway moves to the next platform", err)
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure for the shared decision table", err)
	}
	if f.Kind != core.FailureUpstream {
		t.Fatalf("kind = %q, want %q for a transport fault", f.Kind, core.FailureUpstream)
	}
	// The gateway must be allowed to try a sibling platform on this kind.
	if !core.Retryable(f.Kind) && !core.NextPlatformOn(f.Kind) {
		t.Fatalf("kind %q permits neither rotation nor platform failover", f.Kind)
	}
	if f.Account == "" {
		t.Fatal("the failure names no account, so the operator cannot tell which credential was tried")
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("err = %v, want the vendor's own transport message preserved", err)
	}
}

// A request-shape refusal is not a platform problem: rotating the same body to
// a sibling platform would just be refused again, and the caller still needs
// the vendor's 4xx.
func TestChatKeepsARequestRefusalOutOfThePoolWalk(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid parameter: tools"}}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-glm-5-3-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success on a 400")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureOther {
		t.Fatalf("kind = %q, want %q: a request-shape refusal is not retryable anywhere", f.Kind, core.FailureOther)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want the vendor's %d preserved", f.Status, http.StatusBadRequest)
	}
	if core.Retryable(f.Kind) || core.NextPlatformOn(f.Kind) {
		t.Fatal("a request-shape refusal must not rotate accounts or platforms")
	}
}
