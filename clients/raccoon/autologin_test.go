package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// autologin_test.go covers the one-click flow without a browser: a fake driver
// stands in for the page against the real loopback login server, and the
// existing eomsgStub hands out a number and a code.

// fakeRaccoonDriver records what the browser step was asked to do and walks
// the loopback page's own HTTP handlers, so the run ends in a stored
// credential exactly as a manual sign-in would.
type fakeRaccoonDriver struct {
	mu       sync.Mutex
	url      string
	phone    string
	codeSeen string
	driveErr error
}

func (d *fakeRaccoonDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	d.mu.Lock()
	d.url = url
	d.phone = phone
	d.mu.Unlock()
	logf("浏览器步骤开始")
	if d.driveErr != nil {
		return d.driveErr
	}
	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.codeSeen = code
	d.mu.Unlock()
	// Walk the two loopback posts the real page would make: send with a
	// captcha token, then verify with the code.  The page URL carries the
	// login path, so the handlers are addressed from its origin.
	base := urlOrigin(url)
	if err := raccoonPost(base+pathSmsSend, fmt.Sprintf(`{"phone":%q,"captchaParam":"traceless"}`, phone)); err != "" {
		return fmt.Errorf("sms send: %s", err)
	}
	if err := raccoonPost(base+pathSmsVerify, fmt.Sprintf(`{"smsCode":%q}`, code)); err != "" {
		return fmt.Errorf("sms verify: %s", err)
	}
	return nil
}

// urlOrigin strips the path from a loopback page URL.
func urlOrigin(pageURL string) string {
	i := strings.Index(pageURL, "://")
	if i < 0 {
		return pageURL
	}
	rest := pageURL[i+3:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return pageURL
	}
	return pageURL[:i+3+j]
}

// raccoonPost is a tiny POST helper; the error string keeps the fake free of
// *testing.T coupling.
func raccoonPost(url, body string) string {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), `"ok":false`) {
		return string(raw)
	}
	return ""
}

func shrinkRaccoonTimings(t *testing.T) {
	t.Helper()
	settle, sendWait := raccoonSendSettle, raccoonSendWait
	raccoonSendSettle, raccoonSendWait = 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		raccoonSendSettle = settle
		raccoonSendWait = sendWait
	})
}

func waitRaccoonJob(t *testing.T, c *Client, id string) core.AutoLoginJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := c.PollAutoLogin(context.Background(), id)
		if err != nil {
			t.Fatalf("PollAutoLogin: %v", err)
		}
		if job.State != core.AutoLoginRunning {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never finished: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newAutoLoginClient wires a client whose SMS platform and upstream both
// point at stubs.  The eomsg stub carries the message the flow waits for.
func newAutoLoginClient(t *testing.T, up *loginUpstream, stub *eomsgStub, msg string) *Client {
	t.Helper()
	stub.msg = msg
	ts := httptest.NewServer(up.handler(t))
	t.Cleanup(ts.Close)
	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"sms_token":"tok-test","sms_base":%q}`, ts.URL, stub.server.URL), nil)
	return c
}

func TestRaccoonAutoLoginEndsInAStoredCredential(t *testing.T) {
	shrinkRaccoonTimings(t)
	up := &loginUpstream{smsLoginToken: "tok-auto", userName: "auto-user"}
	stub := newEomsgStub(t)
	c := newAutoLoginClient(t, up, stub, "【商汤科技】验证码246810，10分钟内有效")
	drv := &fakeRaccoonDriver{}
	c.autoDriver = drv

	job, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	done := waitRaccoonJob(t, c, job.ID)
	if done.State != core.AutoLoginSuccess {
		t.Fatalf("job = %+v, want success", done)
	}
	if done.AccountID == "" {
		t.Fatalf("job = %+v, want the stored account reported", done)
	}
	drv.mu.Lock()
	gotPhone, gotCode := drv.phone, drv.codeSeen
	drv.mu.Unlock()
	if gotPhone != stub.phone {
		t.Fatalf("driver phone = %q, want the rented %q", gotPhone, stub.phone)
	}
	if gotCode != "246810" {
		t.Fatalf("driver code = %q, want the code the platform read", gotCode)
	}

	// The credential the loopback verify stored must be live in the pool.
	if got := c.pool.snapshot(); len(got) == 0 {
		t.Fatal("no credential was stored by the run")
	}
}

func TestRaccoonAutoLoginNeedsABrowser(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "tok")
	c := newTestClient(t, t.TempDir(), `{"sms_token":"tok"}`, nil)
	c.autoDriver = nil
	// The browser check runs before StartLogin; on a host without a
	// Chromium-family browser this fails fast.  FindBrowser is host-dependent,
	// so only assert the run either started or refused with the browser
	// sentence — never a panic.
	job, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err != nil {
		if !strings.Contains(err.Error(), "no Chromium-family browser") {
			t.Fatalf("err = %v, want the browser diagnosis", err)
		}
		return
	}
	t.Cleanup(func() { _ = c.CancelAutoLogin(context.Background(), job.ID) })
}

func TestRaccoonAutoLoginFailsWhenTheDriverFails(t *testing.T) {
	shrinkRaccoonTimings(t)
	up := &loginUpstream{}
	stub := newEomsgStub(t)
	c := newAutoLoginClient(t, up, stub, "")
	drv := &fakeRaccoonDriver{driveErr: fmt.Errorf("page crashed")}
	c.autoDriver = drv

	job, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	done := waitRaccoonJob(t, c, job.ID)
	if done.State != core.AutoLoginFailed {
		t.Fatalf("job = %+v, want a failed run", done)
	}
	if !strings.Contains(done.Message, "page crashed") {
		t.Fatalf("message = %q, want the driver failure carried through", done.Message)
	}
}

func TestRaccoonAutoLoginRequiresADataDir(t *testing.T) {
	var raw json.RawMessage
	cl, err := New(core.Deps{DataDir: "", Config: raw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := cl.(*Client)
	if _, err := c.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin without a data directory should fail fast")
	}
}

func TestRaccoonCancelUnknownJobIsNotAnError(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	if err := c.CancelAutoLogin(context.Background(), "no-such-job"); err != nil {
		t.Fatalf("CancelAutoLogin on an unknown id: %v", err)
	}
	if _, err := c.PollAutoLogin(context.Background(), "no-such-job"); err == nil {
		t.Fatal("PollAutoLogin on an unknown id should fail")
	}
}

// TestRaccoonAutoLoginAvoidsPoolPhones pins the pool-phone exclusion: a number
// an existing account already sits on must reach the rental call's avoid list,
// or the platform hands it back and the run signs into a duplicate account.
func TestRaccoonAutoLoginAvoidsPoolPhones(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	c.pool.put(account{AccessToken: "tok-a", Phone: "13800001111"})
	c.pool.put(account{AccessToken: "tok-b", Phone: "13800002222"})
	got := c.pooledPhones()
	if len(got) != 2 {
		t.Fatalf("pooledPhones = %v, want both pool numbers", got)
	}
	for _, want := range []string{"13800001111", "13800002222"} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("pooledPhones = %v, want %q in the list", got, want)
		}
	}
	// An empty pool yields an empty list: the avoid array stays the caller's.
	empty := newTestClient(t, t.TempDir(), `{}`, nil)
	if got := empty.pooledPhones(); len(got) != 0 {
		t.Fatalf("pooledPhones on an empty pool = %v, want none", got)
	}
}

// --- the page-driving helpers, tested against a fake page -------------------

type fakeRaccoonPage struct {
	mu        sync.Mutex
	fillCalls map[string]string
	clicks    []string
	status    string
	exists    map[string]bool
	href      string
}

func newFakeRaccoonPage() *fakeRaccoonPage {
	return &fakeRaccoonPage{fillCalls: map[string]string{}, exists: map[string]bool{}}
}

func (p *fakeRaccoonPage) Exists(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok, seen := p.exists[selector]; seen {
		return ok, nil
	}
	return true, nil
}

func (p *fakeRaccoonPage) Click(_ context.Context, selector string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clicks = append(p.clicks, selector)
	return true, nil
}

func (p *fakeRaccoonPage) Fill(_ context.Context, selector, value string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fillCalls[selector] = value
	return true, nil
}

func (p *fakeRaccoonPage) EvalBool(_ context.Context, _ string) (bool, error) { return false, nil }

func (p *fakeRaccoonPage) EvalString(_ context.Context, expression string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if expression == raccoonStatusTextExpr {
		return p.status, nil
	}
	return p.href, nil
}

func TestRaccoonRequestSMSTurnsTheStatusLineIntoTheVerdict(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := newFakeRaccoonPage()
	page.status = "发送中…"
	go func() {
		time.Sleep(30 * time.Millisecond)
		page.mu.Lock()
		page.status = "验证码已发送，请查收短信"
		page.mu.Unlock()
	}()
	var logs []string
	if err := requestRaccoonSMS(context.Background(), page, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}); err != nil {
		t.Fatalf("requestRaccoonSMS: %v", err)
	}
	if page.fillCalls[raccoonSelPhone] != "" {
		t.Fatalf("fillCalls = %v, the send step must not fill anything", page.fillCalls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已请求发送验证码") {
		t.Fatalf("logs = %v, want the send reported", logs)
	}
}

func TestRaccoonRequestSMSPassesThroughAVendorRefusal(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := newFakeRaccoonPage()
	page.status = "图形验证码校验失败，请重新完成滑块验证"
	err := requestRaccoonSMS(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("the send reported success though the vendor refused")
	}
	if !strings.Contains(err.Error(), "图形验证码校验失败") {
		t.Fatalf("err = %v, want the vendor's own wording carried through", err)
	}
}

func TestRaccoonLoginOutcomeWaitsForThePageVerdict(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := newFakeRaccoonPage()
	page.status = "登录中…"
	go func() {
		time.Sleep(30 * time.Millisecond)
		page.mu.Lock()
		page.status = "登录成功，可以关闭此窗口了"
		page.mu.Unlock()
	}()
	if err := waitRaccoonLoginOutcome(context.Background(), page, func(string, ...any) {}); err != nil {
		t.Fatalf("waitRaccoonLoginOutcome: %v", err)
	}
}

func TestRaccoonLoginOutcomeReportsAFailure(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := newFakeRaccoonPage()
	page.status = "验证码不正确，请重试"
	err := waitRaccoonLoginOutcome(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("the outcome reported success though the page failed")
	}
	if !strings.Contains(err.Error(), "验证码不正确") {
		t.Fatalf("err = %v, want the page's wording carried through", err)
	}
}
