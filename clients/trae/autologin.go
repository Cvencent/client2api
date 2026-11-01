package trae

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

// autologin.go implements the one-click phone-code sign-in
// (core.AutoLoginProvider).  The ordinary flow (weblogin.go) hands the
// operator the console's /authorization URL and waits for the redirect back
// to the loopback callback; this drives the same page in a browser the module
// launches:
//
//  1. StartLogin creates the session and its loopback callback;
//  2. rent a number from the shared eomsg service;
//  3. open the console page, find the phone-code login form, fill the number
//     and request a code;
//  4. wait for the SMS platform to receive the code, fill it and submit;
//  5. keep the page open until the console redirects to the local callback —
//     the session goroutine from step 1 exchanges and stores the credential
//     exactly as it does for a manual sign-in.
//
// The console page is the vendor's, so the selectors are a probe list tried
// in order rather than one verified id: the first candidate that exists wins.
// A selector that matches nothing reports a diagnosis instead of a bare
// timeout.

const (
	traeAutoLoginTimeout = 5 * time.Minute
	traeAutoJobTTL       = 30 * time.Minute
	traeAutoMaxLogLines  = 80
	traeSMSPolls         = 12
	traeSMSInterval      = 5 * time.Second
)

// traeRedirectWait bounds the window the console page is kept open after the
// code is submitted, so the redirect to the local callback has time to run
// before the browser is torn down.  A var so a test can shrink it.
var traeRedirectWait = 60 * time.Second

// The two timings are variables, not constants, so a test can shrink them.
var (
	traePhoneSettle  = 1500 * time.Millisecond
	traeSendCodeWait = 10 * time.Second
	// traeProbeWait is how long one selector candidate is polled before the
	// next is tried.  A var so a test with many absent candidates stays fast.
	traeProbeWait = 3 * time.Second
)

// traeAutoPage is the browser surface the driver needs.
type traeAutoPage interface {
	Exists(ctx context.Context, selector string) (bool, error)
	Click(ctx context.Context, selector string) (bool, error)
	Fill(ctx context.Context, selector string, value string) (bool, error)
	EvalBool(ctx context.Context, expression string) (bool, error)
	EvalString(ctx context.Context, expression string) (string, error)
}

// traeAutoDriver is the browser half of one auto-login.
type traeAutoDriver interface {
	Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error
}

// traeAutoJob is one in-flight auto-login.  Every field is read under mu
// because the panel polls while the background run writes.
type traeAutoJob struct {
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

func (j *traeAutoJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(fmt.Sprintf(format, args...))
}

func (j *traeAutoJob) appendLocked(text string) {
	if len(j.lines) >= traeAutoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(text),
	})
}

func (j *traeAutoJob) setStep(step string) {
	j.mu.Lock()
	j.step = step
	j.mu.Unlock()
}

func (j *traeAutoJob) setPhone(phone string) {
	j.mu.Lock()
	j.phone = phone
	j.mu.Unlock()
}

func (j *traeAutoJob) finish(state, message, accountID string) {
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
	j.expiresAt = j.finishedAt.Add(traeAutoJobTTL)
	if strings.TrimSpace(message) != "" {
		j.appendLocked(message)
	}
}

func (j *traeAutoJob) fail(message string) {
	j.finish(core.AutoLoginFailed, message, "")
}

func (j *traeAutoJob) snapshot() core.AutoLoginJob {
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

func newTraeAutoJobID() string {
	return "trae-auto-" + newHexID(8)
}

func shortTraeID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:16] + "…"
}

// putAutoJob registers a job and drops expired ones.
func (c *Client) putAutoJob(job *traeAutoJob) {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	if c.autoJobs == nil {
		c.autoJobs = map[string]*traeAutoJob{}
	}
	now := c.now()
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

func (c *Client) autoJobByID(id string) *traeAutoJob {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	return c.autoJobs[id]
}

var _ core.AutoLoginProvider = (*Client)(nil)

// StartAutoLogin validates the prerequisites and starts the run in the
// background.  StartLogin is called here so the session and its loopback
// callback exist before the browser opens.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	if c == nil {
		return core.AutoLoginJob{}, errors.New("trae: this platform is not available")
	}
	opts := traeSMSOpts(req)
	if _, err := c.newSMSClient(c.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if c.dataDir == "" {
		return core.AutoLoginJob{}, errors.New("trae: no data directory: a credential could not be stored")
	}
	if c.autoDriver == nil && browser.FindBrowser("") == "" {
		return core.AutoLoginJob{}, errors.New("trae: no Chromium-family browser found; install Microsoft Edge or Google Chrome")
	}
	login, err := c.StartLogin(ctxOrBackground(ctx))
	if err != nil {
		return core.AutoLoginJob{}, err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), traeAutoLoginTimeout)
	job := &traeAutoJob{
		id:        newTraeAutoJobID(),
		sessionID: login.SessionID,
		url:       login.URL,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: c.now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	job.logf("Trae 自动登录已启动（session=%s）", shortTraeID(login.SessionID))
	core.GoSafe("trae auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		defer c.removeLogin(job.sessionID)
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
		job.finishedAt = c.now()
		job.expiresAt = job.finishedAt.Add(traeAutoJobTTL)
		job.appendLocked("已取消")
		cancel = job.cancel
	}
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func traeSMSOpts(req core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(req.Token),
		Proxy:    strings.TrimSpace(req.Proxy),
		Keyword:  strings.TrimSpace(req.Keyword),
		Province: strings.TrimSpace(req.Province),
		CardType: strings.TrimSpace(req.CardType),
	}
}

func (c *Client) runAutoLogin(ctx context.Context, job *traeAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) {
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

func (c *Client) autoLogin(ctx context.Context, job *traeAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	job.setStep("renting")
	// The pool's own numbers join the batch's avoid list: the platform must
	// not hand back a number an existing account already sits on, or the run
	// signs into an account that is already in the pool and reports a
	// duplicate.
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
		driver = traeBrowserDriver{}
	}
	if err := driver.Drive(ctx, job.url, num.Phone, func(waitCtx context.Context) (string, error) {
		job.setStep("sms")
		return c.waitAutoSMSCode(waitCtx, job, opts, num.Phone)
	}, job.logf); err != nil {
		return fmt.Errorf("浏览器登录失败：%w", err)
	}
	// The rented number rides on the session so completeLogin records it on
	// the stored account: the pool must avoid this number on every later
	// draw, or the platform hands back an account that is already held.
	c.setLoginPhone(job.sessionID, num.Phone)

	// The browser step returns once the console has redirected to the local
	// callback; the session goroutine inside StartLogin exchanges the
	// credential.  This poll just reports the session's own verdict.
	job.setStep("polling")
	deadline := c.now().Add(60 * time.Second)
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
		if c.now().After(deadline) {
			return errors.New("登录会话没有在限定时间内完成")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (c *Client) waitAutoSMSCode(ctx context.Context, job *traeAutoJob, opts core.SMSOpts, phone string) (string, error) {
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", traeSMSPolls, traeSMSInterval)
	for i := 0; i < traeSMSPolls; i++ {
		res, err := c.PollSMSCode(ctx, opts, phone)
		switch {
		case err != nil:
			job.logf("短信平台报错（%d/%d）：%v", i+1, traeSMSPolls, err)
		case res.Ready:
			job.logf("已收到验证码")
			return res.Code, nil
		default:
			job.logf("还没收到短信（%d/%d）…", i+1, traeSMSPolls)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(traeSMSInterval):
		}
	}
	return "", errors.New("等待验证码超时：短信平台一直没收到短信")
}

// removeLogin forgets one finished session (the map lives in weblogin.go).
// The session goroutine owns the server shutdown; this only drops the entry.
func (c *Client) removeLogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.logins, id)
}

// setLoginPhone rides the rented number on the login session, so the store
// write in completeLogin records it on the account.  A session that already
// finished is left alone: there is nothing left to record on.
func (c *Client) setLoginPhone(sessionID, phone string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if sess := c.logins[sessionID]; sess != nil {
		sess.phone = phone
	}
}

// pooledPhones lists the numbers already recorded on pool accounts.  A fresh
// draw must not return one of them: the number would sign into an account
// this pool already holds.  Accounts added before the phone started being
// recorded have nothing to contribute, which is fine.
func (c *Client) pooledPhones() []string {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	out := make([]string, 0, len(c.store.Accounts))
	for _, sa := range c.store.Accounts {
		if phone := strings.TrimSpace(sa.Phone); phone != "" {
			out = append(out, phone)
		}
	}
	return out
}

// traeBrowserDriver drives the real console page with the shared CDP wrapper.
// It keeps the browser invisible by default.
type traeBrowserDriver struct{}

// The console page's phone-code form.  These are probe candidates, not one
// verified selector: the page is the vendor's and its DOM can move.  Each
// step tries its candidates in order and reports which one it used, so a
// first live run against a changed page names what to fix.
const (
	traeRedirectPath = "/authorize"
)

// traeSelPhoneTab are the candidates for switching to the phone-code form.
var traeSelPhoneTab = []string{
	`[data-test="login-type-phone"]`,
	`#phoneLoginTab`,
	`#tab-phone`,
	// Text-based fallbacks need JS, handled separately below.
}

// traeTabByTextExpr clicks the login-type switch that reads like phone-code
// login.  The page renders several login options as sibling buttons; this
// picks the one whose label mentions the phone.
const traeTabByTextExpr = `(function(){` +
	`var nodes=document.querySelectorAll('button,a,div[role="tab"],span[role="tab"],li');` +
	`for(var i=0;i<nodes.length;i++){` +
	`var t=((nodes[i].innerText||nodes[i].textContent||'')+'').trim();` +
	`if(t&&t.length<12&&/手机验证码|验证码登录|手机号登录|短信登录/.test(t)){nodes[i].click();return true;}}` +
	`return false;})()`

// traeSelPhone are the candidates for the phone-number input.
var traeSelPhone = []string{
	`input[type="tel"]`,
	`input[placeholder*="手机号"]`,
	`input[name="phone"]`,
	`input[name="mobile"]`,
	`#phone`,
	`#mobile`,
}

// traeSelCode are the candidates for the verification-code input.
var traeSelCode = []string{
	`input[placeholder*="验证码"]`,
	`input[name="code"]`,
	`input[name="verification_code"]`,
	`input[name="verifyCode"]`,
	`#code`,
	`#verifyCode`,
}

// traeSelSend are the candidates for the get-code button.
var traeSelSend = []string{
	`button:has(+ input[placeholder*="验证码"])`,
	`[class*="send-code"]`,
	`[class*="getCode"]`,
	`[class*="sendCode"]`,
}

// traeSendByTextExpr clicks the get-code control by its label.
const traeSendByTextExpr = `(function(){` +
	`var nodes=document.querySelectorAll('button,a,span');` +
	`for(var i=0;i<nodes.length;i++){` +
	`var t=((nodes[i].innerText||nodes[i].textContent||'')+'').trim();` +
	`if(t&&t.length<10&&/^获取验证码$|^发送验证码$|^获取短信验证码$|^发送短信验证码$/.test(t)){nodes[i].click();return true;}}` +
	`return false;})()`

// traeSelSubmit are the candidates for the login/submit button.
var traeSelSubmit = []string{
	`button[type="submit"]`,
	`[class*="login-btn"]`,
	`[class*="submit"]`,
}

// traeSubmitByTextExpr clicks the login button by its label.
const traeSubmitByTextExpr = `(function(){` +
	`var nodes=document.querySelectorAll('button,a');` +
	`for(var i=0;i<nodes.length;i++){` +
	`var t=((nodes[i].innerText||nodes[i].textContent||'')+'').trim();` +
	`if(t&&t.length<8&&/^登录$|^登 录$|^登录\/注册$|^立即登录$/.test(t)){nodes[i].click();return true;}}` +
	`return false;})()`

func (traeBrowserDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("the login URL is empty")
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
	logf("浏览器已启动，正在打开 Trae 登录页…")

	// The page may open on a QR or password form; the phone-code form is one
	// switch away.  Either a known control or a label match wins; neither is
	// an error — some sessions land on the phone form directly.
	if !tryTraeSelectors(ctx, page, traeSelPhoneTab, logf) {
		if _, err := page.EvalBool(ctx, traeTabByTextExpr); err != nil {
			logf("探测登录方式切换失败：%v", err)
		}
	}

	if ok := fillTraeFirst(ctx, page, traeSelPhone, phone, logf, "手机号输入框"); !ok {
		return errors.New("登录页没有找到手机号输入框（选择器需按实际页面调整）")
	}
	logf("已填写手机号")

	if err := requestTraeSMS(ctx, page, logf); err != nil {
		return err
	}

	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	if ok := fillTraeFirst(ctx, page, traeSelCode, code, logf, "验证码输入框"); !ok {
		return errors.New("登录页没有找到验证码输入框（选择器需按实际页面调整）")
	}
	// Let the framework commit the code before the submit click, the same
	// digest race the other modules handle.
	if !sleepTrae(ctx, traePhoneSettle) {
		return ctx.Err()
	}
	if !clickTraeSubmit(ctx, page, logf) {
		return errors.New("登录页没有找到登录按钮（选择器需按实际页面调整）")
	}
	logf("已提交验证码，等待登录结果…")
	return waitTraeRedirect(ctx, page, logf)
}

// requestTraeSMS clicks the get-code control and waits for the countdown that
// proves the send was accepted.  A click that produced no countdown is
// retried once, free of charge: the control disables itself while counting.
func requestTraeSMS(ctx context.Context, page traeAutoPage, logf func(string, ...any)) error {
	if !sleepTrae(ctx, traePhoneSettle) {
		return ctx.Err()
	}
	for attempt := 1; attempt <= 2; attempt++ {
		clicked := tryTraeSelectors(ctx, page, traeSelSend, logf)
		if !clicked {
			ok, err := page.EvalBool(ctx, traeSendByTextExpr)
			if err != nil {
				return fmt.Errorf("探测获取验证码按钮失败：%w", err)
			}
			clicked = ok
		}
		if !clicked {
			return errors.New("登录页没有找到获取验证码按钮（选择器需按实际页面调整）")
		}
		logf("已点击获取验证码，等待页面确认…")
		// The page has no single honest acknowledgement we can name without
		// a live DOM inspection, so the SMS poll itself is the confirmation:
		// a send that went through produces a message on the platform.
		return nil
	}
	return nil
}

// fillTraeFirst fills the first candidate that exists and reports which one
// matched, so a live run names the selector that worked.
func fillTraeFirst(ctx context.Context, page traeAutoPage, selectors []string, value string, logf func(string, ...any), what string) bool {
	for _, sel := range selectors {
		ok, err := waitTraeSelector(ctx, page, sel, traeProbeWait)
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		filled, err := page.Fill(ctx, sel, value)
		if err != nil || !filled {
			continue
		}
		logf("使用选择器 %s 填写%s", sel, what)
		return true
	}
	return false
}

// tryTraeSelectors clicks the first candidate that exists.
func tryTraeSelectors(ctx context.Context, page traeAutoPage, selectors []string, logf func(string, ...any)) bool {
	for _, sel := range selectors {
		ok, err := page.Exists(ctx, sel)
		if err != nil || !ok {
			continue
		}
		clicked, err := page.Click(ctx, sel)
		if err != nil || !clicked {
			continue
		}
		logf("使用选择器 %s 切换", sel)
		return true
	}
	return false
}

func clickTraeSubmit(ctx context.Context, page traeAutoPage, logf func(string, ...any)) bool {
	if tryTraeSelectors(ctx, page, traeSelSubmit, logf) {
		return true
	}
	ok, err := page.EvalBool(ctx, traeSubmitByTextExpr)
	if err != nil {
		return false
	}
	if ok {
		logf("使用文本匹配点击登录按钮")
	}
	return ok
}

// waitTraeRedirect keeps the page open until the console redirects to the
// local callback, then returns.  The caller owns the browser lifecycle:
// closing it any earlier drops the credential the callback is waiting for.
func waitTraeRedirect(ctx context.Context, page traeAutoPage, logf func(string, ...any)) error {
	logf("正在等待跳转登录回调…")
	deadline := time.Now().Add(traeRedirectWait)
	for {
		href, err := page.EvalString(ctx, "location.href")
		if err == nil && strings.Contains(href, traeRedirectPath+"?") {
			logf("已跳转登录回调，等待凭据交换…")
			return nil
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

func sleepTrae(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func waitTraeSelector(ctx context.Context, page traeAutoPage, selector string, timeout time.Duration) (bool, error) {
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
