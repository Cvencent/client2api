package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// cli.go locates the Qoder CN desktop install and the agent-SDK worker it
// ships, then starts that worker as a subprocess.
//
// Why this exists: the vendor's inference edge refuses the two zero-cost
// catalogue keys (dfmodel / qfmodel, "DeepSeek-Flash" and "Qwen3.8-Flash") to a
// request that only carries a device token, however faithfully the desktop
// client's own COSY envelope is reproduced.  The vendor ships an agent SDK for
// exactly this kind of embedding -- "@qoder-ai/qoder-cn-agent-sdk", a documented
// programmatic interface with its own stream-json protocol and multi-instance
// support -- so this module drives that instead of re-issuing anything.
//
// Nothing is copied out of the install and nothing is shipped with client2api:
// the module runs the install the operator already has, from a working
// directory and a --config-dir that both live under the module's own DataDir,
// and reports ErrCLIMissing when there is no install to run.

// ErrCLIMissing means no Qoder CN install was found.  The panel surfaces this as
// "install Qoder CN locally" instead of a generic failure.
var ErrCLIMissing = errors.New("qoder: 需要本机安装 Qoder CN 客户端（免费模型走官方 CLI）")

// clientName is the module's own name, used to stamp a classified failure so
// the gateway can attribute it.
const clientName = "qoder"

// cliPathEnv points at a specific install, for portable copies and tests.
const cliPathEnv = "CLIENT2API_QODER_CLI_PATH"

// cliDirEnv overrides where per-account CLI config roots are kept.  Empty means
// <DataDir>/cli.  Tests use it to keep the real data dir untouched.
const cliDirEnv = "CLIENT2API_QODER_CLI_DIR"

// qoderExeName is the Electron executable inside an install.  Its presence is
// what marks a directory as an install.
const qoderExeName = "Qoder CN.exe"

// workerRelPath is the agent-SDK worker inside an install.
const workerRelPath = `resources/app.asar.unpacked/node_modules/@qoder-ai/qoder-cn-agent-sdk/dist/_worker/qoder-worker-runtime.obf.mjs`

// cliFreeModels are the two catalogue keys the vendor serves at a zero
// multiplier, and the only ones this bridge is used for.  Every other model
// keeps going through the module's ordinary signed gateway path, which already
// serves it.
//
// The map is keyed by the catalogue id; the value is the display name the CLI
// itself prints for it, kept only for diagnostics.
var cliFreeModels = map[string]string{
	"dfmodel": "DeepSeek-Flash",
	"qfmodel": "Qwen3.8-Flash",
}

// cliModel reports whether a catalogue id should be served through the CLI.  A
// model outside this set is never routed here, so a vendor that adds another
// free key cannot silently start spawning processes.
func cliModel(model string) bool {
	_, ok := cliFreeModels[strings.ToLower(strings.TrimSpace(model))]
	return ok
}

// cliInstall is one usable Qoder CN installation.
type cliInstall struct {
	// Exe is the Electron binary.  With ELECTRON_RUN_AS_NODE=1 it acts as a
	// Node interpreter, so no separate Node install is required.
	Exe string
	// Worker is the agent-SDK worker script.
	Worker string
	// Version is the SDK's own version string, when readable.
	Version string
}

var (
	cliOnce    sync.Once
	cliCached  cliInstall
	cliOK      bool
	cliFindErr error
)

// findCLI returns the install to drive, or ErrCLIMissing.
func findCLI() (cliInstall, error) {
	cliOnce.Do(func() {
		cliCached, cliOK = searchCLI()
		if !cliOK {
			cliFindErr = ErrCLIMissing
		}
	})
	if !cliOK {
		return cliInstall{}, cliFindErr
	}
	return cliCached, nil
}

// resetCLIDiscovery clears the memo.  Tests call it between cases.
func resetCLIDiscovery() {
	cliOnce = sync.Once{}
	cliCached, cliOK, cliFindErr = cliInstall{}, false, nil
}

// cliCandidates lists install roots to probe, most likely first.
func cliCandidates() []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv(cliPathEnv)); v != "" {
		out = append(out, v)
	}
	if runtime.GOOS != "windows" {
		// The desktop client is Windows-only.  The module still builds and
		// reports ErrCLIMissing elsewhere rather than failing at startup.
		return out
	}
	for _, drive := range []string{"C", "D", "E", "F", "G"} {
		out = append(out,
			drive+`:\Qoder CN`,
			drive+`:\Program Files\Qoder CN`,
			drive+`:\Program Files (x86)\Qoder CN`,
		)
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		out = append(out, filepath.Join(la, "Programs", "Qoder CN"), filepath.Join(la, "Qoder CN"))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out, filepath.Join(pf, "Qoder CN"))
	}
	if pf := os.Getenv("ProgramFiles(x86)"); pf != "" {
		out = append(out, filepath.Join(pf, "Qoder CN"))
	}
	return out
}

// searchCLI probes every candidate and returns the first complete install.
func searchCLI() (cliInstall, bool) {
	for _, root := range cliCandidates() {
		if inst, ok := probeInstall(root); ok {
			return inst, true
		}
	}
	return cliInstall{}, false
}

// probeInstall reports whether root holds a runnable install.  It also accepts
// a path to the executable itself, which is a natural way for an operator to
// point CLIENT2API_QODER_CLI_PATH and worth absorbing.
func probeInstall(root string) (cliInstall, bool) {
	root = strings.TrimSpace(root)
	if root == "" {
		return cliInstall{}, false
	}
	if strings.EqualFold(filepath.Ext(root), ".exe") {
		return probeInstall(filepath.Dir(root))
	}
	exe := filepath.Join(root, qoderExeName)
	if !fileExists(exe) {
		return cliInstall{}, false
	}
	worker := filepath.Join(root, filepath.FromSlash(workerRelPath))
	if !fileExists(worker) {
		return cliInstall{}, false
	}
	return cliInstall{Exe: exe, Worker: worker, Version: readInstallVersion(root)}, true
}

// readInstallVersion reads the SDK version from the install's own metadata.  A
// missing version never disqualifies an install.
func readInstallVersion(root string) string {
	p := filepath.Join(root, "resources", "app.asar.unpacked", "node_modules",
		"@qoder-ai", "qoder-cn-agent-sdk", "dist", "_worker", "runtime-info.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return jsonStringField(string(b), "version")
}

// jsonStringField pulls one top-level string field out of a small JSON object.
func jsonStringField(body, key string) string {
	needle := `"` + key + `"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return rest[:k]
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// cliConfigDir is the per-account config root a worker is pointed at.  One
// directory per account is what keeps several signed-in accounts apart: the CLI
// keeps exactly one identity per --config-dir and has no account switcher.
func (c *Client) cliConfigDir(leaf string) string {
	base := strings.TrimSpace(os.Getenv(cliDirEnv))
	if base == "" {
		base = filepath.Join(c.deps.DataDir, "cli")
	}
	return filepath.Join(base, leaf)
}

// cliConfigLeaf is the directory name for an account id.  It is deliberately
// derived from the id and not from the credential, and it is sanitised because
// an account id can carry characters a filesystem will not take.
func cliConfigLeaf(accountID string) string {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return "account"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "account"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// cliDirForAccount resolves the config root a stored account actually uses.  A
// record that names its own leaf keeps it -- a login session that could not be
// renamed onto the account id still has to work -- and everything else falls
// back to the id.
func (c *Client) cliDirForAccount(acc account) string {
	leaf := strings.TrimSpace(acc.CLIDir)
	if leaf == "" {
		leaf = cliConfigLeaf(acc.ID)
	}
	return c.cliConfigDir(leaf)
}

// cliUserMarker is the file the CLI writes once an account is signed in.  Its
// presence is how this module tells "this config root has been through login"
// from "the process created the directory on startup".
func cliUserMarker(dir string) string {
	return filepath.Join(dir, ".auth", "user")
}

// cliSignedIn reports whether a config root holds a usable login.
func cliSignedIn(dir string) bool {
	return fileExists(cliUserMarker(dir))
}

// newCLICmd builds the exec.Cmd for one worker session.
//
// cmd.Dir and every path the worker is told about live under baseDir, so the
// module's footprint stays inside its own directory: nothing is written to the
// user's home or into the vendor install.
func newCLICmd(inst cliInstall, baseDir string, args ...string) *exec.Cmd {
	full := append([]string{inst.Worker}, args...)
	cmd := exec.Command(inst.Exe, full...)
	cmd.Dir = baseDir
	cmd.Env = cliEnv(baseDir)
	return cmd
}

// newCLICmdCtx is newCLICmd with the child killed when ctx is done.  It is used
// for the short diagnostic commands (login URL capture, status read) whose
// duration this module bounds, not for workers, which manage their own life.
func newCLICmdCtx(ctx context.Context, inst cliInstall, baseDir string, args ...string) *exec.Cmd {
	full := append([]string{inst.Worker}, args...)
	cmd := exec.CommandContext(ctx, inst.Exe, full...)
	cmd.Dir = baseDir
	cmd.Env = cliEnv(baseDir)
	return cmd
}

// newCLIScriptCmd runs an arbitrary script through the vendor's Electron binary
// (used as a Node host via ELECTRON_RUN_AS_NODE).  Unlike newCLICmd this does
// not prepend the agent worker: the caller names the script, which is what the
// credential-decrypt helper needs.
func newCLIScriptCmd(inst cliInstall, baseDir, script string, args ...string) *exec.Cmd {
	full := append([]string{script}, args...)
	cmd := exec.Command(inst.Exe, full...)
	cmd.Dir = baseDir
	cmd.Env = cliEnv(baseDir)
	return cmd
}

// cliEnv is the environment a worker runs under.  It is deliberately explicit:
// the operator's own QODERCN_* variables are dropped so a worker can never
// attach itself to the desktop client's config directory, and the config root
// is pinned a second time through the environment for the code paths the CLI
// reads before it parses --config-dir.
func cliEnv(baseDir string) []string {
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if isVendorEnv(name) || name == "ELECTRON_RUN_AS_NODE" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"ELECTRON_RUN_AS_NODE=1",
		"QODERCN_CONFIG_DIR="+baseDir,
		"QODER_CONFIG_DIR="+baseDir,
	)
	return out
}

// isVendorEnv reports whether name is one of the vendor's own variables that
// would redirect a worker at the user's desktop install or at a credential the
// operator did not choose for this account.
func isVendorEnv(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "QODERCN_CONFIG_DIR", "QODER_CONFIG_DIR",
		"QODERCN_CLI_HOME", "QODER_CLI_HOME",
		"QODERCN_PERSONAL_ACCESS_TOKEN", "QODER_PERSONAL_ACCESS_TOKEN",
		"QODERCN_SERVICE_ACCOUNT_KEY", "QODER_SERVICE_ACCOUNT_KEY",
		"QODER_SDK_AUTH_PAYLOAD_FILE", "QODER_AGENT_SDK_ENTRYPOINT",
		"QODER_AGENT_SDK_VERSION", "QODER_AGENT_SDK_LANGUAGE",
		"QODERCN_CLIENT_TYPE", "QODER_CLIENT_TYPE",
		"QODERCN_BUSINESS_PRODUCT", "QODER_BUSINESS_PRODUCT",
		"QODERCN_BUSINESS_TYPE", "QODER_BUSINESS_TYPE",
		"QODERCN_SCENE", "QODER_SCENE",
		"QODERCN_DEVICE_TOKEN", "QODER_DEVICE_TOKEN",
		"QODERCN_ACCESS_TOKEN", "QODER_ACCESS_TOKEN",
		"QODERCN_PAT", "QODER_PAT",
		"QODER_SDK_ACCESS_TOKEN":
		return true
	}
	return strings.HasPrefix(upper, "QODERCN_") || strings.HasPrefix(upper, "QODER_")
}

// killProcessTree terminates a worker and everything it spawned.
//
// The tree kill is load-bearing: `Qoder CN.exe` at the install root is a
// launcher whose child (`.qoder-versions/<version>/Qoder CN.exe`) is the process
// that actually runs the worker, and on Windows killing the launcher leaves that
// child running.  Every worker this module starts is therefore killed through
// `taskkill /T`, which takes the descendants with it; anywhere taskkill is not
// available the plain kill is the fallback.
func killProcessTree(p *os.Process) {
	if p == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run(); err == nil {
			return
		}
	}
	_ = p.Kill()
}

// shortHash is the stable, non-secret digest this module uses when it has to
// derive an id from a value it must not print.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
