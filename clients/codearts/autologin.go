package codearts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"client2api/internal/browser"
	"client2api/internal/core"
)

// autologin.go implements the one-click sign-in (core.AutoLoginProvider) for
// the codearts module.  The ordinary flow hands the operator an authorisation
// URL; this drives the same session's page in a browser the module launches:
//
//  1. create the OAuth session (StartLogin: PKCE + a local callback server);
//  2. rent a number from the shared eomsg service;
//  3. open the Huawei sign-in page and choose phone-code login;
//  4. fill the number, request a code and wait for the SMS platform to
//     receive it;
//  5. submit the code, accept the first-login agreement if Huawei puts one
//     up, and keep the page open until the portal redirects to the local
//     callback — the callback server from step 1 redeems the code and stores
//     the credential exactly as it does for a manual sign-in.
//
// A browser driver is injectable so tests never launch a real browser.  The
// selectors are the ones verified against the Huawei UnifiedIDM sign-in page
// in the officeace module, which renders the same family of pages.

const (
	codeartsAutoLoginTimeout = 5 * time.Minute
	codeartsAutoJobTTL       = 30 * time.Minute
	codeartsAutoMaxLogLines  = 80
	codeartsSMSPolls         = 12
	codeartsSMSInterval      = 5 * time.Second
)

// codeartsRedirectWait bounds the window the page is kept open after the
// consent step, so the redirect to the local callback has time to run before
// the browser is torn down.  A var so a test can shrink it.
var codeartsRedirectWait = 45 * time.Second

// The two timings are variables, not constants, so a test can shrink them.
// Production never reassigns them.
var (
	codeartsPhoneSettle  = 1500 * time.Millisecond
	codeartsSendCodeWait = 8 * time.Second
)

// The consent waits: how long the run watches for the first-login agreement
// to render, and how long it gives the confirm click to close the dialog.
// Variables so a test can shrink them the same way.
var (
	codeartsAgreementWait = 12 * time.Second
	codeartsContinueWait  = 10 * time.Second
)

// Selectors from the Huawei UnifiedIDM sign-in page, shared with the
// officeace module's verified set (same page family).
const (
	codeartsSelSMSLogin       = "#smsLogin"
	codeartsSelPhone          = `input[ht="input_changePhoneAccount_phone"]`
	codeartsSelSendCode       = `[ht="click_changePhoneAccount_getAuthcode"]`
	codeartsSelCode           = `input[ht="input_changePhoneAccount_authcode"]`
	codeartsSelSubmit         = `[ht="click_login_phoneSubmit"]`
	codeartsSelAgreementCheck = `[HT="click_reg_select_Agreement"]`
)

// codeartsResendVisibleExpr is true while the "get code" control shows the
// resend countdown Huawei renders after it accepted a send request.
const codeartsResendVisibleExpr = `(function(){` +
	`var e=document.querySelector('[ht="click_changePhoneAccount_getAuthcode"]');` +
	`if(!e){return false;}` +
	`var t=((e.innerText||e.textContent||'')+'').trim();` +
	`return /重新获取|重新发送|重发|秒后/.test(t);})()`

// codeartsAgreementCheckedExpr reads the cas-checkbox aria state, because the
// first-login agreement's checkbox is not an <input>.
const codeartsAgreementCheckedExpr = `(function(){` +
	`var el=document.querySelector('[HT="click_reg_select_Agreement"]');` +
	`return !!(el && el.getAttribute('aria-checked')==='true');})()`

// codeartsAgreementAcceptExpr clicks the dialog's right footer button once it
// is enabled; false while Huawei still blocks an unticked box.
const codeartsAgreementAcceptExpr = `(function(){` +
	`var h=document.querySelector('[HT="agreement_dialog"]');` +
	`if(!h){return false;}` +
	`var root=h.closest('.hwid-dialog-main')||h.parentNode;` +
	`if(!root){return false;}` +
	`var b=root.querySelector('[HT="click_dialog_rightbtn"]');` +
	`if(!b){return false;}` +
	`if(b.querySelector('.hwid-disabled')||b.closest('.hwid-disabled')){return false;}` +
	`b.click();return true;})()`

const codeartsAgreementClosedExpr = `(function(){` +
	`return !document.querySelector('[HT="agreement_dialog"]');})()`

// codeartsAutoPage is the browser surface the driver needs.  Keeping it
// narrow makes the flow testable without launching a browser.
type codeartsAutoPage interface {
	Exists(ctx context.Context, selector string) (bool, error)
	Click(ctx context.Context, selector string) (bool, error)
	Fill(ctx context.Context, selector string, value string) (bool, error)
	EvalBool(ctx context.Context, expression string) (bool, error)
	EvalString(ctx context.Context, expression string) (string, error)
}

// codeartsAutoDriver is the browser half of one auto-login.
type codeartsAutoDriver interface {
	Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error
}

// codeartsAutoJob is one in-flight auto-login.  Every field is read under mu
// because the panel polls while the background run writes.
type codeartsAutoJob struct {
	mu         sync.Mutex
	id         string
	sessionID  string
	url        string
	state      string
	step       string
	phone      string
	message    string
	accountID  string
	lines      []core.AutoLogLine
	startedAt  time.Time
	finishedAt time.Time
	expiresAt  time.Time
	cancel     context.CancelFunc
}

func (j *codeartsAutoJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(fmt.Sprintf(format, args...))
}

func (j *codeartsAutoJob) appendLocked(text string) {
	if len(j.lines) >= codeartsAutoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(text),
	})
}

func (j *codeartsAutoJob) setStep(step string) {
	j.mu.Lock()
	j.step = step
	j.mu.Unlock()
}

func (j *codeartsAutoJob) setPhone(phone string) {
	j.mu.Lock()
	j.phone = phone
	j.mu.Unlock()
}

func (j *codeartsAutoJob) finish(state, message, accountID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != core.AutoLoginRunning {
		return
	}
	j.state = state
	j.message = core.Redact(message)
	j.accountID = core.Redact(accountID)
	j.step = "done"
	j.finishedAt = time.Now()
	j.expiresAt = j.finishedAt.Add(codeartsAutoJobTTL)
	if strings.TrimSpace(message) != "" {
		j.appendLocked(message)
	}
}

func (j *codeartsAutoJob) fail(message string) {
	j.finish(core.AutoLoginFailed, message, "")
}

func (j *codeartsAutoJob) snapshot() core.AutoLoginJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	log := make([]core.AutoLogLine, len(j.lines))
	copy(log, j.lines)
	return core.AutoLoginJob{
		ID:         j.id,
		State:      j.state,
		Phone:      core.Redact(j.phone),
		Step:       j.step,
		Log:        log,
		Message:    core.Redact(j.message),
		AccountID:  core.Redact(j.accountID),
		StartedAt:  j.startedAt.Format(time.RFC3339),
		FinishedAt: j.finishedAt.Format(time.RFC3339),
	}
}

func newCodeartsAutoJobID() string {
	return "ca-auto-" + newUUID()
}

func shortCodeartsID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}

// putAutoJob registers a job and drops expired ones.
func (c *Client) putAutoJob(job *codeartsAutoJob) {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	if c.autoJobs == nil {
		c.autoJobs = map[string]*codeartsAutoJob{}
	}
	now := time.Now()
	for id, other := range c.autoJobs {
		other.mu.Lock()
		stale := !other.expiresAt.IsZero() && now.After(other.expiresAt)
		other.mu.Unlock()
		if stale {
			delete(c.autoJobs, id)
		}
	}
	c.autoJobs[job.id] = job
}

func (c *Client) autoJobByID(id string) *codeartsAutoJob {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	return c.autoJobs[id]
}

var _ core.AutoLoginProvider = (*Client)(nil)

// StartAutoLogin validates the prerequisites and starts the run in the
// background.  StartLogin is called here so the OAuth session and its local
// callback server exist before the browser opens.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	if c == nil {
		return core.AutoLoginJob{}, errors.New("codearts: this platform is not available")
	}
	opts := codeartsSMSOpts(req)
	if _, err := c.newSMSClient(c.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if c.deps.DataDir == "" {
		return core.AutoLoginJob{}, errors.New("codearts: no data directory: a credential could not be stored")
	}
	if c.autoDriver == nil && browser.FindBrowser("") == "" {
		return core.AutoLoginJob{}, errors.New("codearts: no Chromium-family browser found; install Microsoft Edge or Google Chrome")
	}
	login, err := c.StartLogin(ctxOrBackground(ctx))
	if err != nil {
		return core.AutoLoginJob{}, err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), codeartsAutoLoginTimeout)
	job := &codeartsAutoJob{
		id:        newCodeartsAutoJobID(),
		sessionID: login.SessionID,
		url:       login.URL,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	job.logf("CodeArts 自动登录已启动（session=%s）", shortCodeartsID(login.SessionID))
	core.GoSafe("codearts auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		c.runAutoLogin(runCtx, job, req, opts)
	})
	return job.snapshot(), nil
}

// PollAutoLogin reports one run's progress.  A GET never touches the network.
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

// CancelAutoLogin stops a run.  Cancelling an unknown or already finished job
// is not an error.
func (c *Client) CancelAutoLogin(_ context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("job id is required")
	}
	job := c.autoJobByID(id)
	if job == nil {
		return nil
	}
	job.mu.Lock()
	var cancel context.CancelFunc
	if job.state == core.AutoLoginRunning {
		job.state = core.AutoLoginCancelled
		job.message = "已取消"
		job.step = "done"
		job.finishedAt = time.Now()
		job.expiresAt = job.finishedAt.Add(codeartsAutoJobTTL)
		job.appendLocked("已取消")
		cancel = job.cancel
	}
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func codeartsSMSOpts(req core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(req.Token),
		Proxy:    strings.TrimSpace(req.Proxy),
		Keyword:  strings.TrimSpace(req.Keyword),
		Province: strings.TrimSpace(req.Province),
		CardType: strings.TrimSpace(req.CardType),
	}
}

func (c *Client) runAutoLogin(ctx context.Context, job *codeartsAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) {
	err := c.autoLogin(ctx, job, req, opts)
	switch {
	case err == nil:
		return
	case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
		job.finish(core.AutoLoginCancelled, "已取消", "")
	case errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		job.fail("超时：整个流程在限定时间内没有完成")
	default:
		job.fail(err.Error())
	}
}

// pooledPhones lists the phone numbers already bound to pool accounts.  A
// fresh draw avoids them, so the SMS platform cannot hand back a number the
// pool already holds and the auto login would sign out an existing account.
func (c *Client) pooledPhones() []string {
	phones := make([]string, 0)
	for _, e := range c.pool.all() {
		if phone := strings.TrimSpace(e.acct.Phone); phone != "" {
			phones = append(phones, phone)
		}
	}
	return phones
}

// setLoginPhone records the rented number on the in-progress login session so
// the store write puts it on the credential when the sign-in finishes.
func (c *Client) setLoginPhone(id, phone string) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return
	}
	l := c.loginByID(id)
	if l == nil {
		return
	}
	l.mu.Lock()
	l.phone = phone
	l.mu.Unlock()
}

func (c *Client) autoLogin(ctx context.Context, job *codeartsAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	job.setStep("renting")
	avoid := req.Avoid
	if pooled := c.pooledPhones(); len(pooled) > 0 {
		avoid = append(append([]string(nil), avoid...), pooled...)
	}
	num, err := c.AcquirePhone(ctx, opts, strings.TrimSpace(req.Phone), avoid)
	if err != nil {
		return fmt.Errorf("取号失败：%w", err)
	}
	job.setPhone(num.Phone)
	if num.Reused {
		job.logf("已重新占用账号手机号 %s", num.Phone)
	} else {
		job.logf("已取号 %s", num.Phone)
	}
	released := false
	releasePhone := func() {
		if released {
			return
		}
		released = true
		rctx := context.WithoutCancel(ctx)
		if err := c.ReleasePhone(rctx, opts, num.Phone, false); err != nil {
			job.logf("释放号码失败：%v", err)
			return
		}
		job.logf("已释放号码 %s", num.Phone)
	}
	defer releasePhone()

	job.setStep("browser")
	driver := c.autoDriver
	if driver == nil {
		driver = codeartsBrowserDriver{}
	}
	if err := driver.Drive(ctx, job.url, num.Phone, func(waitCtx context.Context) (string, error) {
		job.setStep("sms")
		return c.waitAutoSMSCode(waitCtx, job, opts, num.Phone)
	}, job.logf); err != nil {
		return fmt.Errorf("浏览器登录失败：%w", err)
	}
	c.setLoginPhone(job.sessionID, num.Phone)

	// The browser step returns once the portal has redirected to the local
	// callback; the callback goroutine inside the StartLogin session redeems
	// the code and stores the credential.  This poll just reports the
	// session's own verdict.
	job.setStep("polling")
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := c.PollLogin(ctx, job.sessionID)
		if err != nil {
			return fmt.Errorf("查询登录会话结果失败：%w", err)
		}
		switch st.State {
		case core.LoginSuccess:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "登录成功，凭据已保存"
			}
			releasePhone()
			job.finish(core.AutoLoginSuccess, msg, st.AccountID)
			return nil
		case core.LoginFailed, core.LoginCancelled:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "登录失败"
			}
			return errors.New(msg)
		}
		if time.Now().After(deadline) {
			return errors.New("登录会话没有在限定时间内完成")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) waitAutoSMSCode(ctx context.Context, job *codeartsAutoJob, opts core.SMSOpts, phone string) (string, error) {
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", codeartsSMSPolls, codeartsSMSInterval)
	for i := 0; i < codeartsSMSPolls; i++ {
		res, err := c.PollSMSCode(ctx, opts, phone)
		switch {
		case err != nil:
			job.logf("短信平台报错（%d/%d）：%v", i+1, codeartsSMSPolls, err)
		case res.Ready:
			job.logf("已收到验证码")
			return res.Code, nil
		default:
			job.logf("还没收到短信（%d/%d）…", i+1, codeartsSMSPolls)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codeartsSMSInterval):
		}
	}
	return "", errors.New("等待验证码超时：短信平台一直没收到短信")
}

// codeartsBrowserDriver drives the real Huawei sign-in page with the shared
// CDP wrapper.  It keeps the browser invisible by default.
type codeartsBrowserDriver struct{}

func (codeartsBrowserDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("the authorization URL is empty")
	}
	if waitCode == nil {
		return errors.New("the SMS code reader is missing")
	}
	br, err := browser.Launch(ctx, browser.LaunchOpts{StartURL: url, Headless: true})
	if err != nil {
		return err
	}
	defer br.Close()
	page := br.Page()
	logf("浏览器已启动，正在打开登录页…")

	if ok, err := waitCodeartsSelector(ctx, page, codeartsSelSMSLogin, 30*time.Second); err != nil {
		return fmt.Errorf("等待手机号登录入口失败：%w", err)
	} else if ok {
		clicked, err := page.Click(ctx, codeartsSelSMSLogin)
		if err != nil {
			return fmt.Errorf("切换手机号登录失败：%w", err)
		}
		if clicked {
			logf("已切换到手机号登录")
		}
	}

	if ok, err := waitCodeartsSelector(ctx, page, codeartsSelPhone, 30*time.Second); err != nil {
		return fmt.Errorf("等待手机号输入框失败：%w", err)
	} else if !ok {
		return errors.New("登录页没有出现手机号输入框")
	}
	if ok, err := page.Fill(ctx, codeartsSelPhone, phone); err != nil {
		return fmt.Errorf("填写手机号失败：%w", err)
	} else if !ok {
		return errors.New("找不到手机号输入框")
	}
	logf("已填写手机号")

	if err := requestCode(ctx, page, logf); err != nil {
		return err
	}
	logf("已请求发送验证码")

	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	if ok, err := waitCodeartsSelector(ctx, page, codeartsSelCode, 20*time.Second); err != nil {
		return fmt.Errorf("等待验证码输入框失败：%w", err)
	} else if !ok {
		return errors.New("登录页没有出现验证码输入框")
	}
	if ok, err := page.Fill(ctx, codeartsSelCode, code); err != nil {
		return fmt.Errorf("填写验证码失败：%w", err)
	} else if !ok {
		return errors.New("找不到验证码输入框")
	}
	// Same Angular digest race as the phone field: let the model commit
	// before the submit control is clicked.
	if !sleepCodearts(ctx, codeartsPhoneSettle) {
		return ctx.Err()
	}
	if ok, err := page.Click(ctx, codeartsSelSubmit); err != nil {
		return fmt.Errorf("提交验证码失败：%w", err)
	} else if !ok {
		return errors.New("找不到登录按钮")
	}
	logf("已提交验证码，等待登录结果…")
	if err := acceptCodeartsRegistrationConsent(ctx, page, logf); err != nil {
		return fmt.Errorf("确认用户协议失败：%w", err)
	}
	// The authorization code only reaches the local callback when the page
	// lands on the redirect, so the browser has to stay alive until the
	// redirect has run.
	if err := waitCodeartsRedirect(ctx, page, logf); err != nil {
		return err
	}
	return nil
}

// requestCode clicks Huawei's "get code" control and does not return success
// until the page confirms the request was really accepted.  A bare click is
// not enough: the SPA arms the handler a digest late, so a click issued in
// the same tick is silently dropped.
func requestCode(ctx context.Context, page codeartsAutoPage, logf func(string, ...any)) error {
	if ok, err := waitCodeartsSelector(ctx, page, codeartsSelSendCode, 20*time.Second); err != nil {
		return fmt.Errorf("等待获取验证码按钮失败：%w", err)
	} else if !ok {
		return errors.New("找不到获取验证码按钮")
	}
	if !sleepCodearts(ctx, codeartsPhoneSettle) {
		return ctx.Err()
	}
	for attempt := 1; attempt <= 2; attempt++ {
		ok, err := page.Click(ctx, codeartsSelSendCode)
		if err != nil {
			return fmt.Errorf("点击获取验证码失败：%w", err)
		}
		if !ok {
			return errors.New("找不到获取验证码按钮")
		}
		seen, err := waitCodeartsCondition(ctx, page, codeartsResendVisibleExpr, codeartsSendCodeWait)
		if err != nil {
			return fmt.Errorf("确认验证码请求失败：%w", err)
		}
		if seen {
			return nil
		}
		if attempt == 2 {
			break
		}
		// The click may have been dropped; the retry is free because the
		// control is disabled while the resend countdown is on screen.
	}
	return errors.New("点击后页面始终没有显示重发倒计时，验证码请求可能没有被接受")
}

// acceptCodeartsRegistrationConsent handles the first-login Huawei Cloud
// agreement dialog.  The step is conditional: an existing account goes
// straight to the redirect, so nothing appearing is a successful no-op.
func acceptCodeartsRegistrationConsent(ctx context.Context, page codeartsAutoPage, logf func(string, ...any)) error {
	// The consent dialog may take a moment to render after the submit.
	if ok, err := waitCodeartsSelector(ctx, page, `[HT="agreement_dialog"]`, codeartsAgreementWait); err != nil {
		return err
	} else if !ok {
		return nil
	}
	return confirmCodeartsAgreement(ctx, page, logf)
}

// confirmCodeartsAgreement ticks the dialog's checkbox and confirms it, then
// waits for the dialog to disappear — that wait is what proves the acceptance
// was recorded rather than silently dropped.
func confirmCodeartsAgreement(ctx context.Context, page codeartsAutoPage, logf func(string, ...any)) error {
	logf("检测到用户协议，正在勾选并同意…")

	checked, err := page.EvalBool(ctx, codeartsAgreementCheckedExpr)
	if err != nil {
		return fmt.Errorf("读取协议勾选状态失败：%w", err)
	}
	if !checked {
		clicked, err := page.Click(ctx, codeartsSelAgreementCheck)
		if err != nil {
			return fmt.Errorf("勾选用户协议失败：%w", err)
		}
		if !clicked {
			return errors.New("找不到用户协议勾选框")
		}
		checked, err = waitCodeartsCondition(ctx, page, codeartsAgreementCheckedExpr, codeartsAgreementWait)
		if err != nil {
			return fmt.Errorf("确认协议勾选状态失败：%w", err)
		}
		if !checked {
			return errors.New("勾选用户协议后勾选框仍未选中")
		}
	}

	for attempt := 1; attempt <= 3; attempt++ {
		closed, err := page.EvalBool(ctx, codeartsAgreementClosedExpr)
		if err != nil {
			return fmt.Errorf("读取协议弹窗状态失败：%w", err)
		}
		if closed {
			logf("已同意用户协议，继续登录…")
			return nil
		}
		accepted, err := page.EvalBool(ctx, codeartsAgreementAcceptExpr)
		if err != nil {
			return fmt.Errorf("点击“同意并继续”失败：%w", err)
		}
		if accepted {
			closed, err = waitCodeartsCondition(ctx, page, codeartsAgreementClosedExpr, codeartsContinueWait)
			if err != nil {
				return fmt.Errorf("等待协议弹窗关闭失败：%w", err)
			}
			if closed {
				logf("已同意用户协议，继续登录…")
				return nil
			}
			continue
		}
		if attempt == 3 {
			break
		}
		// Vue re-renders the footer right after the box is ticked; give it a
		// beat before re-issuing the click.
		if !sleepCodearts(ctx, 250*time.Millisecond) {
			return ctx.Err()
		}
	}
	return errors.New("已点击“同意并继续”，但用户协议弹窗仍未关闭")
}

// waitCodeartsRedirect keeps the page open until the portal redirects to the
// local callback, then returns.  The caller owns the browser lifecycle:
// closing it any earlier drops the code the callback server is waiting for.
// A consent dialog that reappears during the window is confirmed again.
func waitCodeartsRedirect(ctx context.Context, page codeartsAutoPage, logf func(string, ...any)) error {
	logf("正在等待跳转登录回调…")
	deadline := time.Now().Add(codeartsRedirectWait)
	for {
		href, err := page.EvalString(ctx, "location.href")
		if err == nil && strings.Contains(href, "127.0.0.1") {
			logf("已跳转登录回调，等待凭据交换…")
			return nil
		}
		if ok, cerr := page.Exists(ctx, `[HT="agreement_dialog"]`); cerr == nil && ok {
			logf("协议弹窗再次出现，重新确认…")
			if err := confirmCodeartsAgreement(ctx, page, logf); err != nil {
				return fmt.Errorf("重新确认用户协议失败：%w", err)
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("读取页面跳转状态失败：%w", err)
			}
			return errors.New("页面没有跳转到登录回调（仍停留在登录流程里）")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func sleepCodearts(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func waitCodeartsCondition(ctx context.Context, page codeartsAutoPage, expression string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := page.EvalBool(ctx, expression)
		if err == nil && ok {
			return true, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return false, lastErr
			}
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitCodeartsSelector(ctx context.Context, page codeartsAutoPage, selector string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := page.Exists(ctx, selector)
		if err == nil && ok {
			return true, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return false, lastErr
			}
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
