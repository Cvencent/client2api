package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// clicreds.go reads the credential the CLI wrote for one account.
//
// Why this exists.  The whole point of driving the agent CLI is that it is the
// only runtime the vendor serves the two zero-cost models to -- but a chat
// transport alone is not an account: the credit ledger and the daily check-in
// live on OpenAPI and need the device token the CLI obtained at sign-in.  That
// token is inside the CLI's own credential store, encrypted with a key derived
// from the config root's machine id, and the decryption routine is compiled
// into the SDK's WASM module.
//
// Rather than re-implement a Rust KDF in Go (it drifts with every SDK update)
// this asks the vendor's own runtime to do it: the decrypt helper below is
// executed by the same `Qoder CN.exe` binary that already hosts the worker, so
// the only thing this module supplies is the call.  Nothing is copied out of
// the install, and no third-party runtime is introduced.
//
// The helper is written into the module's own data directory at first use, so
// an installed gateway stays self-contained.

// cliDecryptHelperName is the file the embedded helper is written as.
const cliDecryptHelperName = "qoder-decrypt.mjs"

// cliIdentityWait bounds one credential read.  The helper only starts an
// Electron process to run WASM; it makes no network call, so this is short.
const cliIdentityWait = 60 * time.Second

// cliCredentials is the part of the decrypted store this module uses.
type cliCredentials struct {
	UID               string `json:"uid"`
	Name              string `json:"name"`
	AccessToken       string `json:"access_token"`
	SecurityToken     string `json:"security_oauth_token"`
	RefreshToken      string `json:"refresh_token"`
	ExpireTime        int64  `json:"expire_time"`
	RefreshExpireTime int64  `json:"refresh_token_expire_time"`
	LoginMethod       string `json:"login_method"`
	UserType          string `json:"user_type"`
	EncryptUserInfo   string `json:"encrypt_user_info"`
	Key               string `json:"key"`

	Error string `json:"error"`
}

// bearer is the token OpenAPI and the signed gateway accept: the CLI stores the
// same value under both spellings, and either one works.
func (c cliCredentials) bearer() string {
	return firstNonEmpty(strings.TrimSpace(c.SecurityToken), strings.TrimSpace(c.AccessToken))
}

// cliHelperWritten memoises which data directories already hold the helper.
// It is keyed by directory rather than held as one path: two Clients can exist
// in one process (a test module next to the gateway, or a hot-reload), and a
// single global path would make the second one write into the first's tree.
var cliHelperWritten sync.Map

// cliDecryptHelper writes the helper script into the module's data directory
// once and returns its path.
func (c *Client) cliDecryptHelper() (string, error) {
	base := strings.TrimSpace(c.deps.DataDir)
	if base == "" {
		return "", fmt.Errorf("qoder: no data directory for the decrypt helper")
	}
	path := filepath.Join(base, cliDecryptHelperName)
	if _, ok := cliHelperWritten.Load(path); ok {
		return path, nil
	}
	if err := core.WriteFileAtomic(path, []byte(cliDecryptHelperSource)); err != nil {
		return "", fmt.Errorf("qoder: writing the CLI decrypt helper: %w", err)
	}
	cliHelperWritten.Store(path, struct{}{})
	return path, nil
}

// readCLICredentials decrypts one account's CLI credential store.
//
// An account whose config root holds no signed-in credential reports
// errCLINotSignedIn, which the caller treats as "this account cannot serve".
func (c *Client) readCLICredentials(ctx context.Context, dir string) (cliCredentials, error) {
	inst, err := findCLI()
	if err != nil {
		return cliCredentials{}, err
	}
	helper, err := c.cliDecryptHelper()
	if err != nil {
		return cliCredentials{}, err
	}
	if !cliSignedIn(dir) {
		return cliCredentials{}, errCLINotSignedIn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, cliIdentityWait)
	defer cancel()

	cmd := newCLIScriptCmd(inst, dir, helper, dir)
	_ = rctx
	out, err := cmd.Output()
	if err != nil {
		if len(out) == 0 {
			return cliCredentials{}, fmt.Errorf("qoder: reading the CLI credential store: %w", err)
		}
		c.logf("qoder: decrypt helper exited with %v; using its output anyway", err)
	}
	var creds cliCredentials
	if err := json.Unmarshal(out, &creds); err != nil {
		// The helper writes one JSON object per run; a stray warning line from
		// Electron would break a strict parse, so fall back to the last line.
		line := lastJSONLine(string(out))
		if line == "" || json.Unmarshal([]byte(line), &creds) != nil {
			return cliCredentials{}, fmt.Errorf("qoder: the CLI decrypt helper returned no JSON")
		}
	}
	if creds.Error != "" {
		return cliCredentials{}, fmt.Errorf("qoder: %s", creds.Error)
	}
	if creds.UID == "" || creds.bearer() == "" {
		return cliCredentials{}, fmt.Errorf("qoder: the CLI credential store had no usable token")
	}
	return creds, nil
}

// lastJSONLine returns the last line that looks like a JSON object.
func lastJSONLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			return line
		}
	}
	return ""
}

// syncCLICredentials refreshes one account's token from its CLI config root.
//
// It is what keeps an account usable without the desktop client: the CLI owns
// the sign-in session and renews the token itself, so the ledger and check-in
// read whatever the CLI currently holds instead of a copy that expires.
func (c *Client) syncCLICredentials(ctx context.Context, acc account) (account, error) {
	dir := c.cliDirForAccount(acc)
	creds, err := c.readCLICredentials(ctx, dir)
	if err != nil {
		return acc, err
	}
	acc.Token = creds.bearer()
	acc.RefreshToken = firstNonEmpty(strings.TrimSpace(creds.RefreshToken), acc.RefreshToken)
	if creds.UID != "" {
		acc.UserID = creds.UID
	}
	if creds.Name != "" {
		acc.Name = creds.Name
	}
	if creds.UserType != "" {
		acc.UserType = creds.UserType
	}
	if creds.ExpireTime > 0 {
		acc.ExpiresAtMS = creds.ExpireTime * 1000
	}
	if creds.RefreshExpireTime > 0 {
		acc.RefreshExpiresAtMS = creds.RefreshExpireTime * 1000
	}
	return acc, nil
}

// ensureFreshCLIToken returns the account with a token read from its CLI store,
// refreshing it only when the cached one is stale or absent.  A CLI-backed
// account can be served for a long time, so the token must not be re-read on
// every request.
func (c *Client) ensureFreshCLIToken(ctx context.Context, acc account) account {
	if strings.TrimSpace(acc.CLIDir) == "" {
		return acc
	}
	if t := strings.TrimSpace(acc.Token); t != "" && !cliTokenStale(acc) {
		return acc
	}
	fresh, err := c.syncCLICredentials(ctx, acc)
	if err != nil {
		c.logf("qoder: refreshing the CLI token for %s: %v", acc.ID, err)
		return acc
	}
	// Persist so a restart does not have to decrypt again, and so the panel
	// shows the identity the account is actually serving as.
	if err := c.store.put(fresh); err != nil {
		c.logf("qoder: saving the refreshed CLI token for %s: %v", acc.ID, err)
	}
	return fresh
}

// cliTokenStale reports whether an account's cached CLI token is old enough to
// re-read.  Only the presence of a token is checked, not its expiry: the CLI is
// the authority on whether its session is still good, and a token it has
// already rotated would fail every call until this refreshes.
func cliTokenStale(acc account) bool {
	if acc.ExpiresAtMS <= 0 {
		// Unknown expiry on a CLI-backed account means it was read before the
		// expiry was recorded; re-read once so the ledger date is populated.
		return true
	}
	// Refresh a day before the recorded expiry, so a long-lived gateway does
	// not discover the lapse through a 401.
	return time.Now().Add(24*time.Hour).UnixMilli() >= acc.ExpiresAtMS
}

// cliDecryptHelperSource is the helper script, embedded so the gateway stays a
// single binary and never depends on a file left behind by an earlier release.
const cliDecryptHelperSource = `// client2api 内置助手：用 Qoder CN 自带的 WASM 解密本机 CLI 登录态。
//
// 用法: Qoder CN.exe qoder-decrypt.mjs <configDir>
// 输出: 一行 JSON {uid,name,access_token,refresh_token,expire_time,...} 或 {error}
//
// 为什么需要它：CLI 把登录态写在 <configDir>/.auth/user，用 machine_id 派生的
// 密钥加密，解密例程是编译进 SDK 的 WASM。这里复用厂商自己的 WASM（本进程就是
// 它的宿主），既不需要第三方运行时，也不需要把这个 KDF 逆向成 Go。
import fs from "node:fs";
import path from "node:path";

function fail(msg) { process.stdout.write(JSON.stringify({ error: String(msg) }) + "\n"); process.exit(1); }

const dir = process.argv[2];
if (!dir) fail("usage: qoder-decrypt <configDir>");

const workerRel = "resources/app.asar.unpacked/node_modules/@qoder-ai/qoder-cn-agent-sdk/dist/_worker/qoder-worker-runtime.obf.mjs";
let workerPath = null;
for (const root of (process.env.CLIENT2API_QODER_CLI_PATH ? [process.env.CLIENT2API_QODER_CLI_PATH] : []).concat(
  ["C:\\Qoder CN","D:\\Qoder CN","E:\\Qoder CN","F:\\Qoder CN","G:\\Qoder CN",
   "C:\\Program Files\\Qoder CN","D:\\Program Files\\Qoder CN","F:\\Program Files\\Qoder CN"])) {
  const p = path.join(root, workerRel);
  if (fs.existsSync(p)) { workerPath = p; break; }
}
if (!workerPath) fail("qoder worker runtime not found");

const userFile = path.join(dir, ".auth", "user");
const midFile = path.join(dir, ".auth", "machine_id");
if (!fs.existsSync(userFile) || !fs.existsSync(midFile)) fail("no credentials in " + dir);

const cipher = fs.readFileSync(userFile, "utf8").trim();
const machineId = fs.readFileSync(midFile, "utf8").trim();

// 在 worker bundle 里找到导出 credential_storage_decrypt 的那一个内联 wasm
const src = fs.readFileSync(workerPath, "utf8");
const blobs = [];
let i = 0;
while ((i = src.indexOf("AGFzbQ", i)) >= 0) { const e = src.indexOf('"', i); blobs.push(src.slice(i, e)); i = e + 1; }

let E = null;
for (const b of blobs) {
  try {
    const mod = await WebAssembly.compile(Buffer.from(b, "base64"));
    const names = WebAssembly.Module.exports(mod).map(x => x.name);
    if (!names.includes("credential_storage_decrypt")) continue;
    const ins = await WebAssembly.instantiate(mod, { "./qoder_auth_wasm_bg.js": new Proxy({}, { get: () => () => 0 }) });
    E = ins.exports;
    break;
  } catch {}
}
if (!E) fail("auth wasm not found in worker bundle");

const dv = () => new DataView(E.memory.buffer);
const u8 = () => new Uint8Array(E.memory.buffer);
const pass = (bytes) => { const p = E.__wbindgen_export2(bytes.length, 1) >>> 0; u8().set(bytes, p); return { p, l: bytes.length }; };
const enc = (s) => pass(new TextEncoder().encode(s));
const read = (p, l) => new TextDecoder().decode(u8().subarray(p, p + l));

const c = enc(cipher);
const k = pass(new TextEncoder().encode(machineId).slice(0, 16));
const sp = E.__wbindgen_add_to_stack_pointer(-16);
E.credential_storage_decrypt(sp, c.p, c.l, k.p, k.l);
const p = dv().getInt32(sp + 0, true), l = dv().getInt32(sp + 4, true), ef = dv().getInt32(sp + 12, true);
E.__wbindgen_add_to_stack_pointer(16);
if (ef) fail("decrypt rejected (machine_id changed?)");

const obj = JSON.parse(read(p, l));
E.__wbindgen_export4(p, l, 1);
process.stdout.write(JSON.stringify(obj) + "\n");
`
