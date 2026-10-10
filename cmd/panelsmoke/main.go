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
	// virtualTimeBudget is Chrome's --virtual-time-budget.  Virtual time pauses
	// while a fetch is in flight, so this is "page time excluding network
	// waits".  4000ms is deliberately below the panel's 5000ms refresh timer:
	// the smoke test wants the initial boot, not a background tick.
	virtualTimeBudget = 4000
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
		runCtx, cancelRun := context.WithTimeout(ctx, browserTimeout)
		dom, err := smokeBrowser(runCtx, browser, base+"/panel/", dir)
		timedOut := runCtx.Err() != nil
		cancelRun()
		if err != nil {
			if timedOut {
				problems = append(problems, fmt.Sprintf("%s: timed out after %s", browser, browserTimeout))
			} else {
				problems = append(problems, fmt.Sprintf("%s: %v", browser, err))
			}
			continue
		}
		if *domFlag != "" {
			_ = os.WriteFile(*domFlag, []byte(dom), 0o644)
		}
		if err := assertBooted(dom); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", browser, err))
			continue
		}
		fmt.Printf("panelsmoke: %s booted the panel cleanly (browser %s)\n",
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

// smokeBrowser loads url in one browser and returns the serialized DOM.  Chrome
// is tried with the modern --headless=new spelling first and the old bare
// --headless second so both a current Edge and an older Chrome work.
func smokeBrowser(ctx context.Context, browser, url, dir string) (string, error) {
	profile := filepath.Join(dir, "profile-"+sanitize(filepath.Base(browser)))
	first, firstErr := runOnce(ctx, browser, "--headless=new", url, profile)
	if firstErr == nil || ctx.Err() != nil {
		return first, firstErr
	}
	second, secondErr := runOnce(ctx, browser, "--headless", url, profile+"-legacy")
	if secondErr != nil {
		return "", fmt.Errorf("%v (also tried --headless: %v)", firstErr, secondErr)
	}
	return second, nil
}

func runOnce(ctx context.Context, browser, headless, url, profile string) (string, error) {
	args := []string{
		headless,
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--user-data-dir=" + profile,
		fmt.Sprintf("--virtual-time-budget=%d", virtualTimeBudget),
		"--dump-dom",
		url,
	}
	// A browser running as root (containers, some CI images) cannot use its own
	// sandbox.  The page is a local build of our own panel, so the fallback is
	// acceptable exactly there and nowhere else.
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		args = append(args, "--no-sandbox")
	}
	cmd := exec.CommandContext(ctx, browser, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %v: %s", headless, err, excerpt(errBuf.String(), 0, 600))
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("%s: no DOM on stdout: %s", headless, excerpt(errBuf.String(), 0, 600))
	}
	return out.String(), nil
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
