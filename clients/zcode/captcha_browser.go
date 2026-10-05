package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The JWT ("start-plan") channel wants an Aliyun traceless-verification
// parameter on every call, and only Aliyun's own JavaScript can mint one --
// against a real browser fingerprint.  This file mints it with the browser the
// machine already has.
//
// Why a real browser and not a headless one.  Measured against the live
// zcode.z.ai scene, a headless Chromium and a jsdom shim both come back
// `F001 verifyResult:false` (traceless verification refused), because Aliyun's
// risk engine treats a bare automation surface as suspicious and falls back to
// the interactive widget.  A normal, windowed Chromium -- moved off-screen so
// nobody has to see it -- passes in about two seconds, because the fingerprint
// it collects is the machine's real one.  So the window is not a nuisance to
// hide from: it is the part that makes the token real.

const (
	// captchaParamTTL is how long a minted parameter is reused.  The reference
	// implementation for this channel keeps the same window.  A parameter the
	// vendor rejects anyway is dropped the moment a 3007 comes back (see
	// Chat), so the worst case is one wasted round trip, never a stale token
	// that a caller cannot recover from.
	captchaParamTTL = 45 * time.Second
	// captchaMintTimeout bounds one browser round trip.  A healthy mint takes
	// about two seconds; the rest is browser start-up on a loaded machine.
	captchaMintTimeout = 40 * time.Second
	// captchaMintAttempts is how many times one solve tries the browser before
	// giving up.  The failure this retries is a flaky browser start, not a
	// vendor refusal, so two attempts is enough.
	captchaMintAttempts = 2
	// captchaReplyLimit caps the loopback callback body a page may send.
	captchaReplyLimit = 16 << 10
	// captchaProfilePrefix names the throwaway browser profile directories so
	// a crashed run is recognisable in %TEMP%.
	captchaProfilePrefix = "zcode-captcha-"
)

// captchaParam is one cached verification parameter.
type captchaParam struct {
	param   string
	region  string
	expires time.Time
}

// captchaCache is a one-slot TTL cache for the minted parameter.
//
// One slot is deliberate: the value describes the vendor's current scene, not
// a credential, so a rotated or rejected token has to replace it rather than
// sit beside it.  Mutex-guarded because Chat is called concurrently.
type captchaCache struct {
	mu  sync.Mutex
	cur captchaParam
}

func (c *captchaCache) get(now time.Time) (captchaParam, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur.param == "" || !now.Before(c.cur.expires) {
		return captchaParam{}, false
	}
	return c.cur, true
}

func (c *captchaCache) put(param, region string, now time.Time) {
	param = strings.TrimSpace(param)
	if param == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = captchaParam{param: param, region: strings.TrimSpace(region), expires: now.Add(captchaParamTTL)}
}

func (c *captchaCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = captchaParam{}
}

// captchaReply is what the page posts back over the loopback channel.
type captchaReply struct {
	OK     bool            `json:"ok"`
	Param  string          `json:"param"`
	Why    string          `json:"why"`
	Detail json.RawMessage `json:"detail"`
	Error  string          `json:"error"`
}

// reason renders the reply as a short, log-safe sentence.  It never contains
// the parameter itself.
func (r captchaReply) reason() string {
	why := strings.TrimSpace(r.Why)
	if why == "" {
		why = "unknown"
	}
	if len(r.Detail) > 0 && string(r.Detail) != "null" {
		why += " " + truncate(string(r.Detail), 160)
	}
	if e := strings.TrimSpace(r.Error); e != "" {
		why += " " + truncate(e, 160)
	}
	return why
}

// captchaSession is the loopback page plus the callback channel it reports on.
// It is split out from the browser launch so the whole exchange can be tested
// without a browser: the test fetches the page and posts a reply itself.
type captchaSession struct {
	ln   net.Listener
	srv  *http.Server
	info regionInfo
	ch   chan captchaReply
	once sync.Once
}

func newCaptchaSession(info regionInfo) (*captchaSession, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("zcode: cannot open the captcha callback port: %w", err)
	}
	s := &captchaSession{ln: ln, info: info, ch: make(chan captchaReply, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.servePage)
	mux.HandleFunc("/reply", s.serveReply)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// url is the page the browser opens.  The scene values travel in the query
// string, so nothing vendor-supplied is interpolated into the document.
func (s *captchaSession) url() string {
	q := url.Values{}
	q.Set("scene", s.info.SceneID)
	q.Set("region", s.info.Region)
	q.Set("prefix", s.info.Prefix)
	return "http://" + s.ln.Addr().String() + "/?" + q.Encode()
}

func (s *captchaSession) servePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, captchaBrowserPage)
}

func (s *captchaSession) serveReply(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("d")
	if len(raw) > captchaReplyLimit {
		raw = ""
	}
	var reply captchaReply
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

// wait blocks until the page reports, the caller gives up, or ctx ends.
func (s *captchaSession) wait(ctx context.Context) (captchaReply, error) {
	select {
	case reply := <-s.ch:
		return reply, nil
	case <-ctx.Done():
		return captchaReply{}, ctx.Err()
	}
}

func (s *captchaSession) close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	_ = s.ln.Close()
}

// browserSolver mints a verification parameter with a locally installed
// browser.  It holds no state: each mint gets its own throwaway profile, so two
// concurrent requests cannot fight over one browser.
type browserSolver struct {
	exe  string
	logf func(string, ...any)
}

func (b *browserSolver) log(format string, args ...any) {
	if b != nil && b.logf != nil {
		b.logf(format, args...)
	}
}

// newBrowserSolver returns nil when the operator turned the browser path off or
// no usable browser is installed.  A nil solver is what makes
// pool.captchaReady false on a machine that cannot mint a parameter, so the
// check stays honest instead of failing at request time.
func newBrowserSolver(cfg *Config, logf func(string, ...any)) *browserSolver {
	if !cfg.captchaBrowser() {
		return nil
	}
	if exe := strings.TrimSpace(cfg.CaptchaBrowserPath); exe != "" {
		if st, err := os.Stat(exe); err != nil || st.IsDir() {
			if logf != nil {
				logf("zcode: captcha_browser_path %q is not a usable executable; the browser captcha path stays off", exe)
			}
			return nil
		}
		return &browserSolver{exe: exe, logf: logf}
	}
	exe := findBrowser()
	if exe == "" {
		if logf != nil {
			logf("zcode: no Edge or Chrome found; the JWT channel needs captcha_command or a browser")
		}
		return nil
	}
	return &browserSolver{exe: exe, logf: logf}
}

// solve opens one off-screen browser window, waits for the vendor's SDK to
// report, and returns the parameter.  The window is closed before it returns.
func (b *browserSolver) solve(ctx context.Context, info regionInfo) (string, error) {
	if strings.TrimSpace(info.SceneID) == "" {
		return "", errors.New("zcode: the vendor did not publish a captcha scene id, so no browser could run the widget")
	}
	var lastErr error
	for attempt := 0; attempt < captchaMintAttempts; attempt++ {
		param, err := b.mintOnce(ctx, info)
		if err == nil {
			return param, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}

func (b *browserSolver) mintOnce(ctx context.Context, info regionInfo) (string, error) {
	sess, err := newCaptchaSession(info)
	if err != nil {
		return "", err
	}
	defer sess.close()

	profile, err := os.MkdirTemp("", captchaProfilePrefix)
	if err != nil {
		return "", fmt.Errorf("zcode: cannot create the captcha browser profile: %w", err)
	}
	kid := 0
	defer func() {
		killBrowserTree(kid, b.logf)
		_ = os.RemoveAll(profile)
	}()

	mintCtx, cancel := context.WithTimeout(ctx, captchaMintTimeout)
	defer cancel()

	cmd := exec.Command(b.exe, browserArgs(profile, sess.url())...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	configureBrowserCommand(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("zcode: cannot start %s: %w", filepath.Base(b.exe), err)
	}
	kid = cmd.Process.Pid
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	defer func() {
		killBrowserTree(kid, b.logf)
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
	}()

	reply, err := sess.wait(mintCtx)
	if err != nil {
		return "", fmt.Errorf("zcode: the captcha browser did not answer in %s", captchaMintTimeout)
	}
	if !reply.OK || strings.TrimSpace(reply.Param) == "" {
		return "", fmt.Errorf("zcode: the captcha widget did not mint a parameter (%s)", reply.reason())
	}
	return strings.TrimSpace(reply.Param), nil
}

// browserArgs opens one throwaway profile on an off-screen window.  Headless is
// deliberately absent -- see the file comment.
func browserArgs(profile, target string) []string {
	return []string{
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-features=Translate,MediaRouter",
		"--window-size=480,400",
		"--window-position=-32000,-32000",
		target,
	}
}

// findBrowser returns the first installed Chromium-family browser, or "".
func findBrowser() string {
	for _, candidate := range browserCandidates() {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if !strings.ContainsAny(candidate, `/\`) {
			if p, err := exec.LookPath(candidate); err == nil {
				return p
			}
			continue
		}
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}

// captchaBrowserPage is the document the throwaway window opens.  It is a
// stripped-down sibling of the panel's own captcha page: same vendor SDK, same
// traceless entry point, but it reports to the module's loopback listener
// instead of to a parent frame.
const captchaBrowserPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>zcode captcha</title>
<style>
  html,body{margin:0;height:100%;font:13px/1.6 "Segoe UI","Microsoft YaHei",sans-serif;background:#14161a;color:#8b939e}
  #wrap{height:100%;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:8px}
</style>
</head>
<body>
<div id="wrap"><p id="hint">verifying</p><div id="host"></div><button id="go" type="button">verify</button></div>
<script>
(function () {
  "use strict";
  var q = new URLSearchParams(location.search);
  var SCENE = q.get("scene") || "";
  var REGION = q.get("region") || "";
  var PREFIX = q.get("prefix") || "";
  var done = false;
  function report(msg) {
    if (done) return;
    done = true;
    msg.type = "zcode-captcha";
    try { new Image().src = "/reply?d=" + encodeURIComponent(JSON.stringify(msg)); } catch (e) {}
  }
  function fail(why, detail) { report({ ok: false, why: why, detail: detail || null }); }
  if (!SCENE) { fail("no-scene"); return; }
  window.AliyunCaptchaConfig = { region: REGION, prefix: PREFIX };
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
        SceneId: SCENE, mode: "popup", region: REGION, prefix: PREFIX,
        element: "#host", button: "#go", showErrorTip: false,
        getInstance: function (inst) {
          try { (inst.startTracelessVerification || inst.show).call(inst); }
          catch (e) { fail("start", String(e && e.message)); }
        },
        success: function (p) {
          var v = typeof p === "string" ? p : (p && p.captchaVerifyParam);
          if (!v) { fail("empty"); return; }
          report({ ok: true, param: v });
        },
        fail: function (e) { fail("rejected", e || null); },
        onError: function (e) { fail("sdk-error", e || null); }
      });
    } catch (e) {
      fail("init", String(e && e.message));
    }
  }
})();
</script>
</body>
</html>
`
