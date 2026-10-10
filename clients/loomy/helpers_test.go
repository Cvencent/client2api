package loomy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// Shared fixtures for the module's tests.  Nothing here touches the network:
// every test injects a stubTransport, and the only files written go to the
// per-test temporary directory t.TempDir() hands out.

const (
	// testToken is shaped like a real Loomy session: 32 lowercase hex.
	testToken    = "0123456789abcdef0123456789abcdef"
	testTokenTwo = "fedcba9876543210fedcba9876543210"
	testUserID   = "123456789012345678"
)

// stubTransport records every request it is handed and answers it from a
// handler, so a test can assert on the exact bytes and headers that went on the
// wire rather than only on the parsed result.
type stubTransport struct {
	mu      sync.Mutex
	seen    []*http.Request
	bodies  []string
	handler func(r *http.Request, body string) *http.Response
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	if r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		body = string(raw)
		_ = r.Body.Close()
	}
	s.mu.Lock()
	s.seen = append(s.seen, r)
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()

	if s.handler == nil {
		return response(http.StatusOK, "application/json", `{"code":"000000","data":null}`), nil
	}
	return s.handler(r, body), nil
}

func (s *stubTransport) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.seen...)
}

func (s *stubTransport) sentBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func (s *stubTransport) lastBody() string {
	bodies := s.sentBodies()
	if len(bodies) == 0 {
		return ""
	}
	return bodies[len(bodies)-1]
}

func (s *stubTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// response builds a canned HTTP response.  A nil Request is fine: the module
// never reads it.
func response(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func jsonResponse(status int, body string) *http.Response {
	return response(status, "application/json", body)
}

func sseResponse(body string) *http.Response {
	return response(http.StatusOK, "text/event-stream; charset=utf-8", body)
}

// okEnvelope wraps a data payload in the vendor's success envelope.
func okEnvelope(data string) string {
	return `{"code":"000000","desc":"success","data":` + data + `}`
}

// failureEnvelope is the shape the vendor uses for a business failure: HTTP 200
// with a non-zero code.
func failureEnvelope(code, desc string) string {
	return `{"code":"` + code + `","desc":"` + desc + `"}`
}

// alwaysJSON answers every request with one fixed JSON body.
func alwaysJSON(status int, body string) *stubTransport {
	return &stubTransport{handler: func(*http.Request, string) *http.Response {
		return jsonResponse(status, body)
	}}
}

// alwaysSSE answers every request with one fixed SSE body, which is what the
// TestAccount chat probe reads.
func alwaysSSE(body string) *stubTransport {
	return &stubTransport{handler: func(*http.Request, string) *http.Response {
		return sseResponse(body)
	}}
}

// failingTransport models a request that never reached the vendor at all.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// newTestClient builds a Client against a private temporary data directory with
// every Loomy environment override cleared, so a developer's own shell cannot
// change what a test observes.
func newTestClient(t *testing.T, cfgJSON string, rt http.RoundTripper) *Client {
	t.Helper()
	for _, name := range []string{
		"CLIENT2API_LOOMY_API_BASE",
		"CLIENT2API_LOOMY_ACCOUNT_BASE",
		"CLIENT2API_LOOMY_ACCESS_KEY_ID",
		"CLIENT2API_LOOMY_ACCESS_KEY_SECRET",
		"CLIENT2API_LOOMY_APP_ID",
		"CLIENT2API_LOOMY_ACCESS_TOKEN",
		"CLIENT2API_LOOMY_USERID",
		"CLIENT2API_LOOMY_PHONE",
		"CLIENT2API_LOOMY_EXPIRES_AT",
		"CLIENT2API_LOOMY_SMS_TOKEN",
		"CLIENT2API_LOOMY_SMS_KEYWORD",
		"CLIENT2API_LOOMY_SMS_BASE",
		"EOMSG_TOKEN",
	} {
		t.Setenv(name, "")
	}
	return newTestClientInDir(t, t.TempDir(), cfgJSON, rt)
}

// newTestClientInDir is newTestClient with the data directory chosen by the
// caller, which is how the persistence tests reopen the same store.
func newTestClientInDir(t *testing.T, dir, cfgJSON string, rt http.RoundTripper) *Client {
	t.Helper()
	built, err := New(core.Deps{DataDir: dir, Config: json.RawMessage(cfgJSON)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}
	if rt != nil {
		c.up.json.Transport = rt
		c.up.sse.Transport = rt
		// The one-time-SMS platform goes through the shared client, so a test
		// that stubs the transport has to install it there too.
		c.deps.HTTPClient = &http.Client{Transport: rt}
	}
	return c
}

// seedAccount installs a credential directly, bypassing the network probe that
// AddAccount performs.
func seedAccount(t *testing.T, c *Client, id, token string, expiresAtMS int64) {
	t.Helper()
	acc := account{
		storedAccount: storedAccount{
			ID:          id,
			Label:       id,
			AccessToken: token,
			UserID:      id,
			ExpiresAtMS: expiresAtMS,
			Enabled:     true,
		},
		origin: originStored,
	}
	if err := c.store.put(acc); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
}

// mustJSON decodes s into v or fails the test.
func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
}
