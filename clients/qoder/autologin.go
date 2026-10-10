package qoder

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/browser"
	"client2api/internal/core"
)

// autologin.go is the module's "run the vendor's sign-in for me" capability
// (core.AutoLoginProvider).
//
// Qoder CN signs in through Alibaba Cloud SSO, so the flow is a browser flow and
// not an HTTP one: the phone form lives in a passport.aliyun.com iframe inside
// account.aliyun.com.  The steps below are the ones a real run takes, verified
// against the live vendor:
//
//  1. ask the module's own LoginProvider for this run's device URL, so a
//     successful run lands on exactly the account a manual sign-in would;
//  2. rent a number from the one-time-SMS platform;
//  3. open the device URL, choose 使用阿里云登录;
//  4. fill the number in the SSO iframe, tick the agreement, ask for the code;
//  5. read the code from the platform and submit it -- the vendor's text is
//     signed "qoder", which is why the keyword default matters;
//  6. if Aliyun answers with its multi-account picker (the rented number already
//     owns accounts), take the first one;
//  7. confirm the device grant on qoder.cn and poll LoginProvider until the
//     credential is stored.
//
// The run is a job rather than a call because it takes minutes: StartAutoLogin
// returns as soon as the run is accepted, PollAutoLogin is what the panel
// renders, and CancelAutoLogin stops a run the operator no longer wants.

const (
	// autoMaxLogLines caps one job's timeline so a stuck run cannot grow it
	// without bound.
	autoMaxLogLines = 80
	// autoJobTTL is how long a finished job stays pollable.
	autoJobTTL = 30 * time.Minute
)

// Selectors and frame markers of the vendor's sign-in pages.  They are the
// vendor's own ids, read from the live page rather than guessed.
const (
	// aliyunFrameMatch identifies the SSO login iframe.  The password form
	// shares the host and path but carries no qoder_sms entrance marker.
	aliyunFrameMatch  = "appEntrance=qoder_sms"
	aliyunPickerMatch = "multiAccountSelect"

	selAliyunPhone   = "#fm-sms-login-id"
	selAliyunCode    = "#fm-smscode"
	selAliyunAgree   = "#fm-agreement-checkbox"
	selAliyunSendBtn = ".send-btn-link"
	selAliyunSubmit  = "button.fm-submit.sms-login"
	selAliyunPick    = "input.next-radio-input"
	selAliyunPicker  = "button.primary-btn"
	selDeviceConfirm = "button.ant-btn-primary"
)

// ---------------------------------------------------------------------------
// The job
// ---------------------------------------------------------------------------

// autoJob is one auto-login run.  Its log is append-only and every field is read
// under mu, because the panel polls while the run goroutine writes.
type autoJob struct {
	mu         sync.Mutex
	id         string
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
	if j.phone == "" {
		j.phone = phone
	}
	j.mu.Unlock()
}

func (j *autoJob) finish(state, message, accountID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != core.AutoLoginRunning {
		return
	}
	j.state = state
	j.message = core.Redact(message)
	j.accountID = accountID
	j.step = "done"
	j.finishedAt = time.Now()
	j.expiresAt = j.finishedAt.Add(autoJobTTL)
	if message != "" {
		j.appendLocked(message)
	}
}

func (j *autoJob) fail(message string) {
	j.finish(core.AutoLoginFailed, message, "")
}

func (j *autoJob) snapshot() core.AutoLoginJob {
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

// putAutoJob stores one run and prunes the finished ones that have aged out.
func (c *Client) putAutoJob(job *autoJob) {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	if c.autoJobs == nil {
		c.autoJobs = map[string]*autoJob{}
	}
	now := time.Now()
	for id, j := range c.autoJobs {
		j.mu.Lock()
		stale := !j.expiresAt.IsZero() && now.After(j.expiresAt)
		j.mu.Unlock()
		if stale {
			delete(c.autoJobs, id)
		}
	}
	c.autoJobs[job.id] = job
}

func (c *Client) autoJobByID(id string) *autoJob {
	c.autoMu.Lock()
	defer c.autoMu.Unlock()
	return c.autoJobs[id]
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
		return core.AutoLoginJob{}, errors.New("no Chromium-family browser found; install Microsoft Edge or Google Chrome, or set clients.qoder.browser_path")
	}
	if c.accountsPath == "" {
		return core.AutoLoginJob{}, errors.New("no data directory: a credential could not be stored")
	}

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.autoLoginTimeout())
	job := &autoJob{
		id:        "qoder-auto-" + newUUID(),
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	core.GoSafe("qoder auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		c.runAutoLogin(runCtx, job, req, opts)
	})
	return job.snapshot(), nil
}

// PollAutoLogin renders one job.  It never touches the network, so the panel can
// call it as often as it likes.
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
		job.step = "done"
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
	err := c.autoLogin(ctx, job, req, opts)
	switch {
	case err == nil:
		return // autoLogin recorded success
	case errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled):
		job.finish(core.AutoLoginCancelled, "已取消", "")
	case errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		job.fail("超时：整个流程在限定时间内没有完成")
	default:
		job.fail(err.Error())
	}
}

// autoLogin is the whole flow.
func (c *Client) autoLogin(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	// 1) Ask the vendor for this run's authorisation URL.  The device poll
	//    behind it is what turns the browser sign-in into a stored credential,
	//    so everything below is really "help the operator finish this grant".
	job.setStep("authorising")
	sess, err := c.StartLogin(ctx)
	if err != nil {
		return fmt.Errorf("发起授权失败：%w", err)
	}
	job.logf("已发起授权（Qoder CN），state=%s", shortID(sess.SessionID))

	// 2) Rent the number before the browser spends time on the vendor's pages,
	//    but after the launch checks, so a bad setup fails fast.
	job.setStep("renting")
	num, err := c.AcquirePhone(ctx, opts, strings.TrimSpace(req.Phone), req.Avoid)
	if err != nil {
		return fmt.Errorf("取号失败：%w", err)
	}
	job.setPhone(num.Phone)
	job.logf("已取号 %s%s", num.Phone, provinceSuffix(num.Province))
	defer c.releaseAutoPhone(ctx, job, opts, num)

	// 3) Drive the vendor's pages.
	job.setStep("browser")
	br, err := browser.Launch(ctx, browser.LaunchOpts{
		ExecPath: c.cfg.BrowserPath,
		Headless: c.cfg.browserHeadless(),
		StartURL: sess.URL,
	})
	if err != nil {
		return fmt.Errorf("启动浏览器失败：%w", err)
	}
	defer br.Close()
	if c.cfg.browserHeadless() {
		job.logf("浏览器已启动（无界面模式，不会弹窗口；要看窗口把 browser_headless 设为 false）")
	} else {
		job.logf("浏览器已启动")
	}
	page := br.Page()
	job.logf("打开授权页…")

	if ok, err := clickByText(ctx, page, "使用阿里云登录", 30*time.Second); err != nil {
		return fmt.Errorf("授权页操作失败：%w", err)
	} else if !ok {
		return errors.New("授权页没有出现「使用阿里云登录」")
	}
	job.logf("已选择「使用阿里云登录」")

	if _, err := page.WaitFrameFor(ctx, aliyunFrameMatch, 30*time.Second); err != nil {
		return fmt.Errorf("阿里云登录框没有出现：%w", err)
	}
	job.logf("已进入阿里云账号登录")

	if ok, err := fillInFrameWhenReady(ctx, page, aliyunFrameMatch, selAliyunPhone, num.Phone, 20*time.Second); err != nil {
		return fmt.Errorf("填写手机号失败：%w", err)
	} else if !ok {
		return errors.New("阿里云登录框里没有手机号输入框")
	}
	job.logf("已填手机号 %s", num.Phone)

	if ok, _ := page.ClickInFrame(ctx, aliyunFrameMatch, selAliyunAgree); ok {
		job.logf("已勾选协议")
	}
	if ok, err := page.ClickInFrame(ctx, aliyunFrameMatch, selAliyunSendBtn); err != nil {
		return fmt.Errorf("点击「获取验证码」失败：%w", err)
	} else if !ok {
		return errors.New("没有找到「获取验证码」按钮")
	}
	job.logf("已点击「获取验证码」")

	// 4) Wait for the platform to see the vendor's text.
	job.setStep("sms")
	code, err := c.waitSMSCode(ctx, job, opts, num.Phone)
	if err != nil {
		return err
	}
	if ok, err := fillInFrameWhenReady(ctx, page, aliyunFrameMatch, selAliyunCode, code, 20*time.Second); err != nil {
		return fmt.Errorf("填写验证码失败：%w", err)
	} else if !ok {
		return errors.New("阿里云登录框里没有验证码输入框")
	}
	if ok, err := page.ClickInFrame(ctx, aliyunFrameMatch, selAliyunSubmit); err != nil {
		return fmt.Errorf("提交验证码失败：%w", err)
	} else if !ok {
		return errors.New("没有找到「登录 / 注册」按钮")
	}
	job.logf("已提交验证码")

	// 5) Aliyun answers with its account picker when the rented number already
	//    owns accounts; taking the first one is what a manual sign-in does.
	job.setStep("aliyun")
	if _, err := page.WaitFrameFor(ctx, aliyunPickerMatch, 8*time.Second); err == nil {
		job.logf("阿里云返回多账号选择页，取第一个账号")
		_, _ = page.ClickInFrame(ctx, aliyunPickerMatch, selAliyunPick)
		time.Sleep(500 * time.Millisecond)
		_, _ = page.ClickInFrame(ctx, aliyunPickerMatch, selAliyunPicker)
	}

	// 6) Confirm the device grant on qoder.cn.
	job.setStep("confirm")
	if ok, err := waitForSelector(ctx, page, selDeviceConfirm, 25*time.Second); err != nil {
		return fmt.Errorf("等待授权确认页失败：%w", err)
	} else if ok {
		if clicked, _ := page.Click(ctx, selDeviceConfirm); clicked {
			job.logf("已点击「继续」，等待授权结果…")
		}
	}

	// 7) The vendor's device poll is the authority on success.
	job.setStep("polling")
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, err := c.PollLogin(ctx, sess.SessionID)
		if err != nil {
			return fmt.Errorf("查询登录结果失败：%w", err)
		}
		switch st.State {
		case core.LoginSuccess:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "登录成功，凭据已保存"
			}
			job.finish(core.AutoLoginSuccess, msg, st.AccountID)
			return nil
		case core.LoginFailed, core.LoginCancelled:
			msg := strings.TrimSpace(st.Message)
			if msg == "" {
				msg = "厂商拒绝了这次登录"
			}
			return errors.New(msg)
		}
		if time.Now().After(deadline) {
			return errors.New("授权没有在限定时间内完成（浏览器里可能还有未完成的确认步骤）")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// releaseAutoPhone returns a freshly rented number when the run ends.  A
// pinned number is the account's own login number, so it must be left alone.
func (c *Client) releaseAutoPhone(ctx context.Context, job *autoJob, opts core.SMSOpts, num core.SMSNumber) {
	if num.Reused {
		job.logf("保留账号绑定号码 %s", num.Phone)
		return
	}
	if err := c.ReleasePhone(context.WithoutCancel(ctx), opts, num.Phone, false); err != nil {
		job.logf("释放号码失败：%v", err)
		return
	}
	job.logf("已释放号码 %s", num.Phone)
}

// waitSMSCode polls the platform until the vendor's text arrives.  A poll that
// has not seen the message yet is the normal waiting state, not a failure.
func (c *Client) waitSMSCode(ctx context.Context, job *autoJob, opts core.SMSOpts, phone string) (string, error) {
	polls := c.cfg.smsPolls()
	interval := c.cfg.smsInterval()
	job.logf("等待短信（最多 %d 次，每 %s 一次）…", polls, interval)
	for i := 0; i < polls; i++ {
		res, err := c.PollSMSCode(ctx, opts, phone)
		switch {
		case err != nil:
			job.logf("短信平台报错（%d/%d）：%v", i+1, polls, err)
		case res.Ready:
			job.logf("已收到验证码")
			return res.Code, nil
		case strings.TrimSpace(res.Raw) != "":
			job.logf("收到短信但没解析出验证码：%s", res.Raw)
		default:
			job.logf("还没收到短信（%d/%d）…", i+1, polls)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	return "", errors.New("没有等到验证码：确认接码平台的发送方关键字是「" + defaultSMSKeyword + "」后重试")
}

// ---------------------------------------------------------------------------
// Small page helpers
// ---------------------------------------------------------------------------

// clickByText finds the first element whose text contains want and clicks it.
// The vendor's sign-in page renders its providers as styled anchors without ids,
// so text is the only stable handle.
func clickByText(ctx context.Context, page *browser.Page, want string, timeout time.Duration) (bool, error) {
	expr := `(function(){var t=` + strconv.Quote(strings.Join(strings.Fields(want), "")) + `;` +
		`var els=document.querySelectorAll('a,button,span,div');` +
		`for(var i=0;i<els.length;i++){var e=els[i];` +
		`var s=(e.innerText||'').replace(/\s+/g,'');` +
		`if(s&&s.indexOf(t)>=0){e.scrollIntoView({block:'center'});e.click();return true;}}` +
		`return false;})()`
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := page.EvalBool(ctx, expr)
		if err == nil && ok {
			return true, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return false, lastErr
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// fillInFrameWhenReady fills one input inside a cross-origin frame, retrying
// while the frame is still navigating.
func fillInFrameWhenReady(ctx context.Context, page *browser.Page, match, selector, value string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := page.FillInFrame(ctx, match, selector, value)
		if err == nil && ok {
			return true, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return false, lastErr
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// waitForSelector waits for one top-document selector to appear.
func waitForSelector(ctx context.Context, page *browser.Page, selector string, timeout time.Duration) (bool, error) {
	expr := `!!document.querySelector(` + strconv.Quote(selector) + `)`
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := page.EvalBool(ctx, expr)
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
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// provinceSuffix renders "（浙江）" for a drawn number, and nothing when the
// platform chose for us.
func provinceSuffix(province string) string {
	province = strings.TrimSpace(province)
	if province == "" {
		return ""
	}
	return "（" + province + "）"
}

// shortID trims an identifier for the log.
func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}
