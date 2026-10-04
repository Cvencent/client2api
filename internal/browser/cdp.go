package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cdp.go drives a Chromium-family browser (Edge, Chrome) over the DevTools
// Protocol.  It is what lets the panel finish a vendor login by itself instead
// of asking the operator to open a page and type a code.
//
// The shape is deliberately small:
//
//	Launch   -> start the browser, find its debug port, attach to the page
//	Page.Eval-> run one expression in the page and return its JSON value
//
// Everything vendor-specific (which selector to click, what text means
// "logged in") lives in the client that owns that vendor, not here.
//
// Two details of current Chromium are load-bearing here:
//
//   - Page-domain commands (Page.*, Runtime.*) only answer when they are sent
//     through a flattened session attached to a target, not over a direct
//     connection to /devtools/page/<id>.  So Launch talks to the browser
//     endpoint and attaches with Target.attachToTarget{flatten:true}.
//   - The renderer process crashes on startup under the default sandbox in
//     some environments, which shows up as "Target crashed" for the first
//     command.  Passing --no-sandbox avoids it; verified on Edge 154.

// DefaultLaunchTimeout bounds how long we wait for the browser to come up.
const DefaultLaunchTimeout = 30 * time.Second

// LaunchOpts describes one browser launch.
type LaunchOpts struct {
	// ExecPath pins the browser binary.  Empty means "find an installed one".
	ExecPath string
	// ProfileRoot is where the throwaway profile directory is created.  Empty
	// means "run/browser" under the process working directory.
	ProfileRoot string
	// Headless runs without a visible window.  A visible window is easier to
	// debug and less likely to trip bot heuristics, but needs a desktop.
	Headless bool
	// StartURL is the first page to open.
	StartURL string
	// Timeout bounds the launch.  Zero means DefaultLaunchTimeout.
	Timeout time.Duration
	// ExtraArgs are appended to the browser command line.
	ExtraArgs []string
}

// Browser is one launched browser process with one attached page.
type Browser struct {
	cmd     *exec.Cmd
	profile string
	port    string
	page    *Page
	exe     string
	once    sync.Once
}

// Page is a single attached tab.  Commands are addressed to the tab through a
// flattened CDP session; sessionID is empty only during the browser-level
// handshake.
type Page struct {
	ws        *wsConn
	sessionID string
	targetID  string
	nextID    int64
	mu        sync.Mutex
	pending   map[int64]chan cdpMessage
	closed    bool
	failErr   error
}

type cdpMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cdpTarget is one entry of the /json/list inventory, or the /json/version
// record (which carries the browser-level websocket URL).
type cdpTarget struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// BrowserCandidates lists the executables we are willing to drive, in order.
// Windows first because that is where this runs; the unix paths keep the code
// honest if it is ever built elsewhere.
func BrowserCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
			filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
		}
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"}
	}
}

// FindBrowser returns the first usable browser executable, or "".
func FindBrowser(pinned string) string {
	if strings.TrimSpace(pinned) != "" {
		if _, err := os.Stat(pinned); err == nil {
			return pinned
		}
	}
	for _, c := range BrowserCandidates() {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
		if !filepath.IsAbs(c) {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		}
	}
	return ""
}

// Launch starts the browser and attaches to its first page.
func Launch(ctx context.Context, opts LaunchOpts) (*Browser, error) {
	exe := FindBrowser(opts.ExecPath)
	if exe == "" {
		return nil, errors.New("no Chromium-family browser found; install Microsoft Edge or Google Chrome, or set the browser path")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultLaunchTimeout
	}
	root := strings.TrimSpace(opts.ProfileRoot)
	if root == "" {
		root = filepath.Join("run", "browser")
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create the browser profile root %q: %w", root, err)
	}
	profile, err := os.MkdirTemp(root, "prof-")
	if err != nil {
		return nil, fmt.Errorf("create a browser profile: %w", err)
	}

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		// The renderer crashes on startup under the default sandbox in this
		// environment; --no-sandbox is what keeps Page.*/Runtime.* answering.
		"--no-sandbox",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-extensions",
		"--disable-default-apps",
		"--disable-features=Translate,MediaRouter",
		"--disable-blink-features=AutomationControlled",
		"--window-size=1280,960",
		"--lang=zh-CN",
	}
	if opts.Headless {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	args = append(args, opts.ExtraArgs...)
	start := strings.TrimSpace(opts.StartURL)
	if start == "" {
		start = "about:blank"
	}
	args = append(args, start)

	cmd := exec.Command(exe, args...)
	stderr := &limitedBuffer{limit: 8 << 10}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("start the browser: %w", err)
	}

	b := &Browser{cmd: cmd, profile: profile, exe: exe}
	cleanupOnErr := func(err error) (*Browser, error) {
		b.Close()
		return nil, err
	}

	port, err := waitForDevToolsPort(profile, timeout)
	if err != nil {
		if tail := stderr.String(); tail != "" {
			err = fmt.Errorf("%w (browser said: %s)", err, firstLine(tail))
		}
		return cleanupOnErr(err)
	}
	b.port = port

	ws, err := dialBrowserEndpoint(port, timeout)
	if err != nil {
		return cleanupOnErr(err)
	}
	page := &Page{ws: ws, pending: map[int64]chan cdpMessage{}}
	go page.readLoop()

	if err := page.attachFirstPage(ctx, timeout); err != nil {
		return cleanupOnErr(err)
	}
	b.page = page

	if _, err := page.call(ctx, "Page.enable", nil); err != nil {
		return cleanupOnErr(fmt.Errorf("enable page events: %w", err))
	}
	if _, err := page.call(ctx, "Runtime.enable", nil); err != nil {
		return cleanupOnErr(fmt.Errorf("enable the page runtime: %w", err))
	}
	return b, nil
}

// Page returns the attached page.
func (b *Browser) Page() *Page { return b.page }

// ExecPath reports which binary was driven, for the operator's log.
func (b *Browser) ExecPath() string { return b.exe }

// Close kills the browser and removes its throwaway profile.  It is safe to
// call more than once.
func (b *Browser) Close() {
	b.once.Do(func() {
		if b.page != nil {
			b.page.shutdown()
		}
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
			_, _ = b.cmd.Process.Wait()
		}
		if b.profile != "" {
			_ = os.RemoveAll(b.profile)
		}
	})
}

// waitForDevToolsPort reads the port Chromium writes once its debug listener
// is up.  --remote-debugging-port=0 asks the OS for a free port, which is why
// the port has to be read back rather than chosen up front.
func waitForDevToolsPort(profile string, timeout time.Duration) (string, error) {
	portFile := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(portFile)
		if err == nil {
			if line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0]); line != "" {
				return line, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", errors.New("the browser never opened its debug port")
}

// dialBrowserEndpoint connects to the browser-level websocket, which is the
// only endpoint that accepts Target.attachToTarget.
func dialBrowserEndpoint(port string, timeout time.Duration) (*wsConn, error) {
	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/json/version")
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var v cdpTarget
		derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
		resp.Body.Close()
		if derr != nil {
			lastErr = derr
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if v.WebSocketDebuggerURL == "" {
			lastErr = errors.New("the browser did not report a websocket URL")
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return wsDial(v.WebSocketDebuggerURL, timeout)
	}
	if lastErr == nil {
		lastErr = errors.New("the browser never exposed its debugger endpoint")
	}
	return nil, fmt.Errorf("connect to the browser debugger: %w", lastErr)
}

func listTargets(client *http.Client, base string) ([]cdpTarget, error) {
	resp, err := client.Get(base + "/json/list")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var out []cdpTarget
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode the target inventory: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

func (p *Page) readLoop() {
	for {
		raw, err := p.ws.readMessage()
		if err != nil {
			p.fail(err)
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.ID == 0 {
			continue // an event; this driver is request/response only
		}
		p.mu.Lock()
		ch := p.pending[msg.ID]
		p.mu.Unlock()
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
	}
}

func (p *Page) fail(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.failErr = err
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.mu.Unlock()
	_ = p.ws.close()
}

func (p *Page) shutdown() {
	p.mu.Lock()
	closed := p.closed
	p.closed = true
	p.mu.Unlock()
	if !closed {
		_ = p.ws.close()
	}
}

// attachFirstPage finds the first page target and attaches to it with a
// flattened session, which is what makes Page.*/Runtime.* answer.
func (p *Page) attachFirstPage(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		res, err := p.callBrowser(ctx, "Target.getTargets", nil)
		if err == nil {
			var m struct {
				TargetInfos []struct {
					TargetID string `json:"targetId"`
					Type     string `json:"type"`
					URL      string `json:"url"`
				} `json:"targetInfos"`
			}
			if jerr := json.Unmarshal(res, &m); jerr == nil {
				for _, ti := range m.TargetInfos {
					if ti.Type != "page" {
						continue
					}
					if err := p.attachTarget(ctx, ti.TargetID); err == nil {
						return nil
					} else {
						lastErr = err
					}
				}
			} else {
				lastErr = jerr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("the browser never exposed a page target")
			}
			return fmt.Errorf("attach to the page: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// attachTarget attaches to one target and remembers its session id.
func (p *Page) attachTarget(ctx context.Context, targetID string) error {
	res, err := p.callBrowser(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	})
	if err != nil {
		return err
	}
	var m struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &m); err != nil {
		return fmt.Errorf("decode the session id: %w", err)
	}
	if m.SessionID == "" {
		return errors.New("the browser returned an empty session id")
	}
	p.mu.Lock()
	p.sessionID = m.SessionID
	p.targetID = targetID
	p.mu.Unlock()
	return nil
}

// Reattach re-establishes the page session after a renderer crash.
func (p *Page) Reattach(ctx context.Context, timeout time.Duration) error {
	p.mu.Lock()
	p.sessionID = ""
	p.mu.Unlock()
	return p.attachFirstPage(ctx, timeout)
}

// call issues one CDP command on the page session and waits for its reply.
func (p *Page) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	sid := p.sessionID
	p.mu.Unlock()
	return p.send(ctx, method, params, sid)
}

// callBrowser issues one CDP command at the browser level, without a session.
// Target.* commands must go here.
func (p *Page) callBrowser(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.send(ctx, method, params, "")
}

func (p *Page) send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	id := atomic.AddInt64(&p.nextID, 1)
	ch := make(chan cdpMessage, 1)

	p.mu.Lock()
	if p.closed {
		err := p.failErr
		p.mu.Unlock()
		if err == nil {
			err = errors.New("the page is closed")
		}
		return nil, err
	}
	p.pending[id] = ch
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := p.ws.writeText(raw); err != nil {
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply, ok := <-ch:
		if !ok {
			p.mu.Lock()
			err := p.failErr
			p.mu.Unlock()
			if err == nil {
				err = errors.New("the browser closed the connection")
			}
			return nil, err
		}
		if reply.Error != nil {
			return nil, &cdpCallError{method: method, code: reply.Error.Code, message: reply.Error.Message}
		}
		return reply.Result, nil
	}
}

// cdpCallError carries the protocol error code so callers can tell a crashed
// target apart from a genuine rejection.
type cdpCallError struct {
	method  string
	code    int
	message string
}

func (e *cdpCallError) Error() string {
	return fmt.Sprintf("%s: %s", e.method, e.message)
}

// Crashed reports whether the error is the browser saying the renderer died.
func Crashed(err error) bool {
	var ce *cdpCallError
	if errors.As(err, &ce) {
		return strings.Contains(ce.message, "crashed")
	}
	return false
}

// Eval runs one expression in the page and returns its value as JSON.
func (p *Page) Eval(ctx context.Context, expression string) (json.RawMessage, error) {
	res, err := p.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode the page result: %w", err)
	}
	if out.ExceptionDetails != nil {
		desc := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil && out.ExceptionDetails.Exception.Description != "" {
			desc = out.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("the page script failed: %s", firstLine(desc))
	}
	if len(out.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return out.Result.Value, nil
}

// EvalString runs an expression expected to produce a string.
func (p *Page) EvalString(ctx context.Context, expression string) (string, error) {
	raw, err := p.Eval(ctx, expression)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", nil // a non-string value reads as empty
	}
	return s, nil
}

// EvalBool runs an expression expected to produce a boolean.
func (p *Page) EvalBool(ctx context.Context, expression string) (bool, error) {
	raw, err := p.Eval(ctx, expression)
	if err != nil {
		return false, err
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, nil
	}
	return v, nil
}

// Navigate points the page at a URL and waits for the load to settle.  If the
// renderer dies on the way there, the session is re-attached once and the
// navigation is retried.
func (p *Page) Navigate(ctx context.Context, url string) error {
	if _, err := p.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		if !Crashed(err) {
			return err
		}
		if rerr := p.Reattach(ctx, 20*time.Second); rerr != nil {
			return err
		}
		if _, err := p.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
			return err
		}
	}
	// A vendor SPA paints well after the load event; give it a beat.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Click dispatches a real click on the first element matching the selector.
// It returns false when nothing matched.
func (p *Page) Click(ctx context.Context, selector string) (bool, error) {
	js := `(function(){var el=document.querySelector(` + jsString(selector) + `);` +
		`if(!el){return false;}el.scrollIntoView({block:'center'});el.click();return true;})()`
	return p.EvalBool(ctx, js)
}

// Fill sets the value of the first input matching the selector the way a human
// typist would, which is what React-controlled inputs require: the value is
// written through the native setter and an input event is dispatched.
func (p *Page) Fill(ctx context.Context, selector string, value string) (bool, error) {
	js := `(function(){var el=document.querySelector(` + jsString(selector) + `);` +
		`if(!el){return false;}` +
		`el.focus();` +
		`var proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;` +
		`var setter=Object.getOwnPropertyDescriptor(proto,'value').set;` +
		`setter.call(el,` + jsString(value) + `);` +
		`el.dispatchEvent(new Event('input',{bubbles:true}));` +
		`el.dispatchEvent(new Event('change',{bubbles:true}));` +
		`return true;})()`
	return p.EvalBool(ctx, js)
}

// Exists reports whether any element matches the selector.
func (p *Page) Exists(ctx context.Context, selector string) (bool, error) {
	return p.EvalBool(ctx, `!!document.querySelector(`+jsString(selector)+`)`)
}

// URL reports the page's current address.
func (p *Page) URL(ctx context.Context) (string, error) {
	return p.EvalString(ctx, "location.href")
}

// BodyText returns the rendered text of the page.
func (p *Page) BodyText(ctx context.Context) (string, error) {
	return p.EvalString(ctx, "document.body ? document.body.innerText : ''")
}

// WaitFor polls an expression until it is truthy or the deadline passes.
// It returns whether the expression became truthy.
func (p *Page) WaitFor(ctx context.Context, expression string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := p.EvalBool(ctx, expression)
		if err == nil && ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Screenshot captures the visible viewport as PNG bytes.
func (p *Page) Screenshot(ctx context.Context) ([]byte, error) {
	res, err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return decodeBase64(out.Data)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// jsString renders s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// limitedBuffer keeps the first limit bytes written to it.  The browser's
// stderr is captured this way so a failed launch can explain itself without
// growing without bound.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
