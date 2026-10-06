package workbuddy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/browser"
	"client2api/internal/core"
)

// autologin_test.go exercises the browser-driven login without a browser and
// without spending money on a real number: the flow's two outside edges (the
// page and the SMS platform) are fakes, and the vendor's own auth endpoints are
// the package's existing scripted RoundTripper.
//
// What is being pinned here is the ORDER and the SAFETY of the flow -- the
// number is rented only after the browser is up, it is always handed back, the
// duplicate guard sees the pool, and a vendor error stops the run instead of
// waiting out a timeout.

// --- fake page --------------------------------------------------------------

// fakePage stands in for the vendor's authorisation page.  It is deliberately
// not a DOM: it answers the handful of queries the flow makes, which keeps the
// test about the flow rather than about JavaScript.
type fakePage struct {
	mu      sync.Mutex
	exists  map[string]bool
	body    string
	navURL  string
	navErr  error
	filled  map[string]string
	clicked []string
}

func newFakePage() *fakePage {
	return &fakePage{
		exists: map[string]bool{
			selPhoneInput: true,
			selCodeBtn:    true,
			selCodeInput:  true,
			selSubmit:     true,
		},
		body:   "手机号登录",
		filled: map[string]string{},
	}
}

func (p *fakePage) Navigate(_ context.Context, url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.navErr != nil {
		return p.navErr
	}
	p.navURL = url
	return nil
}

func (p *fakePage) Exists(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exists[selector], nil
}

func (p *fakePage) Click(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clicked = append(p.clicked, selector)
	return p.exists[selector], nil
}

func (p *fakePage) Fill(_ context.Context, selector, value string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exists[selector] {
		return false, nil
	}
	p.filled[selector] = value
	return true, nil
}

func (p *fakePage) EvalString(_ context.Context, expression string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(expression, "body.innerText") {
		return p.body, nil
	}
	return "", nil
}

// EvalBool answers the two expression families the flow depends on: the
// click-by-text phone tab and the agreement tick.  The needles are the parts of
// the selector that survive JSON escaping.
func (p *fakePage) EvalBool(_ context.Context, expression string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.Contains(expression, "login__tab"):
		return true, nil
	case strings.Contains(expression, "t-checkbox__former"):
		return true, nil
	default:
		return false, nil
	}
}

func (p *fakePage) setBody(body string) {
	p.mu.Lock()
	p.body = body
	p.mu.Unlock()
}

func (p *fakePage) didClick(selector string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clicked {
		if c == selector {
			return true
		}
	}
	return false
}

// --- fake browser -----------------------------------------------------------

type fakeBrowser struct{ page *fakePage }

func (b fakeBrowser) Page() pageDriver { return b.page }
func (b fakeBrowser) Close()           {}

// --- fake platform ----------------------------------------------------------

type fakePlatform struct {
	mu sync.Mutex

	number     core.SMSNumber
	code       string
	readyAfter int
	polls      int

	acquired []string
	avoided  []string
	released []string
	blocked  []string

	acquireErr error
	pollErr    error
}

func (f *fakePlatform) Acquire(_ context.Context, _ core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return core.SMSNumber{}, f.acquireErr
	}
	f.acquired = append(f.acquired, want)
	f.avoided = append(f.avoided, avoid...)
	if want != "" {
		return core.SMSNumber{Phone: want, Reused: true}, nil
	}
	n := f.number
	if n.Phone == "" {
		n = core.SMSNumber{Phone: "13800000000", Province: "北京"}
	}
	return n, nil
}

func (f *fakePlatform) Poll(_ context.Context, _ core.SMSOpts, _ string) (core.SMSCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pollErr != nil {
		return core.SMSCode{}, f.pollErr
	}
	f.polls++
	if f.polls < f.readyAfter {
		return core.SMSCode{Ready: false}, nil
	}
	if f.code == "" {
		return core.SMSCode{Ready: false}, nil
	}
	return core.SMSCode{Ready: true, Code: f.code, Raw: "验证码 " + f.code}, nil
}

func (f *fakePlatform) Release(_ context.Context, _ core.SMSOpts, phone string, block bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if block {
		f.blocked = append(f.blocked, phone)
		return nil
	}
	f.released = append(f.released, phone)
	return nil
}

func (f *fakePlatform) releasedPhones() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

func (f *fakePlatform) avoidedPhones() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.avoided...)
}

// --- fixtures ---------------------------------------------------------------

// autoLoginClient builds a Client over a scripted vendor with no accounts.
func autoLoginClient(t *testing.T, v *fakeVendor) (*Client, string) {
	t.Helper()
	return loginClient(t, v, nil)
}

// successVendor answers the three plugin-auth calls with a completed login.
func successVendor() *fakeVendor {
	return &fakeVendor{
		stateBody: stateFixture("state-abc", "https://example.test/auth"),
		token:     []vendorResp{{body: tokenFixture("access-token-abcdefgh", "refresh-token-abcdefgh", "copilot.tencent.com", 3600)}},
		account:   vendorResp{body: accountFixture("uid-123456", "ent-1", "13800000000")},
	}
}

func runAutoLogin(t *testing.T, c *Client, page *fakePage, platform *fakePlatform, req core.AutoLoginRequest) (*autoJob, error) {
	t.Helper()
	job := &autoJob{
		id:        "test-job",
		realm:     realmCN,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
	}
	err := c.autoLogin(context.Background(), job, req, smsOptsFrom(req), platform,
		func(context.Context, browser.LaunchOpts) (autoBrowser, error) {
			return fakeBrowser{page: page}, nil
		})
	return job, err
}

// --- tests ------------------------------------------------------------------

func TestAutoLoginHappyPath(t *testing.T) {
	v := successVendor()
	c, dir := autoLoginClient(t, v)
	page := newFakePage()
	platform := &fakePlatform{
		number:     core.SMSNumber{Phone: "13800000000", Province: "北京"},
		code:       "654321",
		readyAfter: 1,
	}

	job, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("autoLogin: %v", err)
	}

	snap := job.snapshot()
	if snap.State != core.AutoLoginSuccess {
		t.Fatalf("state = %q (%s), want success", snap.State, snap.Message)
	}
	if snap.AccountID == "" {
		t.Fatalf("no account id recorded on success")
	}
	if snap.Phone != "13800000000" {
		t.Fatalf("phone = %q", snap.Phone)
	}
	if got := page.filled[selPhoneInput]; got != "13800000000" {
		t.Fatalf("phone input = %q", got)
	}
	if got := page.filled[selCodeInput]; got != "654321" {
		t.Fatalf("code input = %q", got)
	}
	if !page.didClick(selCodeBtn) {
		t.Fatalf("never clicked 获取验证码")
	}
	if !page.didClick(selSubmit) {
		t.Fatalf("never clicked submit")
	}
	if page.navURL != "https://example.test/auth" {
		t.Fatalf("navigated to %q", page.navURL)
	}
	// The number is rented, so it must go back.
	if got := platform.releasedPhones(); len(got) != 1 || got[0] != "13800000000" {
		t.Fatalf("released = %v, want [13800000000]", got)
	}
	// The credential must have landed on disk.
	if creds := credentialFiles(t, dir); len(creds) != 1 {
		t.Fatalf("credential files = %d, want 1", len(creds))
	}
	// The log has to name the steps the operator is watching.
	log := snap.Log
	for _, want := range []string{"发起授权", "取号", "已填手机号", "已点击", "已填验证码"} {
		if !logContains(log, want) {
			t.Fatalf("log is missing %q: %v", want, logTexts(log))
		}
	}
	// The outcome has to be the last line, so the log box the operator watches
	// ends with "did it work?" answered instead of trailing off at a step.
	if last := log[len(log)-1].Text; !strings.Contains(last, "登录成功") {
		t.Fatalf("log does not end with the outcome: %q", last)
	}
}

func logContains(lines []core.AutoLogLine, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l.Text, needle) {
			return true
		}
	}
	return false
}

func logTexts(lines []core.AutoLogLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// TestAutoJobFailLandsInTheLog pins the fix for the "log just stops at 打开授权页…"
// confusion: a terminal failure has to append its reason, not only set the
// status line the operator may never look at.
func TestAutoJobFailLandsInTheLog(t *testing.T) {
	job := &autoJob{id: "job", state: core.AutoLoginRunning, startedAt: time.Now()}
	job.logf("打开授权页…")
	job.fail("找不到手机号输入框，厂商登录页可能已改版")

	snap := job.snapshot()
	if snap.State != core.AutoLoginFailed {
		t.Fatalf("state = %q, want failed", snap.State)
	}
	if snap.Message != "找不到手机号输入框，厂商登录页可能已改版" {
		t.Fatalf("message = %q, want the reason", snap.Message)
	}
	last := snap.Log[len(snap.Log)-1].Text
	if !strings.Contains(last, "失败：") || !strings.Contains(last, "找不到手机号输入框") {
		t.Fatalf("last log line = %q, want the failure reason", last)
	}
}

func TestAutoLoginSkipsPoolPhones(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	// Put a phone-number account in the pool by hand.  The pool's label for
	// such an account IS the number.
	// credJSON pins the nickname to "Tester", and the duplicate guard keys
	// off the nickname, so this credential is written by hand.
	raw := `{
		"auth": {"accessToken": "access-token-abcdefgh", "refreshToken": "refresh-token-abcdefgh",
		"expiresAt": 9999999999, "domain": "copilot.tencent.com", "realm": "cn"},
		"account": {"uid": "uid-existing", "enterpriseId": "ent-1", "nickname": "13800001111"}
	}`
	if err := os.WriteFile(filepath.Join(c.accountsDir(), "existing.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	c.refreshAccounts(true)

	page := newFakePage()
	platform := &fakePlatform{number: core.SMSNumber{Phone: "13800002222"}, code: "111111"}
	if _, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	avoid := platform.avoidedPhones()
	if len(avoid) != 1 || avoid[0] != "13800001111" {
		t.Fatalf("avoid = %v, want the number already in the pool", avoid)
	}
}

func TestAutoLoginReleasesTheNumberOnFailure(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	page := newFakePage()
	// The vendor shows its inline error right after 获取验证码 is clicked.
	page.setBody("验证码错误，请重新输入")
	platform := &fakePlatform{number: core.SMSNumber{Phone: "13800003333"}, code: "222222"}

	job, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{})
	if err == nil {
		t.Fatalf("autoLogin succeeded despite the vendor error")
	}
	if !strings.Contains(err.Error(), "验证码错误") {
		t.Fatalf("error = %v, want the vendor's own text", err)
	}
	// The run is not finished by autoLogin itself; the caller records the
	// outcome.  What matters here is that the number went back.
	if got := platform.releasedPhones(); len(got) != 1 || got[0] != "13800003333" {
		t.Fatalf("released = %v, want [13800003333]", got)
	}
	_ = job
}

func TestAutoLoginWaitsForTheSMS(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	page := newFakePage()
	platform := &fakePlatform{
		number:     core.SMSNumber{Phone: "13800004444"},
		code:       "333333",
		readyAfter: 3, // not there yet, not there yet, then it arrives
	}
	if _, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	if platform.polls != 3 {
		t.Fatalf("polls = %d, want 3", platform.polls)
	}
	if got := page.filled[selCodeInput]; got != "333333" {
		t.Fatalf("code input = %q", got)
	}
}

func TestAutoLoginPinnedPhoneIsReused(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	page := newFakePage()
	platform := &fakePlatform{code: "444444"}

	if _, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{Phone: "13800005555"}); err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	// The pinned number is the one asked for, and no avoid list is needed.
	if got := platform.acquired; len(got) != 1 || got[0] != "13800005555" {
		t.Fatalf("acquired = %v, want the pinned number", got)
	}
	if got := page.filled[selPhoneInput]; got != "13800005555" {
		t.Fatalf("phone input = %q", got)
	}
}

func TestStartAutoLoginFailsFastWithoutAPlatformToken(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	// No sms_token configured and none pasted in.
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatalf("StartAutoLogin accepted a run with no platform credential")
	}
}

func TestAutoLoginCapabilityIsAdvertised(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.AutoLogin {
		t.Fatalf("CapabilitiesOf did not advertise auto_login")
	}
	if !caps.SMS {
		t.Fatalf("CapabilitiesOf did not advertise sms")
	}
}

func TestAutoLoginJobSurvivesPolling(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	page := newFakePage()
	platform := &fakePlatform{number: core.SMSNumber{Phone: "13800006666"}, code: "555555"}

	job, err := runAutoLogin(t, c, page, platform, core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("autoLogin: %v", err)
	}
	c.putAutoJob(job)

	got, err := c.PollAutoLogin(context.Background(), job.id)
	if err != nil {
		t.Fatalf("PollAutoLogin: %v", err)
	}
	if got.State != core.AutoLoginSuccess {
		t.Fatalf("polled state = %q", got.State)
	}
	if len(got.Log) == 0 {
		t.Fatalf("polled log is empty")
	}
	// A poll must not mutate what it returns.
	got.Log[0].Text = "mutated"
	again, _ := c.PollAutoLogin(context.Background(), job.id)
	if again.Log[0].Text == "mutated" {
		t.Fatalf("PollAutoLogin handed out its internal slice")
	}
}

func TestCancelAutoLoginStopsARunningJob(t *testing.T) {
	v := successVendor()
	c, _ := autoLoginClient(t, v)
	job := &autoJob{id: "cancel-me", state: core.AutoLoginRunning, step: "login"}
	c.putAutoJob(job)

	if err := c.CancelAutoLogin(context.Background(), job.id); err != nil {
		t.Fatalf("CancelAutoLogin: %v", err)
	}
	got, _ := c.PollAutoLogin(context.Background(), job.id)
	if got.State != core.AutoLoginCancelled {
		t.Fatalf("state = %q, want cancelled", got.State)
	}
	// A late finish from the run goroutine must not overwrite the cancel.
	job.finish(core.AutoLoginSuccess, "too late", "uid-x")
	got, _ = c.PollAutoLogin(context.Background(), job.id)
	if got.State != core.AutoLoginCancelled {
		t.Fatalf("a late finish overwrote the cancel: %q", got.State)
	}
}
