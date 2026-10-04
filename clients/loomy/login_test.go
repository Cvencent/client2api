package loomy

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// The SMS login flow against the iFlytek account endpoint.  The important guard
// here is TestLoginSignsTheBytesItActuallySends: the stub recomputes the
// signature from the raw request it received, using a from-scratch reading of
// the documented nine-segment scheme, so a signature computed over anything
// other than the bytes on the wire is caught.

const testAccountBase = "https://account.test"

func loginConfig() string {
	return `{"account_base":"` + testAccountBase + `"}`
}

// signAsTheServerSeesIt rebuilds the canonical string from the request the stub
// received and signs it.  It is deliberately written from the protocol
// description rather than by calling the module, so it can disagree with it.
func signAsTheServerSeesIt(t *testing.T, r *http.Request, body, secret string) string {
	t.Helper()

	digest := ""
	if body != "" {
		sum := md5.Sum([]byte(body))
		digest = base64.StdEncoding.EncodeToString(sum[:])
	}
	canonical := strings.Join([]string{
		strings.ToUpper(r.Method),
		r.URL.EscapedPath(),
		r.URL.RawQuery,
		digest,
		r.Header.Get("Content-Type"),
		r.Header.Get("Date"),
		r.Header.Get("Nonce"),
		"",
		"",
	}, "\n")

	mac := hmac.New(sha1.New, []byte(secret))
	if _, err := mac.Write([]byte(canonical)); err != nil {
		t.Fatalf("hmac: %v", err)
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// accountBody is the product preamble plus the operation parameters.
type accountBody struct {
	Base struct {
		AppID   string `json:"appid"`
		ModelID string `json:"modelid"`
		Version string `json:"version"`
		DevID   string `json:"devid"`
		UA      string `json:"ua"`
		TraceID string `json:"traceid"`
	} `json:"base"`
	Param map[string]any `json:"param"`
}

func decodeAccountBody(t *testing.T, body string) accountBody {
	t.Helper()
	var out accountBody
	mustJSON(t, body, &out)
	return out
}

func TestSendSMSCodeSignsTheBytesItActuallySends(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"msgid":"msg-1"}`))
	c := newTestClient(t, loginConfig(), rt)

	if _, err := c.up.sendSMSCode(context.Background(), "13800000000"); err != nil {
		t.Fatalf("sendSMSCode: %v", err)
	}

	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("sent %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if got := req.URL.Host; got != "account.test" {
		t.Fatalf("host = %q, want the configured account base", got)
	}
	if got := req.URL.Path; got != "/login/phone/sendMsgCode" {
		t.Fatalf("path = %q", got)
	}
	if got := req.Method; got != http.MethodPost {
		t.Fatalf("method = %q, want POST", got)
	}
	if got := req.URL.RawQuery; got != "" {
		t.Fatalf("query = %q, want none", got)
	}

	auth := req.Header.Get("Authorization")
	prefix := "account " + defaultAccessKeyID + ":"
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("Authorization = %q, want the %q prefix", auth, prefix)
	}
	want := signAsTheServerSeesIt(t, req, rt.lastBody(), defaultAccessKeySecret)
	if got := strings.TrimPrefix(auth, prefix); got != want {
		t.Fatalf("signature = %q, but the bytes on the wire sign to %q", got, want)
	}

	for _, name := range []string{"Date", "Nonce", "Content-MD5"} {
		if strings.TrimSpace(req.Header.Get(name)) == "" {
			t.Fatalf("%s header is missing", name)
		}
	}
	if _, err := time.Parse(http.TimeFormat, req.Header.Get("Date")); err != nil {
		t.Fatalf("Date = %q is not an HTTP date: %v", req.Header.Get("Date"), err)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
}

func TestSendSMSCodeCarriesTheProductPreambleAndThePhoneNumber(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"msgid":"msg-1"}`))
	c := newTestClient(t, loginConfig(), rt)

	if _, err := c.up.sendSMSCode(context.Background(), "13800000000"); err != nil {
		t.Fatalf("sendSMSCode: %v", err)
	}

	body := decodeAccountBody(t, rt.lastBody())
	if body.Base.AppID != defaultAppID {
		t.Fatalf("appid = %q, want %q", body.Base.AppID, defaultAppID)
	}
	if body.Base.ModelID != "Web" || body.Base.Version != "1.0.0" || body.Base.DevID != "web" {
		t.Fatalf("product preamble = %+v", body.Base)
	}
	if body.Base.UA != "Loomy|Desktop|Electron|macOS" {
		t.Fatalf("ua = %q, want the Loomy desktop string", body.Base.UA)
	}
	if body.Base.TraceID != strings.ReplaceAll(body.Base.TraceID, "-", "") {
		t.Fatalf("traceid = %q should carry no dashes", body.Base.TraceID)
	}
	if len(body.Base.TraceID) != 32 {
		t.Fatalf("traceid = %q, want 32 characters", body.Base.TraceID)
	}
	if got := body.Param["ccode"]; got != "86" {
		t.Fatalf("ccode = %v, want 86", got)
	}
	if got := body.Param["phone"]; got != "13800000000" {
		t.Fatalf("phone = %v", got)
	}
	if got := asInt64(t, body.Param["expire"]); got != smsCodeTTLSeconds {
		t.Fatalf("expire = %d, want %d", got, smsCodeTTLSeconds)
	}
}

func TestSendSMSCodeRejectsAResponseWithoutAMessageID(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{}`))
	c := newTestClient(t, loginConfig(), rt)

	_, err := c.up.sendSMSCode(context.Background(), "13800000000")
	if err == nil {
		t.Fatal("sendSMSCode accepted a response with no msgid; the next step cannot work without one")
	}
	if !strings.Contains(err.Error(), "no message id") {
		t.Fatalf("error = %v", err)
	}
}

func TestSendSMSCodeSurfacesABusinessError(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, failureEnvelope(loomyBadRequestCode, "手机号格式错误"))
	c := newTestClient(t, loginConfig(), rt)

	_, err := c.up.sendSMSCode(context.Background(), "not-a-phone")
	if err == nil {
		t.Fatal("sendSMSCode ignored a business error")
	}
	if !strings.Contains(err.Error(), "手机号格式错误") {
		t.Fatalf("error = %v, want the vendor's description", err)
	}
}

func TestLoginWithSMSCodeSignsTheBytesItActuallySends(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"session":"`+testToken+`","userid":"`+testUserID+`"}`))
	c := newTestClient(t, loginConfig(), rt)

	session, err := c.up.loginWithSMSCode(context.Background(), "13800000000", "123456", "msg-1")
	if err != nil {
		t.Fatalf("loginWithSMSCode: %v", err)
	}
	if session.AccessToken != testToken {
		t.Fatalf("AccessToken = %q", session.AccessToken)
	}
	if session.UserID != testUserID {
		t.Fatalf("UserID = %q", session.UserID)
	}
	if session.Phone != "13800000000" {
		t.Fatalf("Phone = %q", session.Phone)
	}

	req := rt.requests()[0]
	if got := req.URL.Path; got != "/login/phone/checkCode" {
		t.Fatalf("path = %q", got)
	}
	prefix := "account " + defaultAccessKeyID + ":"
	want := signAsTheServerSeesIt(t, req, rt.lastBody(), defaultAccessKeySecret)
	if got := strings.TrimPrefix(req.Header.Get("Authorization"), prefix); got != want {
		t.Fatalf("signature = %q, but the bytes on the wire sign to %q", got, want)
	}

	body := decodeAccountBody(t, rt.lastBody())
	if got := body.Param["mcode"]; got != "123456" {
		t.Fatalf("mcode = %v", got)
	}
	if got := body.Param["msgid"]; got != "msg-1" {
		t.Fatalf("msgid = %v", got)
	}
	// The vendor derives the session lifetime from this field, which is why the
	// session cannot outlive 14 days.
	if got := asInt64(t, body.Param["expire"]); got != sessionTTLSeconds {
		t.Fatalf("expire = %d, want %d", got, sessionTTLSeconds)
	}
}

func TestLoginWithSMSCodeRejectsAnIncompleteResponse(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"no session", `{"userid":"` + testUserID + `"}`},
		{"no user id", `{"session":"` + testToken + `"}`},
		{"empty", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := alwaysJSON(http.StatusOK, okEnvelope(tc.data))
			c := newTestClient(t, loginConfig(), rt)

			if _, err := c.up.loginWithSMSCode(context.Background(), "13800000000", "123456", "msg-1"); err == nil {
				t.Fatal("an incomplete login response was accepted")
			} else if !strings.Contains(err.Error(), "no session or user id") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestImportSessionStoresAFourteenDayCredential(t *testing.T) {
	dir := t.TempDir()
	c := newTestClientInDir(t, dir, loginConfig(), alwaysJSON(http.StatusOK, okEnvelope(`{}`)))

	now := time.Now().UTC()
	record, err := c.importSession(loomySession{AccessToken: testToken, UserID: testUserID, Phone: "13800000000"}, now)
	if err != nil {
		t.Fatalf("importSession: %v", err)
	}
	if record.ID != "loomy-"+testUserID {
		t.Fatalf("id = %q, want it derived from the user id", record.ID)
	}
	// The row is labelled with the phone, not the opaque id: that is what the
	// panel's re-login button keys on to re-issue the account's own number.
	if record.Label != "13800000000" {
		t.Fatalf("label = %q, want the phone number", record.Label)
	}
	if record.State != "ready" {
		t.Fatalf("state = %q, want ready", record.State)
	}
	if record.Identity != testUserID {
		t.Fatalf("identity = %q", record.Identity)
	}
	if record.ExpiresAt == "" {
		t.Fatal("the imported credential carries no expiry")
	}

	acc, ok := c.store.lookup(record.ID)
	if !ok {
		t.Fatalf("account %q was not stored", record.ID)
	}
	want := now.Add(sessionTTLSeconds * time.Second).UnixMilli()
	if diff := acc.ExpiresAtMS - want; diff > 1000 || diff < -1000 {
		t.Fatalf("expiry = %d, want about %d (now + 14 days)", acc.ExpiresAtMS, want)
	}
	if acc.AccessToken != testToken {
		t.Fatal("the stored credential is not the session that was imported")
	}

	// Reopening the store must find the credential again: this is the whole
	// point of importing one.
	reopened := newTestClientInDir(t, dir, loginConfig(), nil)
	again, ok := reopened.store.lookup(record.ID)
	if !ok {
		t.Fatal("the imported credential did not survive a restart")
	}
	if again.AccessToken != testToken || again.UserID != testUserID {
		t.Fatalf("reloaded account = %+v", again.storedAccount)
	}
}

func TestImportSessionNeverEchoesTheTokenInTheAccountRecord(t *testing.T) {
	c := newTestClient(t, loginConfig(), alwaysJSON(http.StatusOK, okEnvelope(`{}`)))

	record, err := c.importSession(loomySession{AccessToken: testToken, UserID: testUserID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("importSession: %v", err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testToken) {
		t.Fatalf("the account record echoes the session token: %s", raw)
	}
}

func TestAccountBaseCarriesTheProductPreamble(t *testing.T) {
	base := newAccountBase(defaultAppID)
	if base.AppID != defaultAppID {
		t.Fatalf("AppID = %q", base.AppID)
	}
	if base.ModelID != "Web" || base.Version != "1.0.0" || base.DevID != "web" {
		t.Fatalf("preamble = %+v", base)
	}
	if base.UA != "Loomy|Desktop|Electron|macOS" {
		t.Fatalf("UA = %q", base.UA)
	}
	if len(base.TraceID) != 32 || strings.Contains(base.TraceID, "-") {
		t.Fatalf("TraceID = %q, want 32 characters and no dashes", base.TraceID)
	}
	for _, r := range base.TraceID {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("TraceID = %q is not lowercase hex", base.TraceID)
		}
	}
}

func TestAccountBaseHonoursTheConfiguredAppID(t *testing.T) {
	c := newTestClient(t, `{"app_id":"GM3TEST"}`, alwaysJSON(http.StatusOK, okEnvelope(`{}`)))
	if got := newAccountBase(c.cfg.appID()).AppID; got != "GM3TEST" {
		t.Fatalf("AppID = %q, want the configured value", got)
	}
}

func TestTheSMSFlowIsReachableThroughAutoLoginNotLoginProvider(t *testing.T) {
	// core.LoginProvider is a URL-and-poll interface with no way to submit an
	// SMS code, so the module must not claim it: that would put a "get an
	// authorisation link" button in front of a flow that has no link.
	//
	// The flow IS drivable from the panel, through core.AutoLoginProvider (the
	// module runs the whole exchange itself) and core.SMSProvider (the number
	// and the code come from the one-time-SMS platform).
	var c any = newTestClient(t, loginConfig(), nil)
	if _, ok := c.(core.LoginProvider); ok {
		t.Fatal("the module claims core.LoginProvider, but the panel cannot submit an SMS code through it")
	}
	if _, ok := c.(core.AutoLoginProvider); !ok {
		t.Fatal("the module does not claim core.AutoLoginProvider, so the panel cannot run the SMS login")
	}
	if _, ok := c.(core.SMSProvider); !ok {
		t.Fatal("the module does not claim core.SMSProvider, so the panel cannot rent a number for the login")
	}
}
