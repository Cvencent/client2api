package raccoon

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The loopback login page.
//
// The page is deliberately dumb: it renders a QR SVG the host produced and
// posts forms.  It never learns the qrcode_code, the phone number's cipher
// key, or any token.  Everything sensitive stays in the host process.
// ---------------------------------------------------------------------------

const (
	// Paths the loopback server serves.  They are also inlined into the page's
	// script, so a change here cannot desynchronise the two.
	pathLogin     = "/raccoon/login"
	pathPoll      = "/raccoon/poll"
	pathSmsSend   = "/raccoon/sms/send"
	pathSmsVerify = "/raccoon/sms/verify"

	// Aliyun slider captcha parameters, transcribed from the vendor's own
	// front-end bundle.  They are public client-side constants.
	aliyunCaptchaScript  = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
	aliyunCaptchaSceneID = "1pkmy0x3"
	aliyunCaptchaPrefix  = "hk1r5l"
)

// phonePattern is the mainland mobile number the vendor's own form accepts.
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// handleLoginPage renders the page with the current QR code.
func (c *Client) handleLoginPage(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		sess.mu.Lock()
		code := sess.qrCode
		sess.mu.Unlock()
		svg, err := renderQRSVG(c.qrLoginURL(code), 200)
		if err != nil {
			http.Error(w, "raccoon: cannot render the QR code", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, renderLoginPage(svg))
	}
}

// handleLoginPoll advances the QR state once and reports it to the page.
func (c *Client) handleLoginPoll(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		c.pollQR(r.Context(), sess)

		sess.mu.Lock()
		state := sess.state
		status := sess.qrStatus
		expiredAt := sess.expiredAt
		code := sess.qrCode
		sess.mu.Unlock()
		switch state {
		case core.LoginSuccess:
			status = qrStatusSuccess
		case core.LoginCancelled:
			status = qrStatusCanceled
		case core.LoginFailed:
			status = "failed"
		}

		payload := map[string]any{"status": status}
		if expiredAt != "" {
			payload["expiredAt"] = expiredAt
		}
		if status == qrStatusCanceled {
			if svg, err := renderQRSVG(c.qrLoginURL(code), 200); err == nil {
				payload["qr"] = svg
			}
		}
		writeLoginJSON(w, http.StatusOK, payload)
	}
}

// handleSmsSend validates the form, then asks the vendor to text a code.
func (c *Client) handleSmsSend(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := readLoginBody(w, r)
		if body == nil {
			return
		}
		phone := strings.TrimSpace(asString(body["phone"]))
		captcha := strings.TrimSpace(asString(body["captchaParam"]))
		if !phonePattern.MatchString(phone) {
			writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请输入有效的 11 位手机号"})
			return
		}
		if captcha == "" {
			writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请先完成滑块验证"})
			return
		}
		if err := c.sendSMS(r.Context(), phone, captcha); err != nil {
			// A failed send does NOT end the session: the slider may simply
			// have expired, and the operator can retry in place.
			writeLoginJSON(w, http.StatusOK, map[string]any{"ok": false, "message": c.scrub(err.Error())})
			return
		}
		sess.mu.Lock()
		if !sess.terminal() {
			sess.phone = phone
		}
		sess.mu.Unlock()
		writeLoginJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSmsVerify exchanges the verification code for a credential.
func (c *Client) handleSmsVerify(sess *loginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := readLoginBody(w, r)
		if body == nil {
			return
		}
		code := strings.TrimSpace(asString(body["smsCode"]))
		sess.mu.Lock()
		phone := sess.phone
		sess.mu.Unlock()
		if phone == "" {
			writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请先获取手机验证码"})
			return
		}
		if code == "" {
			writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请输入验证码"})
			return
		}
		cred, err := c.loginWithSMS(r.Context(), phone, code)
		if err != nil {
			writeLoginJSON(w, http.StatusOK, map[string]any{"ok": false, "message": c.scrub(err.Error())})
			return
		}
		c.finishLogin(r.Context(), sess, cred)
		writeLoginJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// readLoginBody decodes a small JSON object.  A malformed body is answered
// here and reported as nil.
func readLoginBody(w http.ResponseWriter, r *http.Request) map[string]any {
	raw, err := io.ReadAll(io.LimitReader(r.Body, loginBodyLimit))
	if err != nil {
		writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "无法读取请求体"})
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		writeLoginJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
		return nil
	}
	return out
}

func writeLoginJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// asString renders a decoded JSON value as text.
func asString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(jsonFloat(s), "0"), ".")
	}
	return ""
}

func jsonFloat(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// renderLoginPage builds the HTML.  Exported state is passed in as a finished
// SVG so the page needs no QR logic of its own.
func renderLoginPage(qrSVG string) string {
	page := loginPageTemplate
	replacements := map[string]string{
		"__QR_SVG__":         qrSVG,
		"__POLL_PATH__":      pathPoll,
		"__SMS_SEND__":       pathSmsSend,
		"__SMS_VERIFY__":     pathSmsVerify,
		"__CAPTCHA_SRC__":    aliyunCaptchaScript,
		"__CAPTCHA_SCENE__":  aliyunCaptchaSceneID,
		"__CAPTCHA_PREFIX__": aliyunCaptchaPrefix,
	}
	for k, v := range replacements {
		page = strings.ReplaceAll(page, k, v)
	}
	return page
}

const loginPageTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Raccoon Work 登录</title>
<style>
  :root { color-scheme: light; }
  body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
         font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", sans-serif;
         background: #f5f6f8; color: #1f2329; }
  .card { background: #fff; border-radius: 12px; padding: 20px 24px 24px; box-shadow: 0 4px 24px rgba(0,0,0,.08);
          text-align: center; width: 320px; }
  h1 { font-size: 16px; margin: 0 0 14px; font-weight: 600; }
  .tabs { display: flex; gap: 4px; background: #f2f3f5; border-radius: 8px; padding: 4px; margin-bottom: 16px; }
  .tabs button { flex: 1; padding: 7px; font-size: 13px; border: 0; border-radius: 6px;
                 background: transparent; color: #4e5969; cursor: pointer; }
  .tabs button[data-active="1"] { background: #fff; color: #1f2329; font-weight: 600;
                                  box-shadow: 0 1px 3px rgba(0,0,0,.08); }
  .pane { display: none; }
  .pane[data-show="1"] { display: block; }
  .qr { width: 200px; height: 200px; display: block; margin: 0 auto; border: 1px solid #eceef1;
        border-radius: 8px; padding: 6px; box-sizing: content-box; background: #fff; }
  .qr svg { display: block; width: 100%; height: 100%; }
  .sub { font-size: 12px; color: #8a9099; margin: 10px 0 0; }
  .status { margin-top: 12px; font-size: 13px; color: #4e5969; min-height: 20px; }
  .status[data-tone="ok"] { color: #0f9d58; font-weight: 600; }
  .status[data-tone="err"] { color: #d93026; }
  label { display: block; font-size: 12px; color: #8a9099; margin-bottom: 4px; text-align: left; }
  input { width: 100%; box-sizing: border-box; padding: 8px 10px; font-size: 14px; margin-bottom: 10px;
          border: 1px solid #d9dde3; border-radius: 6px; }
  button.primary { width: 100%; padding: 9px; font-size: 14px; border: 0; border-radius: 6px;
                   background: #8E6BF2; color: #fff; cursor: pointer; }
  button.primary:disabled { background: #c9cdd4; cursor: not-allowed; }
  .row { display: flex; gap: 8px; align-items: flex-start; }
  #captcha-element { margin-bottom: 10px; }
</style>
</head>
<body>
  <div class="card">
    <h1>登录 Raccoon Work</h1>

    <div class="tabs">
      <button id="tabQr" data-active="1" type="button">微信扫码</button>
      <button id="tabSms" data-active="0" type="button">短信登录</button>
    </div>

    <div class="pane" id="paneQr" data-show="1">
      <div class="qr" id="qrBox">__QR_SVG__</div>
      <p class="sub">打开微信扫一扫，扫描上方二维码</p>
      <div class="status" id="qrStatus">等待扫码…</div>
    </div>

    <div class="pane" id="paneSms" data-show="0">
      <label for="phone">手机号</label>
      <div class="row">
        <input id="phone" type="tel" inputmode="numeric" maxlength="11" placeholder="请输入 11 位手机号">
        <button class="primary" id="sendCode" type="button" style="width:auto;white-space:nowrap;padding:9px 12px;">获取验证码</button>
      </div>
      <div id="captcha-element"></div>
      <label for="smsCode">验证码</label>
      <input id="smsCode" type="text" inputmode="numeric" maxlength="6" placeholder="请输入 6 位验证码">
      <button class="primary" id="doLogin" type="button">登录</button>
      <div class="status" id="smsStatus"></div>
    </div>
  </div>

<script>
(function () {
  'use strict';
  var POLL_INTERVAL_MS = 2000;
  var POLL_PATH = '__POLL_PATH__';
  var SMS_SEND_PATH = '__SMS_SEND__';
  var SMS_VERIFY_PATH = '__SMS_VERIFY__';
  var CAPTCHA_SRC = '__CAPTCHA_SRC__';
  var CAPTCHA_SCENE = '__CAPTCHA_SCENE__';
  var CAPTCHA_PREFIX = '__CAPTCHA_PREFIX__';

  var tabQr = document.getElementById('tabQr');
  var tabSms = document.getElementById('tabSms');
  var paneQr = document.getElementById('paneQr');
  var paneSms = document.getElementById('paneSms');
  function selectTab(sms) {
    tabQr.setAttribute('data-active', sms ? '0' : '1');
    tabSms.setAttribute('data-active', sms ? '1' : '0');
    paneQr.setAttribute('data-show', sms ? '0' : '1');
    paneSms.setAttribute('data-show', sms ? '1' : '0');
    if (sms) { ensureCaptcha(); }
  }
  tabQr.addEventListener('click', function () { selectTab(false); });
  tabSms.addEventListener('click', function () { selectTab(true); });

  var qrStatus = document.getElementById('qrStatus');
  var qrBox = document.getElementById('qrBox');
  function setQrStatus(text, tone) {
    qrStatus.textContent = text;
    if (tone) { qrStatus.setAttribute('data-tone', tone); } else { qrStatus.removeAttribute('data-tone'); }
  }
  function pollOnce() {
    fetch(POLL_PATH).then(function (r) { return r.json(); }).then(function (data) {
      if (data.status === 'success') {
        setQrStatus('登录成功，可以关闭此窗口了', 'ok');
        setTimeout(function () { window.close(); }, 1200);
        return;
      }
      if (data.status === 'logging') { setQrStatus('已扫码，请在微信中确认…'); return; }
      if (data.status === 'canceled') {
        if (data.qr) { qrBox.innerHTML = data.qr; }
        setQrStatus('二维码已刷新，请重新扫码');
        return;
      }
      if (data.status === 'failed') { setQrStatus('登录失败，请重新发起', 'err'); return; }
      setQrStatus('等待扫码…');
    }).catch(function () {});
  }
  setInterval(pollOnce, POLL_INTERVAL_MS);
  pollOnce();

  var smsStatus = document.getElementById('smsStatus');
  var phoneInput = document.getElementById('phone');
  var smsCodeInput = document.getElementById('smsCode');
  var sendBtn = document.getElementById('sendCode');
  var loginBtn = document.getElementById('doLogin');
  var captchaReady = false;
  var captchaLoading = false;

  function setSmsStatus(text, tone) {
    smsStatus.textContent = text || '';
    if (tone) { smsStatus.setAttribute('data-tone', tone); } else { smsStatus.removeAttribute('data-tone'); }
  }

  function submitSmsSend(captchaParam) {
    var phone = (phoneInput.value || '').trim();
    if (!/^1[3-9]\d{9}$/.test(phone)) {
      setSmsStatus('请输入有效的 11 位手机号', 'err');
      return;
    }
    sendBtn.disabled = true;
    setSmsStatus('发送中…');
    fetch(SMS_SEND_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ phone: phone, captchaParam: captchaParam || '' })
    }).then(function (r) { return r.json(); }).then(function (data) {
      if (data && data.ok) { setSmsStatus('验证码已发送，请查收短信', 'ok'); }
      else { setSmsStatus((data && data.message) || '发送失败，请重试', 'err'); }
    }).catch(function () {
      setSmsStatus('发送失败，请检查网络后重试', 'err');
    }).then(function () {
      sendBtn.disabled = false;
    });
  }

  function initCaptcha() {
    if (captchaReady || typeof window.initAliyunCaptcha !== 'function') { return; }
    captchaReady = true;
    window.initAliyunCaptcha({
      SceneId: CAPTCHA_SCENE,
      prefix: CAPTCHA_PREFIX,
      mode: 'popup',
      element: '#captcha-element',
      button: '#sendCode',
      captchaVerifyCallback: function (captchaParam) {
        submitSmsSend(captchaParam);
        return { captchaResult: true, bizResult: true };
      },
      onBizResultCallback: function () { return null; },
      getInstance: function () { return null; },
      slideStyle: { width: 320, height: 40 },
      language: 'cn'
    });
  }

  function ensureCaptcha() {
    if (typeof window.initAliyunCaptcha === 'function') { initCaptcha(); return; }
    if (captchaLoading) { return; }
    captchaLoading = true;
    var s = document.createElement('script');
    s.src = CAPTCHA_SRC;
    s.onload = function () { captchaLoading = false; initCaptcha(); };
    s.onerror = function () { captchaLoading = false; };
    document.head.appendChild(s);
  }

  sendBtn.addEventListener('click', function () {
    if (typeof window.initAliyunCaptcha !== 'function') { ensureCaptcha(); return; }
    if (!captchaReady) { initCaptcha(); }
  });

  loginBtn.addEventListener('click', function () {
    var code = (smsCodeInput.value || '').trim();
    if (!code) { setSmsStatus('请输入验证码', 'err'); return; }
    loginBtn.disabled = true;
    setSmsStatus('登录中…');
    fetch(SMS_VERIFY_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ smsCode: code })
    }).then(function (r) { return r.json(); }).then(function (data) {
      if (data && data.ok) {
        setSmsStatus('登录成功，可以关闭此窗口了', 'ok');
        setTimeout(function () { window.close(); }, 1200);
      } else {
        setSmsStatus((data && data.message) || '验证码不正确，请重试', 'err');
      }
    }).catch(function () {
      setSmsStatus('登录失败，请检查网络后重试', 'err');
    }).then(function () {
      loginBtn.disabled = false;
    });
  });
})();
</script>
</body>
</html>`
