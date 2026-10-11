package autoclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// login.go implements the interactive login the panel drives.
//
// AutoClaw signs in with a phone number and an SMS code:
//
//	POST /userapi/v1/agent-send-code  {"phone":"..."}
//	POST /userapi/v1/agent-login      {"phone":"...","code":123456}
//
// There is no QR path.  Both endpoints are unauthenticated and answered by the
// identity gateway, so the picture here is a small loopback form that collects
// the phone number and the code, then hands the resulting credential to the
// same pool a pasted token uses.
const (
	pathLogin    = "/"
	pathSendCode = "/send-code"
	pathSignIn   = "/sign-in"

	loginBodyLimit = 16 << 10
)

var _ core.LoginProvider = (*Client)(nil)

// loginSession is one in-flight interactive login.
type loginSession struct {
	mu        sync.Mutex
	now       func() time.Time
	sessionID string
	url       string
	expiresAt time.Time

	state     string
	message   string
	accountID string
	closed    bool

	server   *http.Server
	listener net.Listener
}

func (s *loginSession) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *loginSession) terminal() bool {
	switch s.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

func (s *loginSession) set(state, message, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal() {
		return
	}
	s.state = state
	s.message = core.Redact(message)
	if accountID != "" {
		s.accountID = accountID
	}
}

func (s *loginSession) cancel() {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	if !s.terminal() {
		s.state = core.LoginCancelled
		s.message = "cancelled"
	}
	srv := s.server
	s.mu.Unlock()
	if srv != nil && !already {
		_ = srv.Close()
	}
}

func (s *loginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, message := s.state, s.message
	if state == core.LoginPending && s.clock().After(s.expiresAt) {
		state, message = core.LoginFailed, "登录超时，请重新发起"
		s.state, s.message = state, message
	}
	return core.LoginState{
		SessionID: s.sessionID,
		State:     state,
		URL:       s.url,
		Message:   core.Redact(message),
		AccountID: s.accountID,
	}
}

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	now := c.now()
	for id, other := range c.logins {
		if other == nil {
			delete(c.logins, id)
			continue
		}
		other.mu.Lock()
		expired := now.After(other.expiresAt)
		other.mu.Unlock()
		if expired {
			other.cancel()
			delete(c.logins, id)
		}
	}
	c.logins[s.sessionID] = s
}

func (c *Client) loginByID(id string) *loginSession {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// StartLogin binds a loopback page and returns its URL.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return core.LoginState{}, fmt.Errorf("autoclaw: cannot open a loopback listener: %w", err)
	}
	sessionID := "autoclaw:" + randomHex(16)
	sess := &loginSession{
		now:       c.now,
		sessionID: sessionID,
		url:       "http://" + ln.Addr().String() + pathLogin,
		expiresAt: c.now().Add(c.cfg.loginTimeout()),
		state:     core.LoginPending,
		message:   "输入手机号和验证码登录",
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pathLogin, c.handleLoginPage(sess))
	mux.HandleFunc(pathSendCode, c.handleSendCode(sess))
	mux.HandleFunc(pathSignIn, c.handleSignIn(sess))
	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	sess.listener = ln
	c.putLogin(sess)
	core.GoSafe("autoclaw login page", nil, func() { _ = sess.server.Serve(ln) })
	return sess.snapshot(), nil
}

// PollLogin reports the session's progress.  The loopback form drives the
// vendor calls, so polling only observes.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("session id is required")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("login session %q not found", id)
	}
	return sess.snapshot(), nil
}

// CancelLogin stops a session and closes its loopback listener.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("session id is required")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return fmt.Errorf("login session %q not found", id)
	}
	sess.cancel()
	return nil
}

type sendCodeReply struct {
	Result bool `json:"result"`
}

type loginReply struct {
	UserID       string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserName     string `json:"user_name"`
	UserPhone    string `json:"user_phone"`
}

func normalizePhone(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *Client) sendVerificationCode(ctx context.Context, phone string) error {
	p := normalizePhone(phone)
	if len(p) != 11 || p[0] != '1' {
		return errors.New("请输入 11 位手机号")
	}
	ctx, cancel := withTimeout(ctx, requestTimeout)
	defer cancel()
	var res sendCodeReply
	if err := c.do(ctx, http.MethodPost, pathAgentSendCode, managedIdentityHeaders(c.cfg.version()), c.authPayload(map[string]any{"phone": p}), &res); err != nil {
		return err
	}
	if !res.Result {
		return errors.New("验证码没有发送成功，请稍后再试")
	}
	return nil
}

func (c *Client) signInWithCode(ctx context.Context, phone, code string) (credential, error) {
	p := normalizePhone(phone)
	n, err := strconv.Atoi(strings.TrimSpace(code))
	if err != nil || n <= 0 {
		return credential{}, errors.New("请输入 6 位数字验证码")
	}
	ctx, cancel := withTimeout(ctx, requestTimeout)
	defer cancel()
	var res loginReply
	if err := c.do(ctx, http.MethodPost, pathAgentLogin, managedIdentityHeaders(c.cfg.version()), c.authPayload(map[string]any{"phone": p, "code": n}), &res); err != nil {
		return credential{}, err
	}
	if strings.TrimSpace(res.AccessToken) == "" || strings.TrimSpace(res.RefreshToken) == "" {
		return credential{}, errors.New("登录响应缺少 access_token 或 refresh_token")
	}
	cred := credential{
		AccountID:    strings.TrimSpace(res.UserID),
		AccessToken:  strings.TrimSpace(res.AccessToken),
		RefreshToken: strings.TrimSpace(res.RefreshToken),
		UserID:       strings.TrimSpace(res.UserID),
		Nickname:     strings.TrimSpace(res.UserName),
		Phone:        firstNonEmpty(strings.TrimSpace(res.UserPhone), p),
		DeviceID:     c.currentDeviceID(),
	}
	if ms := jwtExpiryMs(cred.AccessToken); ms > 0 {
		cred.ExpiresAt = flexString(strconv.FormatInt(ms, 10))
	}
	return cred, nil
}

// authPayload mirrors AuthApiAdapter.post: every identity-gateway auth call
// carries source_id and the device id alongside its endpoint fields.
func (c *Client) authPayload(fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["source_id"] = "autoclaw"
	if id := c.currentDeviceID(); id != "" {
		fields["device_id"] = id
	}
	return fields
}

// currentDeviceID returns the stable device identity shared by the login
// handshake and the account calls. It reuses the desktop client's id, or
// mints and persists the same document when the client has not run yet.
func (c *Client) currentDeviceID() string {
	c.deviceMu.Lock()
	defer c.deviceMu.Unlock()
	if id := strings.TrimSpace(c.deviceID); id != "" {
		return id
	}
	id := deviceIdentity()
	if id != "" {
		c.deviceID = id
	}
	return id
}

// finishLogin enriches a fresh credential, stores it and marks the session
// successful.
func (c *Client) finishLogin(ctx context.Context, sess *loginSession, cred credential) {
	if info, err := c.fetchUserInfo(ctx, cred); err == nil {
		if v := info.identity(); v != "" {
			cred.UserID = v
			if cred.AccountID == "" {
				cred.AccountID = v
			}
		}
		if v := info.display(); v != "" {
			cred.Nickname = v
		}
		if v := strings.TrimSpace(info.Phone); v != "" {
			cred.Phone = v
		}
		if v := info.numericID(); v != "" {
			cred.NumericUserID = v
		}
	}
	e := c.pool.put(account{credential: cred, Label: cred.displayName(), Origin: originStored})
	sess.set(core.LoginSuccess, "已登录 "+e.acct.label(), e.acct.id())
}

// --- loopback page ---------------------------------------------------------

func (c *Client) handleLoginPage(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(loginPageHTML))
	}
}

func (c *Client) handleSendCode(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sess.snapshot().State != core.LoginPending {
			http.Error(w, "session is finished", http.StatusGone)
			return
		}
		var body struct {
			Phone string `json:"phone"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, loginBodyLimit)).Decode(&body); err != nil {
			writeLoginError(w, "请求格式不正确")
			return
		}
		if err := c.sendVerificationCode(r.Context(), body.Phone); err != nil {
			writeLoginError(w, core.Redact(err.Error()))
			return
		}
		writeLoginJSON(w, map[string]any{"ok": true, "cooldown_seconds": 60})
	}
}

func (c *Client) handleSignIn(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sess.snapshot().State != core.LoginPending {
			http.Error(w, "session is finished", http.StatusGone)
			return
		}
		var body struct {
			Phone string `json:"phone"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, loginBodyLimit)).Decode(&body); err != nil {
			writeLoginError(w, "请求格式不正确")
			return
		}
		cred, err := c.signInWithCode(r.Context(), body.Phone, body.Code)
		if err != nil {
			writeLoginError(w, core.Redact(err.Error()))
			return
		}
		c.finishLogin(r.Context(), sess, cred)
		writeLoginJSON(w, map[string]any{"ok": true})
	}
}

func writeLoginJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeLoginError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

const loginPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>AutoClaw 登录</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f6f8;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid #e3e6ea;border-radius:10px;padding:28px 26px;width:320px;box-shadow:0 6px 24px rgba(15,23,42,.06)}
h1{font-size:18px;margin:0 0 18px}
label{display:block;font-size:13px;color:#475569;margin:12px 0 6px}
input{width:100%;box-sizing:border-box;height:38px;border:1px solid #cbd5e1;border-radius:6px;padding:0 10px;font-size:14px}
button{width:100%;height:38px;margin-top:18px;border:0;border-radius:6px;background:#2563eb;color:#fff;font-size:14px;cursor:pointer}
button:disabled{background:#94a3b8;cursor:default}
.row{display:flex;gap:8px}
.row input{flex:1}
.row button{width:112px;margin-top:0}
.msg{margin-top:12px;font-size:13px;color:#dc2626;min-height:18px}
</style>
</head>
<body>
<div class="card">
<h1>AutoClaw 登录</h1>
<label for="phone">手机号</label>
<input id="phone" inputmode="numeric" maxlength="11" placeholder="11 位手机号">
<div class="row" style="margin-top:6px">
<label for="code" style="flex:1">验证码</label>
</div>
<div class="row">
<input id="code" inputmode="numeric" maxlength="6" placeholder="6 位验证码">
<button id="send" type="button">发送验证码</button>
</div>
<button id="signin" type="button">登录</button>
<div class="msg" id="msg"></div>
</div>
<script>
const phone=document.getElementById('phone'),code=document.getElementById('code'),msg=document.getElementById('msg');
const send=document.getElementById('send'),signin=document.getElementById('signin');
let timer=null;
function show(t){msg.textContent=t||''}
function post(url,body){return fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(async r=>{const j=await r.json().catch(()=>({}));if(!r.ok||!j.ok)throw new Error(j.error||'请求失败');return j})}
send.onclick=async()=>{show('');try{await post('send-code',{phone:phone.value});show('验证码已发送');let n=60;send.disabled=true;timer=setInterval(()=>{send.textContent='重新发送('+n+'s)';if(--n<0){clearInterval(timer);send.disabled=false;send.textContent='发送验证码'}},1000)}catch(e){show(e.message)}};
signin.onclick=async()=>{show('');try{await post('sign-in',{phone:phone.value,code:code.value});signin.disabled=true;signin.textContent='登录成功';show('');msg.style.color='#16a34a';msg.textContent='登录成功，请回到控制台刷新账号列表'}catch(e){show(e.message)}};
</script>
</body>
</html>`
