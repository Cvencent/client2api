package workbuddy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/browser"
	"client2api/internal/core"
)

// autologin.go runs the vendor's phone-code login end to end, in a browser the
// module drives itself.
//
// It is the panel's version of wb-auto/wb_add_account.py's add_one(): rent a
// number from the SMS platform, open the vendor's authorisation page, pass the
// agreement gate, switch to the phone tab, fill the number, ask for the code,
// read the code back from the platform, submit, then wait for this module's own
// login session to hand over a credential.  The operator clicks one button and
// watches a log.
//
// The selectors and page-text signals below are ported from that script (its
// SEL_* and PAGE_SUCCESS / PAGE_ERROR).  They are the part of the flow that
// drifts when the vendor redesigns its page, so they are kept in one place and
// named after their source.
//
// The flow is split into two halves on purpose: autoLogin is pure
// orchestration over two small interfaces (a page driver and an SMS platform),
// and the browser/platform themselves are behind seams.  That is what lets the
// flow be tested without launching Edge or spending money on a real number.

const (
	// defaultAutoLoginTimeout bounds one whole run.  wb-auto's --timeout was
	// 300s, and the same ceiling fits a browser flow that may wait for an SMS.
	defaultAutoLoginTimeout = 5 * time.Minute
	// defaultSMSPolls is wb-auto's --sms-polls default.
	defaultSMSPolls = 12
	// defaultDupRetries is wb-auto's --dup-retries default: how many extra
	// numbers to draw when the platform keeps handing back one already in use.
	defaultDupRetries = 6
	// autoJobTTL is how long a finished job stays readable, so a poller that was
	// asleep across the transition still sees the outcome.
	autoJobTTL = 30 * time.Minute
	// autoMaxLogLines caps one job's log so a pathological run cannot grow the
	// response without bound.
	autoMaxLogLines = 400
)

// Settle pauses: how long to give the vendor's SPA after each click before
// touching the next control. They are wall-clock only -- the code path, the
// order of the clicks and the selectors are the same at 1ms or 2s -- so the
// test binary shrinks them along with the polling cadences above.
const (
	defaultAgreeGateSettle = 2 * time.Second
	defaultPhoneTabSettle  = time.Second
	// defaultCodeButtonSettle gives the vendor time to actually send the SMS
	// before the page is re-read for an inline error.
	defaultCodeButtonSettle = 2500 * time.Millisecond
)

var (
	agreeGateSettle  = defaultAgreeGateSettle
	phoneTabSettle   = defaultPhoneTabSettle
	codeButtonSettle = defaultCodeButtonSettle
)

// submitReadyTimeout bounds how long submitLogin waits for the vendor's confirm
// control to become clickable after the code is filled.  The control is a
// disabled div until the phone and code are accepted, and clicking that div is
// a silent no-op; waiting for the clickable shape lets the run fail with a real
// reason instead of sitting on "已提交登录…" until the whole-run timeout.  It is
// a var only so the test binary can shrink it.
var submitReadyTimeout = 8 * time.Second

// defaultSMSInterval is wb-auto's --sms-interval default: the pause between two
// checks for the texted code.
const defaultSMSIntervalValue = 5 * time.Second

// defaultSMSInterval is that pause as the poller uses it.  It is a var only so
// the test binary can shrink it: the login tests really do wait out this
// interval between polls of a stub that answers immediately, so the package
// spent 13s of a run proving "the poller retries". Production always runs
// defaultSMSIntervalValue. The poll count (defaultSMSPolls) is deliberately not
// made mutable -- how many times we look is behaviour, how long we wait is not.
var defaultSMSInterval = defaultSMSIntervalValue

// defaultAutoPollInterval is how often this module's own login session is polled
// after the code has been submitted.  The reference panel's frontend polls the
// same way every 3s, so this matches it.
const defaultAutoPollInterval = 3 * time.Second

// autoPollInterval is that cadence as the runners use it.  It is a var only so
// the test binary can shorten it: the login tests spend a real 3s per poll tick
// waiting for a session the stub resolves immediately, which is wall-clock
// spent proving nothing. Production always runs defaultAutoPollInterval.
var autoPollInterval = defaultAutoPollInterval

// autoConfirmHeartbeat is how often confirmAutoLogin reports that it is still
// waiting for the vendor.  Without it the panel's log box sits on the submit
// line and looks frozen while the poll is actually alive.  It is a var only so
// the test binary can shrink it.
var autoConfirmHeartbeat = 15 * time.Second

// The vendor's authorisation page.  Ported from wb_add_account.py's SEL_*.
const (
	selAgreeGate  = "button.agree-btn"
	selPhoneTab   = `div.login-methods, div[class*="login__tab"]`
	selPhoneInput = `input#phoneNumber, input[placeholder="请输入你的手机号"]`
	selCodeBtn    = `input.code-btn, button.code-btn, button.oneid-react-verify-code__code-btn`
	selCodeInput  = `input#code, input[placeholder="请输入验证码"]`
	// selSubmit is the confirm control in its clickable shape.  The vendor
	// renders it as a disabled div until the phone and code are accepted and
	// only then as a button, so the disabled placeholder is excluded: a click
	// on it is a silent no-op that must not be mistaken for a submitted login.
	selSubmit   = `input#kc-login, button#kc-login, button.oneid-react-dialog-confirm-button, div.oneid-react-dialog-confirm-button:not(.oneid-react-is-disabled)`
	selAgreeBox = "input.t-checkbox__former"
	// submitButtonText is the confirm control's label, used as a last-resort
	// text match when the class-based selectors do not fit.
	submitButtonText = "手机号登录"
	// autoAllDocsJS walks the top document and every same-origin iframe.  The
	// vendor's login form is sometimes rendered inside an iframe; page.Click
	// already searches frames, and these page-text helpers must do the same or
	// they silently miss the agreement checkbox and its post-submit modal.
	autoAllDocsJS = `var __autoDocs=[];function __autoCollect(doc){__autoDocs.push(doc);var __fs=doc.querySelectorAll('iframe,frame');for(var __fi=0;__fi<__fs.length;__fi++){try{var __child=__fs[__fi].contentDocument;if(__child){__autoCollect(__child);}}catch(e){}}}__autoCollect(document);`
)

var (
	// pageSuccessTexts mean the vendor has finished the sign-in.  They are an
	// early signal only: the credential still has to arrive through PollLogin.
	pageSuccessTexts = []string{"返回 CLI", "返回CLI", "登录成功", "授权成功", "已成功登录", "回到命令行"}
	// pageErrorTexts mean the attempt is dead; the first hit is reported.  The
	// vendor shows these inline, so waiting for the poll timeout would just
	// delay the operator learning what went wrong.
	pageErrorTexts = []string{
		"验证码错误", "验证码已过期", "验证码无效", "验证码不正确", "验证码已失效",
		"请获取并输入验证码", "参数错误，请重新获取验证码",
		"该手机号已经被其他账号绑定",
		"今日发送次数已达上限", "发送过于频繁", "操作过于频繁",
		"账号存在风险", "存在安全风险", "图形验证码", "滑块校验失败",
	}
)

// pageDriver is the slice of browser.Page this flow needs.  It exists so the
// flow can be exercised without launching a browser.
type pageDriver interface {
	Navigate(ctx context.Context, url string) error
	EvalBool(ctx context.Context, expression string) (bool, error)
	EvalString(ctx context.Context, expression string) (string, error)
	Click(ctx context.Context, selector string) (bool, error)
	Fill(ctx context.Context, selector, value string) (bool, error)
	Exists(ctx context.Context, selector string) (bool, error)
}

// autoBrowser is one launched browser the flow can drive and must close.
type autoBrowser interface {
	Page() pageDriver
	Close()
}

type realBrowser struct{ b *browser.Browser }

func (r realBrowser) Page() pageDriver { return r.b.Page() }
func (r realBrowser) Close()           { r.b.Close() }

// launchAutoBrowser is a seam: production always launches a real browser, the
// tests substitute a fake.
var launchAutoBrowser = func(ctx context.Context, opts browser.LaunchOpts) (autoBrowser, error) {
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return nil, err
	}
	return realBrowser{b}, nil
}

// smsPlatform is the slice of core.SMSProvider the flow needs, so a test can
// run the whole flow without the real platform.
type smsPlatform interface {
	Acquire(ctx context.Context, opts core.SMSOpts, want string, avoid []string) (core.SMSNumber, error)
	Poll(ctx context.Context, opts core.SMSOpts, phone string) (core.SMSCode, error)
	Release(ctx context.Context, opts core.SMSOpts, phone string, block bool) error
}

// clientSMS adapts this module's core.SMSProvider methods to smsPlatform.
type clientSMS struct{ c *Client }

func (s clientSMS) Acquire(ctx context.Context, opts core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	return s.c.AcquirePhone(ctx, opts, want, avoid)
}

func (s clientSMS) Poll(ctx context.Context, opts core.SMSOpts, phone string) (core.SMSCode, error) {
	return s.c.PollSMSCode(ctx, opts, phone)
}

func (s clientSMS) Release(ctx context.Context, opts core.SMSOpts, phone string, block bool) error {
	return s.c.ReleasePhone(ctx, opts, phone, block)
}

// ---------------------------------------------------------------------------
// The job
// ---------------------------------------------------------------------------

// autoJob is one auto-login run.  Its log is append-only and every field is
// read under mu, because the panel polls while the run goroutine writes.
type autoJob struct {
	mu         sync.Mutex
	id         string
	realm      string
	phone      string
	step       string
	state      string
	message    string
	accountID  string
	lines      []core.AutoLogLine
	startedAt  time.Time
	finishedAt time.Time
	expiresAt  time.Time
	cancel     context.CancelFunc
}

func (j *autoJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(fmt.Sprintf(format, args...))
}

func (j *autoJob) appendLocked(text string) {
	if len(j.lines) >= autoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(text),
	})
}

func (j *autoJob) setStep(step string) {
	j.mu.Lock()
	j.step = step
	j.mu.Unlock()
}

func (j *autoJob) setPhone(phone string) {
	j.mu.Lock()
	j.phone = phone
	j.mu.Unlock()
}

// finish records a terminal state.  It is a no-op once the job has already
// finished, which is what makes a cancel that races the run safe: whichever
// side gets there first wins and the other cannot overwrite it.
func (j *autoJob) finish(state, message, accountID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != core.AutoLoginRunning {
		return
	}
	j.state = state
	j.message = core.Redact(message)
	if accountID != "" {
		j.accountID = accountID
	}
	j.finishedAt = time.Now()
	j.expiresAt = j.finishedAt.Add(autoJobTTL)
}

// fail records a terminal failure and mirrors the reason into the log.  Without
// this the operator's log box stops at the last step that ran ("打开授权页…") and
// the reason lives only in the status line, which is exactly the "did it even
// launch the browser?" confusion this exists to end.
func (j *autoJob) fail(reason string) {
	j.logf("失败：%s", reason)
	j.finish(core.AutoLoginFailed, reason, "")
}

func (j *autoJob) snapshot() core.AutoLoginJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	lines := make([]core.AutoLogLine, len(j.lines))
	copy(lines, j.lines)
	out := core.AutoLoginJob{
		ID:        j.id,
		State:     j.state,
		Realm:     j.realm,
		Phone:     j.phone,
		Step:      j.step,
		Log:       lines,
		Message:   j.message,
		AccountID: j.accountID,
	}
	if !j.startedAt.IsZero() {
		out.StartedAt = j.startedAt.Format(time.RFC3339)
	}
	if !j.finishedAt.IsZero() {
		out.FinishedAt = j.finishedAt.Format(time.RFC3339)
	}
	return out
}

func (c *Client) putAutoJob(j *autoJob) {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	if c.autos == nil {
		c.autos = make(map[string]*autoJob)
	}
	now := time.Now()
	for id, other := range c.autos {
		other.mu.Lock()
		expired := other.state != core.AutoLoginRunning && now.After(other.expiresAt)
		other.mu.Unlock()
		if expired {
			delete(c.autos, id)
		}
	}
	c.autos[j.id] = j
}

func (c *Client) autoJobByID(id string) *autoJob {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	return c.autos[id]
}

// ---------------------------------------------------------------------------
// core.AutoLoginProvider
// ---------------------------------------------------------------------------

var _ core.AutoLoginProvider = (*Client)(nil)

// StartAutoLogin validates the prerequisites, records a job and returns
// immediately; the run itself happens in the background.  It fails fast on a
// missing browser or platform credential rather than accepting a job that can
// only fail: the operator should not have to watch a log to learn that.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	opts := smsOptsFrom(req)
	if _, err := c.newSMSClient(c.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if browser.FindBrowser(c.cfg.BrowserPath) == "" {
		return core.AutoLoginJob{}, errors.New("no Chromium-family browser found; install Microsoft Edge or Google Chrome, or set clients.workbuddy.browser_path")
	}
	if c.accountsDir() == "" {
		return core.AutoLoginJob{}, errors.New("no data directory: a credential could not be stored")
	}
	realm := c.loginRealm()
	if strings.TrimSpace(req.Realm) != "" {
		realm = normalizeLoginRealm(req.Realm)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.autoLoginTimeout())
	job := &autoJob{
		id:        newAutoJobID(),
		realm:     realm,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	c.up.log("workbuddy: auto login started realm=%s", realm)
	core.GoSafe("workbuddy auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		c.runAutoLogin(runCtx, job, req, opts)
	})
	return job.snapshot(), nil
}

// PollAutoLogin renders one job.  It never touches the network, so the panel
// can call it as often as it likes.
func (c *Client) PollAutoLogin(_ context.Context, id string) (core.AutoLoginJob, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.AutoLoginJob{}, errors.New("job id is required")
	}
	job := c.autoJobByID(id)
	if job == nil {
		return core.AutoLoginJob{}, fmt.Errorf("auto-login job %q not found", id)
	}
	return job.snapshot(), nil
}

// CancelAutoLogin stops a run.  Cancelling a finished job is not an error: the
// panel may have raced the run to completion, and the outcome is already
// recorded either way.
func (c *Client) CancelAutoLogin(_ context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("job id is required")
	}
	job := c.autoJobByID(id)
	if job == nil {
		return fmt.Errorf("auto-login job %q not found", id)
	}
	job.mu.Lock()
	var cancel context.CancelFunc
	if job.state == core.AutoLoginRunning {
		job.state = core.AutoLoginCancelled
		job.appendLocked("已取消")
		job.message = "已取消"
		job.finishedAt = time.Now()
		job.expiresAt = job.finishedAt.Add(autoJobTTL)
		cancel = job.cancel
	}
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// smsOptsFrom folds a request's platform overrides into the shape the platform
// methods take.
func smsOptsFrom(r core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(r.Token),
		Proxy:    strings.TrimSpace(r.Proxy),
		Keyword:  strings.TrimSpace(r.Keyword),
		Province: strings.TrimSpace(r.Province),
		CardType: strings.TrimSpace(r.CardType),
	}
}

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------

func (c *Client) runAutoLogin(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts) {
	err := c.autoLogin(ctx, job, req, opts, clientSMS{c: c}, launchAutoBrowser)
	switch {
	case err == nil:
		return // autoLogin recorded success
	case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
		job.finish(core.AutoLoginCancelled, "已取消", "")
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded):
		job.fail("超时：整个流程在限定时间内没有完成")
	default:
		job.fail(err.Error())
	}
}

// autoLogin is the whole flow.  It is separated from runAutoLogin so a test can
// hand it a fake browser and a fake platform.
func (c *Client) autoLogin(
	ctx context.Context,
	job *autoJob,
	req core.AutoLoginRequest,
	opts core.SMSOpts,
	platform smsPlatform,
	launch func(context.Context, browser.LaunchOpts) (autoBrowser, error),
) error {
	// 1) Ask the vendor for this run's authorisation URL.
	job.setStep("authorising")
	sess, err := c.startLoginSession(ctx, job.realm)
	if err != nil {
		return fmt.Errorf("发起授权失败：%w", err)
	}
	job.logf("已发起授权（%s），state=%s", job.realm, shortID(sess.sessionID))

	// 2) Start the browser before renting a number, so a browser that will not
	//    come up does not burn a number (wb-auto orders it the same way).
	job.setStep("browser")
	br, err := launch(ctx, browser.LaunchOpts{
		ExecPath:    c.cfg.BrowserPath,
		ProfileRoot: c.browserProfileRoot(),
		Headless:    c.cfg.browserHeadless(),
		StartURL:    "about:blank",
	})
	if err != nil {
		return fmt.Errorf("启动浏览器失败：%w", err)
	}
	defer br.Close()
	page := br.Page()
	// Say which mode the window is in: the operator cannot see a headless
	// browser, so "启动" alone reads as "did it even start?".
	if c.cfg.browserHeadless() {
		job.logf("浏览器已启动（无界面模式，不会弹窗口；要看窗口把 browser_headless 设为 false）")
	} else {
		job.logf("浏览器已启动")
	}

	// 3) Rent a number, skipping the ones the pool already holds.
	job.setStep("phone")
	phone, owned, err := c.rentAutoPhone(ctx, job, req, opts, platform)
	if err != nil {
		return err
	}
	job.setPhone(phone)
	if owned {
		defer func() {
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rcancel()
			if rerr := platform.Release(rctx, opts, phone, false); rerr != nil {
				c.up.log("workbuddy: releasing %s failed: %v", core.MaskSecret(phone), rerr)
			}
		}()
	}

	// 4) Drive the vendor's page.
	job.setStep("login")
	if err := c.openPhoneLogin(ctx, job, page, sess.url, phone); err != nil {
		return err
	}
	if err := c.requestSMSCode(ctx, job, page); err != nil {
		return err
	}
	code, err := c.waitSMSCode(ctx, job, opts, phone, platform)
	if err != nil {
		return err
	}
	job.logf("已收到验证码 %s", code)
	if err := c.submitLogin(ctx, job, page, code); err != nil {
		return err
	}

	// 5) Wait for this module's own session to hand over a credential.
	job.setStep("confirming")
	st, err := c.confirmAutoLogin(ctx, job, page, sess)
	if err != nil {
		return err
	}
	job.finish(core.AutoLoginSuccess, st.Message, st.AccountID)
	if st.AccountID != "" {
		job.logf("登录成功，已添加账号 %s", st.AccountID)
	} else {
		job.logf("登录成功")
	}
	return nil
}

// openPhoneLogin walks the authorisation page to "phone filled, agreement
// ticked", which is exactly where wb-auto's open_phone_login leaves it.
func (c *Client) openPhoneLogin(ctx context.Context, job *autoJob, page pageDriver, authURL, phone string) error {
	job.logf("打开授权页…")
	if err := page.Navigate(ctx, authURL); err != nil {
		return fmt.Errorf("打开授权页失败：%w", err)
	}
	// The agreement gate appears on a first visit and is skipped once accepted.
	if ok, err := clickIfPresent(ctx, page, selAgreeGate); err != nil {
		return fmt.Errorf("协议闸门点击失败：%w", err)
	} else if ok {
		job.logf("已过协议闸门")
		sleepCtx(ctx, agreeGateSettle)
	}
	// Switch to the phone-code tab.  A page that already defaults to it has no
	// such tab, which is not an error.
	if ok, err := waitClickByText(ctx, page, selPhoneTab, "手机号", 15*time.Second); err != nil {
		return fmt.Errorf("切换手机号标签失败：%w", err)
	} else if ok {
		job.logf("已切到手机验证码登录")
		sleepCtx(ctx, phoneTabSettle)
	} else {
		job.logf("页面上没有手机号标签，按默认页继续")
	}
	if !waitSelector(ctx, page, selPhoneInput, 15*time.Second) {
		return fmt.Errorf("找不到手机号输入框，厂商登录页可能已改版（%s）", pageWhere(ctx, page))
	}
	filled, err := page.Fill(ctx, selPhoneInput, phone)
	if err != nil {
		return fmt.Errorf("填写手机号失败：%w", err)
	}
	if !filled {
		return errors.New("手机号输入框填写失败")
	}
	job.logf("已填手机号 %s", phone)
	if ok, err := waitCheckAgreement(ctx, page, 5*time.Second); err != nil {
		return fmt.Errorf("勾选协议失败：%w", err)
	} else if ok {
		job.logf("已勾选协议")
	}
	return nil
}

// requestSMSCode clicks 获取验证码 and reports an inline vendor error right
// away, the same early check wb-auto's request_sms_code does.
func (c *Client) requestSMSCode(ctx context.Context, job *autoJob, page pageDriver) error {
	if !waitSelector(ctx, page, selCodeBtn, 15*time.Second) {
		return fmt.Errorf("找不到「获取验证码」按钮，厂商登录页可能已改版（%s）", pageWhere(ctx, page))
	}
	ok, err := page.Click(ctx, selCodeBtn)
	if err != nil {
		return fmt.Errorf("点击「获取验证码」失败：%w", err)
	}
	if !ok {
		return errors.New("「获取验证码」按钮点击失败")
	}
	job.logf("已点击「获取验证码」")
	sleepCtx(ctx, codeButtonSettle)
	if bad := firstText(pageBody(ctx, page), pageErrorTexts); bad != "" {
		return fmt.Errorf("页面提示：%s", bad)
	}
	return nil
}

// waitSMSCode polls the platform for the code the vendor just texted.
func (c *Client) waitSMSCode(ctx context.Context, job *autoJob, opts core.SMSOpts, phone string, platform smsPlatform) (string, error) {
	polls := c.cfg.smsPolls()
	interval := c.cfg.smsInterval()
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", polls, interval)
	for i := 0; i < polls; i++ {
		code, err := platform.Poll(ctx, opts, phone)
		if err != nil {
			return "", fmt.Errorf("取码失败：%w", err)
		}
		if code.Ready && strings.TrimSpace(code.Code) != "" {
			return strings.TrimSpace(code.Code), nil
		}
		if code.Raw != "" {
			job.logf("收到短信但未解析出验证码：%s", code.Raw)
		}
		if i < polls-1 {
			job.logf("还没收到短信（%d/%d）…", i+1, polls)
			if !sleepCtx(ctx, interval) {
				return "", ctx.Err()
			}
		}
	}
	return "", fmt.Errorf("等了 %d 次仍未收到验证码", polls)
}

// submitLogin fills the code and clicks the confirm control once it is
// clickable, tolerating the shapes it has taken (a styled div while the form is
// valid, or a plain button picked out by its text).
func (c *Client) submitLogin(ctx context.Context, job *autoJob, page pageDriver, code string) error {
	if !waitSelector(ctx, page, selCodeInput, 10*time.Second) {
		return fmt.Errorf("找不到验证码输入框，厂商登录页可能已改版（%s）", pageWhere(ctx, page))
	}
	filled, err := page.Fill(ctx, selCodeInput, code)
	if err != nil {
		return fmt.Errorf("填写验证码失败：%w", err)
	}
	if !filled {
		return errors.New("验证码输入框填写失败")
	}
	job.logf("已填验证码")

	// The confirm control is a disabled div until the phone and code are both
	// accepted, and only then a clickable button.  Wait for that clickable
	// shape so a not-yet-valid form is not clicked into a silent no-op that
	// would be logged as a submitted login and then sit out the whole timeout.
	submitted := false
	if waitSelector(ctx, page, selSubmit, submitReadyTimeout) {
		ok, cerr := page.Click(ctx, selSubmit)
		if cerr != nil {
			return fmt.Errorf("提交登录失败：%w", cerr)
		}
		submitted = ok
	}
	if !submitted {
		// Fall back to the plain-button shape the control has taken before.
		if ok, _ := clickByText(ctx, page, "button", submitButtonText); !ok {
			return errors.New("登录提交按钮在填写验证码后仍不可用（验证码可能未被页面接受或厂商登录页已改版）：" + pageWhere(ctx, page))
		}
	}
	job.logf("已提交登录，等待授权结果…")
	return nil
}

// confirmAutoLogin waits for this module's own login session to produce a
// credential, watching the page for the vendor's inline errors while it waits.
// It logs the page's own success marker and a periodic heartbeat so the panel's
// log does not look frozen during the (possibly minutes-long) vendor wait.
func (c *Client) confirmAutoLogin(ctx context.Context, job *autoJob, page pageDriver, sess *panelLogin) (core.LoginState, error) {
	ticker := time.NewTicker(autoPollInterval)
	defer ticker.Stop()
	started := time.Now()
	lastBeat := started
	successLogged := false
	for {
		// On a fresh profile the vendor can open a "服务条款与隐私协议" modal
		// after the first submit click.  That first click never reaches the
		// code-verification API, so accept the modal and click submit again.
		if ok, err := acceptAgreementDialog(ctx, page); err != nil {
			return core.LoginState{}, fmt.Errorf("处理服务条款弹窗失败：%w", err)
		} else if ok {
			job.logf("已同意服务条款与隐私协议，重新提交登录")
			if clicked, cerr := page.Click(ctx, selSubmit); cerr != nil {
				return core.LoginState{}, fmt.Errorf("重新提交登录失败：%w", cerr)
			} else if !clicked {
				return core.LoginState{}, errors.New("同意服务条款后找不到登录提交按钮")
			}
			job.logf("已提交登录，等待授权结果…")
		}

		body := pageBody(ctx, page)
		if bad := firstText(body, pageErrorTexts); bad != "" {
			return core.LoginState{}, fmt.Errorf("页面提示：%s", bad)
		}
		// The page's own success marker is an early signal only -- the
		// credential still has to arrive through PollLogin -- but surfacing it
		// tells the operator the browser half is done.
		if !successLogged {
			if good := firstText(body, pageSuccessTexts); good != "" {
				job.logf("页面已显示「%s」，等待厂商下发凭证…", good)
				successLogged = true
			}
		}
		st, err := c.PollLogin(ctx, sess.sessionID)
		if err != nil {
			return core.LoginState{}, fmt.Errorf("查询授权结果失败：%w", err)
		}
		switch st.State {
		case core.LoginSuccess:
			return st, nil
		case core.LoginFailed, core.LoginCancelled:
			return core.LoginState{}, fmt.Errorf("登录未完成：%s", st.Message)
		}
		select {
		case <-ctx.Done():
			return core.LoginState{}, ctx.Err()
		case <-ticker.C:
		}
		if autoConfirmHeartbeat <= 0 || time.Since(lastBeat) >= autoConfirmHeartbeat {
			job.logf("仍在等待厂商确认登录…（已等待 %s）", time.Since(started).Round(time.Second))
			lastBeat = time.Now()
		}
	}
}

// rentAutoPhone draws a number, skipping the ones the pool already holds.
// want pins one exact number (the restore path).
func (c *Client) rentAutoPhone(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts, platform smsPlatform) (string, bool, error) {
	if want := strings.TrimSpace(req.Phone); want != "" {
		num, err := platform.Acquire(ctx, opts, want, nil)
		if err != nil {
			return "", false, fmt.Errorf("重新占用 %s 失败：%w", want, err)
		}
		job.logf("已占用号码 %s（指定）", num.Phone)
		return num.Phone, true, nil
	}
	avoid := autoLoginAvoid(req, c.poolPhones())
	job.logf("取号中（避开池内及本轮已试 %d 个号码）…", len(avoid))
	num, err := platform.Acquire(ctx, opts, "", avoid)
	if err != nil {
		return "", false, fmt.Errorf("取号失败：%w", err)
	}
	suffix := ""
	if p := strings.TrimSpace(num.Province); p != "" {
		suffix = "（" + p + "）"
	}
	job.logf("已取号 %s%s", num.Phone, suffix)
	return num.Phone, true, nil
}

// autoLoginAvoid merges the numbers this auto-add batch has already tried
// (req.Avoid, sent by the panel) with the numbers the pool already holds.  Both
// must be skipped: the pool set stops the batch from re-renting an account the
// module already carries, and the batch set stops the platform from handing back
// the number that just failed.  Duplicates are dropped and empty entries are
// ignored, so the module never asks the platform to skip a blank.
func autoLoginAvoid(req core.AutoLoginRequest, pool []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range [][]string{req.Avoid, pool} {
		for _, n := range list {
			n = strings.TrimSpace(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// poolPhones is the set of phone-number accounts the pool already holds.  The
// pool's label for such an account IS the number, which is what makes the
// duplicate guard possible at all.  A module with no pool simply returns none.
func (c *Client) poolPhones() []string {
	c.refreshAccounts(false)
	if c.pool == nil {
		return nil
	}
	var out []string
	for _, a := range c.pool.Accounts() {
		if label := strings.TrimSpace(a.Label()); smsPhoneRe.MatchString(label) {
			out = append(out, label)
		}
	}
	return out
}

// browserProfileRoot is where the throwaway browser profile is created.  It
// lives under the data directory rather than beside the credentials, because
// the credential loader globs <accounts_dir>/*.json and would otherwise try to
// parse the browser's own files.
func (c *Client) browserProfileRoot() string {
	if c.deps.DataDir == "" {
		return filepath.Join("run", "browser")
	}
	return filepath.Join(c.deps.DataDir, "browser")
}

// ---------------------------------------------------------------------------
// Page helpers
// ---------------------------------------------------------------------------

// waitSelector polls until the selector exists.  The vendor's page is an SPA,
// so "the element is not there yet" and "the element will never be there" are
// only told apart by waiting.
func waitSelector(ctx context.Context, page pageDriver, selector string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := page.Exists(ctx, selector)
		if err == nil && ok {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return false
		}
	}
}

// clickIfPresent clicks a selector only when it exists, so an optional control
// (the agreement gate on a repeat visit) is not an error when it is absent.
func clickIfPresent(ctx context.Context, page pageDriver, selector string) (bool, error) {
	ok, err := page.Exists(ctx, selector)
	if err != nil || !ok {
		return false, err
	}
	return page.Click(ctx, selector)
}

// clickByText clicks the visible element matching selector whose text contains
// text, preferring the shortest match.  The vendor wraps the clickable
// 手机号 control in a login-methods-row container which also contains the
// label; clicking the wrapper is a no-op and leaves the QR tab active.
func clickByText(ctx context.Context, page pageDriver, selector, text string) (bool, error) {
	js := `(function(){` + autoAllDocsJS +
		`var __best=null,__bestLen=-1;` +
		`for(var __di=0;__di<__autoDocs.length;__di++){` +
		`var els=__autoDocs[__di].querySelectorAll(` + autoJSString(selector) + `);` +
		`for(var __ei=0;__ei<els.length;__ei++){` +
		`var el=els[__ei];` +
		`var t=(el.textContent||'').replace(/\s+/g,'');` +
		`if(t.indexOf(` + autoJSString(text) + `)<0){continue;}` +
		`var r=el.getBoundingClientRect();` +
		`var s=__autoDocs[__di].defaultView.getComputedStyle(el);` +
		`if(r.width<=0||r.height<=0||s.visibility==='hidden'||s.display==='none'){continue;}` +
		`if(__best===null||t.length<__bestLen){__best=el;__bestLen=t.length;}` +
		`}}if(!__best){return false;}__best.click();return true;})()`
	return page.EvalBool(ctx, js)
}

// waitClickByText waits for an SPA control to render before trying to click it.
// The vendor renders the login-method tabs after hydration; clicking too early
// leaves the QR tab active while the hidden phone form can still accept values,
// which produces a real SMS but no usable submission.
func waitClickByText(ctx context.Context, page pageDriver, selector, text string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := clickByText(ctx, page, selector, text)
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, nil
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return false, ctx.Err()
		}
	}
}

// checkAgreement ticks the vendor's agreement checkbox.  It clicks first and
// falls back to writing the property, because the control is a React-managed
// input and a bare .click() does not always move it.
func checkAgreement(ctx context.Context, page pageDriver) (bool, error) {
	js := `(function(){` + autoAllDocsJS +
		`for(var __di=0;__di<__autoDocs.length;__di++){` +
		`var el=__autoDocs[__di].querySelector(` + autoJSString(selAgreeBox) + `);` +
		`if(!el){continue;}` +
		`if(el.checked){return true;}` +
		`el.click();` +
		`if(!el.checked){el.checked=true;el.dispatchEvent(new Event('change',{bubbles:true}));}` +
		`return true;}return false;})()`
	return page.EvalBool(ctx, js)
}

// waitCheckAgreement tolerates the short window in which the phone form is
// visible but its React-owned checkbox has not mounted yet.
func waitCheckAgreement(ctx context.Context, page pageDriver, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := checkAgreement(ctx, page)
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, nil
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return false, ctx.Err()
		}
	}
}

// acceptAgreementDialog accepts the vendor's current post-submit terms modal.
// The modal is part of a newer login flow: clicking 手机号登录 opens it, and no
// code-verification request is sent until its 同意 button is clicked.  Match
// both the dialog title and the positive button so an unrelated modal cannot be
// accepted by accident.
func acceptAgreementDialog(ctx context.Context, page pageDriver) (bool, error) {
	js := `(function(){` + autoAllDocsJS +
		`for(var __di=0;__di<__autoDocs.length;__di++){` +
		`var hs=__autoDocs[__di].querySelectorAll('.t-dialog__header-content');` +
		`for(var i=0;i<hs.length;i++){` +
		`var h=hs[i];if((h.textContent||'').indexOf('服务条款与隐私协议')<0){continue;}` +
		`var d=h.closest('.t-dialog');if(!d){continue;}` +
		`var bs=d.querySelectorAll('.t-dialog__footer button');` +
		`for(var j=0;j<bs.length;j++){` +
		`if((bs[j].textContent||'').trim().indexOf('同意')===0){bs[j].click();return true;}` +
		`}}}return false;})()`
	return page.EvalBool(ctx, js)
}

// pageBody reads the rendered text.  A page that has gone away reads as empty
// rather than failing the whole run: the poll that follows is the real verdict.
func pageBody(ctx context.Context, page pageDriver) string {
	js := `(function(){function read(doc){` +
		`var out=doc.body?doc.body.innerText:'';` +
		`var frames=doc.querySelectorAll('iframe,frame');` +
		`for(var i=0;i<frames.length;i++){` +
		`try{var child=frames[i].contentDocument;if(child){out+='\n'+read(child);}}catch(e){}` +
		`}` +
		`return out;}return read(document);})()`
	body, err := page.EvalString(ctx, js)
	if err != nil {
		return ""
	}
	return body
}

// pageWhere names the page the driver is on.  The operator watching this log
// cannot see the (headless) window, so a selector failure has to say where it
// was looking: the title and URL are what tell "the page never loaded" apart
// from "the vendor redesigned the page".
func pageWhere(ctx context.Context, page pageDriver) string {
	title, _ := page.EvalString(ctx, "document.title")
	href, _ := page.EvalString(ctx, "location.href")
	title, href = strings.TrimSpace(title), strings.TrimSpace(href)
	switch {
	case title != "" && href != "":
		return fmt.Sprintf("当前页面 %q %s", title, href)
	case href != "":
		return "当前页面 " + href
	case title != "":
		return fmt.Sprintf("当前页面 %q", title)
	default:
		return "页面没有加载出标题或地址"
	}
}

// firstText returns the first needle found in haystack, or "".
func firstText(haystack string, needles []string) string {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return n
		}
	}
	return ""
}

// autoJSString renders s as a JavaScript string literal.
func autoJSString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// shortID keeps the first few characters of an opaque token so a log line can
// name a session without printing the whole thing.
func shortID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

func newAutoJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("auto-%d", time.Now().UnixNano())
	}
	return "auto-" + hex.EncodeToString(b[:])
}
