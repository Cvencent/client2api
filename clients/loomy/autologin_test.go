package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// fakeVendor serves both halves of the auto-login flow -- the one-time-SMS
// platform and Loomy's CAccount endpoints -- so one test can run it end to end
// without touching the network or spending a real number.
type fakeVendor struct {
	mu sync.Mutex

	phone        string
	code         string
	msgDelivered bool
	codeAsked    bool
	redeemed     string
	released     []string
	created      int
}

func newFakeVendor() *fakeVendor {
	return &fakeVendor{phone: "13800000000", code: "654321"}
}

func (f *fakeVendor) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zc/data.php":
			q := r.URL.Query()
			switch q.Get("code") {
			case "leftAmount":
				fmt.Fprint(w, "28.55")
			case "getPhone":
				if want := q.Get("phone"); want != "" {
					fmt.Fprint(w, want)
					return
				}
				f.mu.Lock()
				phone := f.phone
				f.mu.Unlock()
				fmt.Fprint(w, phone)
			case "getMsg":
				f.mu.Lock()
				msg := ""
				if f.msgDelivered {
					msg = "【讯飞】验证码 " + f.code + "，5分钟内有效。"
				} else {
					f.msgDelivered = true
				}
				f.mu.Unlock()
				if msg == "" {
					fmt.Fprint(w, "[尚未收到]")
					return
				}
				fmt.Fprint(w, msg)
			case "release", "block":
				f.mu.Lock()
				f.released = append(f.released, q.Get("phone"))
				f.mu.Unlock()
				fmt.Fprint(w, "SUCCESS")
			default:
				fmt.Fprint(w, "ERROR unknown request")
			}
		case r.URL.Path == "/login/phone/sendMsgCode":
			f.mu.Lock()
			f.codeAsked = true
			f.mu.Unlock()
			fmt.Fprint(w, okEnvelope(`{"msgid":"m-1"}`))
		case r.URL.Path == "/login/phone/checkCode":
			var body struct {
				Param struct {
					MCode string `json:"mcode"`
				} `json:"param"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.redeemed = body.Param.MCode
			f.mu.Unlock()
			fmt.Fprint(w, okEnvelope(`{"session":"`+testToken+`","userid":"`+testUserID+`"}`))
		default:
			http.NotFound(w, r)
		}
	}
}

// autoLoginConfig points both halves of the flow at one test server.
func autoLoginConfig(srv *httptest.Server) string {
	return `{"account_base":"` + srv.URL + `",` +
		`"sms_token":"platform-token",` +
		`"sms_base":"` + srv.URL + `/zc/data.php",` +
		`"sms_interval_seconds":1,"sms_polls":3}`
}

func TestAutoLoginRentsANumberReadsTheCodeAndStoresTheSession(t *testing.T) {
	vendor := newFakeVendor()
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	job := &autoJob{state: core.AutoLoginRunning}

	if err := c.autoLogin(context.Background(), job, core.AutoLoginRequest{}, core.SMSOpts{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	if got := job.snapshot().State; got != core.AutoLoginSuccess {
		t.Fatalf("job state = %q, want success", got)
	}

	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	if !vendor.codeAsked {
		t.Error("the vendor was never asked to text a code")
	}
	if vendor.redeemed != vendor.code {
		t.Errorf("redeemed code = %q, want %q", vendor.redeemed, vendor.code)
	}

	accounts := c.store.snapshot()
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d, want 1", len(accounts))
	}
	acc := accounts[0]
	if acc.Phone != vendor.phone {
		t.Errorf("stored phone = %q, want %q", acc.Phone, vendor.phone)
	}
	if acc.Label != vendor.phone {
		t.Errorf("label = %q, want the phone so the row can be re-logged-in", acc.Label)
	}
	if acc.AccessToken != testToken || acc.UserID != testUserID {
		t.Errorf("stored credential = %+v, want the session the vendor issued", acc.storedAccount)
	}
	// A freshly rented number is handed back when the run ends.
	if len(vendor.released) != 1 || vendor.released[0] != vendor.phone {
		t.Errorf("released = %v, want the rented number handed back", vendor.released)
	}
}

func TestAutoLoginPinsTheAccountNumberOnARelogin(t *testing.T) {
	vendor := newFakeVendor()
	vendor.phone = "13900000000"
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	job := &autoJob{state: core.AutoLoginRunning}

	req := core.AutoLoginRequest{Phone: vendor.phone}
	if err := c.autoLogin(context.Background(), job, req, core.SMSOpts{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	if got := job.snapshot().State; got != core.AutoLoginSuccess {
		t.Fatalf("job state = %q, want success", got)
	}

	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	// The account's own number is not ours to release, so nothing may be handed
	// back to the platform.
	if len(vendor.released) != 0 {
		t.Errorf("released = %v, want the account's own number left alone", vendor.released)
	}
	accounts := c.store.snapshot()
	if len(accounts) != 1 || accounts[0].Phone != vendor.phone {
		t.Fatalf("stored accounts = %+v, want the pinned number", accounts)
	}
}

func TestStartAutoLoginRefusesWithoutAPlatformToken(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin accepted a run with no SMS platform token")
	}
}

func TestSMSStatusAnswersNotConfiguredWithoutAToken(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	st := c.SMSStatus(context.Background(), core.SMSOpts{})
	if st.Configured {
		t.Fatal("SMSStatus reported configured with no token")
	}
	if st.Provider != "eomsg" {
		t.Errorf("provider = %q, want eomsg", st.Provider)
	}
	if !strings.EqualFold(st.Keyword, defaultSMSKeyword) {
		t.Errorf("keyword = %q, want %q", st.Keyword, defaultSMSKeyword)
	}
	if len(st.Provinces) == 0 {
		t.Error("provinces = empty, want the built-in rotation pool")
	}
}
