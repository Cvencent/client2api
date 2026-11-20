package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// machineid.go supplies the device fingerprint the campaign routes are gated on.
//
// Why this exists.  GET /sash/api/v1/me/campaigns and the claim POST answer a
// request that carries only the bearer token with a stub list: the vendor's
// long-running campaigns -- including the daily 100-credit claim the desktop
// client shows in its 权益中心 -- are simply absent, so the module reported
// "今天的奖励已经领取过了" off an unrelated one-time trial pack that happened to
// be CLAIMED, and a claim of anything else was answered with
// CAMPAIGN_CLAIM_PRECONDITION_FAILED (HTTP 409).  Confirmed against the live API
// on 2026-11-13: the same bearer token returns the daily campaign only when the
// request also carries Cosy-MachineToken + Cosy-MachineType (the UMID pair the
// desktop client mints), and the claim then succeeds with a real grant.
//
// The pair comes from the vendor's own runtime-info.exe, which ships in every
// Qoder install under resources/umid.  Its single argument selects the
// environment: 0 is the mainland realm and 4 is the international one, which
// matches Vyn.ONLINE / Vyn.SINGAPORE in the SDK.  Running it is the same path the
// desktop client takes, so no third-party tool and no reverse-engineered KDF is
// involved; the module only supplies the call.

// machineIdentity is one environment's device fingerprint.
type machineIdentity struct {
	Token string `json:"machineToken"`
	Type  string `json:"machineType"`
	Code  string `json:"machineCode"`

	// VM describes the virtualisation the vendor detected.  It is diagnostic
	// only; nothing in this module branches on it.
	VM json.RawMessage `json:"vmInfo,omitempty"`
}

// present reports whether the pair the campaign routes require is usable.  The
// vendor accepts a request with the token alone only as an anonymous one, so
// both halves are required.
func (m machineIdentity) present() bool {
	return strings.TrimSpace(m.Token) != "" && strings.TrimSpace(m.Type) != ""
}

// umidEnv maps a realm onto runtime-info.exe's environment argument.  The two
// values are Vyn.ONLINE and Vyn.SINGAPORE from the SDK's own enum.
func umidEnv(realm string) string {
	if normalizeRealm(realm) == realmGlobal {
		return "4"
	}
	return "0"
}

// umidPathEnv points at a specific runtime-info.exe, for a portable install or a
// host where the probe below cannot find one.
const umidPathEnv = "CLIENT2API_QODER_UMID_PATH"

// umidRelPath is where every install keeps the fingerprint binary.
var umidRelPath = filepath.Join("resources", "umid", "runtime-info.exe")

// Install directory names.  qoderExeRootName is the mainland build the CLI
// bridge drives; qoderIntlExeRootName is the international one.  Both ship the
// same resources/umid/ tree, and the environment argument alone selects which
// realm's identity is minted, so either install is a valid source.
const (
	qoderExeRootName     = "Qoder CN"
	qoderIntlExeRootName = "Qoder"
)

// umidTimeout bounds one fingerprint run.  The vendor's own poll gives it a
// 10s budget; the observed cost on this host is under a second, so the bound is
// generous without letting a wedged binary hold a check-in open.
const umidTimeout = 10 * time.Second

// umidTTL is how long one minted identity is reused.  The pair is a machine
// property, not a session, so re-running the binary on every panel draw would
// spawn a process for nothing; an hour keeps a long-running gateway honest
// about a vendor-side rotation without paying that cost.
const umidTTL = time.Hour

// identityCache memoises per (data dir, realm) so two Clients in one process --
// a test module next to the gateway, or a hot reload -- cannot serve each
// other's identity.
var identityCache sync.Map

type identityEntry struct {
	mu      sync.Mutex
	id      machineIdentity
	at      time.Time
	failure time.Time
}

// machineIdentityFor returns the device fingerprint for one realm, minting and
// caching it on first use.  A failure is reported, never guessed: a request
// without a real fingerprint is exactly the state this file exists to fix, so a
// caller that cannot get one still sends the plain request and gets the stub
// answer the vendor gives.
func (c *Client) machineIdentityFor(ctx context.Context, realm string) (machineIdentity, error) {
	key := strings.TrimSpace(c.deps.DataDir) + "|" + normalizeRealm(realm)
	raw, _ := identityCache.LoadOrStore(key, &identityEntry{})
	entry := raw.(*identityEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	if entry.id.present() && now.Sub(entry.at) < umidTTL {
		return entry.id, nil
	}
	// A failed mint is remembered briefly: the common cause is "this host has no
	// install", which will not fix itself between two panel draws.
	if !entry.failure.IsZero() && now.Sub(entry.failure) < time.Minute {
		return machineIdentity{}, fmt.Errorf("qoder: no machine identity yet (retrying after a failure)")
	}

	id, err := c.mintMachineIdentity(ctx, realm)
	if err != nil {
		entry.failure = now
		return machineIdentity{}, err
	}
	entry.id, entry.at, entry.failure = id, now, time.Time{}
	return id, nil
}

// runUMID invokes the fingerprint binary.  It is a variable so a test can stand
// in for the vendor's executable without shipping a fake PE image.
var runUMID = func(ctx context.Context, bin, env string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, env)
	// The installer puts sgsdk.dll next to the exe and Windows resolves it from
	// the same directory, so the working directory does not matter -- but it is
	// pinned anyway so a relative path in a future manifest cannot drift.
	cmd.Dir = filepath.Dir(bin)
	return cmd.Output()
}

// mintMachineIdentity runs the vendor's fingerprint binary for one realm.
func (c *Client) mintMachineIdentity(ctx context.Context, realm string) (machineIdentity, error) {
	bin, err := c.umidBinary()
	if err != nil {
		return machineIdentity{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, umidTimeout)
	defer cancel()

	out, err := runUMID(rctx, bin, umidEnv(realm))
	if err != nil {
		return machineIdentity{}, fmt.Errorf("qoder: running the device-fingerprint binary: %w", c.redactBinErr(err))
	}
	var id machineIdentity
	if err := json.Unmarshal(out, &id); err != nil {
		return machineIdentity{}, fmt.Errorf("qoder: the device-fingerprint binary returned no identity")
	}
	if !id.present() {
		return machineIdentity{}, fmt.Errorf("qoder: the device-fingerprint binary returned an incomplete identity")
	}
	return id, nil
}

// redactBinErr keeps a failed exec's argv out of operator-facing text.  The
// error is a local path and an exit code, never a credential, but the general
// redactor is cheap and this string reaches the panel.
func (c *Client) redactBinErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", core.Redact(err.Error()))
}

// umidBinary locates runtime-info.exe.  Order matters: an explicit override,
// then the install the CLI bridge already resolved, then a scan of the standard
// install roots, then the fingerprint copy the CLI extracted under its own
// per-account config roots.
func (c *Client) umidBinary() (string, error) {
	if v := strings.TrimSpace(os.Getenv(umidPathEnv)); v != "" {
		if fileExists(v) {
			return v, nil
		}
	}
	if inst, err := findCLI(); err == nil && inst.Root != "" {
		if p := filepath.Join(inst.Root, umidRelPath); fileExists(p) {
			return p, nil
		}
	}
	for _, root := range installRoots() {
		if p := filepath.Join(root, umidRelPath); fileExists(p) {
			return p, nil
		}
	}
	if p := c.extractedUMIDBinary(); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("qoder: no Qoder install with resources/umid/runtime-info.exe was found")
}

// installRoots lists install directories to scan, most likely first.  It is the
// same shape as cliCandidates but names the international build too, because the
// fingerprint binary is shipped identically by both and is the one thing this
// module needs from an install it does not otherwise drive.
func installRoots() []string {
	var out []string
	names := []string{qoderExeRootName, qoderIntlExeRootName}
	for _, drive := range []string{"C", "D", "E", "F", "G"} {
		for _, name := range names {
			out = append(out, drive+`:\`+name, drive+`:\Program Files\`+name)
		}
	}
	for _, la := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if strings.TrimSpace(la) == "" {
			continue
		}
		for _, name := range names {
			out = append(out, filepath.Join(la, name), filepath.Join(la, "Programs", name))
		}
	}
	return out
}

// extractedUMIDBinary finds the copy the CLI published under one of its own
// config roots.  The CLI stages it as .bin/umid-<platform>-<arch>-<hash>/, so a
// host that has ever run the CLI has a usable binary even when the install moved.
func (c *Client) extractedUMIDBinary() string {
	roots := []string{c.cliRootDir()}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".qoder"), filepath.Join(home, ".qoder-cn"))
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, "*", ".bin", "umid-*", "runtime-info.exe"))
		matches2, _ := filepath.Glob(filepath.Join(root, ".bin", "umid-*", "runtime-info.exe"))
		for _, p := range append(matches, matches2...) {
			if fileExists(p) {
				return p
			}
		}
	}
	return ""
}
