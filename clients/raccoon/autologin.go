package raccoon

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

// autologin.go implements the one-click SMS sign-in (core.AutoLoginProvider).
//
// The interactive login already has everything the run needs: StartLogin
// serves the loopback QR/SMS page, handleSmsSend carries the Aliyun captcha
// handshake, and handleSmsVerify stores the credential.  The 2026-10-21
// removal of the old 一键接码 was about bypassing the browser to mint a
// captcha token by hand, which the vendor's hardened scene rejects.  A real
// Chromium runs the page's own Aliyun JS instead: the traceless path passes
// there exactly as it does for an operator driving the page manually.
//
// The flow, one step per log line:
//
//  1. StartLogin creates the session and its loopback page;
//  2. rent a number from the shared eomsg service;
//  3. drive the loopback page in a headless browser: switch to the SMS tab,
//     fill the number, click 获取验证码 (the page's JS solves the traceless
//     captcha and posts the send itself);
//  4. wait for the SMS platform to receive the code, fill it, click 登录;
//  5. poll the session until finishLogin has stored the credential.
//
// A browser driver is injectable so tests never launch a real browser.

const (
	raccoonAutoLoginTimeout = 5 * time.Minute
	raccoonAutoJobTTL       = 30 * time.Minute
	raccoonAutoMaxLogLines  = 80
	raccoonSMSPolls         = 12
	raccoonSMSInterval      = 5 * time.Second
)

// raccoonSendWait bounds how long the page has to report the send or login
// outcome after the corresponding button is clicked; the traceless captcha
// handshake and the vendor round-trip both live inside this window.  A var so
// a test can shrink it.
var raccoonSendWait = 20 * time.Second

// raccoonSendSettle lets the page's own click-to-captcha wiring arm before
// the driver clicks 获取验证码.  The Aliyun script attaches its listener to
// the button, so an immediate click can be dropped the same way an SPA
// digest race drops one.
var raccoonSendSettle = 1500 * time.Millisecond

// The loopback page's DOM.  These ids are ours, not a vendor SPA's, so they
// are stable by construction.
const (
	raccoonSelSmsTab  = `#tabSms`
	raccoonSelPhone   = `#phone`
	raccoonSelSend    = `#sendCode`
	raccoonSelCode    = `#smsCode`
	raccoonSelLogin   = `#doLogin`
	raccoonSelStatus  = `#smsStatus`
	raccoonSelCaptcha = `#captcha-element`
)

// raccoonAutoPage is the browser surface the driver needs.
type raccoonAutoPage interface {
	Exists(ctx context.Context, selector string) (bool, error)
	Click(ctx context.Context, selector string) (bool, error)
	Fill(ctx context.Context, selector string, value string) (bool, error)
	EvalBool(ctx context.Context, expression string) (bool, error)
	EvalString(ctx context.Context, expression string) (string, error)
}

// raccoonAutoDriver is the browser half of one auto-login.
type raccoonAutoDriver interface {
	Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error
}

// raccoonAutoJob is one in-flight auto-login.  Every field is read under mu
// because the panel polls while the background run writes.
type raccoonAutoJob struct {
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

func (j *raccoonAutoJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(fmt.Sprintf(format, args...))
}

func (j *raccoonAutoJob) appendLocked(text string) {
	if len(j.lines) >= raccoonAutoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(text),
	})
}

func (j *raccoonAutoJob) setStep(step string) {
	j.mu.Lock()
	j.step = step
	j.mu.Unlock()
}

func (j *raccoonAutoJob) setPhone(phone string) {
	j.mu.Lock()
	j.phone = phone
	j.mu.Unlock()
}

func (j *raccoonAutoJob) finish(state, message, accountID string) {
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
	j.expiresAt = j.finishedAt.Add(raccoonAutoJobTTL)
	if strings.TrimSpace(message) != "" {
		j.appendLocked(message)
	}
}

func (j *raccoonAutoJob) fail(message string) {
	j.finish(core.AutoLoginFailed, message, "")
}

func (j *raccoonAutoJob) snapshot() core.AutoLoginJob {
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

func newRaccoonAutoJobID() string {
	buf, err := generateQRCode()
	if err != nil {
		return "raccoon-auto"
	}
	return "raccoon-auto:" + buf
}

// putAutoJob registers a job and drops expired ones.
func (c *Client) putAutoJob(job *raccoonAutoJob) {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	if c.autoJobs == nil {
		c.autoJobs = map[string]*raccoonAutoJob{}
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

func (c *Client) autoJobByID(id string) *raccoonAutoJob {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	return c.autoJobs[id]
}

var _ core.AutoLoginProvider = (*Client)(nil)

// StartAutoLogin validates the prerequisites and starts the run in the
// background.  StartLogin is called here so the loopback page and the session
// it owns exist before the browser opens.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	if c == nil {
		return core.AutoLoginJob{}, errors.New("raccoon: this platform is not available")
	}
	if c.deps.DataDir == "" {
		return core.AutoLoginJob{}, errors.New("raccoon: no data directory: a credential could not be stored")
	}
	if c.autoDriver == nil && browser.FindBrowser("") == "" {
		return core.AutoLoginJob{}, errors.New("raccoon: no Chromium-family browser found; install Microsoft Edge or Google Chrome")
	}
	login, err := c.StartLogin(context.Background())
	if err != nil {
		return core.AutoLoginJob{}, err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), raccoonAutoLoginTimeout)
	job := &raccoonAutoJob{
		id:        newRaccoonAutoJobID(),
		sessionID: login.SessionID,
		url:       login.URL,
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: c.now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	job.logf("小浣熊自动登录已启动（session=%s）", shortRaccoonID(login.SessionID))
	core.GoSafe("raccoon auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		// Dropping the session when the run ends keeps the loopback server
		// from outliving its only consumer.
		defer c.removeLogin(job.sessionID)
		c.runAutoLogin(runCtx, job, req)
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
		job.expiresAt = job.finishedAt.Add(raccoonAutoJobTTL)
		job.appendLocked("已取消")
		cancel = job.cancel
	}
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func shortRaccoonID(id string) string {
	if len(id) <= 24 {
		return id
	}
	return id[:24] + "…"
}

// raccoonSMSOpts folds the per-run request overrides into one SMSOpts, the
// same way the officeace module does.  The panel always sends the platform
// token the operator pasted, and dropping it would strand the run at the
// very first rental call.
func raccoonSMSOpts(req core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(req.Token),
		Proxy:    strings.TrimSpace(req.Proxy),
		Keyword:  strings.TrimSpace(req.Keyword),
		Province: strings.TrimSpace(req.Province),
		CardType: strings.TrimSpace(req.CardType),
	}
}

// pooledPhones lists the phone numbers already bound to pool accounts.  A
// fresh draw must not return one of them: the number would text a code for
// an account this module already holds.
func (c *Client) pooledPhones() []string {
	entries := c.pool.all()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if phone := strings.TrimSpace(e.acct.Phone); phone != "" {
			out = append(out, phone)
		}
	}
	return out
}

func (c *Client) runAutoLogin(ctx context.Context, job *raccoonAutoJob, req core.AutoLoginRequest) {
	err := c.autoLogin(ctx, job, req, raccoonSMSOpts(req))
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

func (c *Client) autoLogin(ctx context.Context, job *raccoonAutoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
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
		driver = raccoonBrowserDriver{}
	}
	if err := driver.Drive(ctx, job.url, num.Phone, func(waitCtx context.Context) (string, error) {
		job.setStep("sms")
		return c.waitAutoSMSCode(waitCtx, job, opts, num.Phone)
	}, job.logf); err != nil {
		return fmt.Errorf("浏览器登录失败：%w", err)
	}

	// The browser step returns once the page has posted the login and the
	// loopback handler stored the credential.  This poll just reports the
	// session's own verdict.
	job.setStep("polling")
	deadline := c.now().Add(30 * time.Second)
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

func (c *Client) waitAutoSMSCode(ctx context.Context, job *raccoonAutoJob, opts core.SMSOpts, phone string) (string, error) {
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", raccoonSMSPolls, raccoonSMSInterval)
	for i := 0; i < raccoonSMSPolls; i++ {
		res, err := c.PollSMSCode(ctx, opts, phone)
		switch {
		case err != nil:
			job.logf("短信平台报错（%d/%d）：%v", i+1, raccoonSMSPolls, err)
		case res.Ready:
			job.logf("已收到验证码")
			return res.Code, nil
		default:
			job.logf("还没收到短信（%d/%d）…", i+1, raccoonSMSPolls)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(raccoonSMSInterval):
		}
	}
	return "", errors.New("等待验证码超时：短信平台一直没收到短信")
}

// raccoonBrowserDriver drives the module's own loopback login page with the
// shared CDP wrapper.  The page is ours, so every selector is stable by
// construction; the Aliyun captcha runs inside the page exactly as it does
// for a manual sign-in.
type raccoonBrowserDriver struct{}

func (raccoonBrowserDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("the login page URL is empty")
	}
	if waitCode == nil {
		return errors.New("the SMS code reader is missing")
	}
	// Visible on purpose (issue #63).  This vendor's login page runs an Aliyun
	// traceless challenge whose token is scored on environmental signals; under
	// headless=new the token it mints is rejected by the vendor IAM three
	// seconds after the send, so every attempt burned a rented phone number and
	// released it again.  The feature exists for an operator sitting at the
	// machine, so a window is the honest trade -- internal/browser documents a
	// visible window as "less likely to trip bot heuristics" for exactly this
	// reason.  A machine with no desktop still gets an error rather than a
	// silently-succeeding login that cannot work.
	br, err := browser.Launch(ctx, browser.LaunchOpts{StartURL: url, Headless: false})
	if err != nil {
		return err
	}
	defer br.Close()
	page := br.Page()
	logf("浏览器已启动，正在打开登录页…")

	if ok, err := waitRaccoonSelector(ctx, page, raccoonSelSmsTab, 15*time.Second); err != nil {
		return fmt.Errorf("等待短信登录入口失败：%w", err)
	} else if ok {
		clicked, err := page.Click(ctx, raccoonSelSmsTab)
		if err != nil {
			return fmt.Errorf("切换短信登录失败：%w", err)
		}
		if clicked {
			logf("已切换到短信登录")
		}
	}

	if ok, err := waitRaccoonSelector(ctx, page, raccoonSelPhone, 10*time.Second); err != nil {
		return fmt.Errorf("等待手机号输入框失败：%w", err)
	} else if !ok {
		return errors.New("登录页没有出现手机号输入框")
	}
	if ok, err := page.Fill(ctx, raccoonSelPhone, phone); err != nil {
		return fmt.Errorf("填写手机号失败：%w", err)
	} else if !ok {
		return errors.New("找不到手机号输入框")
	}
	logf("已填写手机号")

	if err := requestRaccoonSMS(ctx, page, logf); err != nil {
		return err
	}

	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	if ok, err := waitRaccoonSelector(ctx, page, raccoonSelCode, 10*time.Second); err != nil {
		return fmt.Errorf("等待验证码输入框失败：%w", err)
	} else if !ok {
		return errors.New("登录页没有出现验证码输入框")
	}
	if ok, err := page.Fill(ctx, raccoonSelCode, code); err != nil {
		return fmt.Errorf("填写验证码失败：%w", err)
	} else if !ok {
		return errors.New("找不到验证码输入框")
	}
	if ok, err := page.Click(ctx, raccoonSelLogin); err != nil {
		return fmt.Errorf("点击登录失败：%w", err)
	} else if !ok {
		return errors.New("找不到登录按钮")
	}
	logf("已提交验证码，等待登录结果…")
	return waitRaccoonLoginOutcome(ctx, page, logf)
}

// requestRaccoonSMS clicks 获取验证码 and does not return until the page's
// status line confirms the send went through.  The Aliyun traceless captcha
// handshake runs inside that click, so a bare click returning true means
// nothing — the status line is the only honest signal.
func requestRaccoonSMS(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) error {
	logf("正在请求发送验证码（页面内无痕验证）…")
	// One retry, in place (issue #63).  A refusal naming the challenge
	// ("图形验证码校验失败，请重新完成滑块验证") is recoverable: the widget re-arms and
	// the next click mints a fresh token.  Before this, the first refusal ended
	// the run and released the rented number, which turned a stale challenge
	// into a lost attempt.  A second refusal is reported as-is -- clicking again
	// at a vendor that keeps saying no only burns more numbers.
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// The Aliyun script wires its own listener to the button; give it a beat
		// so the click cannot land before the handler is armed.
		if !sleepRaccoon(ctx, raccoonSendSettle) {
			return ctx.Err()
		}
		lastErr = clickAndWaitRaccoonSend(ctx, page, logf)
		if lastErr == nil {
			return nil
		}
		if attempt < attempts {
			logf("发送验证码未成功（%v），重新完成验证后再试一次", lastErr)
		}
	}
	return lastErr
}

// clickAndWaitRaccoonSend clicks the send button and reads the page's verdict.
func clickAndWaitRaccoonSend(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) error {
	clicked, err := page.Click(ctx, raccoonSelSend)
	if err != nil {
		return fmt.Errorf("点击获取验证码失败：%w", err)
	}
	if !clicked {
		return errors.New("找不到获取验证码按钮")
	}
	// The status line moves through 发送中… to one of the three outcomes the
	// page can report.  "验证码已发送" is the success; anything with an error
	// tone is the vendor's refusal, passed through verbatim.
	deadline := time.Now().Add(raccoonSendWait)
	for {
		text, err := page.EvalString(ctx, raccoonStatusTextExpr)
		if err == nil {
			if strings.Contains(text, "验证码已发送") {
				logf("已请求发送验证码")
				return nil
			}
			if strings.TrimSpace(text) != "" && !strings.Contains(text, "发送中") {
				return fmt.Errorf("发送验证码失败：%s", strings.TrimSpace(text))
			}
		}
		if time.Now().After(deadline) {
			return errors.New("等待发送结果超时：页面状态一直没有变化")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// raccoonStatusTextExpr reads the SMS status line the page writes.
const raccoonStatusTextExpr = `(function(){` +
	`var e=document.getElementById('smsStatus');` +
	`return e?((e.textContent||'')+''):'';})()`

// waitRaccoonLoginOutcome waits for the page to report the login result.  The
// page sets its status line on both outcomes; the session poll afterwards is
// what reports the stored credential.
func waitRaccoonLoginOutcome(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) error {
	deadline := time.Now().Add(raccoonSendWait)
	for {
		text, err := page.EvalString(ctx, raccoonStatusTextExpr)
		if err == nil {
			if strings.Contains(text, "登录成功") {
				logf("页面报告登录成功")
				return nil
			}
			if strings.TrimSpace(text) != "" && !strings.Contains(text, "登录中") {
				return fmt.Errorf("登录失败：%s", strings.TrimSpace(text))
			}
		}
		if time.Now().After(deadline) {
			return errors.New("等待登录结果超时：页面状态一直没有变化")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func sleepRaccoon(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func waitRaccoonSelector(ctx context.Context, page raccoonAutoPage, selector string, timeout time.Duration) (bool, error) {
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
