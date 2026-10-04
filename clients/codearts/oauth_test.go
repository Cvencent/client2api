package codearts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// oauth_test.go covers the OAuth half: the authorisation URL's three
// load-bearing details, the two token grants, and the deliberate omission of
// InvalidDPoPHeader from the terminal-refresh classification.

func TestBuildAuthorizeURL(t *testing.T) {
	raw := buildAuthorizeURL(12345, "CHALLENGE", "")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "codearts.huaweicloud.com" || u.Path != "/portal/authorize" {
		t.Fatalf("url = %q, want the portal authorize endpoint", raw)
	}
	q := u.Query()

	// The literal SHA-256, NOT the RFC 7636 S256.  The portal silently falls
	// back to the legacy ticket flow when it sees S256.
	if got := q.Get("code_challenge_method"); got != "SHA-256" {
		t.Errorf("code_challenge_method = %q, want the literal SHA-256", got)
	}
	if q.Get("code_challenge") != "CHALLENGE" {
		t.Errorf("code_challenge = %q, want the challenge", q.Get("code_challenge"))
	}
	if q.Get("client_id") != "codearts-agent" {
		t.Errorf("client_id = %q, want codearts-agent", q.Get("client_id"))
	}
	if q.Get("uri_scheme") != "codearts-agent" {
		t.Errorf("uri_scheme = %q, want codearts-agent", q.Get("uri_scheme"))
	}
	if q.Get("port") != "12345" {
		t.Errorf("port = %q, want 12345", q.Get("port"))
	}
	if q.Get("theme") != "2" || q.Get("locale") != "zh-cn" {
		t.Errorf("theme/locale = %q/%q, want 2/zh-cn", q.Get("theme"), q.Get("locale"))
	}
	if q.Get("plugin-name") != "snap_AIIDE" || q.Get("plugin-version") != "5.2.0" {
		t.Errorf("plugin = %q/%q, want snap_AIIDE/5.2.0 (the real extension version, not ours)",
			q.Get("plugin-name"), q.Get("plugin-version"))
	}
	// The portal treats auth_callback_url as an abnormal request and falls
	// back to the ticket flow.
	if _, ok := q["auth_callback_url"]; ok {
		t.Error("the authorisation URL carries auth_callback_url")
	}
	if _, ok := q["ticket_id"]; ok {
		t.Error("a ticket_id was sent even though none was supplied")
	}
}

func TestBuildAuthorizeURLIncludesTheTicketID(t *testing.T) {
	u, err := url.Parse(buildAuthorizeURL(15000, "C", "TICKET"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("ticket_id"); got != "TICKET" {
		t.Errorf("ticket_id = %q, want TICKET", got)
	}
}

func TestBuildPortalLoginResultURL(t *testing.T) {
	raw := buildPortalLoginResultURL(true)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "codearts.huaweicloud.com" || u.Path != "/portal/login" {
		t.Fatalf("url = %q, want the portal login endpoint", raw)
	}
	q := u.Query()
	if q.Get("login_succeed") != "true" {
		t.Errorf("login_succeed = %q, want true", q.Get("login_succeed"))
	}
	if q.Get("uri_scheme") != "codearts-agent" || q.Get("locale") != "zh-cn" {
		t.Errorf("uri_scheme/locale = %q/%q, want codearts-agent/zh-cn", q.Get("uri_scheme"), q.Get("locale"))
	}
}

// The reference implementation classifies exactly two conditions as a dead
// refresh token, and DELIBERATELY not InvalidDPoPHeader — that is a
// per-request rejection, and treating it as terminal would park a perfectly
// good account because one proof was malformed.
func TestIsTerminalRefreshFailure(t *testing.T) {
	cases := []struct {
		resp *tokenResponse
		want bool
		why  string
	}{
		{nil, false, "no response is not evidence of anything"},
		{&tokenResponse{Error: "invalid_grant"}, true, "invalid_grant means the refresh token is gone"},
		{&tokenResponse{Error: "INVALID_GRANT"}, true, "the comparison is case-insensitive"},
		{&tokenResponse{ErrorCode: "IAM.ExpiredRefreshToken"}, true, "ExpiredRefreshToken is terminal"},
		{&tokenResponse{ErrorCode: "InvalidDPoPHeader"}, false, "a bad proof is NOT a dead refresh token"},
		{&tokenResponse{Error: "invalid_dpop_proof"}, false, "a bad proof is NOT a dead refresh token"},
		{&tokenResponse{Error: "server_error"}, false, "a transient server error is retryable"},
		{&tokenResponse{}, false, "an empty answer is not evidence of anything"},
	}
	for _, c := range cases {
		if got := isTerminalRefreshFailure(c.resp); got != c.want {
			t.Errorf("isTerminalRefreshFailure(%+v) = %v, want %v (%s)", c.resp, got, c.want, c.why)
		}
	}
}

func TestTokenErrorText(t *testing.T) {
	if got := tokenErrorText(nil); !strings.Contains(got, "empty body") {
		t.Errorf("tokenErrorText(nil) = %q, want the empty-body message", got)
	}
	got := tokenErrorText(&tokenResponse{Error: "invalid_grant", ErrorCode: "IAM.0403", ErrorMsg: "expired"})
	for _, want := range []string{"invalid_grant", "IAM.0403", "expired"} {
		if !strings.Contains(got, want) {
			t.Errorf("tokenErrorText = %q, want it to mention %q", got, want)
		}
	}
	if got := tokenErrorText(&tokenResponse{}); !strings.Contains(got, "without saying why") {
		t.Errorf("tokenErrorText = %q, want the no-reason message", got)
	}
}

// dpopPayload decodes the payload of a compact JWS, so a test can assert what
// the proof actually claims.
func dpopPayload(t *testing.T, compact string) map[string]any {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("the DPoP proof has %d parts, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the DPoP payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the DPoP payload is not JSON: %v", err)
	}
	return out
}

// stsServer stands in for the STS endpoint.  It records the request so the
// test can assert the form and the proof.
type stsCapture struct {
	form      url.Values
	dpop      string
	htu       string
	userAgent string
	authz     string
	calls     int
}

func newSTSServer(t *testing.T, status int, body string) (*httptest.Server, *stsCapture) {
	t.Helper()
	cap := &stsCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.calls++
		_ = r.ParseForm()
		cap.form = r.PostForm
		cap.dpop = r.Header.Get("DPoP")
		cap.userAgent = r.Header.Get("User-Agent")
		cap.authz = r.Header.Get("Authorization")
		if cap.dpop != "" {
			if payload := dpopPayload(t, cap.dpop); payload != nil {
				cap.htu, _ = payload["htu"].(string)
			}
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want the form encoding", ct)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestExchangeAuthorizationCodeSendsTheRightForm(t *testing.T) {
	srv, cap := newSTSServer(t, http.StatusOK, `{"credentials":{
		"access_key_id":"AKNEW","secret_access_key":"SKNEW","security_token":"TOKNEW",
		"expiration":"2031-01-02T03:04:05Z"},"refresh_token":"RTNEW"}`)

	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	got, err := c.exchangeAuthorizationCode(context.Background(), "THE-CODE", "THE-VERIFIER", "http://127.0.0.1:15000/oauth/callback", key)
	if err != nil {
		t.Fatalf("exchangeAuthorizationCode: %v", err)
	}

	if cap.form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", cap.form.Get("grant_type"))
	}
	if cap.form.Get("client_id") != "codearts-agent" {
		t.Errorf("client_id = %q, want codearts-agent", cap.form.Get("client_id"))
	}
	if cap.form.Get("code") != "THE-CODE" {
		t.Errorf("code = %q, want THE-CODE", cap.form.Get("code"))
	}
	if cap.form.Get("code_verifier") != "THE-VERIFIER" {
		t.Errorf("code_verifier = %q, want THE-VERIFIER", cap.form.Get("code_verifier"))
	}
	// Byte-for-byte, because the server compares it with the request.
	if cap.form.Get("redirect_uri") != "http://127.0.0.1:15000/oauth/callback" {
		t.Errorf("redirect_uri = %q, want the exact value passed in", cap.form.Get("redirect_uri"))
	}
	if cap.authz != "" {
		t.Errorf("Authorization = %q, want none — the token request is authenticated by the DPoP proof alone", cap.authz)
	}

	if got.AccessKeyID != "AKNEW" || got.SecretAccessKey != "SKNEW" || got.SecurityToken != "TOKNEW" {
		t.Errorf("credential = %+v, want the returned triple", got)
	}
	if got.RefreshToken != "RTNEW" {
		t.Errorf("refresh_token = %q, want RTNEW", got.RefreshToken)
	}
	if got.CodeVerifier != "THE-VERIFIER" {
		t.Errorf("code_verifier = %q, want it persisted alongside the credential", got.CodeVerifier)
	}
	if got.DpopPrivateJwk == "" {
		t.Error("the DPoP private key was not persisted; the credential could never be refreshed")
	}
	if got.ExpiresAt != time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC).Unix() {
		t.Errorf("ExpiresAt = %d, want the parsed expiration", got.ExpiresAt)
	}
	if got.ID == "" {
		t.Error("the credential has no id")
	}
}

// The DPoP proof must name the endpoint actually called, not the built-in
// default, or the server rejects it.
func TestExchangeRefreshTokenProofNamesTheEndpointCalled(t *testing.T) {
	srv, cap := newSTSServer(t, http.StatusOK, `{"credentials":{
		"access_key_id":"AKNEW","secret_access_key":"SKNEW","security_token":"TOKNEW",
		"expiration":"2031-01-02T03:04:05Z"}}`)

	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	if _, err := c.exchangeRefreshToken(context.Background(), "RTOLD", "VERIFIER", key); err != nil {
		t.Fatalf("exchangeRefreshToken: %v", err)
	}

	if cap.form.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", cap.form.Get("grant_type"))
	}
	if cap.form.Get("refresh_token") != "RTOLD" {
		t.Errorf("refresh_token = %q, want RTOLD", cap.form.Get("refresh_token"))
	}
	// The token endpoint requires the PKCE verifier even on the refresh
	// grant, which is why it is persisted with the credential.
	if cap.form.Get("code_verifier") != "VERIFIER" {
		t.Errorf("code_verifier = %q, want the stored verifier", cap.form.Get("code_verifier"))
	}
	if cap.htu != srv.URL {
		t.Errorf("the DPoP htu = %q, want the endpoint actually called %q", cap.htu, srv.URL)
	}
	if cap.userAgent == "" {
		t.Error("the token request carried no user agent")
	}
}

// A refresh that returns no new refresh token keeps the old one; dropping it
// would silently make the account unrefreshable.
func TestRefreshKeepsTheOldRefreshToken(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusOK, `{"credentials":{
		"access_key_id":"AKNEW","secret_access_key":"SKNEW","security_token":"TOKNEW",
		"expiration":"2031-01-02T03:04:05Z"}}`)

	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	e := &entry{acct: account{
		ID: "acct-1", AccessKeyID: "AKOLD", SecretAccessKey: "SKOLD", SecurityToken: "TOKOLD",
		RefreshToken: "RTOLD", CodeVerifier: "V", DpopPrivateJwk: key.PrivateJwk,
		DomainID: "DOM", UserID: "USER", UserName: "someone", Note: "the note",
	}}
	c.pool.entries = append(c.pool.entries, e)

	if err := c.refreshCredential(context.Background(), e); err != nil {
		t.Fatalf("refreshCredential: %v", err)
	}
	got := e.account()
	if got.RefreshToken != "RTOLD" {
		t.Errorf("refresh_token = %q, want the previous one kept", got.RefreshToken)
	}
	if got.AccessKeyID != "AKNEW" || got.SecurityToken != "TOKNEW" {
		t.Errorf("credential = %+v, want the refreshed triple", got)
	}
	// The refresh answer does not repeat the identifying fields.
	if got.ID != "acct-1" || got.Note != "the note" || got.DomainID != "DOM" || got.UserID != "USER" || got.UserName != "someone" {
		t.Errorf("credential = %+v, want the identifying fields carried over", got)
	}
}

func TestRefreshCredentialWithoutARefreshToken(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	e := &entry{acct: account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"}}
	err := c.refreshCredential(context.Background(), e)
	if err == nil {
		t.Fatal("an unrefreshable credential produced no error")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("err = %q, want it to say why", err.Error())
	}
}

// A dead refresh token is terminal: no retry can fix it and the operator has
// to sign in again.
func TestExchangeRefreshTokenTerminalFailure(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusBadRequest, `{"error":"invalid_grant","error_msg":"expired"}`)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, _ := generateDpopKeyPair()

	_, err := c.exchangeRefreshToken(context.Background(), "RT", "V", key)
	if err == nil {
		t.Fatal("a dead refresh token produced no error")
	}
	if !errors.Is(err, errRefreshTerminal) {
		t.Fatalf("err = %v, want it to wrap errRefreshTerminal", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %q, want the server's own reason carried through", err.Error())
	}
}

func TestExchangeRefreshTokenExpiredRefreshTokenCode(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusBadRequest, `{"error_code":"IAM.0403.ExpiredRefreshToken"}`)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, _ := generateDpopKeyPair()

	_, err := c.exchangeRefreshToken(context.Background(), "RT", "V", key)
	if !errors.Is(err, errRefreshTerminal) {
		t.Fatalf("err = %v, want it to wrap errRefreshTerminal", err)
	}
}

// THE DELIBERATE OMISSION.  A malformed proof must NOT park the account.
func TestExchangeRefreshTokenInvalidDpopIsNotTerminal(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusBadRequest, `{"error_code":"InvalidDPoPHeader","error_msg":"the proof is malformed"}`)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, _ := generateDpopKeyPair()

	_, err := c.exchangeRefreshToken(context.Background(), "RT", "V", key)
	if err == nil {
		t.Fatal("a rejected proof produced no error")
	}
	if errors.Is(err, errRefreshTerminal) {
		t.Fatalf("err = %v, want it NOT to be terminal — one bad proof is not a dead refresh token", err)
	}
	if !strings.Contains(err.Error(), "InvalidDPoPHeader") {
		t.Errorf("err = %q, want the server's own code carried through", err.Error())
	}
}

// The STS endpoint reports a dead refresh token with a 400 and a JSON body, so
// the body must be read before the status is judged.
func TestPostTokenReadsTheBodyOnANon2xx(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusForbidden, `{"error":"invalid_grant"}`)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, _ := generateDpopKeyPair()

	_, err := c.exchangeRefreshToken(context.Background(), "RT", "V", key)
	if err == nil {
		t.Fatal("a 403 produced no error")
	}
	if !errors.Is(err, errRefreshTerminal) {
		t.Fatalf("err = %v, want the 403 body to have been read and classified", err)
	}
}

func TestPostTokenRequiresADpopKey(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	if _, err := c.postToken(context.Background(), url.Values{}, nil, nil); err == nil {
		t.Fatal("a token request with no DPoP key produced no error")
	}
}

func TestPostTokenRejectsANonJSONBody(t *testing.T) {
	srv, _ := newSTSServer(t, http.StatusOK, `<html>not json</html>`)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","sts_url":`+jsonString(srv.URL)+`}`)
	key, _ := generateDpopKeyPair()

	_, err := c.exchangeRefreshToken(context.Background(), "RT", "V", key)
	if err == nil {
		t.Fatal("a non-JSON token body produced no error")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("err = %q, want the decode failure named", err.Error())
	}
}

func TestCredentialFromTokenResponseRejectsMissingFields(t *testing.T) {
	key, _ := generateDpopKeyPair()
	if _, err := credentialFromTokenResponse(nil, "V", key); err == nil {
		t.Error("a nil response produced no error")
	}
	if _, err := credentialFromTokenResponse(&tokenResponse{}, "V", key); err == nil {
		t.Error("a response with no credentials produced no error")
	}
	if _, err := credentialFromTokenResponse(&tokenResponse{
		Credentials: &tokenCredentials{AccessKeyID: "AK", SecretAccessKey: "SK"},
	}, "V", key); err == nil {
		t.Error("a credential with no security token was accepted")
	}
	if _, err := credentialFromTokenResponse(&tokenResponse{
		Credentials: &tokenCredentials{SecretAccessKey: "SK", SecurityToken: "TOK"},
	}, "V", key); err == nil {
		t.Error("a credential with no access key was accepted")
	}
}

// The vendor always sends an expiry; if it ever stops, the reference's own
// 24-hour fallback is used rather than treating the credential as eternal.
func TestCredentialFromTokenResponseDefaultsTheExpiry(t *testing.T) {
	key, _ := generateDpopKeyPair()
	got, err := credentialFromTokenResponse(&tokenResponse{
		Credentials: &tokenCredentials{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "TOK"},
	}, "V", key)
	if err != nil {
		t.Fatalf("credentialFromTokenResponse: %v", err)
	}
	if got.ExpiresAt == 0 {
		t.Fatal("ExpiresAt = 0, want a fallback so the credential is not treated as eternal")
	}
	if d := time.Until(time.Unix(got.ExpiresAt, 0)); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("the fallback expiry is %v away, want about 24h", d)
	}
}
