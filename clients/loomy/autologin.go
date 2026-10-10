package loomy

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
	"client2api/internal/smscap"
)

// autologin.go implements core.AutoLoginProvider: one panel button runs the
// whole add-account flow.
//
// Loomy is the easy case among the modules that support this.  Its vendor login
// is a plain HTTP phone-code exchange -- POST sendMsgCode, then POST checkCode
// with the code -- so there is no browser and no page for the operator to
// touch.  The run is:
//
//	1. rent a number from the one-time-SMS platform (or re-issue the account's
//	   own number on a re-login), skipping numbers the pool already holds;
//	2. ask CAccount to text a login code to that number;
//	3. poll the platform until the vendor's SMS arrives and read the code out;
//	4. redeem the code for a 14-day session;
//	5. store the session, stamped with the expiry the vendor does not return.
//
// Everything after step 1 is this package's existing HTTP layer (login.go), so
// this file is orchestration and job bookkeeping only.

const (
	// defaultAutoLoginTimeout bounds one whole run.  It is generous compared to
	// a browser flow's needs because the only slow step is waiting for a text.
	defaultAutoLoginTimeout = 3 * time.Minute
	// defaultSMSPolls and defaultSMSInterval are workbuddy's values: 12 checks
	// five seconds apart is one minute of waiting per number.
	defaultSMSPolls    = 12
	defaultSMSInterval = 5 * time.Second
	// defaultDupRetries is how many extra numbers to draw when the platform
	// keeps handing back one that is already in the pool.
	defaultDupRetries = 6
	// autoJobTTL is how long a finished job stays readable, so a poller that
	// was asleep across the transition still sees the outcome.
	autoJobTTL = 30 * time.Minute
	// autoMaxLogLines caps one job's log so a pathological run cannot grow the
	// response without bound.
	autoMaxLogLines = 400
)

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
	if len(j.lines) >= autoMaxLogLines {
		return
	}
	j.lines = append(j.lines, core.AutoLogLine{
		At:   time.Now().Format(time.RFC3339),
		Text: core.Redact(fmt.Sprintf(format, args...)),
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
// missing platform credential or data directory rather than accepting a job
// that can only fail: the operator should not have to watch a log to learn that.
func (c *Client) StartAutoLogin(ctx context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	opts := smsOptsFrom(req)
	if _, err := c.newSMSClient(c.resolveSMS(opts)); err != nil {
		return core.AutoLoginJob{}, err
	}
	if c.accountsPath == "" {
		return core.AutoLoginJob{}, errors.New("no data directory: a credential could not be stored")
	}

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.autoLoginTimeout())
	job := &autoJob{
		id:        newAutoJobID(),
		state:     core.AutoLoginRunning,
		step:      "starting",
		startedAt: time.Now(),
		cancel:    cancel,
	}
	c.putAutoJob(job)
	c.logf("loomy: auto login started")
	core.GoSafe("loomy auto login", func(msg string) {
		cancel()
		job.finish(core.AutoLoginFailed, "internal error: "+msg, "")
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
	err := c.autoLogin(ctx, job, req, opts)
	switch {
	case err == nil:
		return // autoLogin recorded success
	case errors.Is(err, context.DeadlineExceeded):
		job.finish(core.AutoLoginFailed, "超时：整个流程在限定时间内没有完成", "")
	case errors.Is(err, context.Canceled):
		job.finish(core.AutoLoginCancelled, "已取消", "")
	default:
		job.finish(core.AutoLoginFailed, err.Error(), "")
	}
}

// autoLogin is the whole flow.  It is separated from runAutoLogin so a test can
// drive it without a real platform.
func (c *Client) autoLogin(ctx context.Context, job *autoJob, req core.AutoLoginRequest, opts core.SMSOpts) error {
	// 1) Rent a number, or re-issue the account's own number on a re-login.
	job.setStep("phone")
	want := strings.TrimSpace(req.Phone)
	avoid := c.autoLoginAvoid(req)
	if want == "" {
		job.logf("取号中（避开池内及本轮已试 %d 个号码）…", len(avoid))
	}
	num, err := c.AcquirePhone(ctx, opts, want, avoid)
	if err != nil {
		return fmt.Errorf("取号失败：%w", err)
	}
	job.setPhone(num.Phone)
	if num.Reused {
		job.logf("已重新占用账号手机号 %s", core.MaskSecret(num.Phone))
	} else {
		job.logf("已取号 %s（%s）", core.MaskSecret(num.Phone), num.Province)
		// A freshly rented number is handed back when the run ends.  A number
		// that belongs to the account is not ours to release.
		defer func() {
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rcancel()
			if rerr := c.ReleasePhone(rctx, opts, num.Phone, false); rerr != nil {
				c.logf("loomy: releasing %s failed: %v", core.MaskSecret(num.Phone), rerr)
			}
		}()
	}

	// 2) Ask the vendor to text the login code.
	job.setStep("login")
	msgid, err := c.up.sendSMSCode(ctx, num.Phone)
	if err != nil {
		return fmt.Errorf("发送验证码失败：%w", err)
	}
	job.logf("已向 %s 发送验证码", core.MaskSecret(num.Phone))

	// 3) Read the code back from the platform.
	code, err := c.waitAutoCode(ctx, job, opts, num.Phone)
	if err != nil {
		return err
	}
	job.logf("已收到验证码 %s", code)

	// 4) Redeem the code for a session and store it.
	job.setStep("confirming")
	session, err := c.up.loginWithSMSCode(ctx, num.Phone, code, msgid)
	if err != nil {
		return fmt.Errorf("验证码登录失败：%w", err)
	}
	rec, err := c.importSession(session, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("保存会话失败：%w", err)
	}
	job.logf("会话已保存：%s（有效期 14 天）", rec.ID)
	job.finish(core.AutoLoginSuccess, "登录成功，会话有效期 14 天", rec.ID)
	return nil
}

// waitAutoCode polls the platform for the vendor's SMS.  A poll that finds
// nothing yet is the normal case, not an error; giving up after smsPolls
// attempts is what turns "the text never arrived" into a reported failure.
func (c *Client) waitAutoCode(ctx context.Context, job *autoJob, opts core.SMSOpts, phone string) (string, error) {
	polls := c.cfg.smsPolls()
	for i := 0; i < polls; i++ {
		if i > 0 && !sleepCtx(ctx, c.cfg.smsInterval()) {
			return "", ctx.Err()
		}
		res, err := c.PollSMSCode(ctx, opts, phone)
		if err != nil {
			return "", fmt.Errorf("取验证码失败：%w", err)
		}
		if res.Ready && res.Code != "" {
			return res.Code, nil
		}
		if res.Raw != "" {
			job.logf("收到短信但未解析出验证码：%s", smscap.Truncate(res.Raw))
			continue
		}
		job.logf("等待短信验证码…（%d/%d）", i+1, polls)
	}
	return "", errors.New("超时：接码平台一直没有收到验证码")
}

// autoLoginAvoid is the set of numbers a fresh draw must skip: the numbers the
// caller has already tried in this batch (req.Avoid) plus everything the pool
// already holds.  Pool numbers are always skipped, so a batch cannot re-rent an
// account the module already carries; the batch's own history is what stops the
// platform from returning the number that just failed.
//
// The pinned restore path asks for one exact number, so Avoid does not apply
// there and neither list is consulted.
func (c *Client) autoLoginAvoid(req core.AutoLoginRequest) []string {
	if strings.TrimSpace(req.Phone) != "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, list := range [][]string{req.Avoid, c.phonePool()} {
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

// phonePool is the set of phone numbers the module already holds, so a fresh
// rent never duplicates one of them.  Only accounts that carry a phone number
// contribute; a token-only import has no number to skip.
func (c *Client) phonePool() []string {
	var out []string
	for _, a := range c.store.snapshot() {
		if p := strings.TrimSpace(a.Phone); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sleepCtx waits for d, or returns false when the context is done first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newAutoJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("auto-%d", time.Now().UnixNano())
	}
	return "auto-" + hex.EncodeToString(b[:])
}
