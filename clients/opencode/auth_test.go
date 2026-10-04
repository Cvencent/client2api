package opencode

import (
	"context"
	"net/http"
	"testing"
)

// TestChatUsesAnonymousCredentialHeaders proves the chat path, not just do,
// selects the anonymous credential shape: x-api-key carries the public key and
// no Authorization header is sent.
func TestChatUsesAnonymousCredentialHeaders(t *testing.T) {
	var got *http.Request
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		got = r
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(accountRecord{
		ID:       "opencode:anonymous",
		AuthMode: "anonymous",
		APIKey:   "public",
		Enabled:  true,
		Source:   sourcePanel,
	})

	stream, err := c.Chat(context.Background(), chatRequest("space-bunny-free"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	if got == nil {
		t.Fatal("no request was sent")
	}
	if got.Header.Get("x-api-key") != "public" {
		t.Fatalf("x-api-key = %q, want public", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q, want empty", got.Header.Get("Authorization"))
	}
}

// TestTestAccountUsesOAuthHeaders proves the probe path carries the OAuth
// bearer token and the organisation, and never falls back to a blank API key.
func TestTestAccountUsesOAuthHeaders(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var got *http.Request
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		got = r
		return sseResponse(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
				"data: [DONE]\n\n"), nil
	})
	c.pool.upsert(accountRecord{
		ID:           "opencode:oauth:user:org",
		AuthMode:     "oauth",
		AccessToken:  "oauth-access",
		RefreshToken: "oauth-refresh",
		OrgID:        "org-1",
		Enabled:      true,
		Source:       sourcePanel,
	})

	res, err := c.TestAccount(context.Background(), "opencode:oauth:user:org")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want OK", res)
	}
	if got == nil {
		t.Fatal("no request was sent")
	}
	if got.Header.Get("Authorization") != "Bearer oauth-access" {
		t.Fatalf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("x-opencode-org-id") != "org-1" {
		t.Fatalf("x-opencode-org-id = %q, want org-1", got.Header.Get("x-opencode-org-id"))
	}
	if got.Header.Get("x-api-key") != "" {
		t.Fatalf("x-api-key = %q, want empty", got.Header.Get("x-api-key"))
	}
}

// TestPoolUpsertMergesOAuthRefresh proves a refresh that supplies only the new
// OAuth fields updates the stored credential without dropping the org id.
func TestPoolUpsertMergesOAuthRefresh(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "old",
		RefreshToken: "r1", OrgID: "org-1", Enabled: true,
	})
	p.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "new",
		RefreshToken: "r2", ExpiresAt: "2026-10-02T00:00:00Z", Enabled: true,
	})
	got, ok := p.byID("opencode:oauth")
	if !ok {
		t.Fatal("account disappeared")
	}
	if got.AccessToken != "new" || got.RefreshToken != "r2" || got.ExpiresAt != "2026-10-02T00:00:00Z" {
		t.Fatalf("refresh fields not merged: %+v", got)
	}
	if got.OrgID != "org-1" {
		t.Fatalf("OrgID = %q, want org-1 preserved", got.OrgID)
	}
}

func TestDoUsesAPIKeyHeaderForAnonymousCredential(t *testing.T) {
	var got *http.Request
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		got = r
		return jsonResponse(http.StatusOK, `{}`), nil
	})

	resp, err := c.do(context.Background(), requestSpec{
		method: http.MethodGet,
		url:    "https://example.test/models",
		apiKey: "public",
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if got == nil {
		t.Fatal("request was not sent")
	}
	if got.Header.Get("x-api-key") != "public" {
		t.Fatalf("x-api-key = %q, want public", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q, want empty", got.Header.Get("Authorization"))
	}
}

func TestDoUsesOAuthHeaders(t *testing.T) {
	var got *http.Request
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		got = r
		return jsonResponse(http.StatusOK, `{}`), nil
	})

	resp, err := c.do(context.Background(), requestSpec{
		method: http.MethodPost,
		url:    "https://example.test/chat/completions",
		body:   []byte(`{}`),
		bearer: "oauth-access",
		orgID:  "org-1",
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if got.Header.Get("Authorization") != "Bearer oauth-access" {
		t.Fatalf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("x-opencode-org-id") != "org-1" {
		t.Fatalf("x-opencode-org-id = %q, want org-1", got.Header.Get("x-opencode-org-id"))
	}
	if got.Header.Get("x-api-key") != "" {
		t.Fatalf("x-api-key = %q, want empty", got.Header.Get("x-api-key"))
	}
}

func TestAnonymousCredentialIsSelectable(t *testing.T) {
	c := newTestClient(t, Config{})
	if !c.pool.upsert(accountRecord{
		ID:       "opencode:anonymous",
		AuthMode: "anonymous",
		APIKey:   "public",
		Enabled:  true,
		Source:   sourcePanel,
	}) {
		t.Fatal("anonymous account was not stored")
	}
	acct, err := c.pool.acquire(testNow, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if acct.AuthMode != "anonymous" {
		t.Fatalf("AuthMode = %q, want anonymous", acct.AuthMode)
	}
}

func TestOAuthCredentialIsSelectableWithoutAPIKey(t *testing.T) {
	c := newTestClient(t, Config{})
	if !c.pool.upsert(accountRecord{
		ID:           "opencode:oauth:user:org",
		AuthMode:     "oauth",
		AccessToken:  "oauth-access",
		RefreshToken: "oauth-refresh",
		OrgID:        "org-1",
		Enabled:      true,
		Source:       sourcePanel,
	}) {
		t.Fatal("OAuth account was not stored")
	}
	acct, err := c.pool.acquire(testNow, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if acct.AccessToken != "oauth-access" || acct.OrgID != "org-1" {
		t.Fatalf("account = %+v", acct)
	}
}
