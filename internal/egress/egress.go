// Package egress owns the gateway's optional outbound relay.
//
// The ordinary top-level "proxy" remains supported.  This package adds the
// two shapes the OFM lane uses: a client proxy URL, and a subscription that is
// handed to a managed mihomo process.  The gateway only ever receives the
// resulting local proxy URL, so existing modules keep using core.Deps.HTTPClient
// and do not need to know which mode is active.
package egress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// readyTimeout is how long a freshly spawned mihomo gets to open its mixed
	// port.  The subscription fetch itself happens inside mihomo and may take
	// longer than the listener bind, so readiness is deliberately a port probe.
	readyTimeout = 15 * time.Second
	// closeTimeout is the grace period for a managed child.  A wedged mihomo
	// must not keep the gateway from exiting.
	closeTimeout = 3 * time.Second
)

// Config is the top-level egress block in client2api.json.
type Config struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	URL        string `json:"url"`
	MihomoPath string `json:"mihomo_path"`
}

// Normalize trims operator input and applies the documented mode default.
func (c Config) Normalize() Config {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = "subscription"
	}
	c.URL = strings.TrimSpace(c.URL)
	c.MihomoPath = strings.TrimSpace(c.MihomoPath)
	return c
}

// Validate checks the block the process will actually use.  A disabled block
// is allowed to be empty: it is the default in every generated config.
func (c Config) Validate() error {
	c = c.Normalize()
	if !c.Enabled {
		return nil
	}
	if c.URL == "" {
		return errors.New("egress.url must not be empty when egress is enabled")
	}
	switch c.Mode {
	case "client":
		u, err := url.Parse(c.URL)
		if err != nil {
			return fmt.Errorf("egress.url: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("egress.url: unsupported proxy scheme %q; use http, https, socks5 or socks5h", u.Scheme)
		}
		if u.Hostname() == "" {
			return errors.New("egress.url must include a host")
		}
	case "subscription":
		u, err := url.Parse(c.URL)
		if err != nil {
			return fmt.Errorf("egress.url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("egress.url: unsupported subscription scheme %q; use http or https", u.Scheme)
		}
		if u.Hostname() == "" {
			return errors.New("egress.url must include a host")
		}
	default:
		return fmt.Errorf("egress.mode must be client or subscription, got %q", c.Mode)
	}
	return nil
}

// Manager owns one configured outlet.  Outlet is the URL passed to
// newHTTPClient; it is empty when the manager has no active outlet.
type Manager struct {
	mu     sync.Mutex
	cfg    Config
	outlet string
	child  *exec.Cmd
	dead   chan struct{}
	closed bool
	logf   func(string, ...any)
}

// Start builds an outlet from cfg.  Disabled configurations return a no-op
// manager so callers can keep one lifecycle path.
func Start(ctx context.Context, cfg Config, dataDir string, logf func(string, ...any)) (*Manager, error) {
	cfg = cfg.Normalize()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{cfg: cfg, logf: logf}
	if !cfg.Enabled {
		return m, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Mode {
	case "client":
		m.outlet = cfg.URL
		logf("egress: client outlet %s", outletLabel(cfg.URL))
		return m, nil
	case "subscription":
		if err := m.startSubscription(ctx, dataDir); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unsupported egress mode %q", cfg.Mode)
	}
}

// Outlet returns the effective proxy URL, empty when egress is disabled.
func (m *Manager) Outlet() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.outlet
}

// Close stops a managed child.  It is safe to call more than once.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	child := m.child
	m.child = nil
	m.mu.Unlock()
	if child != nil {
		stopProcess(child, m.dead, closeTimeout)
	}
}

func (m *Manager) startSubscription(ctx context.Context, dataDir string) error {
	binary, err := findMihomoBinary(m.cfg.MihomoPath)
	if err != nil {
		return err
	}
	if dataDir == "" {
		return errors.New("egress subscription mode needs a data directory")
	}
	dir := filepath.Join(dataDir, "egress")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("egress data directory: %w", err)
	}
	mixedPort, err := freePort()
	if err != nil {
		return fmt.Errorf("egress: selecting mixed port: %w", err)
	}
	apiPort, err := freePort()
	if err != nil {
		return fmt.Errorf("egress: selecting controller port: %w", err)
	}
	secret, err := randomHex(16)
	if err != nil {
		return err
	}
	auth, err := randomHex(12)
	if err != nil {
		return err
	}
	auth = "client2api:" + auth
	configPath := filepath.Join(dir, "mihomo.yaml")
	logPath := filepath.Join(dir, "mihomo.log")
	body := renderMihomoConfig(mihomoOptions{
		Subscription: m.cfg.URL,
		MixedPort:    mixedPort,
		APIPort:      apiPort,
		Secret:       secret,
		Auth:         auth,
		LogFile:      logPath,
	})
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("egress: writing mihomo config: %w", err)
	}

	child := exec.Command(binary, "-d", dir, "-f", configPath)
	child.Dir = dir
	child.Stdout = nil
	child.Stderr = nil
	setHiddenProcess(child)
	if err := child.Start(); err != nil {
		return fmt.Errorf("egress: starting mihomo: %w", err)
	}
	spin := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(spin)
	}()

	if err := waitForPort(ctx, "127.0.0.1", mixedPort, readyTimeout, spin); err != nil {
		stopProcess(child, spin, closeTimeout)
		return fmt.Errorf("egress: mihomo did not become ready: %w (see %s)", err, logPath)
	}
	u := &url.URL{Scheme: "http", User: url.UserPassword("client2api", strings.TrimPrefix(auth, "client2api:")), Host: net.JoinHostPort("127.0.0.1", fmt.Sprint(mixedPort))}
	m.mu.Lock()
	m.child = child
	m.dead = spin
	m.outlet = u.String()
	m.mu.Unlock()
	m.logf("egress: managed mihomo listening on 127.0.0.1:%d (controller 127.0.0.1:%d)", mixedPort, apiPort)
	return nil
}

type mihomoOptions struct {
	Subscription string
	MixedPort    int
	APIPort      int
	Secret       string
	Auth         string
	LogFile      string
}

// renderMihomoConfig is the managed subscription configuration.  It is kept
// as a pure string builder so the generated shape is testable without mihomo.
func renderMihomoConfig(o mihomoOptions) string {
	return strings.Join([]string{
		"# Managed by client2api. Edits are overwritten on the next start.",
		fmt.Sprintf("mixed-port: %d", o.MixedPort),
		"bind-address: 127.0.0.1",
		"allow-lan: false",
		"authentication:",
		"  - " + strconvQuote(o.Auth),
		"mode: rule",
		"log-level: warning",
		"log-file: " + strconvQuote(o.LogFile),
		fmt.Sprintf("external-controller: 127.0.0.1:%d", o.APIPort),
		"secret: " + strconvQuote(o.Secret),
		"dns:",
		"  enable: false",
		"proxy-providers:",
		"  egress:",
		"    type: http",
		"    url: " + strconvQuote(o.Subscription),
		"    interval: 86400",
		"    path: ./egress-provider.yaml",
		"    health-check:",
		"      enable: true",
		"      url: \"http://www.gstatic.com/generate_204\"",
		"      interval: 300",
		"      timeout: 5000",
		"      expected-status: 204",
		"proxy-groups:",
		"  - name: ofm-outlet",
		"    type: url-test",
		"    use:",
		"      - egress",
		"    url: \"http://www.gstatic.com/generate_204\"",
		"    interval: 300",
		"    tolerance: 50",
		"rules:",
		"  - MATCH,ofm-outlet",
		"",
	}, "\n")
}

// strconvQuote keeps generated YAML scalar values unambiguous without pulling
// in a YAML dependency just for five strings.
func strconvQuote(s string) string {
	return strconv.Quote(s)
}

func findMihomoBinary(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if !isFile(explicit) {
			return "", fmt.Errorf("egress.mihomo_path %q does not exist", explicit)
		}
		return explicit, nil
	}
	names := mihomoNames()
	dirs := filepath.SplitList(os.Getenv("PATH"))
	roots := mihomoRoots()
	for _, dir := range append(dirs, roots...) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if isFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", errors.New("no mihomo binary found; set egress.mihomo_path to a mihomo/clash-meta executable")
}

func mihomoNames() []string {
	if runtimeGOOS() == "windows" {
		return []string{"mihomo.exe", "verge-mihomo.exe", "verge-mihomo-alpha.exe", "clash-meta.exe", "clash.exe"}
	}
	return []string{"mihomo", "clash-meta", "clash"}
}

func mihomoRoots() []string {
	if runtimeGOOS() == "windows" {
		var out []string
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")} {
			if strings.TrimSpace(root) == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, "Clash Verge"),
				filepath.Join(root, "clash-verge"),
				filepath.Join(root, "mihomo"))
		}
		return out
	}
	home := os.Getenv("HOME")
	return []string{"/usr/local/bin", "/usr/bin", "/opt/homebrew/bin", filepath.Join(home, ".local", "bin")}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitForPort(ctx context.Context, host string, port int, timeout time.Duration, childExit <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-childExit:
			return errors.New("the managed mihomo process exited during startup")
		default:
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %s:%d did not open within %s", host, port, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("egress: reading randomness: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// outletLabel keeps a subscription URL's path (its credential) out of logs.
func outletLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
