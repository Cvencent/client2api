package tabbit

// This file reads the vendor session cookie out of the RUNNING Tabbit browser.
//
// The browser is the only place the cookie lives: it is httpOnly, so no page
// script can read it, and the vendor never hands it out over an API.  The
// Tabbit browser ships a Playwright bridge for exactly this kind of automation,
// and its launcher is the supported entry point ("tabbit-cli").  This module
// drives that launcher:
//
//	launcher nodejs --task <name> --request-id <id>   (program on stdin)
//	launcher resource --task <name> --resource <id> [--offset N --max-bytes M]
//	launcher finish --task <name>
//
// The contract comes from the installed Tabbit skill
// (%USERPROFILE%\.agents\skills\tabbit\SKILL.md).  Two of its rules shape the
// code below:
//
//   - Only the launcher in %LOCALAPPDATA%\Tabbit\LocalAgent\bin may be run.
//     The versioned CLI inside the application bundle is an implementation
//     detail; the launcher owns the handshake with the browser's runtime.
//   - The program goes in on stdin.  This module hands the launcher a real file
//     handle rather than building a shell redirection, so the bytes arrive
//     exactly as written and no shell can re-quote them.
//
// Nothing here writes the cookie anywhere: it is read into memory, validated as
// a JWT, and stored in this module's account file (mode 0600) like any other
// credential.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// browserCookiePath is what the operator sees in the import list.  The
	// credential is not a file, so this is a readable stand-in for one.
	browserCookiePath = "Tabbit Browser · web.tabbit.com cookie"

	// browserCookieKind labels the discovered credential in the panel.
	browserCookieKind = "browser-cookie"

	// cliTaskName is the skill task the cookie probe runs in.
	cliTaskName = "client2api-cookie"

	// loginTaskName is the task the browser hand-off opens.  It is a
	// separate name on purpose: the login page is left open for the
	// operator, and the cookie probe ends its own task with `finish`, so
	// sharing one name would release the login page on every probe.
	loginTaskName = "client2api-login"

	// cliCallTimeout bounds one launcher call.  The skill allows 60000-180000ms
	// for a program; the rest of the calls answer immediately.
	cliCallTimeout = 90 * time.Second
	// cliCreateTimeout bounds the create call that has to precede every program.
	cliCreateTimeout = 30 * time.Second
	// cliFinishTimeout bounds the mandatory finish call, which must still run
	// when the caller's context is already cancelled.
	cliFinishTimeout = 15 * time.Second

	// cliProgramTimeout is the --timeout-ms value handed to the launcher.
	cliProgramTimeoutMS = 60000

	// cliMaxResourceBytes is the skill's per-call ceiling for `resource`.
	cliMaxResourceBytes = 65536

	// cliMaxResourceCalls stops a pathological read loop.
	cliMaxResourceCalls = 64
)

// cookieProgram is the Playwright program the launcher runs.  It only reads
// cookies for the vendor's two hosts and reports one small JSON object, so the
// result never approaches the 16 KiB spill threshold.
const cookieProgram = `const hosts = ['https://web.tabbit.com/', 'https://web.tab-browser.com/'];
let jar = [];
for (const host of hosts) {
  const found = await context.cookies(host);
  if (found && found.length) {
    jar = jar.concat(found);
  }
}
const token = jar.find((c) => c.name === 'token' && c.value);
if (!token) {
  return { error: 'no token cookie for web.tabbit.com in the running Tabbit browser', cookies: jar.length };
}
return { token: token.value, domain: token.domain, expires: token.expires, cookies: jar.length };
`

// tabbitCLI returns the launcher path: the config first, then the environment,
// then the documented install location.
func (c *Client) tabbitCLI() string {
	if p := strings.TrimSpace(c.cfg.TabbitCLI); p != "" {
		return p
	}
	for _, key := range []string{"CLIENT2API_TABBIT_CLI", "TABBIT_CLI"} {
		if p := strings.TrimSpace(c.env(key)); p != "" {
			return p
		}
	}
	return defaultTabbitCLI()
}

// defaultTabbitCLI is where the Tabbit browser installs its launcher.
func defaultTabbitCLI() string {
	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		if base == "" {
			base = filepath.Join(strings.TrimSpace(os.Getenv("USERPROFILE")), "AppData", "Local")
		}
		if base == "" {
			return ""
		}
		return filepath.Join(base, "Tabbit", "LocalAgent", "bin", "tabbit-cli.exe")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "bin", "tabbit-cli")
}

// ---------------------------------------------------------------------------
// CredentialImporter
// ---------------------------------------------------------------------------

// loginPageProgram is the Playwright program that puts the vendor's sign-in page
// in front of the operator inside the Tabbit browser.
//
// It navigates the launcher's own initial page -- the skill's documented way to
// reach a known destination -- and brings the window forward, because a login
// page the operator cannot see is the same as no page at all.  The page is then
// left open on purpose: the whole point of the hand-off is that a human finishes
// the sign-in there, and the vendor's own flow closes the tab when it is done.
//
// The URL comes from this module's own configuration, so it is quoted rather than
// interpolated into a template: an operator-supplied web_host must never be able
// to turn into program text.
func loginPageProgram(url string) string {
	return "await page.goto(" + strconv.Quote(url) + ", {waitUntil: 'domcontentloaded'});\n" +
		"await page.bringToFront();\n" +
		"return {url: page.url(), title: await page.title()};\n"
}

// runTabbitLauncher is the seam the tests replace.  Production always drives the
// installed launcher through runTabbitProgram.
var runTabbitLauncher = func(c *Client, ctx context.Context, task, cli, program string) (json.RawMessage, error) {
	return c.runTabbitProgram(ctx, task, cli, program)
}

// openLoginPageInTabbit hands the login URL to the Tabbit browser itself.
//
// The URL cannot be opened anywhere else: the vendor's sign-in only completes in
// that browser, and a normal one bounces straight back to the vendor's site.  A
// missing launcher, or one that refuses to come up, is reported as an error so
// the caller can fall back to telling the operator to open it by hand.
func (c *Client) openLoginPageInTabbit(ctx context.Context, url string) error {
	cli, err := c.requireTabbitCLI()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, runErr := runTabbitLauncher(c, ctx, loginTaskName, cli, loginPageProgram(url))
	return runErr
}

// requireTabbitCLI resolves the launcher and insists that it is actually on this
// machine: the default location is a guess, and a guess that produces a hand-off
// nobody can complete is worse than saying so.
func (c *Client) requireTabbitCLI() (string, error) {
	cli := c.tabbitCLI()
	if cli == "" {
		return "", errors.New("the Tabbit browser launcher could not be located")
	}
	if _, err := os.Stat(cli); err != nil {
		return "", fmt.Errorf("the Tabbit browser launcher is not installed at %s", cli)
	}
	return cli, nil
}

// discoverBrowserCookie offers the running browser as one more importable
// credential.  It reports whether the launcher exists, and never talks to the
// browser: the panel calls this whenever the import tab is opened, and a probe
// that starts a browser task on every refresh would be rude.
func (c *Client) discoverBrowserCookie(ctx context.Context) (core.DiscoveredCredential, bool) {
	cli := c.tabbitCLI()
	if cli == "" {
		return core.DiscoveredCredential{}, false
	}
	dc := core.DiscoveredCredential{
		Path:       browserCookiePath,
		Kind:       browserCookieKind,
		Label:      "web.tabbit.com session cookie",
		Imported:   len(c.webAccounts()) > 0,
		Importable: false,
	}
	if _, err := os.Stat(cli); err != nil {
		dc.Note = "the Tabbit browser launcher is not installed at " + cli +
			"; install the Tabbit browser, or paste the `token` cookie in by hand"
		return dc, true
	}
	dc.Importable = true
	dc.Note = "reads the httpOnly `token` cookie out of the running Tabbit browser " +
		"(the browser must be open); the cookie is stored in this module's account file and never logged"
	return dc, true
}

// importBrowserCookie reads the cookie and stores it as a web-token account.
func (c *Client) importBrowserCookie(ctx context.Context) (core.AccountRecord, error) {
	cli := c.tabbitCLI()
	if cli == "" {
		return core.AccountRecord{}, errors.New("the Tabbit browser launcher could not be located")
	}
	if _, err := os.Stat(cli); err != nil {
		return core.AccountRecord{}, fmt.Errorf(
			"the Tabbit browser launcher is not installed at %s: %w", cli, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	raw, err := c.runTabbitProgram(ctx, cliTaskName, cli, cookieProgram)
	if err != nil {
		return core.AccountRecord{}, err
	}
	var out struct {
		Token   string `json:"token"`
		Domain  string `json:"domain"`
		Expires any    `json:"expires"`
		Cookies int    `json:"cookies"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return core.AccountRecord{}, fmt.Errorf(
			"the Tabbit browser answered with something this module cannot read: %w", err)
	}
	if strings.TrimSpace(out.Error) != "" {
		return core.AccountRecord{}, errors.New(out.Error)
	}
	token := strings.TrimSpace(out.Token)
	if token == "" {
		return core.AccountRecord{}, errors.New(
			"the Tabbit browser returned no token cookie; sign in to Tabbit in that browser first")
	}

	claims, err := parseWebToken(token)
	if err != nil {
		return core.AccountRecord{}, fmt.Errorf(
			"the cookie the Tabbit browser returned cannot be used: %w", err)
	}
	label := webCookieHost + " · " + shortUID(claims.Sub)
	rec, err := c.addWebAccount(core.AccountSpec{
		Label: label,
		Fields: map[string]string{
			"kind":  kindWebToken,
			"token": token,
			"label": label,
		},
	})
	if err != nil {
		return core.AccountRecord{}, err
	}
	c.deps.Log("tabbit: imported the web session cookie from the Tabbit browser (uid %s)",
		shortUID(claims.Sub))
	return rec, nil
}

// ---------------------------------------------------------------------------
// The launcher
// ---------------------------------------------------------------------------

// cliEnvelope is the launcher's stdout shape.
type cliEnvelope struct {
	Status string `json:"status"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Result struct {
		RequestID  string          `json:"requestId"`
		Value      json.RawMessage `json:"value"`
		ResourceID string          `json:"resourceId"`
		ByteLength int             `json:"byteLength"`
	} `json:"result"`
}

// browserLaunchFailed is the launcher's own code for "I could not come up".  It
// is named here because it is the one launcher failure the operator has to work
// around outside this process, and the raw JSON does not say how.
const browserLaunchFailed = "BROWSER_LAUNCH_FAILED"

// launcherHint appends the manual route to a failure the operator cannot fix
// from the panel.  A launcher that refuses to come up under this process will
// refuse again on every retry, so the message has to name the way out instead
// of inviting the operator to press the button a second time.
// launcherAction names what a launcher call was trying to do, so a failure
// never claims the browser was reading cookies when it was opening a login page.
func launcherAction(task string) string {
	if task == loginTaskName {
		return "open the Tabbit sign-in page"
	}
	return "read the Tabbit browser's cookies"
}

func launcherHint(task, msg string) string {
	if !strings.Contains(msg, browserLaunchFailed) {
		return msg
	}
	return msg + " — the launcher would not come up under this process; either run it by hand " +
		"from an interactive shell (`tabbit-cli.exe create --task " + task + "`), or open the " +
		"Tabbit browser, copy the `token` cookie for web.tabbit.com (DevTools → Application → " +
		"Cookies) and paste it into the Web session cookie field"
}

// createTabbitTask opens the skill task the program will run in.  It is a
// separate call because the launcher refuses to run a program for a task that
// does not exist yet; a failure here is reported with the launcher's own words.
func (c *Client) createTabbitTask(ctx context.Context, task, cli string) error {
	cctx, cancel := context.WithTimeout(ctx, cliCreateTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, cli, "create", "--task", task)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if cctx.Err() != nil {
		return fmt.Errorf("the Tabbit browser did not answer within %s: %w", cliCreateTimeout, cctx.Err())
	}
	if runErr != nil {
		// The launcher prints its instance banner on stderr even when it
		// succeeds, so only a non-zero exit counts as a failure here.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("the Tabbit browser launcher could not open its task: %s",
			truncate(launcherHint(task, msg), 400))
	}
	return nil
}

// runTabbitProgram runs one program on the launcher and returns its value.
func (c *Client) runTabbitProgram(ctx context.Context, task, cli, program string) (json.RawMessage, error) {
	reqID, err := cliRequestID()
	if err != nil {
		return nil, err
	}
	prog, err := writeProgramFile(c.deps.DataDir, program)
	if err != nil {
		return nil, err
	}
	defer os.Remove(prog)

	// The skill's recipe ends in `< "<progfile>"`; handing the launcher the file
	// handle directly is the same bytes with no shell in between.
	f, err := os.Open(prog)
	if err != nil {
		return nil, fmt.Errorf("opening the Tabbit browser program: %w", err)
	}
	defer f.Close()

	// The launcher answers BROWSER_LAUNCH_FAILED for a task it was never asked
	// to open, so the task is created first.  The skill documents the step:
	// "create --task NAME" returns an inventory and takes no tab.
	if err := c.createTabbitTask(ctx, task, cli); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, cliCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, cli, "nodejs",
		"--task", task,
		"--request-id", reqID,
		"--timeout-ms", strconv.Itoa(cliProgramTimeoutMS))
	cmd.Stdin = f
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// The skill requires the task to be finished on every path.
	defer c.finishTabbitTask(task, cli)

	runErr := cmd.Run()
	if cctx.Err() != nil {
		return nil, fmt.Errorf("the Tabbit browser did not answer within %s: %w", cliCallTimeout, cctx.Err())
	}
	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("the Tabbit browser launcher failed: %s", truncate(launcherHint(task, msg), 400))
		}
		if runErr != nil {
			return nil, fmt.Errorf("the Tabbit browser launcher failed: %w", runErr)
		}
		return nil, errors.New("the Tabbit browser launcher printed nothing")
	}
	var env cliEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("the Tabbit browser launcher printed something this module cannot read: %s",
			truncate(string(raw), 300))
	}
	if env.Status != "succeeded" {
		msg := strings.TrimSpace(env.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(env.Result.Value))
		}
		if msg == "" {
			msg = "status " + strings.TrimSpace(env.Status)
		}
		if runErr != nil {
			return nil, fmt.Errorf("the Tabbit browser could not %s (%s): %s",
				launcherAction(task), truncate(runErr.Error(), 120), truncate(launcherHint(task, msg), 400))
		}
		return nil, fmt.Errorf("the Tabbit browser could not %s: %s",
			launcherAction(task), truncate(launcherHint(task, msg), 400))
	}
	value := env.Result.Value
	if len(value) == 0 && env.Result.ResourceID != "" {
		value, err = c.readTabbitResource(cctx, task, cli, env.Result.ResourceID)
		if err != nil {
			return nil, err
		}
	}
	if len(value) == 0 {
		return nil, errors.New("the Tabbit browser returned an empty result")
	}
	return value, nil
}

// readTabbitResource pulls a result the launcher spilled to a resource.
func (c *Client) readTabbitResource(ctx context.Context, task, cli, id string) (json.RawMessage, error) {
	var out bytes.Buffer
	offset := 0
	for call := 0; call < cliMaxResourceCalls; call++ {
		cmd := exec.CommandContext(ctx, cli, "resource",
			"--task", task,
			"--resource", id,
			"--offset", strconv.Itoa(offset),
			"--max-bytes", strconv.Itoa(cliMaxResourceBytes))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("reading the Tabbit browser resource %s: %s",
				id, truncate(strings.TrimSpace(stderr.String()), 200))
		}
		var chunk struct {
			Value      string `json:"value"`
			NextOffset int    `json:"nextOffset"`
			EOF        bool   `json:"eof"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &chunk); err != nil {
			return nil, fmt.Errorf("reading the Tabbit browser resource %s: %w", id, err)
		}
		out.WriteString(chunk.Value)
		if chunk.EOF || chunk.NextOffset <= offset {
			break
		}
		offset = chunk.NextOffset
	}
	return json.RawMessage(out.Bytes()), nil
}

// finishTabbitTask ends the skill task.  It uses its own context so a cancelled
// caller cannot leave the task open in the browser.
func (c *Client) finishTabbitTask(task, cli string) {
	fctx, cancel := context.WithTimeout(context.Background(), cliFinishTimeout)
	defer cancel()
	cmd := exec.CommandContext(fctx, cli, "finish", "--task", task)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		c.deps.Log("tabbit: could not finish the browser task %s: %s", task,
			truncate(strings.TrimSpace(stderr.String()), 200))
	}
}

// writeProgramFile stages the program in a UTF-8 file with no BOM.
func writeProgramFile(dir, program string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("preparing the Tabbit browser program directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "tabbit-cookie-*.js")
	if err != nil {
		return "", fmt.Errorf("writing the Tabbit browser program: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(program); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("writing the Tabbit browser program: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("writing the Tabbit browser program: %w", err)
	}
	return name, nil
}

// cliRequestID builds an id the skill accepts: 1-128 ASCII letters, digits,
// ".", "_" or "-", starting with a letter or digit.
func cliRequestID() (string, error) {
	n := time.Now().UnixNano()
	if n <= 0 {
		return "", errors.New("the system clock is unusable, so no request id can be built")
	}
	return "c2a-cookie-" + strconv.FormatInt(n, 10), nil
}
