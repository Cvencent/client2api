package autoclaw

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

// autoLoginVendor fakes both halves of the automatic flow -- the eomsg
// one-time-SMS platform and AutoClaw's identity gateway -- so one test can run
// it end to end without touching the network or spending a real number.
type autoLoginVendor struct {
	mu sync.Mutex

	phone      string
	phoneQueue []string
	code       string
	// emptyPolls is how many getMsg calls report "nothing yet" before the SMS
	// is delivered; 0 means the message is already waiting.
	emptyPolls int
	delivered  bool

	sendPhone     string
	redeemed      string
	redeemedCodes []string
	loginCalls    int
	loginFailures int
	released      []string
	keywords      []string
	cardTypes     []string
	userInfos     int
}

func newAutoLoginVendor() *autoLoginVendor {
	return &autoLoginVendor{phone: "13800000000", code: "654321"}
}

func (f *autoLoginVendor) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zc/data.php":
			f.platform(w, r)
		case pathAgentSendCode:
			var body struct {
				Phone string `json:"phone"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.sendPhone = body.Phone
			f.mu.Unlock()
			fmt.Fprint(w, `{"code":0,"data":{"result":true}}`)
		case pathAgentLogin:
			var body struct {
				Phone string `json:"phone"`
				Code  int    `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			code := fmt.Sprintf("%06d", body.Code)
			f.mu.Lock()
			f.redeemed = code
			f.redeemedCodes = append(f.redeemedCodes, code)
			f.loginCalls++
			fail := f.loginCalls <= f.loginFailures
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"code":0,"message":"temporary identity gateway failure"}`)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"user_id":"u-1","access_token":"access-1","refresh_token":"refresh-1","user_name":"tester","user_phone":"`+body.Phone+`"}}`)
		case pathUserInfo:
			f.mu.Lock()
			f.userInfos++
			f.mu.Unlock()
			fmt.Fprint(w, `{"code":0,"data":{"user_id":"u-1","user_name":"tester","numeric_user_id":42}}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *autoLoginVendor) platform(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("code") {
	case "leftAmount":
		fmt.Fprint(w, "28.55")
	case "getPhone":
		f.mu.Lock()
		f.keywords = append(f.keywords, q.Get("keyWord"))
		f.cardTypes = append(f.cardTypes, q.Get("cardType"))
		queued := ""
		if len(f.phoneQueue) > 0 {
			queued = f.phoneQueue[0]
			f.phoneQueue = f.phoneQueue[1:]
		}
		f.mu.Unlock()
		if queued != "" {
			fmt.Fprint(w, queued)
			return
		}
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
		switch {
		case f.delivered:
			msg = "【智谱华章】验证码 " + f.code + "，5分钟内有效。"
		case f.emptyPolls > 0:
			f.emptyPolls--
		default:
			f.delivered = true
			msg = "【智谱华章】验证码 " + f.code + "，5分钟内有效。"
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
}

// newAutoLoginTestClient builds a Client whose vendor calls and platform calls
// both go to one test server, with every environment override cleared so a
// developer's own shell cannot change what the test observes.
func newAutoLoginTestClient(t *testing.T, dir, cfgJSON string, rt http.RoundTripper) *Client {
	t.Helper()
	for _, name := range []string{
		envAccessToken, envRefreshToken, envUserID, envNickname, envPhone,
		envDeviceID, envAccountID, envSMSToken, envSMSKeyword, envSMSBase,
		"EOMSG_TOKEN",
	} {
		t.Setenv(name, "")
	}
	// The login handshake persists a device id; point it at a scratch dir so a
	// test never touches the operator's real AutoClaw desktop data.
	t.Setenv(envUserData, t.TempDir())
	deps := core.Deps{DataDir: dir, Config: json.RawMessage(cfgJSON)}
	if rt != nil {
		deps.HTTPClient = &http.Client{Transport: rt}
	}
	built, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}
	return c
}

// autoLoginConfig points both halves of the flow at one test server.
func autoLoginConfig(srv *httptest.Server) string {
	return `{"base_url":"` + srv.URL + `",` +
		`"sms_token":"platform-token",` +
		`"sms_base":"` + srv.URL + `/zc/data.php",` +
		`"sms_interval_seconds":1,"sms_polls":3}`
}

func TestAutoLoginRentsANumberReadsTheCodeAndStoresTheCredential(t *testing.T) {
	vendor := newAutoLoginVendor()
	vendor.emptyPolls = 1 // the first poll finds nothing; the loop must wait.
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newAutoLoginTestClient(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	job := &autoJob{state: core.AutoLoginRunning}

	if err := c.autoLogin(context.Background(), job, core.AutoLoginRequest{}, core.SMSOpts{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	snap := job.snapshot()
	if snap.State != core.AutoLoginSuccess {
		t.Fatalf("job state = %q, want success", snap.State)
	}
	if snap.Phone != vendor.phone {
		t.Errorf("job phone = %q, want the raw rented number so a batch can avoid it", snap.Phone)
	}

	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	if vendor.sendPhone != vendor.phone {
		t.Errorf("send-code phone = %q, want %q", vendor.sendPhone, vendor.phone)
	}
	if vendor.redeemed != vendor.code {
		t.Errorf("redeemed code = %q, want %q", vendor.redeemed, vendor.code)
	}
	if len(vendor.released) != 1 || vendor.released[0] != vendor.phone {
		t.Errorf("released = %v, want the rented number handed back", vendor.released)
	}
	if len(vendor.keywords) == 0 || vendor.keywords[0] != defaultSMSKeyword {
		t.Errorf("platform keyword = %v, want %q", vendor.keywords, defaultSMSKeyword)
	}
	if len(vendor.cardTypes) == 0 || vendor.cardTypes[0] != defaultSMSCardType {
		t.Errorf("platform cardType = %v, want %q", vendor.cardTypes, defaultSMSCardType)
	}
	if vendor.userInfos == 0 {
		t.Error("the freshly minted credential was never enriched through user-info")
	}

	accounts := c.pool.all()
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d, want 1", len(accounts))
	}
	acc := accounts[0].acct
	if acc.Phone != vendor.phone {
		t.Errorf("stored phone = %q, want %q", acc.Phone, vendor.phone)
	}
	if acc.AccessToken != "access-1" || acc.RefreshToken != "refresh-1" {
		t.Errorf("stored credential = %+v, want the tokens the gateway issued", acc.credential)
	}
	if acc.Nickname != "tester" || acc.NumericUserID != "42" {
		t.Errorf("profile fields = %q / %q, want the user-info values", acc.Nickname, acc.NumericUserID)
	}
	if snap.AccountID != acc.id() {
		t.Errorf("job account = %q, want %q", snap.AccountID, acc.id())
	}
}

// TestAutoLoginRetriesTheSameCodeBeforeFailingTheNumber: a transient
// identity-gateway refusal must not discard a code the SMS platform has already
// delivered. The automatic flow gets one short retry with the same phone and
// code; only if that also fails may the panel move on to another number.
func TestAutoLoginRetriesTheSameCodeBeforeFailingTheNumber(t *testing.T) {
	vendor := newAutoLoginVendor()
	vendor.loginFailures = 1
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newAutoLoginTestClient(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	job := &autoJob{state: core.AutoLoginRunning}

	if err := c.autoLogin(context.Background(), job, core.AutoLoginRequest{}, core.SMSOpts{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	if got := job.snapshot().State; got != core.AutoLoginSuccess {
		t.Fatalf("job state = %q, want success after the retry", got)
	}

	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	if vendor.loginCalls != 2 {
		t.Fatalf("login calls = %d, want 2 (initial attempt plus one retry)", vendor.loginCalls)
	}
	if len(vendor.redeemedCodes) != 2 || vendor.redeemedCodes[0] != vendor.code || vendor.redeemedCodes[1] != vendor.code {
		t.Fatalf("redeemed codes = %v, want the same delivered code %q on both attempts", vendor.redeemedCodes, vendor.code)
	}
	accounts := c.pool.all()
	if len(accounts) != 1 || accounts[0].acct.Phone != vendor.phone {
		t.Fatalf("stored accounts = %+v, want the original number to remain in use", accounts)
	}
}

func TestAutoLoginPinsTheAccountNumberOnARelogin(t *testing.T) {
	vendor := newAutoLoginVendor()
	vendor.phone = "13900000000"
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newAutoLoginTestClient(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
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
	accounts := c.pool.all()
	if len(accounts) != 1 || accounts[0].acct.Phone != vendor.phone {
		t.Fatalf("stored accounts = %+v, want the pinned number", accounts)
	}
}

// TestAutoLoginSkipsNumbersTheBatchAlreadyTried: 一个批次里刚失败过的号码必须
// 进入下一轮的避让表，否则平台把同一个号发回来，批次就会一直原地空转。
// 这里平台先发被避让的号，再发可用的号，流程必须只停在后者上。
func TestAutoLoginSkipsNumbersTheBatchAlreadyTried(t *testing.T) {
	vendor := newAutoLoginVendor()
	vendor.phone = "13800000001"
	vendor.phoneQueue = []string{"13800000000", vendor.phone}
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newAutoLoginTestClient(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	job := &autoJob{state: core.AutoLoginRunning}

	req := core.AutoLoginRequest{Avoid: []string{"13800000000"}}
	if err := c.autoLogin(context.Background(), job, req, core.SMSOpts{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	if got := job.snapshot().State; got != core.AutoLoginSuccess {
		t.Fatalf("job state = %q, want success", got)
	}

	accounts := c.pool.all()
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d, want 1", len(accounts))
	}
	if got := accounts[0].acct.Phone; got != vendor.phone {
		t.Errorf("stored phone = %q, want the avoided number skipped and %q used", got, vendor.phone)
	}

	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	// 被避让的那个号要直接还给平台，不能占着不用还得计费。
	if len(vendor.released) == 0 || vendor.released[0] != "13800000000" {
		t.Errorf("released = %v, want the avoided number handed straight back first", vendor.released)
	}
}

func TestStartAutoLoginRefusesWithoutAPlatformToken(t *testing.T) {
	c := newAutoLoginTestClient(t, t.TempDir(), `{}`, nil)
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin accepted a run with no SMS platform token")
	}
}

func TestSMSStatusAnswersNotConfiguredWithoutAToken(t *testing.T) {
	c := newAutoLoginTestClient(t, t.TempDir(), `{}`, nil)
	st := c.SMSStatus(context.Background(), core.SMSOpts{})
	if st.Configured {
		t.Fatal("SMSStatus reported configured with no token")
	}
	if st.Provider != "eomsg" {
		t.Errorf("provider = %q, want eomsg", st.Provider)
	}
	if st.Keyword != defaultSMSKeyword {
		t.Errorf("keyword = %q, want %q", st.Keyword, defaultSMSKeyword)
	}
	if len(st.Provinces) == 0 {
		t.Error("provinces = empty, want the built-in rotation pool")
	}
	if len(st.CardTypes) == 0 {
		t.Error("card types = empty, want the platform's card classes")
	}
}

func TestSMSOptionsOverrideTheConfig(t *testing.T) {
	c := &Client{cfg: config{
		SMSToken:    "file-token",
		SMSKeyword:  "file-keyword",
		SMSBase:     "https://example.test/zc/data.php",
		SMSCardType: "实卡",
	}}
	r := c.resolveSMS(core.SMSOpts{
		Token:    "panel-token",
		Keyword:  "panel-keyword",
		CardType: "虚卡",
		Proxy:    "socks5://127.0.0.1:1080",
	})
	if r.token != "panel-token" {
		t.Errorf("token = %q, want the per-call override", r.token)
	}
	if r.keyword != "panel-keyword" {
		t.Errorf("keyword = %q, want the per-call override", r.keyword)
	}
	if r.cardType != "虚卡" {
		t.Errorf("cardType = %q, want the per-call override", r.cardType)
	}
	if r.proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q, want the per-call override", r.proxy)
	}
	if r.base != "https://example.test/zc/data.php" {
		t.Errorf("base = %q, want the configured endpoint", r.base)
	}
	if len(r.provinces) == 0 {
		t.Error("provinces = empty, want the built-in rotation pool")
	}
}

func TestSMSPlatformTokenFallsBackToEOMSGToken(t *testing.T) {
	t.Setenv(envSMSToken, "")
	t.Setenv(envSMSKeyword, "")
	t.Setenv(envSMSBase, "")
	t.Setenv("EOMSG_TOKEN", "shared-token")
	cfg := config{}
	applyEnv(&cfg)
	if cfg.SMSToken != "shared-token" {
		t.Fatalf("SMSToken = %q, want the shared EOMSG_TOKEN fallback", cfg.SMSToken)
	}
}

func TestAutoLoginRequestCarriesTheProxyIntoSMSPtions(t *testing.T) {
	got := smsOptsFrom(core.AutoLoginRequest{Proxy: "http://127.0.0.1:8080"})
	if got.Proxy != "http://127.0.0.1:8080" {
		t.Fatalf("proxy = %q, want the auto-login request's proxy", got.Proxy)
	}
}

func TestAutoLoginTimeoutFallsBackToTheDefault(t *testing.T) {
	var cfg config
	if got := cfg.autoLoginTimeout(); got != defaultAutoLoginTimeout {
		t.Errorf("autoLoginTimeout = %v, want %v", got, defaultAutoLoginTimeout)
	}
	if got := cfg.smsPolls(); got != defaultSMSPolls {
		t.Errorf("smsPolls = %d, want %d", got, defaultSMSPolls)
	}
	if got := cfg.smsInterval(); got != defaultSMSInterval {
		t.Errorf("smsInterval = %v, want %v", got, defaultSMSInterval)
	}
	if got := cfg.dupRetries(); got != defaultDupRetries {
		t.Errorf("dupRetries = %d, want %d", got, defaultDupRetries)
	}
	if !strings.EqualFold(defaultSMSKeyword, "智谱华章") {
		t.Errorf("defaultSMSKeyword = %q, want 智谱华章", defaultSMSKeyword)
	}
}
