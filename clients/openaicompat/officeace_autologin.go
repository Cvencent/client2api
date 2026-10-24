package openaicompat

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

// officeace_autologin.go implements the one-click Huawei account flow.  The
// existing OfficeAce login already knows how to turn a Huawei authorization
// code into a model-gateway credential; this file only automates the browser
// step between StartLogin and PollLogin:
//
//  1. create a Huawei authorization session;
//  2. rent a number from eomsg;
//  3. open the Huawei login page and choose phone-code login;
//  4. fill the number, request a code and wait for the SMS platform to receive
//     it;
//  5. submit the code and let the existing PollLogin exchange the authorization
//     code for credentials.
//
// A browser driver is injectable so tests never launch a real browser.  The
// default driver uses the same Chromium CDP wrapper as the other modules.

const (
	officeAceAutoLoginTimeout = 3 * time.Minute
	officeAceSMSPolls         = 12
	officeAceSMSInterval      = 5 * time.Second
	officeAceAutoJobTTL       = 30 * time.Minute
	officeAceAutoMaxLogLines  = 80
)

// Selectors verified against the Huawei account page used by OfficeAce.
const (
	officeAceSelSMSLogin = "#smsLogin"
	officeAceSelPhone    = `input[ht="input_changePhoneAccount_phone"]`
	officeAceSelSendCode = `[ht="click_changePhoneAccount_getAuthcode"]`
	officeAceSelCode     = `input[ht="input_changePhoneAccount_authcode"]`
	officeAceSelSubmit   = `[ht="click_login_phoneSubmit"]`
	// A fresh Huawei account lands on the OfficeAce/AgentArts registration
	// consent page after the SMS code is accepted.  The page stays there until
	// this checkbox is selected and the primary action is clicked; otherwise
	// the OAuth callback never runs and auto-login eventually times out.
	officeAceSelRegisterAgree    = `input.ti-checkbox-input`
	officeAceSelRegisterContinue = `#submitBtn_reg_open`
)

// Huawei's sign-in page is an Angular SPA, and its phone-code control is a DIV
// wired up by the framework rather than by the markup.  Fill() dispatches
// input/change synchronously, so a click issued in the same tick lands before
// the next digest: the handler is not armed yet, the click is silently
// dropped, and the page fires no SMS request at all.  The operator then watches
// the code reader poll a number that was never texted.
//
// officeAcePhoneSettle lets that digest run before the click is issued.
// officeAceResendVisibleExpr is the acknowledgement Huawei renders only once a
// send request was really accepted ("重新获取 (NN)"), which the control's own
// disabled/enabled class cannot tell us.  A click that produced neither is
// retried; the retry is free because the control is disabled while the resend
// countdown is on screen.
const officeAceSendCodeAttempts = 2

// The two timings are variables, not constants, so a test can shrink them the
// same way the WorkBuddy settle knobs do.  Production never reassigns them.
var (
	officeAcePhoneSettle  = 1500 * time.Millisecond
	officeAceSendCodeWait = 8 * time.Second
)

// officeAceResendVisibleExpr is true while the "get code" control shows the
// resend countdown Huawei renders after it accepted a send request.
const officeAceResendVisibleExpr = `(function(){` +
	`var e=document.querySelector('[ht="click_changePhoneAccount_getAuthcode"]');` +
	`if(!e){return false;}` +
	`var t=((e.innerText||e.textContent||'')+'').trim();` +
	`return /重新获取|重新发送|重发|秒后/.test(t);})()`

// officeAceAutoDriver is the browser half of one auto-login.  Drive must
// return after submitting the phone code; the caller then polls the existing
// OfficeAce login provider until the cloud has the authorization result.
type officeAceAutoDriver interface {
	Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error
}

// officeAceAutoPage is the browser surface needed after the phone-code
// submission. Keeping it narrow makes the conditional registration-consent
// path testable without launching a browser.
type officeAceAutoPage interface {
	Exists(ctx context.Context, selector string) (bool, error)
	Click(ctx context.Context, selector string) (bool, error)
	EvalBool(ctx context.Context, expression string) (bool, error)
	EvalString(ctx context.Context, expression string) (string, error)
}

const (
	officeAceRegistrationAgreeCheckedExpr = `(function(){` +
		`var el=document.querySelector('input.ti-checkbox-input');` +
		`return !!(el && el.checked);})()`
	officeAceRegistrationContinueEnabledExpr = `(function(){` +
		`var el=document.querySelector('#submitBtn_reg_open');` +
		`return !!(el && !el.disabled && !el.classList.contains('is-disabled'));})()`
	officeAceRegistrationAgreementWait = 5 * time.Second
	officeAceRegistrationContinueWait  = 10 * time.Second
)

// officeAceAutoJob is one in-flight auto-login.  Every field is read under mu
// because the panel polls while the background run writes.
type officeAceAutoJob struct {
	mu         sync.Mutex
	id         string
	sessionID  string
	loginURL   string
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

func (j *officeAceAutoJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(fmt.Sprintf(format, args...))
}

func (j *officeAceAutoJob) appendLocked(text string) {
	if len(j.lines) >= officeAceAutoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(text),
	})
}

func (j *officeAceAutoJob) setStep(step string) {
	j.mu.Lock()
	j.step = step
	j.mu.Unlock()
}

func (j *officeAceAutoJob) setPhone(phone string) {
	j.mu.Lock()
	j.phone = phone
	j.mu.Unlock()
}

func (j *officeAceAutoJob) finish(state, message, accountID string) {
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
	j.expiresAt = j.finishedAt.Add(officeAceAutoJobTTL)
	if strings.TrimSpace(message) != "" {
		j.appendLocked(message)
	}
}

func (j *officeAceAutoJob) fail(message string) {
	j.finish(core.AutoLoginFailed, message, "")
}

func (j *officeAceAutoJob) snapshot() core.AutoLoginJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	lines := make([]core.AutoLogLine, len(j.lines))
	copy(lines, j.lines)
	out := core.AutoLoginJob{
		ID:        j.id,
		State:     j.state,
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

func (o *officeAceSource) putAutoJob(job *officeAceAutoJob) {
	o.autoMu.Lock()
	defer o.autoMu.Unlock()
	if o.autoJobs == nil {
		o.autoJobs = map[string]*officeAceAutoJob{}
	}
	now := time.Now()
	for id, other := range o.autoJobs {
		other.mu.Lock()
		stale := !other.expiresAt.IsZero() && now.After(other.expiresAt)
		other.mu.Unlock()
		if stale {
			delete(o.autoJobs, id)
		}
	}
	o.autoJobs[job.id] = job
}

func (o *officeAceSource) autoJobByID(id string) *officeAceAutoJob {
	o.autoMu.Lock()
	defer o.autoMu.Unlock()
	return o.autoJobs[id]
}

var _ core.AutoLoginProvider = (*officeAceSource)(nil)

// StartAutoLogin validates the prerequisites and starts the run in the
// background.  StartLogin is called before returning so a cloud that cannot
// create a Huawei state fails the request immediately instead of making the
// operator watch a job log.
func (o *officeAceSource) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	if o == nil || o.source == nil {
		return core.AutoLoginJob{}, errors.New("officeace: this platform is not available")
	}
	opts := officeAceSMSOpts(req)
	if _, err := o.newSMSClient(o.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if strings.TrimSpace(o.source.deps.DataDir) == "" {
		return core.AutoLoginJob{}, errors.New("officeace: no data directory: a credential could not be stored")
	}
	if o.autoDriver == nil && browser.FindBrowser("") == "" {
		return core.AutoLoginJob{}, errors.New("officeace: no Chromium-family browser found; install Microsoft Edge or Google Chrome")
	}
	login, err := o.StartLogin(ctxOrBackground(ctx))
	if err != nil {
		return core.AutoLoginJob{}, err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), officeAceAutoLoginTimeout)
	job := &officeAceAutoJob{
		id:        newOfficeAceAutoJobID(),
		sessionID: login.SessionID,
		loginURL:  login.URL,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	o.putAutoJob(job)
	job.logf("华为自动登录已启动（state=%s）", shortOfficeAceID(login.SessionID))
	core.GoSafe("officeace auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		defer o.CancelLogin(context.Background(), login.SessionID)
		o.runAutoLogin(runCtx, job, req, opts)
	})
	return job.snapshot(), nil
}

func (o *officeAceSource) PollAutoLogin(_ context.Context, id string) (core.AutoLoginJob, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.AutoLoginJob{}, errors.New("job id is required")
	}
	job := o.autoJobByID(id)
	if job == nil {
		return core.AutoLoginJob{}, fmt.Errorf("auto-login job %q not found", id)
	}
	return job.snapshot(), nil
}

func (o *officeAceSource) CancelAutoLogin(_ context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("job id is required")
	}
	job := o.autoJobByID(id)
	if job == nil {
		return fmt.Errorf("auto-login job %q not found", id)
	}
	job.mu.Lock()
	var cancel context.CancelFunc
	if job.state == core.AutoLoginRunning {
		job.state = core.AutoLoginCancelled
		job.message = "已取消"
		job.step = "done"
		job.finishedAt = time.Now()
		job.expiresAt = job.finishedAt.Add(officeAceAutoJobTTL)
		job.appendLocked("已取消")
		cancel = job.cancel
	}
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func officeAceSMSOpts(req core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(req.Token),
		Proxy:    strings.TrimSpace(req.Proxy),
		Keyword:  strings.TrimSpace(req.Keyword),
		Province: strings.TrimSpace(req.Province),
		CardType: strings.TrimSpace(req.CardType),
	}
}

func (o *officeAceSource) runAutoLogin(ctx context.Context, job *officeAceAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) {
	err := o.autoLogin(ctx, job, req, opts)
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

func (o *officeAceSource) autoLogin(ctx context.Context, job *officeAceAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	job.setStep("renting")
	num, err := o.AcquirePhone(ctx, opts, strings.TrimSpace(req.Phone), req.Avoid)
	if err != nil {
		return fmt.Errorf("取号失败：%w", err)
	}
	job.setPhone(num.Phone)
	if num.Reused {
		job.logf("已重新占用账号手机号 %s", num.Phone)
	} else {
		job.logf("已取号 %s%s", num.Phone, provinceSuffix(num.Province))
	}
	released := false
	releasePhone := func() {
		if released {
			return
		}
		released = true
		o.releaseAutoPhone(ctx, job, opts, num)
	}
	defer releasePhone()

	job.setStep("browser")
	driver := o.autoDriver
	if driver == nil {
		driver = officeAceBrowserDriver{}
	}
	if err := driver.Drive(ctx, job.loginURL, num.Phone, func(waitCtx context.Context) (string, error) {
		job.setStep("sms")
		return o.waitAutoSMSCode(waitCtx, job, opts, num.Phone)
	}, job.logf); err != nil {
		return fmt.Errorf("浏览器登录失败：%w", err)
	}

	job.setStep("polling")
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, err := o.PollLogin(ctx, job.sessionID)
		if err != nil {
			return fmt.Errorf("查询华为授权结果失败：%w", err)
		}
		switch st.State {
		case core.LoginSuccess:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "华为账号登录成功，凭据已保存"
			}
			releasePhone()
			job.finish(core.AutoLoginSuccess, msg, st.AccountID)
			return nil
		case core.LoginFailed, core.LoginCancelled:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "华为账号登录失败"
			}
			return errors.New(msg)
		}
		if time.Now().After(deadline) {
			return errors.New("华为授权没有在限定时间内完成")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (o *officeAceSource) releaseAutoPhone(ctx context.Context, job *officeAceAutoJob, opts core.SMSOpts, num core.SMSNumber) {
	if num.Reused {
		job.logf("保留账号绑定号码 %s", num.Phone)
		return
	}
	rctx := context.WithoutCancel(ctx)
	if err := o.ReleasePhone(rctx, opts, num.Phone, false); err != nil {
		job.logf("释放号码失败：%v", err)
		return
	}
	job.logf("已释放号码 %s", num.Phone)
}

func (o *officeAceSource) waitAutoSMSCode(ctx context.Context, job *officeAceAutoJob, opts core.SMSOpts, phone string) (string, error) {
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", officeAceSMSPolls, officeAceSMSInterval)
	for i := 0; i < officeAceSMSPolls; i++ {
		res, err := o.PollSMSCode(ctx, opts, phone)
		switch {
		case err != nil:
			job.logf("短信平台报错（%d/%d）：%v", i+1, officeAceSMSPolls, err)
		case res.Ready:
			job.logf("已收到验证码")
			return res.Code, nil
		case strings.TrimSpace(res.Raw) != "":
			job.logf("收到短信但没解析出验证码：%s", res.Raw)
		default:
			job.logf("还没收到短信（%d/%d）…", i+1, officeAceSMSPolls)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(officeAceSMSInterval):
		}
	}
	return "", errors.New("没有等到验证码：确认接码平台的发送方关键字是「" + officeAceDefaultSMSKey + "」后重试")
}

func newOfficeAceAutoJobID() string {
	if token, err := officeAceRandomToken(12); err == nil {
		return "officeace-auto-" + token
	}
	return fmt.Sprintf("officeace-auto-%d", time.Now().UnixNano())
}

func shortOfficeAceID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}

// provinceSuffix renders "（浙江）" for a drawn number, and nothing when the
// platform chose the province for us.
func provinceSuffix(province string) string {
	province = strings.TrimSpace(province)
	if province == "" {
		return ""
	}
	return "（" + province + "）"
}

// officeAceBrowserDriver drives the real Huawei login page with the shared CDP
// wrapper.  It keeps the browser invisible by default; the page selectors are
// the same ones verified in the Huawei client page.
type officeAceBrowserDriver struct{}

func (officeAceBrowserDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("Huawei authorization URL is empty")
	}
	if waitCode == nil {
		return errors.New("SMS code reader is missing")
	}
	br, err := browser.Launch(ctx, browser.LaunchOpts{StartURL: url, Headless: true})
	if err != nil {
		return err
	}
	defer br.Close()
	page := br.Page()
	logf("浏览器已启动，正在打开华为登录页…")

	if ok, err := waitOfficeAceSelector(ctx, page, officeAceSelSMSLogin, 30*time.Second); err != nil {
		return fmt.Errorf("等待手机号登录入口失败：%w", err)
	} else if ok {
		clicked, err := page.Click(ctx, officeAceSelSMSLogin)
		if err != nil {
			return fmt.Errorf("切换手机号登录失败：%w", err)
		}
		if clicked {
			logf("已切换到手机号登录")
		}
	}

	if ok, err := waitOfficeAceSelector(ctx, page, officeAceSelPhone, 30*time.Second); err != nil {
		return fmt.Errorf("等待手机号输入框失败：%w", err)
	} else if !ok {
		return errors.New("华为登录页没有出现手机号输入框")
	}
	if ok, err := page.Fill(ctx, officeAceSelPhone, phone); err != nil {
		return fmt.Errorf("填写手机号失败：%w", err)
	} else if !ok {
		return errors.New("找不到手机号输入框")
	}
	logf("已填写手机号")

	if err := requestOfficeAceSMSCode(ctx, page, logf); err != nil {
		return err
	}
	logf("已请求发送验证码")

	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	if ok, err := waitOfficeAceSelector(ctx, page, officeAceSelCode, 20*time.Second); err != nil {
		return fmt.Errorf("等待验证码输入框失败：%w", err)
	} else if !ok {
		return errors.New("华为登录页没有出现验证码输入框")
	}
	if ok, err := page.Fill(ctx, officeAceSelCode, code); err != nil {
		return fmt.Errorf("填写验证码失败：%w", err)
	} else if !ok {
		return errors.New("找不到验证码输入框")
	}
	// Same Angular digest race as the phone field: let the model commit before
	// the submit control is clicked.
	if !sleepOfficeAce(ctx, officeAcePhoneSettle) {
		return ctx.Err()
	}
	if ok, err := page.Click(ctx, officeAceSelSubmit); err != nil {
		return fmt.Errorf("提交验证码失败：%w", err)
	} else if !ok {
		return errors.New("找不到登录按钮")
	}
	logf("已提交验证码，等待华为授权结果…")
	if err := acceptOfficeAceRegistrationConsent(ctx, page, logf); err != nil {
		return fmt.Errorf("确认华为云用户协议失败：%w", err)
	}
	return nil
}

// acceptOfficeAceRegistrationConsent handles the extra Huawei Cloud
// registration page shown for a freshly created account. The page is
// conditional: an existing account goes straight back to the OAuth callback,
// so a missing checkbox is a successful no-op rather than an error.
func acceptOfficeAceRegistrationConsent(ctx context.Context, page officeAceAutoPage, logf func(string, ...any)) error {
	return acceptOfficeAceRegistrationConsentWithin(ctx, page, officeAceRegistrationAgreementWait, officeAceRegistrationContinueWait, logf)
}

func acceptOfficeAceRegistrationConsentWithin(ctx context.Context, page officeAceAutoPage, agreementWait, continueWait time.Duration, logf func(string, ...any)) error {
	appeared, err := waitOfficeAceSelector(ctx, page, officeAceSelRegisterAgree, agreementWait)
	if err != nil {
		return err
	}
	if !appeared {
		return nil
	}

	logf("检测到华为云用户协议，正在确认…")
	checked, err := page.EvalBool(ctx, officeAceRegistrationAgreeCheckedExpr)
	if err != nil {
		return fmt.Errorf("读取协议勾选状态失败：%w", err)
	}
	if !checked {
		clicked, err := page.Click(ctx, officeAceSelRegisterAgree)
		if err != nil {
			return fmt.Errorf("勾选用户协议失败：%w", err)
		}
		if !clicked {
			return errors.New("找不到用户协议勾选框")
		}
	}

	enabled, err := waitOfficeAceCondition(ctx, page, officeAceRegistrationContinueEnabledExpr, continueWait)
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("勾选用户协议后“同意并继续”按钮仍未启用")
	}
	clicked, err := page.Click(ctx, officeAceSelRegisterContinue)
	if err != nil {
		return fmt.Errorf("点击“同意并继续”失败：%w", err)
	}
	if !clicked {
		return errors.New("找不到“同意并继续”按钮")
	}
	logf("已同意华为云用户协议，继续华为授权…")
	return nil
}

// requestOfficeAceSMSCode clicks Huawei's "get code" control and does not
// return success until the page confirms the request was really accepted.  A
// bare click is not enough: see the officeAcePhoneSettle comment above for the
// settle/digest race this closes.
func requestOfficeAceSMSCode(ctx context.Context, page officeAceAutoPage, logf func(string, ...any)) error {
	if ok, err := waitOfficeAceSelector(ctx, page, officeAceSelSendCode, 20*time.Second); err != nil {
		return fmt.Errorf("等待获取验证码按钮失败：%w", err)
	} else if !ok {
		return errors.New("找不到获取验证码按钮")
	}
	if !sleepOfficeAce(ctx, officeAcePhoneSettle) {
		return ctx.Err()
	}
	for attempt := 1; attempt <= officeAceSendCodeAttempts; attempt++ {
		ok, err := page.Click(ctx, officeAceSelSendCode)
		if err != nil {
			return fmt.Errorf("点击获取验证码失败：%w", err)
		}
		if !ok {
			return errors.New("找不到获取验证码按钮")
		}
		if attempt > 1 {
			logf("第一次点击未触发发码，正在重试…")
		}
		sent, err := waitOfficeAceCondition(ctx, page, officeAceResendVisibleExpr, officeAceSendCodeWait)
		if err != nil {
			return fmt.Errorf("确认验证码已发送失败：%w", err)
		}
		if sent {
			return nil
		}
	}
	return errors.New("点了「获取验证码」但华为没有发码（页面未进入重新获取倒计时）：确认手机号格式和登录页版本后重试")
}

// sleepOfficeAce waits for d, reporting false when ctx ends first.
func sleepOfficeAce(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func waitOfficeAceCondition(ctx context.Context, page officeAceAutoPage, expression string, timeout time.Duration) (bool, error) {
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

func waitOfficeAceSelector(ctx context.Context, page officeAceAutoPage, selector string, timeout time.Duration) (bool, error) {
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
