package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"client2api/internal/browser"
)

// autologin_browser.go is the browser half of the Raccoon auto-login run.  The
// vendor's SMS API is plain HTTP, but its Aliyun slider is minted by JavaScript
// against a real browser fingerprint.  This file opens a throwaway browser on a
// loopback page, lets the vendor SDK mint the parameter, and reports it back to
// the job.  If the headless attempt is refused, the same page is opened once
// more in a visible window so the operator can finish the slider by hand.

const (
	// raccoonCaptchaReplyLimit caps the loopback callback body a page may send.
	raccoonCaptchaReplyLimit = 16 << 10
	// raccoonCaptchaMintTimeout bounds one browser round trip.
	raccoonCaptchaMintTimeout = 45 * time.Second
)

type raccoonCaptchaReply struct {
	OK    bool   `json:"ok"`
	Param string `json:"param"`
	Why   string `json:"why"`
	Error string `json:"error"`
}

type raccoonCaptchaSession struct {
	ln   net.Listener
	srv  *http.Server
	ch   chan raccoonCaptchaReply
	once sync.Once
}

func newRaccoonCaptchaSession() (*raccoonCaptchaSession, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("raccoon: cannot open the captcha callback port: %w", err)
	}
	s := &raccoonCaptchaSession{ln: ln, ch: make(chan raccoonCaptchaReply, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.servePage)
	mux.HandleFunc("/reply", s.serveReply)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *raccoonCaptchaSession) url() string {
	q := url.Values{}
	q.Set("scene", aliyunCaptchaSceneID)
	q.Set("prefix", aliyunCaptchaPrefix)
	return "http://" + s.ln.Addr().String() + "/?" + q.Encode()
}

func (s *raccoonCaptchaSession) servePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, raccoonCaptchaPage)
}

func (s *raccoonCaptchaSession) serveReply(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("d")
	if len(raw) > raccoonCaptchaReplyLimit {
		raw = ""
	}
	var reply raccoonCaptchaReply
	_ = json.Unmarshal([]byte(raw), &reply)
	if err := r.ParseForm(); err == nil {
		if reply.Param == "" {
			reply.Param = r.Form.Get("param")
		}
		if reply.Why == "" {
			reply.Why = r.Form.Get("why")
		}
		if reply.Error == "" {
			reply.Error = r.Form.Get("error")
		}
	}
	s.once.Do(func() { s.ch <- reply })
	w.WriteHeader(http.StatusNoContent)
}

func (s *raccoonCaptchaSession) wait(ctx context.Context) (raccoonCaptchaReply, error) {
	select {
	case reply := <-s.ch:
		return reply, nil
	case <-ctx.Done():
		return raccoonCaptchaReply{}, ctx.Err()
	}
}

func (s *raccoonCaptchaSession) close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	_ = s.ln.Close()
}

// solveRaccoonCaptcha mints one Aliyun parameter.  The first attempt honors
// browser_headless; when that is refused, a visible retry gives the operator a
// chance to finish an interactive slider without abandoning the run.
func (c *Client) solveRaccoonCaptcha(ctx context.Context, _ string) (string, error) {
	headless := c.cfg.browserHeadless()
	param, err := c.mintRaccoonCaptcha(ctx, headless)
	if err == nil {
		return param, nil
	}
	if !headless || ctx.Err() != nil {
		return "", err
	}
	c.deps.Log("raccoon: headless captcha mint failed (%v); retrying with a visible browser", err)
	return c.mintRaccoonCaptcha(ctx, false)
}

func (c *Client) mintRaccoonCaptcha(ctx context.Context, headless bool) (string, error) {
	sess, err := newRaccoonCaptchaSession()
	if err != nil {
		return "", err
	}
	defer sess.close()

	mintCtx, cancel := context.WithTimeout(ctx, raccoonCaptchaMintTimeout)
	defer cancel()
	br, err := browser.Launch(mintCtx, browser.LaunchOpts{
		ExecPath: c.cfg.BrowserPath,
		Headless: headless,
		StartURL: sess.url(),
		Timeout:  20 * time.Second,
		ExtraArgs: []string{
			"--window-size=480,400",
			"--lang=zh-CN",
		},
	})
	if err != nil {
		return "", fmt.Errorf("raccoon: cannot start the captcha browser: %w", err)
	}
	defer br.Close()

	reply, err := sess.wait(mintCtx)
	if err != nil {
		return "", fmt.Errorf("raccoon: the captcha browser did not answer in %s", raccoonCaptchaMintTimeout)
	}
	if !reply.OK || strings.TrimSpace(reply.Param) == "" {
		why := strings.TrimSpace(reply.Why)
		if why == "" {
			why = "the Aliyun widget did not return a parameter"
		}
		if e := strings.TrimSpace(reply.Error); e != "" {
			why += ": " + e
		}
		return "", errors.New("raccoon: " + why)
	}
	return strings.TrimSpace(reply.Param), nil
}

// raccoonCaptchaPage mirrors the SDK entry point the vendor's own login page
// uses, including the traceless fast path.  If Aliyun insists on an interactive
// slider, the popup is visible and the callback still reports the parameter
// after the operator solves it.
const raccoonCaptchaPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Raccoon captcha</title>
<style>
  html,body{margin:0;height:100%;font:13px/1.6 "Segoe UI","Microsoft YaHei",sans-serif;background:#f6f7f9;color:#1f2937}
  #wrap{height:100%;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:10px}
  #hint{color:#6b7280}
</style>
</head>
<body>
<div id="wrap"><p id="hint">正在验证…</p><div id="host"></div><button id="go" type="button">验证</button></div>
<script>
(function () {
  "use strict";
  var q = new URLSearchParams(location.search);
  var SCENE = q.get("scene") || "";
  var PREFIX = q.get("prefix") || "";
  var done = false;
  function report(msg) {
    if (done) return;
    done = true;
    try { new Image().src = "/reply?d=" + encodeURIComponent(JSON.stringify(msg)); } catch (e) {}
  }
  function fail(why, detail) { report({ ok: false, why: why, error: detail ? String(detail) : "" }); }
  if (!SCENE) { fail("no-scene"); return; }
  window.AliyunCaptchaConfig = { region: "", prefix: PREFIX };
  var sdk = document.createElement("script");
  sdk.src = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js";
  sdk.async = true;
  sdk.onload = function () { start(); };
  sdk.onerror = function () { fail("sdk-load"); };
  document.head.appendChild(sdk);
  function start() {
    if (typeof window.initAliyunCaptcha !== "function") { fail("no-init"); return; }
    try {
      window.initAliyunCaptcha({
        SceneId: SCENE,
        prefix: PREFIX,
        mode: "popup",
        element: "#host",
        button: "#go",
        showErrorTip: false,
        captchaVerifyCallback: function (param) {
          if (!param) { fail("empty"); return { captchaResult: false, bizResult: false }; }
          report({ ok: true, param: param });
          return { captchaResult: true, bizResult: true };
        },
        onBizResultCallback: function () { return null; },
        getInstance: function (inst) {
          try {
            if (inst && typeof inst.startTracelessVerification === "function") {
              inst.startTracelessVerification();
            } else if (inst && typeof inst.show === "function") {
              inst.show();
            }
          } catch (e) { fail("start", e && e.message); }
        },
        success: function (param) {
          var v = typeof param === "string" ? param : (param && param.captchaVerifyParam);
          if (!v) { fail("empty"); return; }
          report({ ok: true, param: v });
        },
        fail: function (e) { fail("rejected", e); },
        onError: function (e) { fail("sdk-error", e); },
        slideStyle: { width: 320, height: 40 },
        language: "cn"
      });
    } catch (e) {
      fail("init", e && e.message);
    }
  }
})();
</script>
</body>
</html>
`
