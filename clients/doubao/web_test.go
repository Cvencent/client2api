package doubao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"client2api/internal/core"
)

// newWebClient builds a client whose web_base points at a test server.
func newWebClient(t *testing.T, base string) *Client {
	t.Helper()
	raw, err := New(core.Deps{
		DataDir: t.TempDir(),
		Config:  []byte(`{"web_base":"` + base + `"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return raw.(*Client)
}

// TestGetJSONSendsCookieAndCommonParams pins the request shape: the cookie
// header, Agw-Js-Conv, and the common query parameters the web app sends on
// every call.
func TestGetJSONSendsCookieAndCommonParams(t *testing.T) {
	var gotCookie, gotConv string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotConv = r.Header.Get("Agw-Js-Conv")
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"data":{"nickname":"ok"}}`))
	}))
	defer srv.Close()

	c := newWebClient(t, srv.URL)
	acc := &account{ID: "a1", Cookie: "sessionid=abc; ttwid=xyz", Enabled: true}
	var out struct {
		Data accountInfo `json:"data"`
	}
	if err := c.up.getJSON(context.Background(), acc, pathAccountInfo, nil, &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if gotCookie != "sessionid=abc; ttwid=xyz" {
		t.Fatalf("Cookie = %q", gotCookie)
	}
	if gotConv != agentJSConv {
		t.Fatalf("Agw-Js-Conv = %q, want %q", gotConv, agentJSConv)
	}
	if gotQuery.Get("aid") != paramAid || gotQuery.Get("samantha_web") != "1" {
		t.Fatalf("common params missing: %v", gotQuery)
	}
}

// TestClassifyHTTPMapsAuthAndRateLimit pins the two classifications the pool
// acts on differently.
func TestClassifyHTTPMapsAuthAndRateLimit(t *testing.T) {
	auth := classifyHTTP(http.StatusUnauthorized, []byte(`{"message":"login"}`))
	if !isAuthError(auth) {
		t.Fatalf("401 = %v, want an auth error", auth)
	}
	rate := classifyHTTP(http.StatusTooManyRequests, []byte(`{"message":"slow down"}`))
	if !isRateLimitError(rate) {
		t.Fatalf("429 = %v, want a rate-limit error", rate)
	}
	if got := rate.Error(); !strings.Contains(got, "rate limited") {
		t.Fatalf("rate error = %q, want it to mention rate limiting", got)
	}
}

// TestProbeAccountRejectsAnAnonymousReply pins the "present but not signed in"
// case: a 200 with no user id is an auth failure, not a success.
func TestProbeAccountRejectsAnAnonymousReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	_, err := c.up.accountInfo(context.Background(), &account{ID: "a1", Cookie: "sessionid=abc"})
	if !isAuthError(err) {
		t.Fatalf("anonymous reply error = %v, want an auth error", err)
	}
}

// TestRawPostStreamsAndCarriesBody pins the streamed POST path used by Chat:
// the body reaches the vendor and the response is returned unread.
func TestRawPostStreamsAndCarriesBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"event_type\":\"FIN\"}\n\n")
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	resp, err := c.up.rawPost(context.Background(), &account{ID: "a1", Cookie: "sessionid=abc"}, pathChat, map[string]any{"messages": []any{}})
	if err != nil {
		t.Fatalf("rawPost: %v", err)
	}
	defer resp.Body.Close()
	if _, ok := body["messages"]; !ok {
		t.Fatalf("body did not reach the vendor: %v", body)
	}
}

