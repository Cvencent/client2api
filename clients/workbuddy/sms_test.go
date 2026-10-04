package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// fakeEomsg stands in for https://api.eomsg.com/zc/data.php.  Every answer is
// plain text, exactly like the real platform, so the module's parsing is
// exercised rather than bypassed.
type fakeEomsg struct {
	mu sync.Mutex

	balance string
	// phones is drawn in order for getPhone without a phone argument; the last
	// entry repeats, so a one-entry slice is "always the same number".
	phones []string
	draws  int
	// msgs is phone -> successive getMsg answers; the last repeats.
	msgs   map[string][]string
	polls  map[string]int
	errFor map[string]string // code -> "ERROR …" text
	calls  []url.Values
}

func newFakeEomsg() *fakeEomsg {
	return &fakeEomsg{balance: "28.55", msgs: map[string][]string{}, polls: map[string]int{}, errFor: map[string]string{}}
}

func (f *fakeEomsg) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)

	code := q.Get("code")
	if msg, bad := f.errFor[code]; bad {
		fmt.Fprint(w, "ERROR "+msg)
		return
	}
	switch code {
	case "leftAmount":
		fmt.Fprint(w, f.balance)
	case "getPhone":
		if want := q.Get("phone"); want != "" {
			fmt.Fprint(w, want)
			return
		}
		if len(f.phones) == 0 {
			fmt.Fprint(w, "ERROR 号码池为空")
			return
		}
		i := f.draws
		f.draws++
		if i >= len(f.phones) {
			i = len(f.phones) - 1
		}
		fmt.Fprint(w, f.phones[i])
	case "getMsg":
		phone := q.Get("phone")
		list := f.msgs[phone]
		if len(list) == 0 {
			fmt.Fprint(w, "[尚未收到]")
			return
		}
		i := f.polls[phone]
		f.polls[phone]++
		if i >= len(list) {
			i = len(list) - 1
		}
		fmt.Fprint(w, list[i])
	case "release", "block":
		fmt.Fprint(w, "SUCCESS")
	default:
		fmt.Fprint(w, "ERROR unknown code")
	}
}

func (f *fakeEomsg) callsFor(code string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []url.Values{}
	for _, c := range f.calls {
		if c.Get("code") == code {
			out = append(out, c)
		}
	}
	return out
}

func smsTestClient(t *testing.T, srv *httptest.Server, cfg string) *Client {
	t.Helper()
	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     []byte(cfg),
		HTTPClient: srv.Client(),
	}
	raw, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := raw.(*Client)
	if !ok {
		t.Fatalf("New returned %T", raw)
	}
	return c
}

func smsConfig(srv *httptest.Server, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"sms_token":"tok-abc","sms_base":%q%s}`, srv.URL, extra)
}

func TestExtractSMSCodePrefersTheCodeWord(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"【腾讯科技】验证码123456，5分钟内有效", "123456"},
		{"您的校验码为 8765，请勿泄露", "8765"},
		{"verification code: 4321", "4321"},
		{"动态码 998877 用于登录", "998877"},
		{"random 12345 digits", "12345"},
		{"no digits here", ""},
		{"【腾讯科技】本次登录验证码为 5678。", "5678"},
	}
	for _, tc := range tests {
		if got := extractSMSCode(tc.msg); got != tc.want {
			t.Errorf("extractSMSCode(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestSMSStatusReportsBalanceAndDefaults(t *testing.T) {
	f := newFakeEomsg()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	st := c.SMSStatus(context.Background(), core.SMSOpts{})
	if !st.Configured {
		t.Fatalf("Configured = false, want true (a token is in the config)")
	}
	if st.Balance != "28.55" {
		t.Errorf("Balance = %q, want 28.55", st.Balance)
	}
	if st.Keyword != defaultSMSKeyword {
		t.Errorf("Keyword = %q, want the built-in default", st.Keyword)
	}
	if len(st.Provinces) != len(smsProvinces) {
		t.Errorf("Provinces = %d entries, want the built-in %d", len(st.Provinces), len(smsProvinces))
	}
	if st.Error != "" {
		t.Errorf("Error = %q, want none", st.Error)
	}
}

func TestSMSStatusWithoutATokenNeverCallsThePlatform(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := newFakeEomsg()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, `{"sms_base":"`+srv.URL+`"}`)

	st := c.SMSStatus(context.Background(), core.SMSOpts{})
	if st.Configured {
		t.Fatalf("Configured = true with no token anywhere")
	}
	if len(f.calls) != 0 {
		t.Errorf("the platform was called %d times with no token", len(f.calls))
	}
}

func TestAcquirePhoneSkipsNumbersAlreadyInUse(t *testing.T) {
	f := newFakeEomsg()
	f.phones = []string{"17000000001", "17000000002"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	num, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", []string{"17000000001"})
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "17000000002" {
		t.Errorf("Phone = %q, want the second draw", num.Phone)
	}
	// The rejected number has to go straight back, or the platform bills us for
	// a number we never used.
	released := f.callsFor("release")
	if len(released) != 1 || released[0].Get("phone") != "17000000001" {
		t.Errorf("release calls = %v, want one for 17000000001", released)
	}
}

func TestAcquirePhoneReissuesAKnownNumber(t *testing.T) {
	f := newFakeEomsg()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	num, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "13800000000", nil)
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "13800000000" || !num.Reused {
		t.Errorf("number = %+v, want the requested number flagged as reused", num)
	}
}

func TestPollSMSCodeWaitsThenReturns(t *testing.T) {
	f := newFakeEomsg()
	f.msgs["17000000001"] = []string{"[尚未收到]", "【腾讯科技】验证码246810，5分钟内有效"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	first, err := c.PollSMSCode(context.Background(), core.SMSOpts{}, "17000000001")
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if first.Ready || first.Code != "" {
		t.Errorf("first poll = %+v, want not ready", first)
	}
	second, err := c.PollSMSCode(context.Background(), core.SMSOpts{}, "17000000001")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if !second.Ready || second.Code != "246810" {
		t.Errorf("second poll = %+v, want code 246810", second)
	}
}

func TestPollSMSCodeReportsAnUnparseableMessage(t *testing.T) {
	f := newFakeEomsg()
	f.msgs["17000000001"] = []string{"【腾讯科技】你的账号出现异常登录，请及时处理"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	got, err := c.PollSMSCode(context.Background(), core.SMSOpts{}, "17000000001")
	if err != nil {
		t.Fatalf("PollSMSCode: %v", err)
	}
	if got.Ready || !strings.Contains(got.Raw, "异常登录") {
		t.Errorf("poll = %+v, want not ready with the raw text", got)
	}
}

func TestReleasePhoneCanBlacklist(t *testing.T) {
	f := newFakeEomsg()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	if err := c.ReleasePhone(context.Background(), core.SMSOpts{}, "17000000001", false); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := c.ReleasePhone(context.Background(), core.SMSOpts{}, "17000000002", true); err != nil {
		t.Fatalf("block: %v", err)
	}
	if got := f.callsFor("release"); len(got) != 1 || got[0].Get("phone") != "17000000001" {
		t.Errorf("release calls = %v", got)
	}
	if got := f.callsFor("block"); len(got) != 1 || got[0].Get("phone") != "17000000002" {
		t.Errorf("block calls = %v", got)
	}
}

func TestSMSPlatformErrorsSurfaceWithoutTheToken(t *testing.T) {
	f := newFakeEomsg()
	f.errFor["getPhone"] = "余额不足，请充值"
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	_, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil)
	if err == nil {
		t.Fatal("AcquirePhone succeeded against an ERROR reply")
	}
	if !strings.Contains(err.Error(), "余额不足") {
		t.Errorf("error = %q, want the platform's own text", err)
	}
	if strings.Contains(err.Error(), "tok-abc") {
		t.Errorf("error leaked the token: %q", err)
	}
}

func TestAcquirePhoneNeedsAToken(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := newFakeEomsg()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, `{"sms_base":"`+srv.URL+`"}`)

	if _, err := c.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil); err == nil {
		t.Fatal("AcquirePhone succeeded without a token")
	} else if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to name the missing token", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("the platform was called without a token: %v", f.calls)
	}
}

func TestSMSOptsTokenOverridesTheConfig(t *testing.T) {
	f := newFakeEomsg()
	f.phones = []string{"17000000003"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := smsTestClient(t, srv, smsConfig(srv, ""))

	if _, err := c.AcquirePhone(context.Background(), core.SMSOpts{Token: "pasted-token"}, "", nil); err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	calls := f.callsFor("getPhone")
	if len(calls) != 1 || calls[0].Get("token") != "pasted-token" {
		t.Errorf("token sent = %v, want the pasted override", calls)
	}
}

func TestProvinceRotationNeverRepeatsConsecutively(t *testing.T) {
	c := &Client{}
	pool := []string{"广东", "浙江", "江苏"}
	var last string
	for i := 0; i < 12; i++ {
		got := c.nextProvince(pool)
		if got == "" {
			t.Fatalf("nextProvince returned an empty province on draw %d", i)
		}
		if got == last {
			t.Fatalf("province %q repeated on draw %d", got, i)
		}
		last = got
	}
	if c.nextProvince(nil) != "" {
		t.Errorf("an empty pool should mean 'let the platform choose'")
	}
}

func TestResolveSMSHonoursTheNoneEscapeHatch(t *testing.T) {
	c := &Client{cfg: config{SMSProvinces: []string{"none"}}}
	if got := c.resolveSMS(core.SMSOpts{}).provinces; len(got) != 0 {
		t.Errorf("provinces = %v, want none", got)
	}
}
