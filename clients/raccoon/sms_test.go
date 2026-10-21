package raccoon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
