package raccoon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// autologin.go implements core.AutoLoginProvider for Raccoon.  The vendor's SMS
// login is a plain HTTP flow, but its Aliyun slider is minted by vendor
// JavaScript.  The run therefore rents a number, mints a slider parameter in a
// browser, sends the number through the module's normal SMS API, reads the code
// from the platform, exchanges it for a credential and stores the account.
//
// The run is a job rather than one call because a rented number can take a
// minute to receive its message: StartAutoLogin returns as soon as the
// prerequisites are checked, PollAutoLogin renders progress, and
// CancelAutoLogin stops a run the operator no longer wants.

const (
	// autoMaxLogLines caps one job's timeline.
	autoMaxLogLines = 80
	// autoJobTTL is how long a finished job stays pollable.
	autoJobTTL = 30 * time.Minute
)

// autoJob is one auto-login run.  Its log is append-only and every field is
// read under mu because the panel polls while the run goroutine writes.
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

// StartAutoLogin validates the prerequisites, records a job and returns
// immediately.  It fails fast on a missing browser, platform credential or
// data directory rather than accepting a run that can only fail.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	opts := autoSMSOpts(req)
	if _, err := c.newSMSClient(c.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if c.browserPath() == "" {
		return core.AutoLoginJob{}, errors.New("no Chromium-family browser found; install Microsoft Edge or Google Chrome, or set clients.raccoon.browser_path")
	}
	if c.accountsPath == "" {
		return core.AutoLoginJob{}, errors.New("no data directory: a credential could not be stored")
	}

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.autoLoginTimeout())
	job := &autoJob{
		id:        "raccoon-auto-" + newAutoID(),
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	core.GoSafe("raccoon auto login", func(msg string) {
		cancel()
		job.fail("internal error: " + msg)
	}, func() {
		defer cancel()
		c.runAutoLogin(runCtx, job, req, opts)
	})
	return job.snapshot(), nil
}

// PollAutoLogin renders one job.  It never touches the network.
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

// CancelAutoLogin stops a run.  Cancelling a finished job is not an error.
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

func autoSMSOpts(r core.AutoLoginRequest) core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(r.Token),
		Proxy:    strings.TrimSpace(r.Proxy),
		Keyword:  strings.TrimSpace(r.Keyword),
		Province: strings.TrimSpace(r.Province),
		CardType: strings.TrimSpace(r.CardType),
	}
}

func (c *Client) runAutoLogin(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts) {
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

// autoLogin is the whole flow.
func (c *Client) autoLogin(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	job.setStep("renting")
	num, err := c.AcquirePhone(ctx, opts, strings.TrimSpace(req.Phone), req.Avoid)
	if err != nil {
		return fmt.Errorf("取号失败：%w", err)
	}
	job.setPhone(num.Phone)
	job.logf("已取号 %s%s", num.Phone, provinceSuffix(num.Province))
	defer c.releaseAutoPhone(ctx, job, opts, num)

	job.setStep("captcha")
	job.logf("正在浏览器里完成阿里云滑块验证…")
	var param string
	if c.autoCaptcha != nil {
		param, err = c.autoCaptcha(ctx, num.Phone)
	} else {
		param, err = c.solveRaccoonCaptcha(ctx, num.Phone)
	}
	if err != nil {
		return fmt.Errorf("滑块验证失败：%w", err)
	}
	if strings.TrimSpace(param) == "" {
		return errors.New("滑块验证没有返回参数")
	}
	job.logf("滑块验证已通过")

	job.setStep("sending")
	if err := c.sendSMS(ctx, num.Phone, param); err != nil {
		return fmt.Errorf("发送验证码失败：%w", err)
	}
	job.logf("验证码已发送，等待接码平台收到短信…")

	job.setStep("sms")
	code, err := c.waitSMSCode(ctx, job, opts, num.Phone)
	if err != nil {
		return err
	}

	job.setStep("login")
	cred, err := c.loginWithSMS(ctx, num.Phone, code)
	if err != nil {
		return fmt.Errorf("验证码登录失败：%w", err)
	}
	e := c.storeCredential(ctx, cred)
	job.finish(core.AutoLoginSuccess, "登录成功，凭据已保存", e.acct.id())
	return nil
}

// storeCredential enriches, stores and rewards a fresh credential the same way
// the interactive login flow does.
func (c *Client) storeCredential(ctx context.Context, cred credential) *entry {
	if info, err := c.fetchUserInfo(ctx, cred); err == nil {
		if info.ID != "" {
			cred.UserID = info.ID
		}
		if info.Name != "" {
			cred.Nickname = info.Name
		}
		if info.Phone != "" {
			cred.Phone = info.Phone
		}
		if info.OfficeIdentity != "" {
			cred.OfficeIdentity = info.OfficeIdentity
		}
	}
	e := c.pool.put(account{credential: cred, Label: cred.displayName(), Origin: originStored})
	c.grantLoginPointsBestEffort(ctx, e)
	return e
}

// releaseAutoPhone returns a freshly rented number when the run ends.  A pinned
// number is the account's own login number, so it must be left alone.
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

// waitSMSCode polls the platform until the vendor's text arrives.
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
	return "", errors.New("接码平台在限定次数内没有收到验证码")
}

func provinceSuffix(province string) string {
	if strings.TrimSpace(province) == "" {
		return ""
	}
	return "（" + province + "）"
}

// newAutoID returns a job id without pulling in a UUID dependency.
func newAutoID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}
