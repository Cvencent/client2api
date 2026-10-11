package kilocode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// testJWT builds a fake-but-well-formed JWT whose payload carries the given
// claims.  The signature is a placeholder: nothing verifies it.
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal claim: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := enc(map[string]any{"alg": "HS256", "typ": "JWT"})
	payload := enc(claims)
	return header + "." + payload + ".sig"
}

// fakeClock is a manually advanced clock so a test can step past a poll
// interval without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now().UTC()} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestJWTClaims(t *testing.T) {
	tok := testJWT(t, map[string]any{"kiloUserId": "user-42", "exp": float64(2_000_000_000)})
	if got := jwtStringClaim(tok, "kiloUserId"); got != "user-42" {
		t.Fatalf("kiloUserId = %q", got)
	}
	if got := jwtExpiry(tok); !got.Equal(time.Unix(2_000_000_000, 0).UTC()) {
		t.Fatalf("expiry = %v", got)
	}
	if got := jwtStringClaim("not-a-jwt", "kiloUserId"); got != "" {
		t.Fatalf("non-JWT should answer no claim, got %q", got)
	}
}

// --- device login ---------------------------------------------------------

func TestDeviceLoginFlow(t *testing.T) {
	clk := newFakeClock()
	token := testJWT(t, map[string]any{
		"kiloUserId": "user-123",
		"exp":        float64(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()),
	})
	var approved bool
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/api/device-auth/codes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("start method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"ABCD-EFGH","user_code":"ABCD-EFGH",`+
			`"device_code":"dev-secret","verificationUrl":"https://app.kilo.ai/device-auth?code=ABCD-EFGH",`+
			`"expiresIn":599}`)
	})
	mux.HandleFunc("/api/device-auth/codes/ABCD-EFGH", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("poll method = %s, want GET", r.Method)
		}
		mu.Lock()
		ok := approved
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"pending"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"approved","token":"`+token+`","userEmail":"dev@example.com"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, `{"auth_base":"`+srv.URL+`","anonymous":false}`, srv.Client())
	c.now = clk.now

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending || st.Code != "ABCD-EFGH" {
		t.Fatalf("start state = %+v", st)
	}
	if !strings.Contains(st.URL, "app.kilo.ai/device-auth") {
		t.Fatalf("verification URL = %q", st.URL)
	}

	// First poll: still pending upstream.
	st, err = c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin(pending): %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("pending poll state = %+v", st)
	}

	// A poll inside the interval must be throttled and stay pending, without
	// reaching the vendor again.
	st, _ = c.PollLogin(context.Background(), st.SessionID)
	if st.State != core.LoginPending {
		t.Fatalf("throttled poll state = %+v", st)
	}

	mu.Lock()
	approved = true
	mu.Unlock()
	clk.advance(defaultDeviceInterval + time.Second)

	st, err = c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin(approved): %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Fatalf("approved poll state = %+v", st)
	}
	rec, ok := c.pool.byID(oauthIDForToken(token))
	if !ok {
		t.Fatalf("signed-in account was not stored")
	}
	if rec.bearer() != token || rec.authMode() != authModeOAuth {
		t.Fatalf("stored record = %+v", rec)
	}
	if rec.Source != sourcePanel {
		t.Fatalf("source = %q, want panel", rec.Source)
	}
}

func TestDeviceLoginDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/device-auth/codes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"AAAA-BBBB","verificationUrl":"https://app.kilo.ai/device-auth?code=AAAA-BBBB","expiresIn":599}`)
	})
	mux.HandleFunc("/api/device-auth/codes/AAAA-BBBB", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, `{"auth_base":"`+srv.URL+`"}`, srv.Client())
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	st, err = c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if st.State != core.LoginFailed {
		t.Fatalf("denied state = %+v", st)
	}
}

// --- authenticated requests ----------------------------------------------

func oauthTestClient(t *testing.T, srv *httptest.Server, token string) *Client {
	t.Helper()
	c := newTestClient(t, `{"base_url":"`+srv.URL+`","anonymous":false}`, srv.Client())
	c.pool.upsert(accountRecord{
		ID:           oauthIDForToken(token),
		Label:        "signed in",
		AuthMode:     authModeOAuth,
		AccessToken:  token,
		RefreshToken: token,
		Source:       sourcePanel,
	})
	return c
}

func TestOAuthModelFetchSendsBearer(t *testing.T) {
	token := testJWT(t, map[string]any{"kiloUserId": "user-1"})
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, modelsFixture)
	}))
	defer srv.Close()

	c := oauthTestClient(t, srv, token)
	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
	if len(list) == 0 {
		t.Fatalf("no models returned")
	}
}

func TestOAuthChatSendsBearer(t *testing.T) {
	token := testJWT(t, map[string]any{"kiloUserId": "user-1"})
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := oauthTestClient(t, srv, token)
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kilo-auto/free",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = stream.Close()
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
}

// --- persistence and import ----------------------------------------------

func TestOAuthCredentialPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	token := testJWT(t, map[string]any{"kiloUserId": "user-9"})
	deps := core.Deps{DataDir: dir, Config: json.RawMessage(`{"anonymous":false}`)}

	c1, _ := New(deps)
	first := c1.(*Client)
	first.pool.upsert(accountRecord{
		ID:           oauthIDForToken(token),
		Label:        "signed in",
		AuthMode:     authModeOAuth,
		AccessToken:  token,
		RefreshToken: token,
		ExpiresAt:    "2030-01-01T00:00:00Z",
		Source:       sourcePanel,
		AddedAt:      first.now().UTC().Format(time.RFC3339),
	})
	first.persist()

	c2, _ := New(deps)
	second := c2.(*Client)
	rec, ok := second.pool.byID(oauthIDForToken(token))
	if !ok {
		t.Fatalf("OAuth credential did not survive a restart")
	}
	if rec.bearer() != token {
		t.Fatalf("bearer = %q after restart", rec.bearer())
	}
	if rec.ExpiresAt != "2030-01-01T00:00:00Z" {
		t.Fatalf("expiry = %q", rec.ExpiresAt)
	}
}

func TestParseLocalAuthFile(t *testing.T) {
	token := testJWT(t, map[string]any{"kiloUserId": "user-1"})
	body := `{"kilo":{"type":"oauth","access":"` + token + `","refresh":"` + token + `","expires":1823226085656}}`
	keys := parseCredentialFile([]byte(body))
	if len(keys) != 1 {
		t.Fatalf("parseCredentialFile = %+v", keys)
	}
	if keys[0].AuthMode != authModeOAuth || keys[0].AccessToken != token {
		t.Fatalf("parsed = %+v", keys[0])
	}
	if !strings.HasPrefix(keys[0].ExpiresAt, "2027-") {
		t.Fatalf("expiry = %q", keys[0].ExpiresAt)
	}
}

func TestImportLocalAuthFile(t *testing.T) {
	dir := t.TempDir()
	token := testJWT(t, map[string]any{"kiloUserId": "user-77"})
	authPath := filepath.Join(dir, "auth.json")
	body := `{"kilo":{"type":"oauth","access":"` + token + `","refresh":"` + token + `","expires":1823226085656}}`
	if err := os.WriteFile(authPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, `{"anonymous":false}`, nil)
	recs, err := c.Import(context.Background(), []string{authPath}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != oauthIDForToken(token) {
		t.Fatalf("imported records = %+v", recs)
	}
	rec, ok := c.pool.byID(oauthIDForToken(token))
	if !ok || rec.bearer() != token {
		t.Fatalf("pool record after import = %+v (ok=%v)", rec, ok)
	}
}

func TestReadOAuthTokenFileRejectsKeyFiles(t *testing.T) {
	dir := t.TempDir()
	key := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmMifQ.signature"
	p := filepath.Join(dir, "creds.json")
	_ = os.WriteFile(p, []byte(`{"api_key":"`+key+`"}`), 0o600)
	if _, ok := readOAuthTokenFile(p); ok {
		t.Fatalf("a plain key file must not read as an OAuth sign-in")
	}
}

func TestOAuthBearerFallback(t *testing.T) {
	// A record written by an older build (no AuthMode) still answers with its
	// access token as the bearer.
	rec := accountRecord{AccessToken: "tok", APIKey: "key"}
	if got := rec.bearer(); got != "tok" {
		t.Fatalf("bearer = %q, want the access token", got)
	}
	keyed := accountRecord{APIKey: "key"}
	if got := keyed.bearer(); got != "key" {
		t.Fatalf("keyed bearer = %q", got)
	}
}
