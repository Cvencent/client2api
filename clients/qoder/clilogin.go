package qoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// clilogin.go adds the CLI sign-in as a second way to add an account.
//
// The two paths produce different capabilities and neither replaces the other:
//
//   - the browser (device-token) grant lands on the OpenAPI credential the
//     credit ledger, the daily check-in and the task board all read, and on the
//     signed gateway path for every model except the two free ones; and
//   - this CLI sign-in lands on a per-account config root, which is the only
//     thing the vendor will serve the two free models to.
//
// The panel already offers a realm picker for a module with two upstreams, so
// the choice is exposed as two realms ("cli" and "browser") and the panel needs
// no changes at all.  An account signed in both ways is one account row, not
// two: both paths resolve the vendor's own user id, which is what the store
// keys on.

// cliRealm and browserRealm are the two sign-in routes this module offers.
const (
	cliRealm     = "cli"
	browserRealm = "browser"
)

// cliLoginWait bounds how long StartLogin waits for the CLI to print its
// device URL.  The worker's own startup includes a security-scan hook, so this
// is generous; the operator sees a spinner either way.
const cliLoginWait = 60 * time.Second

// cliLoginTTL is how long an unfinished CLI sign-in is kept before it is
// dropped.  It matches the vendor's own device window.
const cliLoginTTL = 5 * time.Minute

// cliLoginURLPattern finds the device URL in the CLI's own progress output.
var cliLoginURLPattern = regexp.MustCompile(`https://[^\s"'<>]+`)

// cliWelcomePattern reads the account name out of the CLI's final line.
var cliWelcomePattern = regexp.MustCompile(`Login successful!\s*Welcome,\s*(\S+)`)

// cliUIDPattern reads the vendor's user id out of the avatar URL `status`
// prints.  Both sign-in routes resolve the same id, which is what lets an
// account signed in twice stay one row.
var cliUIDPattern = regexp.MustCompile(`/users/([0-9a-fA-F-]{36})/`)

// cliLoginSession is one in-flight CLI sign-in.  It lives in memory only: a
// login that survives a restart is a login nobody can reason about.
type cliLoginSession struct {
	id        string
	leaf      string
	dir       string
	expiresAt time.Time

	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu        sync.Mutex
	url       string
	who       string
	ok        bool
	message   string
	cancelled bool
	finished  bool
}

func (s *cliLoginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := core.LoginPending
	message := s.message
	switch {
	case s.cancelled:
		state = core.LoginCancelled
	case s.ok:
		state = core.LoginSuccess
	case s.finished:
		state = core.LoginFailed
	case time.Now().After(s.expiresAt):
		state = core.LoginFailed
		message = "登录等待已超时，请重新发起登录。"
	}
	return core.LoginState{
		SessionID: s.id,
		State:     state,
		URL:       s.url,
		Message:   core.Redact(message),
	}
}

func (s *cliLoginSession) setURL(u string) {
	s.mu.Lock()
	if s.url == "" {
		s.url = u
	}
	s.mu.Unlock()
}

func (s *cliLoginSession) succeed(who string) {
	s.mu.Lock()
	s.ok, s.who = true, who
	s.mu.Unlock()
}

func (s *cliLoginSession) fail(msg string) {
	s.mu.Lock()
	if !s.ok {
		s.message = msg
	}
	s.finished = true
	s.mu.Unlock()
}

// stop terminates the login child.  It is safe to call more than once.
func (s *cliLoginSession) stop() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// ---------------------------------------------------------------------------
// LoginProvider
// ---------------------------------------------------------------------------

// StartLogin keeps the historical behaviour: the device-token browser grant.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	return c.StartLoginRealm(ctx, browserRealm)
}

// LoginRealms implements core.RealmLoginProvider.  The local CLI is listed
// first when it is actually installed, because it is the route the free models
// need; a machine without it still gets the browser grant as the default rather
// than an error from a picker entry that cannot work.
func (c *Client) LoginRealms(ctx context.Context) []core.LoginRealm {
	_ = ctx
	realmCLI := core.LoginRealm{
		Code: cliRealm,
		Name: "本机 CLI 登录（免费模型）",
		Help: "需要本机安装 Qoder CN 客户端。登录后由 client2api 在本机驱动官方 CLI，" +
			"可调用 DeepSeek-Flash / Qwen3.8-Flash 两个免费模型。",
	}
	realmBrowser := core.LoginRealm{
		Code: browserRealm,
		Name: "浏览器登录（积分/签到/其它模型）",
		Help: "在浏览器里完成设备授权。积分查询、每日签到、任务和其余模型都依赖这份凭据。",
	}
	if _, err := findCLI(); err != nil {
		realmCLI.Help = "本机没有找到 Qoder CN 客户端：" + realmCLI.Help
		return []core.LoginRealm{realmBrowser, realmCLI}
	}
	return []core.LoginRealm{realmCLI, realmBrowser}
}

// StartLoginRealm starts one sign-in on the chosen realm.
func (c *Client) StartLoginRealm(ctx context.Context, realm string) (core.LoginState, error) {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "", browserRealm:
		return c.startBrowserLogin(ctx)
	case cliRealm:
		return c.startCLILogin(ctx)
	default:
		return core.LoginState{}, fmt.Errorf(
			"qoder: unknown sign-in realm %q (want %q or %q)", realm, cliRealm, browserRealm)
	}
}

// ---------------------------------------------------------------------------
// the CLI sign-in
// ---------------------------------------------------------------------------

// startCLILogin launches the CLI's own `login` subcommand and waits only for
// its device URL.  The child keeps running until the operator confirms in a
// browser; PollLogin is what notices the confirmation.
func (c *Client) startCLILogin(ctx context.Context) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	inst, err := findCLI()
	if err != nil {
		return core.LoginState{}, err
	}
	if err := core.EnsureDir(c.cliRootDir()); err != nil {
		return core.LoginState{}, fmt.Errorf("qoder: creating the CLI data directory: %w", err)
	}

	nonce := newUUID()
	sess := &cliLoginSession{
		id:        "qoder-cli-login-" + nonce,
		leaf:      c.cliStageLeaf(nonce),
		expiresAt: time.Now().Add(cliLoginTTL),
		done:      make(chan struct{}),
		message:   "正在启动本机 Qoder CLI（首次启动约 10–20 秒），请稍候……",
	}
	sess.dir = c.cliConfigDir(sess.leaf)
	if err := core.EnsureDir(sess.dir); err != nil {
		return core.LoginState{}, fmt.Errorf("qoder: creating the CLI config directory: %w", err)
	}

	cmd := newCLICmd(inst, sess.dir, "--config-dir", sess.dir, "login")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return core.LoginState{}, fmt.Errorf("qoder: cli login stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return core.LoginState{}, fmt.Errorf("qoder: cli login stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return core.LoginState{}, fmt.Errorf("qoder: cli login stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return core.LoginState{}, fmt.Errorf("%w: qoder: starting the Qoder CLI: %v", ErrCLIMissing, err)
	}
	sess.cmd, sess.stdin = cmd, stdin

	urlFound := make(chan struct{})
	var once sync.Once
	core.GoSafe("qoder cli login reader", func(msg string) { c.logf("qoder: %s", msg) }, func() {
		_ = scanLines(stdout, func(line []byte) error {
			text := string(line)
			if m := cliWelcomePattern.FindStringSubmatch(text); len(m) == 2 {
				sess.succeed(strings.TrimSpace(m[1]))
			} else if u := cliLoginURLPattern.FindString(text); u != "" {
				sess.setURL(strings.TrimRight(u, ".,)"))
				once.Do(func() { close(urlFound) })
			}
			return nil
		})
	})
	core.GoSafe("qoder cli login stderr", nil, func() {
		raw, _ := io.ReadAll(io.LimitReader(stderr, 8<<10))
		if msg := strings.TrimSpace(string(raw)); msg != "" {
			c.logf("qoder: cli login stderr: %s", core.Redact(msg))
		}
	})
	core.GoSafe("qoder cli login wait", nil, func() {
		_ = cmd.Wait()
		close(sess.done)
		// The child exits as soon as the browser confirms, and it writes the
		// credential in the same breath.  Give that write a moment to land
		// before calling the exit a failure, so a successful sign-in is not
		// recorded as an error by a millisecond of scheduling.
		for i := 0; i < 20; i++ {
			if cliSignedIn(sess.dir) || sess.snapshot().State != core.LoginPending {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		if sess.snapshot().State == core.LoginPending {
			sess.fail("本机 Qoder CLI 的登录进程已退出，可能是设备码过期或被取消。")
		}
	})

	c.putCLILogin(sess)
	// The child outlives StartLogin, so it needs its own deadline: an operator
	// who starts a sign-in and walks away must not leave an Electron process
	// holding a half-finished login until the next attempt.
	core.GoSafe("qoder cli login ttl", nil, func() {
		select {
		case <-sess.done:
		case <-time.After(cliLoginTTL + 30*time.Second):
			sess.stop()
		}
	})

	select {
	case <-urlFound:
		// The URL is on screen; from here the operator's phone is the only
		// thing that can move the flow forward, so say so instead of leaving
		// the generic "starting the CLI" line up.
		sess.mu.Lock()
		sess.message = "请在浏览器/手机里确认登录；确认后本页会自动完成，无需再点任何按钮。"
		sess.mu.Unlock()
		return sess.snapshot(), nil
	case <-sess.done:
		st := sess.snapshot()
		if st.State == core.LoginSuccess {
			return st, nil
		}
		if st.Message == "" {
			st.Message = "本机 Qoder CLI 未能返回登录链接。"
		}
		return st, nil
	case <-time.After(cliLoginWait):
		sess.stop()
		sess.fail("等待本机 Qoder CLI 返回登录链接超时。")
		return sess.snapshot(), nil
	case <-ctx.Done():
		sess.stop()
		sess.fail("登录已取消。")
		return sess.snapshot(), nil
	}
}

// PollLogin advances a session.  The panel owns the cadence, so a CLI session
// is checked rather than driven: the child is doing its own polling, and all
// this has to notice is that it exited successfully.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("qoder: PollLogin: empty session id")
	}
	if sess := c.cliLoginByID(id); sess != nil {
		return c.pollCLILogin(ctx, sess)
	}
	return c.pollBrowserLogin(ctx, id)
}

// CancelLogin stops a session, whichever realm it belongs to.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	_ = ctx
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("qoder: CancelLogin: empty session id")
	}
	if sess := c.cliLoginByID(id); sess != nil {
		sess.mu.Lock()
		sess.cancelled = true
		sess.mu.Unlock()
		sess.stop()
		c.dropCLILogin(id)
		// A cancelled login leaves a staged root behind only when the vendor
		// had already written credentials into it; the ordinary case removes
		// the directory so failed attempts do not accumulate under DataDir.
		c.removeStagedRoot(sess.leaf)
		return nil
	}
	return c.cancelBrowserLogin(id)
}

// pollCLILogin advances one CLI session.
func (c *Client) pollCLILogin(ctx context.Context, sess *cliLoginSession) (core.LoginState, error) {
	st := sess.snapshot()
	if st.State == core.LoginSuccess || st.State == core.LoginCancelled {
		return st, nil
	}
	if st.State == core.LoginFailed && !cliSignedIn(sess.dir) {
		return st, nil
	}
	// A "failed" session whose config root DOES hold a credential is not a
	// failure: the login child exits the moment the browser confirms, and that
	// exit used to be recorded as a failure a beat before the credential was
	// visible.  The store is the authority, so fall through and adopt it.
	if st.State == core.LoginFailed {
		st = core.LoginState{SessionID: sess.id, State: core.LoginPending, URL: st.URL}
	}
	sess.mu.Lock()
	who := sess.who
	sess.mu.Unlock()

	// The credential store is the authority, not the CLI's console output.
	//
	// This used to wait for a "who" parsed out of the CLI's success line before
	// it would look at the store at all.  When that line's wording did not
	// match, a sign-in that had already written a perfectly good credential sat
	// in "pending" forever and the panel spun on "请稍后" -- the store was right
	// there the whole time.  Reading the store unconditionally, and treating a
	// readable credential as the success signal, is what makes the wait end.
	if !cliSignedIn(sess.dir) {
		// Nothing written yet: the browser step has not finished.  A child that
		// has already exited without writing one is a failure, not a wait.
		select {
		case <-sess.done:
			return sess.snapshot(), nil
		default:
			return st, nil
		}
	}

	creds, err := c.readCLICredentials(ctx, sess.dir)
	if err != nil {
		// Fall back to the old console-identity path so a store the helper
		// cannot open does not turn a successful sign-in into a failure.
		c.logf("qoder: reading the CLI credential store after login: %v", err)
		accountID, label, userType, ierr := c.cliIdentity(ctx, sess.dir, who)
		if ierr != nil {
			sess.fail(redactErr(ierr))
			return sess.snapshot(), nil
		}
		leaf := c.adoptCLIDir(sess.leaf, accountID)
		if !cliSignedIn(c.cliConfigDir(leaf)) {
			sess.fail("本机 CLI 报告登录成功，但没有写出凭据文件。")
			return sess.snapshot(), nil
		}
		sess.mu.Lock()
		sess.leaf = leaf
		sess.mu.Unlock()
		acc := account{
			storedAccount: storedAccount{
				ID:       accountID,
				Label:    label,
				Name:     label,
				UserType: userType,
				CLIDir:   leaf,
				Enabled:  true,
			},
			origin: originStored,
		}
		if err := c.store.attachCLIDir(acc); err != nil {
			sess.fail("登录成功，但凭据无法写入：" + redactErr(err))
			return sess.snapshot(), nil
		}
		// Mark the terminal state through succeed(), not by assigning message:
		// snapshot() decides "success" from ok, so a bare message assignment
		// left the panel polling a login that had already been stored.
		sess.succeed(label)
		sess.mu.Lock()
		sess.message = "本机 CLI 登录成功，已保存 " + label + "（凭据读取失败，仅免费模型可用：" + redactErr(err) + "）"
		sess.mu.Unlock()
		c.dropCLILogin(sess.id)
		return sess.snapshot(), nil
	}

	accountID := "qoder-" + creds.UID
	label := firstNonEmpty(creds.Name, who, "Qoder CLI 账号")
	// adoptCLIDir MOVES the staged root onto the account's own name, so the
	// leaf this session was created with no longer exists afterwards.  Carry
	// the returned leaf through, or the check below looks in the old path and
	// reports a missing credential the move just created.
	leaf := c.adoptCLIDir(sess.leaf, accountID)
	if !cliSignedIn(c.cliConfigDir(leaf)) {
		sess.fail("本机 CLI 报告登录成功，但没有写出凭据文件。")
		return sess.snapshot(), nil
	}
	sess.mu.Lock()
	sess.leaf = leaf
	sess.mu.Unlock()
	acc := account{
		storedAccount: storedAccount{
			ID:       accountID,
			Label:    label,
			Name:     label,
			UserType: creds.UserType,
			CLIDir:   leaf,
			Enabled:  true,
			// The token the ledger, check-in and the signed gateway all use.
			// Carrying it here is what makes a single CLI sign-in enough: the
			// account gets the free models from the CLI and everything else
			// from this credential, with no desktop client involved.
			Token:              creds.bearer(),
			RefreshToken:       strings.TrimSpace(creds.RefreshToken),
			UserID:             creds.UID,
			ExpiresAtMS:        creds.ExpireTime * 1000,
			RefreshExpiresAtMS: creds.RefreshExpireTime * 1000,
		},
		origin: originStored,
	}
	if err := c.store.attachCLIDir(acc); err != nil {
		sess.fail("登录成功，但凭据无法写入：" + redactErr(err))
		return sess.snapshot(), nil
	}
	// succeed() is what snapshot() turns into LoginSuccess; assigning message
	// alone would leave the panel polling a sign-in that already succeeded.
	sess.succeed(label)
	sess.mu.Lock()
	sess.message = "本机 CLI 登录成功，已保存 " + label
	sess.mu.Unlock()
	c.dropCLILogin(sess.id)
	return sess.snapshot(), nil
}

// cliIdentity reads who just signed in.  The CLI's `status` subcommand prints
// the account name and an avatar URL that carries the vendor's own user id;
// that id is what keeps a CLI-signed-in account on the same row as the same
// account imported from the desktop client.
func (c *Client) cliIdentity(ctx context.Context, dir, who string) (id, label, userType string, err error) {
	inst, err := findCLI()
	if err != nil {
		return "", "", "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()

	cmd := newCLICmdCtx(rctx, inst, dir, "--config-dir", dir, "status")
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		// `status` is a convenience: without it the login is still usable, it
		// just cannot be matched onto an existing account row.
		c.logf("qoder: cli status after login: %v", err)
		return c.fallbackCLIIdentity(who), firstNonEmpty(who, "Qoder CLI 账号"), "", nil
	}
	status := parseCLIStatus(string(out))
	name := firstNonEmpty(status.Username, who, "Qoder CLI 账号")
	if uid := status.UserID; uid != "" {
		return "qoder-" + uid, name, status.UserType, nil
	}
	return c.fallbackCLIIdentity(name), name, status.UserType, nil
}

// fallbackCLIIdentity derives a stable id when the vendor's user id could not
// be read.  It hashes the account name rather than the config root so a
// re-login of the same account lands on the same row.
func (c *Client) fallbackCLIIdentity(name string) string {
	return "qoder-cli-" + shortHash(strings.ToLower(strings.TrimSpace(name)))
}

// cliStatus is the part of `qoderclicn status` this module reads.
type cliStatus struct {
	Version  string
	Username string
	UserID   string
	UserType string
	Method   string
}

// parseCLIStatus reads the key/value block the CLI prints.
func parseCLIStatus(out string) cliStatus {
	var st cliStatus
	for _, raw := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "version":
			st.Version = value
		case "username":
			st.Username = value
		case "user type":
			st.UserType = value
		case "login method":
			st.Method = value
		case "avatar":
			if m := cliUIDPattern.FindStringSubmatch(value); len(m) == 2 {
				st.UserID = m[1]
			}
		}
	}
	return st
}

// ---------------------------------------------------------------------------
// CLI login session bookkeeping
// ---------------------------------------------------------------------------

func (c *Client) putCLILogin(sess *cliLoginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.cliLogins == nil {
		c.cliLogins = make(map[string]*cliLoginSession)
	}
	now := time.Now()
	for id, other := range c.cliLogins {
		if now.After(other.expiresAt) {
			other.stop()
			delete(c.cliLogins, id)
		}
	}
	c.cliLogins[sess.id] = sess
}

func (c *Client) cliLoginByID(id string) *cliLoginSession {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.cliLogins[id]
}

func (c *Client) dropCLILogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.cliLogins, id)
}

// stopCLILogins terminates every in-flight CLI sign-in.  The service calls it
// on shutdown so no orphaned login child outlives the process.
func (c *Client) stopCLILogins() {
	c.loginMu.Lock()
	sessions := c.cliLogins
	c.cliLogins = nil
	c.loginMu.Unlock()
	for _, sess := range sessions {
		sess.stop()
	}
}

// removeStagedRoot deletes a login's staged config root when it holds no
// credentials.  It is deliberately a no-op on anything that is signed in, and
// on anything outside this module's own CLI root, so a mis-stored leaf can
// never delete account state.
func (c *Client) removeStagedRoot(leaf string) {
	if strings.TrimSpace(leaf) == "" {
		return
	}
	dir := c.cliConfigDir(leaf)
	root := c.cliRootDir()
	if root == "" || !pathWithin(root, dir) || cliSignedIn(dir) {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		c.logf("qoder: removing the staged CLI root %s: %v", dir, err)
	}
}
