package raccoon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// eomsgStub is the one-time-SMS platform the module talks to. Every request
// is recorded so the tests can prove the keyword, the number and the code all
// travelled the way the provider contract requires.
type eomsgStub struct {
	t      *testing.T
	server *httptest.Server

	phone string
	msg   string
	calls []url.Values
}

func newEomsgStub(t *testing.T) *eomsgStub {
	t.Helper()
	s := &eomsgStub{t: t, phone: "13800001111"}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.calls = append(s.calls, q)
		switch q.Get("code") {
		case "leftAmount":
			_, _ = io.WriteString(w, "12.30")
		case "getPhone":
			if q.Get("keyWord") != defaultSMSKeyword {
				t.Errorf("keyWord = %q, want %q", q.Get("keyWord"), defaultSMSKeyword)
			}
			_, _ = io.WriteString(w, s.phone)
		case "getMsg":
			_, _ = io.WriteString(w, s.msg)
		case "release", "block":
			_, _ = io.WriteString(w, "OK")
		default:
			t.Errorf("unexpected platform code %q", q.Get("code"))
			http.Error(w, "bad request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func TestRaccoonSMSProviderRentsAndReadsTheCode(t *testing.T) {
	stub := newEomsgStub(t)
	stub.msg = "【商汤科技】验证码 654321，5分钟内有效。"
	c := newTestClient(t, t.TempDir(), `{"sms_token":"tok","sms_base":`+strconvQuote(stub.server.URL)+`}`, nil)

	status := c.SMSStatus(context.Background(), core.SMSOpts{})
	if !status.Configured || status.Provider != "eomsg" {
		t.Fatalf("SMSStatus = %+v, want a configured eomsg provider", status)
	}
	if status.Keyword != defaultSMSKeyword || status.Balance != "12.30" {
		t.Fatalf("SMSStatus = %+v, want the 商汤科技 keyword and balance", status)
	}

	num, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil)
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "13800001111" {
		t.Fatalf("phone = %q, want the rented number", num.Phone)
	}
	code, err := c.PollSMSCode(context.Background(), core.SMSOpts{}, num.Phone)
	if err != nil {
		t.Fatalf("PollSMSCode: %v", err)
	}
	if !code.Ready || code.Code != "654321" {
		t.Fatalf("code = %+v, want 654321", code)
	}
	if err := c.ReleasePhone(context.Background(), core.SMSOpts{}, num.Phone, false); err != nil {
		t.Fatalf("ReleasePhone: %v", err)
	}
}

func TestRaccoonSMSRequiresAToken(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	if _, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil); err == nil {
		t.Fatal("AcquirePhone accepted a missing platform token")
	}
}

func TestRaccoonSMSDefaultKeywordIsSenseTime(t *testing.T) {
	if defaultSMSKeyword != "商汤科技" {
		t.Fatalf("defaultSMSKeyword = %q, want 商汤科技", defaultSMSKeyword)
	}
}

func TestRaccoonCaptchaSessionRoundTrip(t *testing.T) {
	sess, err := newRaccoonCaptchaSession()
	if err != nil {
		t.Fatalf("newRaccoonCaptchaSession: %v", err)
	}
	defer sess.close()

	page, err := http.Get(sess.url())
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "AliyunCaptcha") {
		t.Fatalf("page did not contain the vendor SDK: %q", string(body))
	}
	if !strings.Contains(sess.url(), aliyunCaptchaSceneID) || !strings.Contains(sess.url(), aliyunCaptchaPrefix) {
		t.Fatalf("captcha URL = %q, want the vendor scene and prefix", sess.url())
	}

	raw := `{"ok":true,"param":"captcha-1"}`
	resp, err := http.Get("http://" + sess.ln.Addr().String() + "/reply?d=" + url.QueryEscape(raw))
	if err != nil {
		t.Fatalf("GET reply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reply status = %d", resp.StatusCode)
	}
	got, err := sess.wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !got.OK || got.Param != "captcha-1" {
		t.Fatalf("reply = %+v, want the minted parameter", got)
	}
}

func TestRaccoonAutoLoginFailsFastWithoutAPlatformToken(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin accepted a run with no platform token")
	}
}

func TestRaccoonAutoLoginFailsFastWithNoBrowser(t *testing.T) {
	missing := t.TempDir() + string(filepath.Separator) + "missing-browser"
	c := newTestClient(t, t.TempDir(), `{"sms_token":"tok","browser_path":`+strconvQuote(missing)+`,"sms_polls":1}`, nil)
	// A CI runner ships Chromium, so the configured path alone cannot prove
	// the guard fired.  Pin the lookup to "nothing installed" instead.
	c.findBrowser = func(string) string { return "" }
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin accepted a run with no usable browser")
	}
}

func TestRaccoonAutoLoginStoresTheRentedAccount(t *testing.T) {
	up := &loginUpstream{}
	ts := httptest.NewServer(up.handler(t))
	t.Cleanup(ts.Close)
	stub := newEomsgStub(t)
	stub.msg = "验证码 654321，5分钟内有效。"

	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(ts.URL)+`,"sms_token":"tok","sms_base":`+strconvQuote(stub.server.URL)+`,"auto_login_timeout_seconds":5,"sms_polls":2,"sms_interval_seconds":1}`, nil)
	c.autoCaptcha = func(context.Context, string) (string, error) { return "captcha-token", nil }

	job, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		got, err := c.PollAutoLogin(context.Background(), job.ID)
		if err != nil {
			t.Fatalf("PollAutoLogin: %v", err)
		}
		if got.State != core.AutoLoginRunning {
			if got.State != core.AutoLoginSuccess {
				t.Fatalf("auto-login ended %s: %s (%v)", got.State, got.Message, got.Log)
			}
			if got.AccountID == "" || c.pool.find(got.AccountID) == nil {
				t.Fatalf("auto-login did not store an account: %+v", got)
			}
			if up.countPath(pathSendSMS) != 1 || up.countPath(pathLoginSMS) != 1 {
				t.Fatalf("vendor SMS calls = send %d, login %d; want one each", up.countPath(pathSendSMS), up.countPath(pathLoginSMS))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("auto-login did not finish in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRaccoonAutoLoginReleasesAFailedNumberAndRetries(t *testing.T) {
	up := &loginUpstream{}
	ts := httptest.NewServer(up.handler(t))
	t.Cleanup(ts.Close)
	stub := newEomsgStub(t)
	stub.msg = "验证码 654321，5分钟内有效。"

	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(ts.URL)+`,"sms_token":"tok","sms_base":`+strconvQuote(stub.server.URL)+`,"auto_login_timeout_seconds":8,"sms_polls":2,"sms_interval_seconds":1}`, nil)
	var captchas int
	c.autoCaptcha = func(context.Context, string) (string, error) {
		captchas++
		if captchas == 1 {
			return "", errors.New("first slider mint was rejected")
		}
		return "captcha-token", nil
	}

	job, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		got, err := c.PollAutoLogin(context.Background(), job.ID)
		if err != nil {
			t.Fatalf("PollAutoLogin: %v", err)
		}
		if got.State != core.AutoLoginRunning {
			if got.State != core.AutoLoginFailed {
				t.Fatalf("state = %s, want a failed run whose number was released", got.State)
			}
			if !strings.Contains(got.Message, "滑块") {
				t.Fatalf("message = %q, want the captcha failure", got.Message)
			}
			if len(stub.calls) == 0 {
				t.Fatal("the rented number was never released")
			}
			released := false
			for _, q := range stub.calls {
				if q.Get("code") == "release" {
					released = true
				}
			}
			if !released {
				t.Fatal("a failed auto-login must release its rented number")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("auto-login did not finish in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
