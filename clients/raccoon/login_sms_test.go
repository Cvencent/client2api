package raccoon

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// postLocalLogin posts a JSON body to a path on the loopback login server.
func postLocalLogin(t *testing.T, base, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// decryptPhone reverses the vendor's transport cipher so a test can prove the
// phone really travelled encrypted and really decodes to what was typed.
func decryptPhone(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("phone is not base64: %v", err)
	}
	if len(raw) <= aes.BlockSize {
		t.Fatalf("phone payload is too short: %d bytes", len(raw))
	}
	block, err := aes.NewCipher([]byte(phoneCipherSecret))
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	iv, ct := raw[:aes.BlockSize], raw[aes.BlockSize:]
	out := make([]byte, len(ct))
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(out, ct)
	return string(out)
}

func lastBodyFor(t *testing.T, up *loginUpstream, path string) map[string]any {
	t.Helper()
	up.mu.Lock()
	defer up.mu.Unlock()
	for i := len(up.paths) - 1; i >= 0; i-- {
		if up.paths[i] != path {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(up.bodies[i]), &out); err != nil {
			t.Fatalf("request %d body is not JSON: %v", i, err)
		}
		return out
	}
	t.Fatalf("no request was made to %s", path)
	return nil
}

func startLogin(t *testing.T, up *loginUpstream) (*Client, string) {
	t.Helper()
	c := newLoginClient(t, up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	return c, loginOrigin(t, st.URL)
}

// loginOrigin is the scheme+host the loopback server is bound to.
func loginOrigin(t *testing.T, pageURL string) string {
	t.Helper()
	return "http://" + mustHost(t, pageURL)
}

func TestSmsSendRejectsMissingPhoneAndCaptcha(t *testing.T) {
	up := &loginUpstream{}
	_, base := startLogin(t, up)

	status, body := postLocalLogin(t, base, pathSmsSend, `{"phone":"123","captchaParam":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad phone answered %d, want 400", status)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatal("a bad phone was accepted")
	}

	status, body = postLocalLogin(t, base, pathSmsSend, `{"phone":"13800001111","captchaParam":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("missing captcha answered %d, want 400", status)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatal("a missing captcha was accepted")
	}
	if up.countPath(pathSendSMS) != 0 {
		t.Fatal("an invalid form reached the vendor")
	}
}

func TestSmsSendEncryptsThePhone(t *testing.T) {
	up := &loginUpstream{}
	_, base := startLogin(t, up)

	status, body := postLocalLogin(t, base, pathSmsSend, `{"phone":"13800001111","captchaParam":"captcha-token"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("response = %v, want ok", body)
	}
	sent := lastBodyFor(t, up, pathSendSMS)
	if sent["nation_code"] != "86" {
		t.Fatalf("nation_code = %v, want 86", sent["nation_code"])
	}
	if sent["captcha_param"] != "captcha-token" {
		t.Fatalf("captcha_param = %v, want the slider token", sent["captcha_param"])
	}
	enc, _ := sent["phone"].(string)
	if enc == "" || enc == "13800001111" {
		t.Fatalf("phone = %q, want an encrypted payload", enc)
	}
	if got := decryptPhone(t, enc); got != "13800001111" {
		t.Fatalf("decrypted phone = %q, want 13800001111", got)
	}
}

func TestSmsLoginStoresTheAccount(t *testing.T) {
	up := &loginUpstream{}
	c, base := startLogin(t, up)

	if status, _ := postLocalLogin(t, base, pathSmsSend, `{"phone":"13800001111","captchaParam":"captcha-token"}`); status != http.StatusOK {
		t.Fatalf("sms send answered %d", status)
	}
	status, body := postLocalLogin(t, base, pathSmsVerify, `{"smsCode":"654321"}`)
	if status != http.StatusOK {
		t.Fatalf("sms verify answered %d", status)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("response = %v, want ok", body)
	}
	login := lastBodyFor(t, up, pathLoginSMS)
	if login["sms_code"] != "654321" {
		t.Fatalf("sms_code = %v, want 654321", login["sms_code"])
	}
	if enc, _ := login["phone"].(string); decryptPhone(t, enc) != "13800001111" {
		t.Fatalf("login phone did not decrypt to the typed number")
	}
	polled, err := c.PollLogin(context.Background(), mustSessionID(t, c))
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginSuccess || polled.AccountID == "" {
		t.Fatalf("state = %+v, want success with an account", polled)
	}
	e := c.pool.find(polled.AccountID)
	if e == nil || e.acct.AccessToken != "sms-token" {
		t.Fatalf("stored account = %+v, want the SMS token", e)
	}
}

// mustSessionID returns the id of the single registered session.
func mustSessionID(t *testing.T, c *Client) string {
	t.Helper()
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	for id := range c.logins {
		return id
	}
	t.Fatal("no login session is registered")
	return ""
}

func TestSmsErrorsDoNotEndTheSession(t *testing.T) {
	up := &loginUpstream{smsSendErr: "验证码错误"}
	c, base := startLogin(t, up)

	status, body := postLocalLogin(t, base, pathSmsSend, `{"phone":"13800001111","captchaParam":"captcha-token"}`)
	if status != http.StatusOK {
		t.Fatalf("a vendor refusal must be a 200 with ok:false, got %d", status)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("response = %v, want ok:false", body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "滑块") {
		t.Fatalf("message = %q, want the captcha-specific hint", msg)
	}
	polled, err := c.PollLogin(context.Background(), mustSessionID(t, c))
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginPending {
		t.Fatalf("state = %q, want the session still pending", polled.State)
	}

	// The operator fixes the slider and retries: the second send must work.
	up.mu.Lock()
	up.smsSendErr = ""
	up.mu.Unlock()
	status, body = postLocalLogin(t, base, pathSmsSend, `{"phone":"13800001111","captchaParam":"fresh-token"}`)
	if status != http.StatusOK {
		t.Fatalf("retry answered %d", status)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("retry = %v, want ok", body)
	}
}

func TestSmsVerifyWithoutASendIsRefused(t *testing.T) {
	up := &loginUpstream{}
	_, base := startLogin(t, up)
	status, body := postLocalLogin(t, base, pathSmsVerify, `{"smsCode":"123456"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("verify before send answered %d, want 400", status)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatal("a verify with no phone on record was accepted")
	}
	if up.countPath(pathLoginSMS) != 0 {
		t.Fatal("a verify with no phone reached the vendor")
	}
}

// The page must never carry the phone cipher secret or a token; only the host
// holds them.
func TestLoginPageCarriesNoCipherSecret(t *testing.T) {
	up := &loginUpstream{}
	_, base := startLogin(t, up)
	resp, err := http.Get(base + pathLogin)
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	if bytes.Contains(raw, []byte(phoneCipherSecret)) {
		t.Fatal("the page leaked the phone cipher secret")
	}
	if strings.Contains(page, "access_token") {
		t.Fatal("the page mentions a token field")
	}
}
