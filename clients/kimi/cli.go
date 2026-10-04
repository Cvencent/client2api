package kimi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The kimi CLI child process
//
// Improvements over the MIT reference, all of which are defects there:
//
//   - binaryCacheTTL: the reference spawned without bound; here a buffered
//     channel caps concurrency (Config.MaxConcurrency).
//   - processKillGrace: the reference had no timeout at all; here every request
//     runs under Config.TimeoutSeconds and the child is killed when it expires.
//   - cancellation propagates: the child is killed when the caller's context is
//     cancelled, so a disconnected client does not leave an agent running.
// ---------------------------------------------------------------------------

const (
	// binaryCacheTTL is how long a binary-discovery result is reused.  Status()
	// is refreshed by the panel every ~10s and must not stat the disk on every
	// call, but the answer must also not go stale forever.
	binaryCacheTTL = 10 * time.Second

	// processKillGrace bounds how long we wait for a killed child to exit
	// before giving up on its pipes and releasing the concurrency slot.
	processKillGrace = 5 * time.Second

	// stderrTailBytes is how much stderr we keep for diagnostics.
	stderrTailBytes = 4 << 10

	// eventBuffer decouples the process from the consumer.
	eventBuffer = 128

	// emitGrace bounds the terminal sends so a vanished consumer cannot pin a
	// goroutine (or a concurrency slot) forever.
	emitGrace = 2 * time.Second
)

var (
	// errBinaryNotFound means no `kimi` executable could be located.
	errBinaryNotFound = errors.New("kimi binary not found")
	// errNotLoggedIn means the binary exists but no credential evidence did.
	errNotLoggedIn = errors.New("kimi CLI is not logged in")
)

// runner owns the process-level resources of one Client: the concurrency
// semaphore and the two caches (binary path, credential report).
type runner struct {
	c   *Client
	sem chan struct{}

	binMu  sync.Mutex
	bin    string
	binErr error
	binAt  time.Time
	// binAccount is the panel account the cached bin came from: the imported
	// binding that pinned it, or cli-login when it was found any other way.  A
	// run's outcome is remembered against that id (see health.go), so a failure
	// lands on the row the operator can actually act on.
	binAccount string

	credMu sync.Mutex
	cred   credentialReport
	credAt time.Time
}

func newRunner(c *Client) *runner {
	return &runner{c: c, sem: make(chan struct{}, c.cfg.MaxConcurrency)}
}

// ---------------------------------------------------------------------------
// Binary discovery
// ---------------------------------------------------------------------------

func kimiBinaryName() string {
	if runtime.GOOS == "windows" {
		return "kimi.exe"
	}
	return "kimi"
}

// candidateBinaryPaths lists the well-known install locations, mirroring the
// paths named in docs/upstream/kimi.md.  PATH is always searched first; this is
// only a fallback for the common case of a shell profile the gateway does not
// inherit.
func candidateBinaryPaths() []string {
	home, _ := os.UserHomeDir()
	var out []string
	if home != "" {
		out = append(out,
			filepath.Join(home, ".kimi", "bin", kimiBinaryName()),
			filepath.Join(home, ".kimi-code", "bin", kimiBinaryName()),
			filepath.Join(home, ".local", "bin", "kimi"),
			filepath.Join(home, ".bun", "bin", kimiBinaryName()),
			filepath.Join(home, "bin", kimiBinaryName()),
		)
	}
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			out = append(out,
				filepath.Join(appdata, "npm", "kimi.cmd"),
				filepath.Join(appdata, "npm", "kimi"),
			)
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out,
				filepath.Join(local, "Programs", "kimi", kimiBinaryName()),
				filepath.Join(local, "kimi", "bin", kimiBinaryName()),
			)
		}
	case "darwin":
		out = append(out,
			"/opt/homebrew/bin/kimi",
			"/usr/local/bin/kimi",
		)
	default:
		out = append(out, "/usr/local/bin/kimi", "/usr/bin/kimi")
	}
	return out
}

// binaryPath returns the executable to run, caching the answer briefly.
func (r *runner) binaryPath() (string, error) {
	bin, _, err := r.binaryPathFrom()
	return bin, err
}

// binaryPathFrom is binaryPath plus the account the path came from, which is
// what makes a failure attributable: see runner.binAccount.
func (r *runner) binaryPathFrom() (string, string, error) {
	r.binMu.Lock()
	defer r.binMu.Unlock()
	if r.binAt.Unix() > 0 && time.Since(r.binAt) < binaryCacheTTL {
		return r.bin, r.binAccount, r.binErr
	}
	bin, account, err := r.locateFrom()
	r.bin, r.binAccount, r.binErr, r.binAt = bin, account, err, time.Now()
	return bin, account, err
}

func (r *runner) locate() (string, error) {
	bin, _, err := r.locateFrom()
	return bin, err
}

// locateFrom resolves the executable and names the account it belongs to.  The
// account is always non-empty: an unresolved binary is remembered against
// cli-login, because "there is no kimi CLI here" is the CLI login's problem.
func (r *runner) locateFrom() (string, string, error) {
	if p := strings.TrimSpace(r.c.cfg.Binary); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return "", cliLoginID, fmt.Errorf("configured clients.kimi.binary %s: %w", abs, err)
		}
		if fi.IsDir() {
			return "", cliLoginID, fmt.Errorf("configured clients.kimi.binary %s is a directory", abs)
		}
		return abs, cliLoginID, nil
	}

	// An executable the operator imported through the panel is an explicit
	// choice, so it outranks PATH and the well-known install locations -- but
	// never clients.kimi.binary, which is why it is checked second.  A binding
	// whose file has vanished falls through silently; the panel shows it as
	// invalid, which is where the operator will look.  A binding this module
	// remembers as cooling or dead is skipped for the same reason: it failed,
	// and asking again immediately would only repeat the failure.
	if b, ok := r.c.boundBinding(); ok {
		return b.Path, b.ID, nil
	}

	if p, err := exec.LookPath(kimiBinaryName()); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs, cliLoginID, nil
		}
		return p, cliLoginID, nil
	}

	for _, cand := range candidateBinaryPaths() {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, cliLoginID, nil
		}
	}
	return "", cliLoginID, errBinaryNotFound
}

// ---------------------------------------------------------------------------
// Command line
// ---------------------------------------------------------------------------

// cliArgs builds the argument vector.  Deliberate differences from the
// reference:
//
//   - no unconditional "--skills-dir empty-skills": that flag replaced the
//     user's own skill directory and silently changed model behaviour, so it is
//     opt-in (Config.SkillsDir).
//   - a permission policy (Config.PermissionMode) is passed only when
//     Config.PermissionFlag actually names one.  The reference left the CLI on
//     its own default, which is an auto-approving agent that can run shell
//     commands on this host; the fix for that is a real flag, not an invented
//     one.  See defaultPermissionFlag: prompt mode takes no policy flag, so by
//     default nothing is appended here.
func (c *Client) cliArgs(prompt, model string) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json"}

	// Both halves are required.  A flag name with no value, or a mode with no
	// flag, would put a stray argument in the vector.
	if mode := c.cfg.permissionMode(); mode != "" {
		if flag := strings.TrimSpace(c.cfg.PermissionFlag); flag != "" {
			args = append(args, flag, mode)
		}
	}
	if dir := strings.TrimSpace(c.cfg.SkillsDir); dir != "" {
		args = append(args, "--skills-dir", dir)
	}
	if m := strings.TrimSpace(model); m != "" && m != defaultCLIModel {
		args = append(args, "-m", m)
	}
	args = append(args, c.cfg.ExtraArgs...)
	return args
}

// ---------------------------------------------------------------------------
// Process start
// ---------------------------------------------------------------------------

// startStream launches one CLI process and returns a stream over its NDJSON
// output.  On error nothing is left running; the caller still owns the
// semaphore slot and the media set.
func startStream(ctx context.Context, cancel context.CancelFunc, c *Client, accountID, bin string, args []string, media *mediaSet) (core.Stream, error) {
	// CommandContext kills the child when ctx is done, which is what makes
	// cancellation and the request timeout propagate to the agent process.
	cmd := exec.CommandContext(ctx, bin, args...)

	cmd.Dir = strings.TrimSpace(c.cfg.CWD)
	if cmd.Dir == "" {
		if wd, err := os.Getwd(); err == nil {
			cmd.Dir = wd
		}
	}
	cmd.Env = childEnv(c.cfg.Env)
	cmd.Stdin = nil // the CLI is prompt-driven; never let it read our stdin
	cmd.WaitDelay = processKillGrace

	s := &stream{
		ctx:       ctx,
		cancel:    cancel,
		cmd:       cmd,
		client:    c,
		media:     media,
		accountID: accountID,
		events:    make(chan core.Event, eventBuffer),
		finished:  make(chan struct{}),
		errTail:   newTailBuffer(stderrTailBytes),
	}
	s.out = &ndjsonWriter{s: s}
	cmd.Stdout = s.out
	cmd.Stderr = s.errTail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kimi: could not start %s: %w", bin, err)
	}
	// GoSafe is the last-resort net: run reports a panic as an event, but a
	// panic in that report path would still take the whole process down.
	core.GoSafe("kimi stream", nil, s.run)
	return s, nil
}

// childEnv layers cfg over the parent environment.  Windows compares variable
// names case-insensitively, so the override map is keyed by the upper-cased
// name and the original spelling is preserved.
func childEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	type entry struct {
		name  string
		value string
	}
	index := map[string]int{}
	var list []entry
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		key := strings.ToUpper(name)
		if _, seen := index[key]; seen {
			continue
		}
		index[key] = len(list)
		list = append(list, entry{name: name, value: kv[i+1:]})
	}
	for name, value := range overrides {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToUpper(name)
		if i, ok := index[key]; ok {
			list[i].value = value
			continue
		}
		index[key] = len(list)
		list = append(list, entry{name: name, value: value})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].name < list[j].name })
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.name+"="+e.value)
	}
	return out
}

// ---------------------------------------------------------------------------
// Secret redaction for error text
// ---------------------------------------------------------------------------

var (
	bearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	skRe     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{6,}`)
	jwtRe    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)
)

// redactSecrets removes anything that looks like a credential from a string
// destined for a log or a Status detail.  Never log a full token: this is the
// last line of defence before core.MaskSecret's prefix-only rendering.
func redactSecrets(s string) string {
	if s == "" {
		return ""
	}
	for _, name := range credentialEnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); len(v) >= 8 && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, core.MaskSecret(v))
		}
	}
	s = jwtRe.ReplaceAllStringFunc(s, core.MaskSecret)
	s = bearerRe.ReplaceAllStringFunc(s, func(m string) string {
		if i := strings.LastIndexAny(m, " \t"); i >= 0 {
			return m[:i+1] + core.MaskSecret(m[i+1:])
		}
		return core.MaskSecret(m)
	})
	s = skRe.ReplaceAllStringFunc(s, core.MaskSecret)
	return s
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// tailBuffer is an io.Writer that keeps only the last max bytes.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
