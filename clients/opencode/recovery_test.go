package opencode

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestUnavailableEndpointDoesNotRateLimitAccount(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "a", "zen-key")
	acct, _ := c.pool.byID("a")
	err := c.classifyHTTP("chat", acct, 429, []byte(`{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`))
	if core.FailureKindOf(err) != core.FailureUpstream {
		t.Fatalf("failure kind = %v, want upstream", core.FailureKindOf(err))
	}
	stored, _ := c.pool.byID("a")
	if stored.CooldownUntil != "" || !stored.Enabled {
		t.Fatal("one unavailable endpoint parked the account")
	}
}

func TestExpiredCooldownIsReadyDespiteOldError(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "a", "zen-key")
	c.pool.setCooldown("a", testNow.Add(-time.Second), "rate limit")
	if h := c.Health(); !h.Servable || h.Ready != 1 {
		t.Fatalf("health = %+v, want a ready account after cooldown expires", h)
	}
}

func TestConsoleTokenAloneCannotServeZen(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		calls++
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", Enabled: true})
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if stream != nil {
		stream.Close()
	}
	if !errors.Is(err, core.ErrNotConfigured) || calls != 0 {
		t.Fatalf("chat error = %v, outbound calls = %d; Console token must not reach Zen", err, calls)
	}
}

func TestLoginWithoutZenConfigRetainsLoginButReportsNotReady(t *testing.T) {
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/user"):
			return jsonResponse(200, `{"id":"user","email":"user@example.test"}`), nil
		case strings.HasSuffix(r.URL.Path, "/api/orgs"):
			return jsonResponse(200, `[{"id":"org","name":"workspace"}]`), nil
		case strings.HasSuffix(r.URL.Path, "/api/config"):
			return jsonResponse(200, `{"config":{"disabled_providers":["opencode"],"provider":{}}}`), nil
		}
		t.Fatal("unexpected inference call")
		return nil, nil
	})
	result := c.finishOAuthLogin(context.Background(), deviceToken{AccessToken: "console-token", ExpiresIn: 3600})
	if result.cred == nil {
		t.Fatalf("Console login was lost: %s", result.message)
	}
	if h := c.Health(); h.Servable || h.Ready != 0 {
		t.Fatalf("health = %+v, workspace without Zen must not be ready", h)
	}
	if c.Status(context.Background()).Ready {
		t.Fatal("status incorrectly advertises inference readiness")
	}
	rows, _ := c.Accounts(context.Background())
	if len(rows) != 1 || rows[0].Note == "" {
		t.Fatal("missing explanation for workspace without Zen")
	}
}

func TestChatWaitsForGatewayAccountSlot(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	attempted := make(chan struct{}, 1)
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "a", "zen-key")
	req := chatRequest("gpt-5.1")
	var released atomic.Int32
	req.SetAccountAcquirer(func(string) (func(), error) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		if busy.Load() {
			return nil, core.ErrBusy
		}
		return func() { released.Add(1) }, nil
	})
	done := make(chan error, 1)
	go func() {
		stream, err := c.Chat(context.Background(), req)
		if stream != nil {
			stream.Close()
		}
		req.ReleaseAccountSlot()
		done <- err
	}()
	<-attempted
	select {
	case err := <-done:
		t.Fatalf("chat rejected a temporarily busy slot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	busy.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not resume when capacity returned")
	}
	if n, _ := c.pool.stats(4); n != 0 || released.Load() != 1 {
		t.Fatalf("leaked slots: pool=%d, gateway releases=%d", n, released.Load())
	}
}

func TestBusyWaitHonorsCancellation(t *testing.T) {
	c := newTestClient(t, Config{MaxInFlight: 1})
	addAccount(t, c, "a", "zen-key")
	c.pool.acquire(testNow, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Chat(ctx, chatRequest("gpt-5.1"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
	if n, _ := c.pool.stats(1); n != 1 {
		t.Fatalf("waiting reserved a slot: %d", n)
	}
}

func TestWorkspaceConfigSuppliesInferenceCredentialAndURL(t *testing.T) {
	var inference bool
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/user"):
			return jsonResponse(200, `{"id":"user"}`), nil
		case strings.HasSuffix(r.URL.Path, "/api/orgs"):
			return jsonResponse(200, `[{"id":"org"}]`), nil
		case strings.HasSuffix(r.URL.Path, "/api/config"):
			if r.Header.Get("Authorization") != "Bearer console-token" || r.Header.Get("x-org-id") != "org" {
				t.Fatal("Console config request lost login credentials")
			}
			return jsonResponse(200, `{"config":{"provider":{"opencode":{"options":{"apiKey":"zen-key","baseURL":"https://inference.test/v1"},"models":{"gpt-5.1":{"cost":{"input":1,"output":2}}}}}}}`), nil
		case r.URL.Host == "inference.test" && r.URL.Path == "/v1/chat/completions":
			inference = true
			if r.Header.Get("Authorization") != "Bearer zen-key" {
				t.Fatal("inference did not use workspace API key")
			}
			return sseResponse(happySSE), nil
		}
		t.Fatalf("unexpected request to %s", r.URL.Host)
		return nil, nil
	})
	res := c.finishOAuthLogin(context.Background(), deviceToken{AccessToken: "console-token", ExpiresIn: 3600})
	if res.cred == nil {
		t.Fatal("login failed")
	}
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if stream != nil {
		stream.Close()
	}
	if err != nil || !inference {
		t.Fatalf("inference = %t, error = %v", inference, err)
	}
}

func TestRefreshWorkspaceRevocationClearsInferenceKey(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"config":{"disabled_providers":["opencode"],"provider":{}}}`), nil
	})
	c.deps.DataDir = t.TempDir()
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org",
		APIKey: "old-zen-key", AllowedModels: []string{"gpt-5.1"}, Enabled: true})
	res, err := c.RefreshAccount(context.Background(), "oauth")
	if err != nil || len(res) != 1 || res[0].OK || res[0].Error == "" {
		t.Fatalf("refresh = %+v, error = %v", res, err)
	}
	acct, _ := c.pool.byID("oauth")
	if acct.APIKey != "" || len(acct.AllowedModels) != 0 || !acct.Enabled || acct.AccessToken != "console-token" {
		t.Fatal("workspace revocation did not clear stale inference data while preserving login")
	}
	stored := loadCredentials(c.credentialsPath())
	if len(stored) != 1 || stored[0].APIKey != "" {
		t.Fatal("revocation was not persisted")
	}
}

func TestRefreshLegacyConsoleAccountResolvesKeyWithoutEnablingIt(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"config":{"provider":{"opencode":{"options":{"apiKey":"zen-key"}}}}}`), nil
	})
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: false})
	res, err := c.RefreshAccount(context.Background(), "oauth")
	if err != nil || !res[0].OK {
		t.Fatalf("refresh = %+v, error = %v", res, err)
	}
	acct, _ := c.pool.byID("oauth")
	if acct.APIKey != "zen-key" || acct.Enabled {
		t.Fatal("refresh must resolve inference configuration and preserve the enable switch")
	}
}

func TestWorkspaceAPIAndModelProviderObjectAreDecoded(t *testing.T) {
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/api/config") {
			return jsonResponse(200, `{"config":{"provider":{"opencode":{"api":"https://workspace.test/v1","options":{"headers":{"Authorization":"Bearer {env:OPENCODE_CONSOLE_TOKEN}","x-org-id":"org"}},"models":{"gpt-5.1":{"cost":{"input":1,"output":2}},"claude":{"provider":{"api":"https://other.test","npm":"@ai-sdk/anthropic"}}}}}}}`), nil
		}
		if r.URL.Host != "workspace.test" || r.Header.Get("Authorization") != "Bearer console-token" || r.Header.Get("x-org-id") != "org" {
			t.Fatal("workspace inference configuration was not applied")
		}
		return sseResponse(happySSE), nil
	})
	acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: true}
	c.resolveWorkspace(context.Background(), &acct)
	c.pool.upsert(acct)
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if stream != nil {
		stream.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestFreeModelSkipsConsoleAccountInMixedPool(t *testing.T) {
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("x-api-key") != "public" {
			t.Fatal("free model was routed to a Console account")
		}
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", APIKey: "zen-key", Enabled: true})
	c.pool.upsert(accountRecord{ID: "free", AuthMode: "anonymous", APIKey: "public", Enabled: true})
	stream, err := c.Chat(context.Background(), chatRequest("step-5-preview-free"))
	if stream != nil {
		stream.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestInferenceErrorDoesNotExposeConsoleOrZenSecrets(t *testing.T) {
	c := newTestClient(t, Config{})
	acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "plain-console-secret", APIKey: "plain-zen-secret", Enabled: true}
	c.pool.upsert(acct)
	err := c.classifyHTTP("chat", &acct, 500, []byte(`{"error":{"type":"server_error","message":"plain-console-secret plain-zen-secret"}}`))
	stored, _ := c.pool.byID("oauth")
	if strings.Contains(err.Error()+stored.LastError, acct.AccessToken) || strings.Contains(err.Error()+stored.LastError, acct.APIKey) {
		t.Fatal("error exposed login or inference credential")
	}
}

func TestWorkspaceCannotSendConsoleTokenToDefaultZenInHeader(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"config":{"provider":{"opencode":{"options":{"headers":{"Authorization":"Bearer console-token"}}}}}}`), nil
	})
	acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: true}
	c.resolveWorkspace(context.Background(), &acct)
	if acct.inferenceReady() {
		t.Fatal("Console token header was accepted for the public Zen endpoint")
	}
}

func TestChatRotatesPastBusyBoundAccount(t *testing.T) {
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer key-b" {
			t.Fatal("chat did not choose the second account")
		}
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "a", "key-a")
	addAccount(t, c, "b", "key-b")
	c.affinity = core.NewAffinity(time.Hour)
	req := chatRequest("gpt-5.1")
	req.ConversationID = "conversation"
	c.BindConversation("conversation", "a")
	var held bool
	req.SetAccountAcquirer(func(id string) (func(), error) {
		if id == "a" {
			return nil, core.ErrBusy
		}
		held = true
		return func() { held = false }, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := c.Chat(ctx, req)
	if stream != nil {
		stream.Close()
	}
	req.ReleaseAccountSlot()
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := c.pool.stats(4); n != 0 || held {
		t.Fatal("rotation leaked a slot")
	}
}

func TestSingleUpstreamOutageDoesNotRetrySameAccountIntoCooldown(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(429, `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`), nil
	})
	addAccount(t, c, "a", "zen-key")
	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	acct, _ := c.pool.byID("a")
	if core.FailureKindOf(err) != core.FailureUpstream || acct.CooldownUntil != "" || calls != 1 {
		t.Fatalf("calls=%d, parked=%t, error=%v", calls, acct.CooldownUntil != "", err)
	}
}

func TestWorkspaceWithNoSupportedModelsIsNotReady(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"config":{"provider":{"opencode":{"options":{"apiKey":"zen-key"},"models":{"gpt-5.1":{"disabled":true},"claude":{"provider":{"api":"https://other.test","npm":"@ai-sdk/anthropic"}}}}}}}`), nil
	})
	acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: true}
	c.resolveWorkspace(context.Background(), &acct)
	if acct.inferenceReady() {
		t.Fatal("empty supported workspace list was treated as unrestricted")
	}
}

func TestWorkspaceWhitelistWithoutOverridesIsRespected(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"config":{"provider":{"opencode":{"options":{"apiKey":"zen-key"},"whitelist":["gpt-5.1"]}}}}`), nil
	})
	acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: true}
	c.resolveWorkspace(context.Background(), &acct)
	if c.accountSupportsModel(&acct, "not-allowed") || !c.accountSupportsModel(&acct, "gpt-5.1") {
		t.Fatal("workspace whitelist was lost when model overrides were absent")
	}
}

func TestStreamErrorScrubsConsoleAndInferenceKeys(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse("data: {\"error\":{\"type\":\"server_error\",\"message\":\"plain-console-secret plain-zen-secret\"}}\n\n"), nil
	})
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "plain-console-secret", APIKey: "plain-zen-secret", Enabled: true})
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, _, _, _, _, err = drain(stream)
	if err == nil {
		t.Fatal("stream error was lost")
	}
	if strings.Contains(err.Error(), "plain-console-secret") || strings.Contains(err.Error(), "plain-zen-secret") {
		t.Fatal("stream error exposed a credential")
	}
}

func TestConsoleTokenCannotUseAlternateSpellingsOfPublicZen(t *testing.T) {
	for _, base := range []string{"https://OPENCODE.AI/zen/v1", "https://opencode.ai:443/zen/v1/"} {
		t.Run(base, func(t *testing.T) {
			c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, string(mustJSON(map[string]any{"config": map[string]any{"provider": map[string]any{
					"opencode": map[string]any{"options": map[string]any{"baseURL": base, "apiKey": "{env:OPENCODE_CONSOLE_TOKEN}"}}}}}))), nil
			})
			acct := accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "console-token", OrgID: "org", Enabled: true}
			c.resolveWorkspace(context.Background(), &acct)
			if acct.inferenceReady() {
				t.Fatal("equivalent public Zen URL accepted a Console token")
			}
		})
	}
}

func TestRetriesShareOneQueueDeadline(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{QueueTimeout: Duration(90 * time.Millisecond)}, func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(503, `{"error":{"type":"server_error","message":"unavailable"}}`), nil
	})
	addAccount(t, c, "a", "key-a")
	addAccount(t, c, "b", "key-b")
	req := chatRequest("gpt-5.1")
	start := time.Now()
	req.SetAccountAcquirer(func(id string) (func(), error) {
		if id == "b" || time.Since(start) < 60*time.Millisecond {
			return nil, core.ErrBusy
		}
		return func() {}, nil
	})
	_, err := c.Chat(context.Background(), req)
	req.ReleaseAccountSlot()
	if core.FailureKindOf(err) != core.FailureUpstream || calls != 1 {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
	if elapsed := time.Since(start); elapsed > 140*time.Millisecond {
		t.Fatalf("retry reset queue budget: %s", elapsed)
	}
}

func TestChatRefreshErrorScrubsBothCredentials(t *testing.T) {
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/auth/device/token") {
			t.Fatal("chat proceeded after failed token refresh")
		}
		return jsonResponse(200, `{"error":"server_error","error_description":"plain-console-secret plain-zen-secret"}`), nil
	})
	c.pool.upsert(accountRecord{ID: "oauth", AuthMode: "oauth", AccessToken: "plain-console-secret",
		APIKey: "plain-zen-secret", RefreshToken: "refresh-secret", OrgID: "org",
		ExpiresAt: testNow.Add(-time.Minute).Format(time.RFC3339), Enabled: true})
	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err == nil {
		t.Fatal("refresh failure was lost")
	}
	if strings.Contains(err.Error(), "plain-console-secret") || strings.Contains(err.Error(), "plain-zen-secret") {
		t.Fatal("refresh failure exposed a credential")
	}
	if n, _ := c.pool.stats(4); n != 0 {
		t.Fatal("failed refresh retained its pool slot")
	}
}
