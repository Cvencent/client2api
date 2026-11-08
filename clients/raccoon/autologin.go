package raccoon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// raccoonArmTimeout bounds the wait for the page's own captcha-armed signal.
// The fixed 1.5s settle it replaces was a guess at how long the Aliyun script
// needs to wire its listener onto #sendCode -- cold cache blows past it, and a
// click that lands before the wiring finishes is swallowed no matter how
// trusted the event is (issue #229, both live runs).  The page now sets
// window.__captchaArmed when initAliyunCaptcha returns; the driver polls it.
var raccoonArmTimeout = 15 * time.Second

// raccoonRefusalRearm is how long to let the challenge widget re-arm after a
// refusal, before the single in-place retry clicks again (issue #141).  The
// refusal already lowered the environment score the widget scores against, so a
// click a second and a half later is the shape most likely to be refused twice;
// this gives the widget a real window.  A var so a test can shrink it.
var raccoonRefusalRearm = 20 * time.Second

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
	// ClickTrusted clicks through the CDP input domain (isTrusted=true, issue
	// #229): the Aliyun captcha widget swallows el.click()'s synthesised
	// event, so the captcha-bound buttons must go through the same channel a
	// physical pointer uses.
	ClickTrusted(ctx context.Context, selector string) (bool, error)
	// Screenshot captures the visible viewport (PNG) for the transcript when a
	// click does not land -- the operator sees the page state at the miss.
	Screenshot(ctx context.Context) ([]byte, error)
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
	mu        sync.Mutex
	id        string
	sessionID string
	url       string
	state     string
	step      string
	phone     string
	message   string
	accountID string
	lines     []core.AutoLogLine
	// logLine, when set, mirrors each transcript line to the module log so the
	// failure is greppable in client2api.log after the job has expired
	// (issue #141).  Nil keeps a hand-built job in-memory only.
	logLine    func(format string, args ...any)
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

// redactAutoLine is the one place the transcript's text is redacted, so the ring
// copy and the file copy cannot disagree (issue #141).  core.Redact is
// idempotent, which keeps the ring's own redaction harmless.
func redactAutoLine(text string) string { return core.Redact(text) }

// appendLocked is the ONE writer of j.lines, so the mirror to the module log
// belongs here rather than in logf (issue #141).  It used to sit in logf(), and
// finish() and the cancel path call appendLocked directly -- so the lines that
// matter most for a post-mortem (the terminal failure message and "已取消")
// were the only ones that never reached client2api.log.  A guard test that only
// greps logf() cannot see that; this one asserts the mirror is in the writer.
//
// The transcript lives in an in-memory ring that expires with the job, so
// grepping client2api.log for a failure that already happened returned zero
// hits *by construction* -- not because nothing happened.  That is the trap the
// reporter fell into (#96 has the same ring-vs-file shape).
//
// Callers hold j.mu, which is why the ring append and the log line are one
// atomic step: the file's order matches the ring's.
func (j *raccoonAutoJob) appendLocked(text string) {
	text = redactAutoLine(text)
	if j.logLine != nil {
		j.logLine("%s", text)
	}
	if len(j.lines) >= raccoonAutoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: text,
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
		// The durable surface (issue #141): deps.Logf is already scoped to this
		// module and fans out to the ring, the log file and stdout.
		logLine: c.deps.Logf,
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
		// The persistent profile lives under the module's own data directory
		// (issue #141): one fixed location the vendor's device history can
		// learn across attempts, instead of a fresh throwaway every run.
		driver = raccoonBrowserDriver{profileRoot: raccoonLoginProfileRoot(c.deps.DataDir)}
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
//
// raccoonBrowserProfileName is the fixed subdirectory under the raccoon data
// directory where the auto-login browser keeps its profile.  One name, reused
// across attempts (issue #141).
const raccoonBrowserProfileName = "login-browser"

// raccoonBrowserDriver launches the visible browser with a PERSISTENT profile.
type raccoonBrowserDriver struct {
	// profileRoot is where the profile directory lives.  Nil-ish empty means
	// "derive it from the working directory", which keeps a hand-built driver
	// working; production wires the raccoon data directory in.
	profileRoot string
}

// raccoonLoginProfileRoot is where the auto-login browser profile lives.  It is
// deliberately NOT a throwaway (issue #141): a fresh os.MkdirTemp profile per
// attempt is a device with no history every single time, and the operator's
// ruling on the vendor's refusal is that the official client -- a real browser
// with real, accumulated state -- sails through where the automation did not.
// Keeping cookies, cache and the challenge's own device marks between attempts
// is the closest a second browser gets to "the real browser that works".
func raccoonLoginProfileRoot(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return filepath.Join("run", "browser", raccoonBrowserProfileName)
	}
	return filepath.Join(dataDir, raccoonBrowserProfileName)
}

func (d raccoonBrowserDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
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
	//
	// The profile is persistent on purpose (issue #141).  Launch() builds a
	// fresh throwaway profile per attempt, and the operator's side-by-side test
	// settled the question the refusal left open: the official client -- a real
	// browser with accumulated state -- is NOT even shown the slider, while
	// this automation was refused twice in a row.  A profile that vanishes on
	// Close is a device with no history every time; keeping it (cookies, cache,
	// the challenge's own device marks) is what lets the score build up the way
	// a real browser's does.  Attempt one may still be refused -- the score has
	// to start somewhere -- but the refusal now costs a number only when it is
	// genuinely fresh, not on every run for ever.
	root := d.profileRoot
	if strings.TrimSpace(root) == "" {
		root = filepath.Join("run", "browser", raccoonBrowserProfileName)
	}
	// Profile (not ProfileRoot) is the field that makes the comment above
	// true (issue #183): ProfileRoot only fixed the parent of throwaways --
	// every attempt still got a fresh os.MkdirTemp profile that Close
	// deleted, so the "device with no history" outcome the operator ruled on
	// kept happening.  Profile is used verbatim as --user-data-dir and Close
	// leaves it in place.
	br, err := browser.Launch(ctx, browser.LaunchOpts{
		StartURL: url,
		Headless: false,
		Profile:  root,
	})
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
	if ok, err := page.ClickTrusted(ctx, raccoonSelLogin); err != nil {
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
		// Wait for the page to say the captcha wiring is armed instead of
		// guessing a delay (issue #229): a click on an unarmed button is
		// swallowed silently, and no amount of re-arming fixes a shot that
		// was never taken.
		if !waitForRaccoonArmed(ctx, page, logf) {
			return ctx.Err()
		}
		lastErr = clickAndWaitRaccoonSend(ctx, page, logf)
		if lastErr == nil {
			return nil
		}
		if attempt < attempts {
			// Give the widget time to re-arm before the retry (issue #141): a
			// refusal is exactly when the environment score drops, so clicking
			// again right away is the repeat the reporter measured.
			logf("发送验证码未成功（%v），等 %s 让滑块重新武装后再试一次", lastErr, raccoonRefusalRearm)
			if !sleepRaccoon(ctx, raccoonRefusalRearm) {
				return ctx.Err()
			}
			continue
		}
	}
	return lastErr
}

// clickAndWaitRaccoonSend clicks the send button and reads the page's verdict.
// The click goes through the CDP input domain (issue #229): el.click() is
// isTrusted=false and the Aliyun widget swallowed it -- CDP said "clicked",
// the page sat still, and the run burned its wait on a button nobody pressed.
func clickAndWaitRaccoonSend(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) error {
	clicked, err := page.ClickTrusted(ctx, raccoonSelSend)
	if err != nil {
		return fmt.Errorf("点击获取验证码失败：%w", err)
	}
	if !clicked {
		return errors.New("找不到获取验证码按钮")
	}
	// 点击「发出」≠ 点击「生效」（issue #229）。The page's own handler flips
	// the button to disabled and writes the status line on EVERY accepted
	// click -- even a bad phone number gets 状态文字 -- so within a couple of
	// seconds, silence means the event never reached the widget.  Say so and
	// let the caller re-arm; waiting out the full send timeout here would
	// just relabel a dead click as a vendor outage.
	if !waitForRaccoonClickEffect(ctx, page) {
		// 按钮没进发送状态的三种原因（issue #141 操作员两轮实盯后修正）：这是
		// **无痕验证**——操作员手动点同一页面直接通过、全程无滑块；环境可信时
		// 点击静默通过，环境被识别时静默挂掉。自动化窗口里两种都可能：
		//   a) 挑战弹窗出现（环境分不足 → 升级为滑块）；
		//   b) 什么都不出现（无痕验证静默拒绝本次点击）。
		if waitForRaccoonCaptchaPopup(ctx, page) {
			logf("验证挑战（滑块）已弹出：自动流程到此为止，滑块需人工拖动或环境分通过后才会发送验证码")
			if shot, serr := page.Screenshot(ctx); serr == nil && len(shot) > 0 {
				logf("[screenshot png %d bytes]", len(shot))
				saveRaccoonScreenshot(logf, shot)
			}
			return errors.New("点击已生效：验证挑战（滑块）已弹出但未完成；滑块需人工拖动，自动化不能替拖")
		}
		logf("点击未生效：按钮没有进入发送状态，挑战弹窗也没有出现（无痕验证静默拒绝）；截取现场存入 transcript")
		if shot, serr := page.Screenshot(ctx); serr == nil && len(shot) > 0 {
			logf("[screenshot png %d bytes]", len(shot))
			saveRaccoonScreenshot(logf, shot)
		} else {
			logf("现场截屏失败：%v", serr)
		}
		// 人工接管窗口（issue #141 操作员指正「手动点一下就获取到了」）：同一
		// 窗口里人工点一下——能过 = 我们合成点击的通道/时序被识别（修点击），
		// 不能过 = 自动化环境指纹判死（修环境）。这是最快的裁决实验，同时
		// 号码在窗口内被保住，人工点到即整个流程继续走下去。
		logf("窗口保留 %s 供人工接管：请在弹出的浏览器窗口里手动点击「获取验证码」；人工点击成功后本流程自动继续", raccoonManualTakeover)
		if waitForRaccoonManualSend(ctx, page, logf) {
			logf("人工点击已生效（验证码已发送），自动流程继续")
			return nil
		}
		return errors.New("点击未生效且人工接管窗口内未完成发送：自动化点击通道或环境指纹被验证组件识别")
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

// raccoonEffectExpr reports an accepted click: the page flips sendBtn.disabled
// or writes status text on every accepted click, however the click arrived
// (automated or by hand) -- the manual-takeover watcher reads the same signal.
const raccoonEffectExpr = `(function(){` +
	`var b=document.getElementById('sendCode');` +
	`var e=document.getElementById('smsStatus');` +
	`var txt=e?((e.textContent||'')+''):'';` +
	`return (b&&b.disabled===true)||(txt.trim()!=='');})()`

// raccoonArmedExpr reads the page's captcha-armed flag; the login page sets it
// once initAliyunCaptcha has wired the click handler (issue #229).
const raccoonArmedExpr = `(function(){return window.__captchaArmed === true;})()`

// raccoonPopupExpr reports whether the Aliyun widget has rendered its
// challenge: the #captcha-element mount gains children (popup mode attaches
// the slider there), or a captcha-named iframe appears.  A click on an armed
// button in popup mode shows the slider FIRST -- the button only flips after
// the challenge is solved -- so "no button state" alone cannot distinguish a
// swallowed click from an open challenge (issue #141 operator follow-up).
const raccoonPopupExpr = `(function(){` +
	`var el=document.getElementById('captcha-element');` +
	`if(el&&el.childElementCount>0){return true;}` +
	`var fr=document.querySelectorAll('iframe');` +
	`for(var i=0;i<fr.length;i++){` +
	`var tag=((fr[i].id||'')+' '+(fr[i].className||'')+' '+(fr[i].src||'')).toLowerCase();` +
	`if(tag.indexOf('captcha')>=0){return true;}` +
	`}` +
	`return false;})()`

// waitForRaccoonCaptchaPopup polls for the challenge UI for two seconds.
func waitForRaccoonCaptchaPopup(ctx context.Context, page raccoonAutoPage) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ok, err := page.EvalBool(ctx, raccoonPopupExpr); err == nil && ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// raccoonManualTakeover is how long the browser window stays open for the
// operator to click the button by hand after an automated click was silently
// dropped.
var raccoonManualTakeover = 60 * time.Second

// waitForRaccoonManualSend watches the status line (and button state) for the
// duration of the takeover window: the operator's own click, if it lands,
// drives the page into 发送中…/验证码已发送 exactly like an automated one
// would have.  A cancellation ends the wait immediately.
func waitForRaccoonManualSend(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) bool {
	deadline := time.Now().Add(raccoonManualTakeover)
	for {
		if ok, err := page.EvalBool(ctx, raccoonEffectExpr); err == nil && ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// waitForRaccoonArmed polls the page's own armed signal until it flips or the
// deadline passes.  Clicking before the signal is a wasted attempt either way:
// the event is swallowed, the effect probe reports the miss, and the rented
// number burns.
func waitForRaccoonArmed(ctx context.Context, page raccoonAutoPage, logf func(string, ...any)) bool {
	if ok, err := page.EvalBool(ctx, raccoonArmedExpr); err == nil && ok {
		return true
	}
	deadline := time.Now().Add(raccoonArmTimeout)
	logf("等待验证组件武装（页面信号 window.__captchaArmed）…")
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
		if ok, err := page.EvalBool(ctx, raccoonArmedExpr); err == nil && ok {
			return true
		}
		if time.Now().After(deadline) {
			logf("验证组件武装信号在 %s 内未出现，仍按原计划点击（页面可能在弹挑战）", raccoonArmTimeout)
			return true
		}
	}
}

// saveRaccoonScreenshot writes the swallowed-click snapshot next to the
// module's own log target so the operator sees the page state at the miss
// (issue #229).  The transcript carries the byte count; the file carries the
// pixels.
// screenshotDir is a var so tests can redirect the evidence files.
var screenshotDir = "data/raccoon/login-browser-shots"

func saveRaccoonScreenshot(logf func(string, ...any), shot []byte) {
	dir := screenshotDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("截屏目录创建失败：%v", err)
		return
	}
	name := filepath.Join(dir, "click-not-effective-"+time.Now().Format("20060102-150405")+".png")
	if err := os.WriteFile(name, shot, 0o600); err != nil {
		logf("截屏写入失败：%v", err)
		return
	}
	logf("现场截屏已保存：%s", name)
}

// waitForRaccoonClickEffect reports whether the send click actually took:
// the page flips sendBtn.disabled or writes any status text on an accepted
// click, so two seconds of silence on BOTH is a swallowed event (issue #229).
func waitForRaccoonClickEffect(ctx context.Context, page raccoonAutoPage) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ok, err := page.EvalBool(ctx, raccoonEffectExpr); err == nil && ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
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
