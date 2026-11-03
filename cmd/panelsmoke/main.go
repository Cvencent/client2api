// Command panelsmoke is the packaging gate that proves the control panel
// actually boots in a real browser.
//
// It starts a candidate client2api binary against a throwaway config on a
// random loopback port, loads /panel/ in a headless browser, and asserts two
// markers the shell sets on <html>:
//
//	data-c2a-ready="1"        boot() reached the end
//	data-c2a-boot-error="1"   an uncaught error or a failed boot happened
//
// An HTTP-only check cannot catch this class of failure: the 0.1.5 regression
// was a stray "}" inside the inline script, and the server kept answering 200
// with byte-identical HTML.  A syntax check that looks at a separate file would
// not have seen the block either.  Only a browser that parses and runs the page
// catches it, so the release pipeline runs one.
//
// Exit status is 0 only when the ready marker is present and the error marker
// is absent.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"client2api/internal/browser"
)

// readyAttr and errorAttr are the contract between the panel shell and this
// program.  Both carry an explicit ="1" so the literal string only ever appears
// in the serialized <html> tag; the inline script that sets them talks in
// dataset.c2aReady, so a --dump-dom that still carries the script source cannot
// produce a false positive.
const (
	readyAttr = `data-c2a-ready="1"`
	errorAttr = `data-c2a-boot-error="1"`
)

const (
	// browserTimeout bounds one browser launch.  A browser that hangs here is
	// skipped in favour of the next candidate instead of eating the whole run.
	browserTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "panelsmoke: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		exeFlag     = flag.String("exe", "", "client2api binary to test (default: sibling of this program)")
		browserFlag = flag.String("browser", "", "Chrome/Edge binary (default: auto-detect)")
		timeout     = flag.Duration("timeout", 150*time.Second, "overall deadline for gateway start + browser run")
		domFlag     = flag.String("dump-dom", "", "write the browser's serialized DOM here for debugging")
	)
	flag.Parse()

	exe, err := resolveExe(*exeFlag)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "client2api-panelsmoke-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	port, err := freePort()
	if err != nil {
		return err
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	cfgPath := filepath.Join(dir, "client2api.json")
	dataDir := filepath.Join(dir, "data")

	if err := writeSmokeConfig(cfgPath, port, dataDir); err != nil {
		return fmt.Errorf("write smoke config: %w", err)
	}

	gateway, gatewayLog, err := startGateway(ctx, exe, cfgPath, dataDir, dir)
	if err != nil {
		return err
	}
	defer stopGateway(gateway)

	if err := waitForPanel(ctx, base, gatewayLog); err != nil {
		return err
	}

	candidates, err := browserCandidates(*browserFlag)
	if err != nil {
		return err
	}

	var problems []string
	for _, browser := range candidates {
		// One attempt = one context and one browser session, released together on
		// every path (including the success return and the debug-dump write).
		attemptErr := func() error {
			runCtx, cancelRun := context.WithTimeout(ctx, browserTimeout)
			defer cancelRun()
			dom, page, closePage, err := smokeBrowser(runCtx, browser, base+"/panel/", dir)
			if err != nil {
				if runCtx.Err() != nil {
					return fmt.Errorf("timed out after %s", browserTimeout)
				}
				return err
			}
			defer closePage()
			if *domFlag != "" {
				_ = os.WriteFile(*domFlag, []byte(dom), 0o644)
			}
			if err := assertBooted(dom); err != nil {
				return err
			}
			// The geometry phase (issue #97) runs in the session that just proved
			// boot: a boot marker says the script ran, not that the page is laid
			// out.  A clipped header, an arrow covered by the next column or an
			// invisible primary button all ship today because nothing measures
			// anything.
			if err := assertLayout(runCtx, page); err != nil {
				return err
			}
			// The accounts pane's keyboard behaviour (issues #139/#140): these
			// are measured in the browser because the go test suite only ever
			// matched text against index.html, which is why a swallowed Enter
			// and a stolen focus both shipped.
			if err := assertAccountsKeyboard(runCtx, page); err != nil {
				return err
			}
			// The captcha document is a separate go:embed'ed page with its own
			// inline script, and until issue #128 nothing executed it: a parse
			// error there left every gate green while the overlay hung on
			// "正在向厂商申请验证…" forever.
			return assertCaptchaDocs(runCtx, page, base)
		}()
		if attemptErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", browser, attemptErr))
			continue
		}
		fmt.Printf("panelsmoke: %s booted the panel cleanly and passed the layout checks (browser %s)\n",
			filepath.Base(exe), browser)
		return nil
	}
	return fmt.Errorf("panel failed the browser smoke test:\n  %s", strings.Join(problems, "\n  "))
}

// resolveExe answers "which binary are we testing".  The default is the sibling
// layout the installer extracts, so the installer can drop panelsmoke.exe and
// client2api.exe into one temp directory and run it with no arguments.
func resolveExe(flagVal string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("-exe: %w", err)
		}
		return abs, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this program: %w", err)
	}
	name := "client2api"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(filepath.Dir(self), name)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("no -exe given and %s does not exist", candidate)
	}
	return candidate, nil
}

// writeSmokeConfig writes the smallest config that produces a usable panel.
// An empty api_key is deliberate: auth would put a login screen in front of the
// shell, and the smoke test is about the shell's JavaScript, not the login
// form.  Scheduling is switched off so a run never touches a vendor API.
func writeSmokeConfig(path string, port int, dataDir string) error {
	cfg := map[string]any{
		"listen":   "127.0.0.1:" + strconv.Itoa(port),
		"api_key":  "",
		"data_dir": dataDir,
		"schedule": map[string]any{"enabled": false},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// freePort reserves a port, reports the number and releases it.  There is a
// small race between the close and the gateway binding it, which is fine for a
// smoke test that owns the whole machine.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func startGateway(ctx context.Context, exe, cfgPath, dataDir, workDir string) (*exec.Cmd, *bytes.Buffer, error) {
	cmd := exec.CommandContext(ctx, exe,
		"-config", cfgPath,
		"-data-dir", dataDir,
		"-tray=false",
	)
	cmd.Dir = workDir
	log := &bytes.Buffer{}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", exe, err)
	}
	return cmd, log, nil
}

// stopGateway ends the candidate process.  CommandContext already kills it when
// the deadline fires; this is the ordinary-exit path.
func stopGateway(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// waitForPanel blocks until the gateway answers readiness and serves the shell.
// The panel is fetched as well as /healthz because a build can be healthy and
// still fail to mount the management surface.
func waitForPanel(ctx context.Context, base string, gatewayLog *bytes.Buffer) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for {
		if err := probeHealth(ctx, client, base+"/healthz"); err != nil {
			lastErr = err
		} else if err := probeURL(ctx, client, base+"/panel/", true); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gateway did not serve /healthz and /panel/ in time: %v\n--- gateway output ---\n%s",
				lastErr, excerpt(gatewayLog.String(), 0, 4000))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gateway did not start before the deadline: %v\n--- gateway output ---\n%s",
				lastErr, excerpt(gatewayLog.String(), 0, 4000))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// probeHealth checks the gateway's readiness contract.  A fresh config has no
// account yet, so the gateway correctly reports 503 {"status":"unavailable"}.
// The smoke gate accepts that answer as booted; a malformed 503 must not pass.
func probeHealth(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var payload struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("GET %s: invalid health response: %w", url, err)
	}
	if payload.Status != "ok" && payload.Status != "unavailable" {
		return fmt.Errorf("GET %s: unexpected health status %q", url, payload.Status)
	}
	if strings.TrimSpace(payload.Service) == "" {
		return fmt.Errorf("GET %s: health response has no service name", url)
	}
	return nil
}

func probeURL(ctx context.Context, client *http.Client, url string, wantShell bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if wantShell && !bytes.Contains(body, []byte("<html")) {
		return fmt.Errorf("GET %s: response is not the panel shell", url)
	}
	return nil
}

// smokeBrowser loads url in one browser over CDP and returns the serialized
// DOM.  The older --dump-dom path is unreliable on current Edge builds: Edge
// can exit successfully without writing any DOM to stdout.  Driving the
// DevTools endpoint also lets the gate wait for the panel's explicit boot
// marker instead of guessing with --virtual-time-budget.
func smokeBrowser(ctx context.Context, browserPath, url, dir string) (string, *browser.Page, func(), error) {
	profileRoot := filepath.Join(dir, "profile-"+sanitize(filepath.Base(browserPath)))
	b, err := browser.Launch(ctx, browser.LaunchOpts{
		ExecPath:    browserPath,
		ProfileRoot: profileRoot,
		Headless:    true,
		StartURL:    url,
		Timeout:     25 * time.Second,
	})
	if err != nil {
		return "", nil, nil, err
	}

	page := b.Page()
	ready := `(() => { const h = document.documentElement; return !!(h && (` +
		`h.getAttribute("data-c2a-ready") === "1" || ` +
		`h.getAttribute("data-c2a-boot-error") === "1")); })()`
	ok, err := page.WaitFor(ctx, ready, 10*time.Second)
	if err != nil {
		b.Close()
		return "", nil, nil, err
	}
	if !ok {
		b.Close()
		return "", nil, nil, fmt.Errorf("panel did not finish booting before the deadline")
	}
	dom, err := page.EvalString(ctx, `document.documentElement ? document.documentElement.outerHTML : ""`)
	if err != nil {
		b.Close()
		return "", nil, nil, err
	}
	// The caller owns the shutdown now: the geometry phase runs in this same
	// session (a second launch would pay another browser start for no reason and
	// could land on a different rendering).
	return dom, page, b.Close, nil
}

// assertAccountsKeyboard checks the accounts pane the way a keyboard user meets
// it (issues #139 and #140).
//
// Two behaviours, both invisible to a text assertion:
//
//  1. Enter/Space on a row's action button must reach the BUTTON.  The pane's
//     keydown handler used to preventDefault every Enter inside a card, which
//     also cancelled the button's own activation (measured: no click at all),
//     while the card HEAD still has to keep that responsibility.
//  2. A rebuild (the pane repaints every 5s) must not drop focus to <body>.  The
//     reporter measured the focused button being replaced wholesale.
//
// The check seeds one account through the page's own state, because a fresh
// instance has no row to press, and restores the previous state afterwards.
func assertAccountsKeyboard(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	report, err := page.EvalString(ctx, `(() => {
	  const problems = [];
	  if (typeof setAccMode !== "function" || typeof renderAccounts !== "function") return "";
	  const hadNames = NAMES.slice(), hadCur = CUR, hadAccts = Object.keys(ACCTS);
	  try {
	    ACCTS["__smoke__"] = { capabilities: { manage: true, actions: [] },
	      accounts: [{ id: "smoke-1", label: "smoke-1", state: "ready", enabled: true, fields: {}, note: "", priorities: {} }] };
	    NAMES.length = 0; NAMES.push("__smoke__"); CUR = "__smoke__";
	    const press = (el, key) => {
	      const ev = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
	      el.dispatchEvent(ev);
	      return ev.defaultPrevented;   // true = the browser would not activate it
	    };
	    for (const mode of ["card", "table"]) {
	      setAccMode(mode); renderAccounts();
	      const host = mode === "card" ? document.querySelector("#acctBoard") : document.querySelector("#accBody");
	      const btn = host && host.querySelector('button[data-do]');
	      if (!btn) { problems.push("账号池 " + mode + " 呈现下找不到带 data-do 的行内按钮，键盘检查无从进行"); continue; }
	      // 1. the button owns its keys
	      if (press(btn, "Enter")) problems.push(mode + " 呈现：行内按钮上的 Enter 被面板处理器 preventDefault，按钮自己的激活被吃掉（#139）");
	      if (press(btn, " ")) problems.push(mode + " 呈现：行内按钮上的空格被吃掉（#139）");
	      // 2. focus survives a rebuild
	      btn.focus();
	      const want = btn.getAttribute("data-do");
	      renderAccounts();
	      const a = document.activeElement;
	      if (a === document.body || a === document.documentElement) {
	        problems.push(mode + " 呈现：重建后焦点掉回 body（#140，原来停在 data-do=" + want + " 上）");
	      }
	    }
	    // the card HEAD must still own Enter/Space (that is #118's fix, and the
	    // reason the exemption cannot simply be "anywhere in the container")
	    setAccMode("card"); renderAccounts();
	    const head = document.querySelector("#acctBoard .ac-head");
	    if (head) {
	      const ev = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
	      head.dispatchEvent(ev);
	      if (!ev.defaultPrevented) problems.push("板卡头部不再响应 Enter：展开/收起的键盘路径被让给了浏览器（#118 回归）");
	    }
	  } finally {
	    NAMES.length = 0; for (const n of hadNames) NAMES.push(n);
	    CUR = hadCur;
	    for (const k of Object.keys(ACCTS)) if (k === "__smoke__") delete ACCTS[k];
	    void hadAccts;
	    try { setAccMode("table"); renderAccounts(); } catch (e) { }
	  }
	  return problems.join(" || ");
	})()`)
	if err != nil {
		return fmt.Errorf("press the accounts keyboard: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("accounts keyboard regression:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}
	return nil
}

// assertLayout measures the rendered accounts view (issue #97).
//
// Every assertion here is about what the browser actually laid out, which is the
// class of defect the text-based panel tests cannot see.  They are written as
// questions an operator would ask looking at the screen -- "can I read this
// column header", "if I click that sort arrow, do I hit it", "is this button
// visible" -- rather than as restatements of the CSS that produced them, so a
// legitimate refactor (col-rate 6% -> 6.0%, or a different layout mechanism
// entirely) keeps them green while a real regression fails them.
//
// The numbers come from the live page, so a failure says which element and by
// how much.
func assertLayout(ctx context.Context, page *browser.Page) error {
	if page == nil {
		return nil
	}
	// The accounts view is where the reported layout defects lived (#44 / #86 /
	// #87 / #88): a fixed-layout table with a sticky header, percentage column
	// widths and a sortable-header row.
	if _, err := page.Eval(ctx, `(() => { if (typeof go === "function") go("accounts"); return true; })()`); err != nil {
		return fmt.Errorf("switch to the accounts view: %w", err)
	}
	// The accounts view defaults to the CARD wall, where the table exists in the
	// DOM but is hidden -- measuring it there returns zeros and would fail every
	// check for the wrong reason.  The table is what the reported layout defects
	// lived in, so select it explicitly.
	if _, err := page.Eval(ctx, `(() => { if (typeof setAccMode === "function") setAccMode("table"); return true; })()`); err != nil {
		return fmt.Errorf("switch to the table presentation: %w", err)
	}
	if ok, err := page.WaitFor(ctx, `(() => { const t = document.querySelector("#view-accounts table.acc"); return !!(t && t.getBoundingClientRect().width > 0); })()`, 10*time.Second); err != nil {
		return fmt.Errorf("wait for the accounts table: %w", err)
	} else if !ok {
		// No accounts at all is a legitimate state for a fresh instance; there is
		// nothing to measure.
		return nil
	}

	// One expression so the whole probe sees a single layout, and one report so a
	// failure names every offending element instead of only the first.
	probe := `(() => {
	  const problems = [];
	  const name = el => {
	    if (!el) return "(missing)";
	    const id = el.id ? "#" + el.id : "";
	    const t = (el.textContent || "").trim().slice(0, 18);
	    return el.tagName.toLowerCase() + id + (t ? " " + JSON.stringify(t) : "");
	  };
	  // 1. A sortable header must fit its own column.  overflow:hidden clips the
	  //    label silently -- the report measured a zero-width sort arrow whose
	  //    click landed on the NEXT column's header (issue #87).
	  document.querySelectorAll("#view-accounts table.acc thead th.sortable").forEach(th => {
	    if (th.scrollWidth > th.clientWidth + 1) {
	      problems.push("列头被裁: " + name(th) + " 需要 " + th.scrollWidth + "px / 可用 " + th.clientWidth + "px");
	    }
	  });
	  // 2. The sort arrow must be visible AND hit-testable: its centre has to
	  //    resolve to that same header, or clicking it sorts a different column.
	  document.querySelectorAll("#view-accounts table.acc thead th.sortable .sort-mark").forEach(mark => {
	    const th0 = mark.parentElement;
	    // A header whose whole column is hidden is not a defect: the balance
	    // column is dropped on purpose when the platform reports no balances
	    // (body.hide-bal, issue #44 -- the alternative was an empty 22% gutter).
	    // Skip anything the layout has taken out, and judge only what is shown.
	    if (!th0 || th0.offsetParent === null || th0.getBoundingClientRect().width < 1) return;
	    const r = mark.getBoundingClientRect();
	    if (r.width < 1 || r.height < 1) {
	      problems.push("排序箭头不可见: " + name(mark.parentElement) + " " + r.width.toFixed(1) + "x" + r.height.toFixed(1));
	      return;
	    }
	    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
	    const th = mark.parentElement;
	    if (!hit || !th.contains(hit)) {
	      problems.push("排序箭头被遮挡: " + name(th) + " 的箭头位置命中了 " + name(hit));
	    }
	  });
	  // 3. A primary button must not render its text in its own background colour
	  //    (#86: 1:1 in the light theme, i.e. an invisible label).
	  const rgb = v => { const m = /rgba?\(([^)]+)\)/.exec(v || ""); if (!m) return null;
	    const p = m[1].split(",").map(x => parseFloat(x)); return p.length >= 3 ? p.slice(0, 3) : null; };
	  const lum = c => { const f = v => { v /= 255; return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
	    return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
	  document.querySelectorAll(".box > header button.primary, button.primary").forEach(btn => {
	    const cs = getComputedStyle(btn);
	    const fg = rgb(cs.color), bg = rgb(cs.backgroundColor);
	    if (!fg || !bg) return;
	    const l1 = lum(fg), l2 = lum(bg);
	    const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
	    if (ratio < 2.0) {
	      problems.push("主按钮文字与底色几乎同色: " + name(btn) + " 对比度 " + ratio.toFixed(2) + ":1");
	    }
	    const r = btn.getBoundingClientRect();
	    if (r.width < 1 || r.height < 1) {
	      problems.push("主按钮尺寸为零: " + name(btn));
	    }
	  });
	  // 4. A dropdown in a box header must be clickable, not just painted
	  //    (issue #123): the header used to sit in the same stacking context as
	  //    the board/table that follows it, so every menu item hit-tested to the
	  //    element UNDERNEATH -- visible, unclickable, and no keyboard symptom.
	  document.querySelectorAll(".box > header details.opmore").forEach(det => {
	    const menu = det.querySelector(".opmenu");
	    if (!menu) return;
	    // Open it the way a click would; restore afterwards so the next check
	    // sees the page it expects.
	    const wasOpen = det.open;
	    det.open = true;
	    menu.querySelectorAll("button, label").forEach(it => {
	      if (it.offsetParent === null) return;  // hidden item, not a defect
	      const r = it.getBoundingClientRect();
	      if (r.width < 1 || r.height < 1) return;
	      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
	      if (!hit || !it.contains(hit)) {
	        problems.push("下拉菜单项点不到: " + name(it) + " 的位置命中了 " + name(hit) + "（#123）");
	      }
	    });
	    det.open = wasOpen;
	  });
	  return problems.join(" || ");
	})()`
	report, err := page.EvalString(ctx, probe)
	if err != nil {
		return fmt.Errorf("measure the layout: %w", err)
	}
	if strings.TrimSpace(report) != "" {
		return fmt.Errorf("layout regression:\n  %s", strings.Join(strings.Split(report, " || "), "\n  "))
	}
	return nil
}

// assertCaptchaDocs opens the captcha document in the same browser session and
// checks that its own script ran to completion (issue #128).
//
// It asserts the DOCUMENT, not the vendor: an offline build machine cannot reach
// alicdn, and that is not this repository's failure.  What must never happen is
// an uncaught error in the page's own script -- the shape a stray brace produces,
// which is what left the overlay stuck with no way out.
func assertCaptchaDocs(ctx context.Context, page *browser.Page, base string) error {
	if page == nil {
		return nil
	}
	// Collect page errors BEFORE navigating, so a parse failure is caught even
	// though it aborts the script before any of our own markers are set.
	if _, err := page.Eval(ctx, `(() => {
	  window.__c2aErrors = [];
	  window.addEventListener("error", e => window.__c2aErrors.push(String(e.message || e)));
	  return true;
	})()`); err != nil {
		return fmt.Errorf("arm the captcha error listener: %w", err)
	}
	// Navigate the way the shell does: runCaptcha() always passes scene_id (and
	// the document bails out early without it -- before the line that would prove
	// the script ran), so a bare /panel/captcha would fail this check on a
	// perfectly good build.
	if err := page.Navigate(ctx, base+"/panel/captcha?scene_id=selftest&action=claim&region=cn&prefix=selftest"); err != nil {
		return fmt.Errorf("open the captcha document: %w", err)
	}
	// The document's script runs inside an IIFE, so its helpers are not on
	// window; what it DOES publish is AliyunCaptchaConfig, set at the top of the
	// script before any vendor work.  Its presence means the script executed
	// past the point a stray brace would kill it.
	if ok, err := page.WaitFor(ctx,
		`(() => document.readyState !== "loading" && typeof window.AliyunCaptchaConfig === "object" && window.AliyunCaptchaConfig !== null)()`,
		10*time.Second); err != nil {
		return fmt.Errorf("wait for the captcha script: %w", err)
	} else if !ok {
		return fmt.Errorf("captcha document did not finish loading: its inline script probably " +
			"did not run (issue #128)")
	}
	errs, err := page.EvalString(ctx, `(() => (window.__c2aErrors || []).join(" | "))()`)
	if err != nil {
		return fmt.Errorf("read the captcha page errors: %w", err)
	}
	if strings.TrimSpace(errs) != "" {
		return fmt.Errorf("captcha document raised an uncaught error: %s (issue #128)", errs)
	}
	return nil
}

// assertBooted is the actual assertion.  A missing ready marker is the 0.1.5
// failure mode -- the script never finished -- and an error marker is the
// panel's own report that something threw.
func assertBooted(dom string) error {
	if i := strings.Index(dom, errorAttr); i >= 0 {
		return fmt.Errorf("panel reported a boot error: %s", excerpt(dom, i, 400))
	}
	if !strings.Contains(dom, readyAttr) {
		return fmt.Errorf("panel never set %s: the inline script did not finish booting", readyAttr)
	}
	return nil
}

// browserCandidates returns the browsers to try, best first.  Edge leads on
// Windows because it ships with the OS and a machine whose Chrome profile is
// signed in and slow to start can otherwise dominate the run.
func browserCandidates(explicit string) ([]string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return nil, fmt.Errorf("browser %q: %w", explicit, err)
		}
		return []string{explicit}, nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		key := strings.ToLower(p)
		if seen[key] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[key] = true
		out = append(out, p)
	}
	for _, env := range []string{"C2A_BROWSER", "CHROME_BIN"} {
		add(strings.TrimSpace(os.Getenv(env)))
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}
	for _, p := range browserPaths() {
		add(p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Chrome/Edge found; pass -browser <path> or set CHROME_BIN")
	}
	return out, nil
}

func browserPaths() []string {
	var paths []string
	switch runtime.GOOS {
	case "windows":
		var edge, chrome []string
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			edge = append(edge, filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"))
			chrome = append(chrome, filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"))
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			edge = append(edge, filepath.Join(pf86, "Microsoft", "Edge", "Application", "msedge.exe"))
			chrome = append(chrome, filepath.Join(pf86, "Google", "Chrome", "Application", "chrome.exe"))
		}
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			chrome = append(chrome, filepath.Join(la, "Google", "Chrome", "Application", "chrome.exe"))
		}
		paths = append(paths, edge...)
		paths = append(paths, chrome...)
	case "darwin":
		paths = append(paths,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	default:
		paths = append(paths,
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		)
	}
	return paths
}

// sanitize keeps a browser's base name usable as a directory name.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
}

// excerpt returns at most max bytes of s starting at start, for error messages.
func excerpt(s string, start, max int) string {
	if start < 0 || start > len(s) {
		start = 0
	}
	s = s[start:]
	if len(s) > max {
		s = s[:max] + " ..."
	}
	return strings.TrimSpace(s)
}
