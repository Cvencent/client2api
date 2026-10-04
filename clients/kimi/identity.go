package kimi

// The identity block the official CLI puts on every vendor call.
//
// A live wire capture of `kimi.exe` 2.1.1 against api.kimi.com (see
// client2api-lab/_sniff/kimi-cli-protocol.md) shows three groups of headers
// this module used to omit entirely:
//
//   - a product User-Agent, `kimi-code-cli/2.1.1`.  Without it Go announces
//     `Go-http-client/1.1`, which is the single clearest tell that a request
//     did not come from the CLI.
//   - six `x-msh-*` headers, built by createKimiDeviceHeaders().
//   - the generated SDK's `x-stainless-*` block.  The CLI bundles the OpenAI
//     Node SDK; `x-stainless-package-version` is the only trace of which
//     generated SDK built a call, so it is worth getting right.
//
// None of this is a secret.  The product name, version and platform string are
// constants inside the published bundle, and the device id is a random uuid the
// CLI mints once and persists at `<home>/device_id`.
//
// What is deliberately NOT reproduced is the CLI's HTTP/1.1 request framing.
// Node's undici writes lower-case header names in insertion order; net/http
// writes canonical names in sorted order and owns Host, Content-Length and
// Connection itself.  A fingerprint that lower-cases names before hashing --
// the usual practice, because HTTP/2 mandates lower case -- sees the same
// header set, in a different order.  Matching the order would mean writing
// requests by hand onto a raw connection, which is a far larger change than the
// remaining signal justifies.  See README.md, "Header identity".

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// Product identity.  Observed on the wire as `user-agent: kimi-code-cli/2.1.1`;
	// createKimiUserAgent() renders `${product}/${version}` with no suffix.
	cliProduct = "kimi-code-cli"
	cliVersion = "2.1.1"

	// The platform marker, which is also the wire value of x-msh-platform.
	mshPlatform = "kimi_code_cli"
	mshVersion  = "2.1.1"

	// The bundled SDK's self-description.  Identical on every install.
	stainlessLang     = "js"
	stainlessPackage  = "6.34.0"
	stainlessRuntime  = "node"
	stainlessRuntimeV = "v24.15.0"

	// cliHomeName is the directory the CLI keeps its state in, and
	// cliDeviceName the file inside it holding the persisted device id.  We use
	// the same file name for our own fallback so the two are interchangeable.
	cliHomeName   = ".kimi-code"
	cliDeviceName = "device_id"
)

// deviceIdentity is the x-msh-* block.  Every field but the id is read off the
// machine; the id is minted once and then reused, because a fresh id per
// request is precisely what a real installation never does.
type deviceIdentity struct {
	name  string // os.hostname()
	model string // "<platform> <release> <arch>"
	os    string // os.release()
	id    string // a uuid, stable for this installation
}

// identity returns the block, resolving it at most once per client.
func (c *Client) identity() deviceIdentity {
	c.identityOnce.Do(func() { c.identityVal = c.resolveIdentity() })
	return c.identityVal
}

func (c *Client) resolveIdentity() deviceIdentity {
	release := osRelease()
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		// The CLI sends whatever node_os.hostname() returned, empty included.
		// An empty header is still better than no header: the CLI always sets
		// all six.
		name = ""
	}
	return deviceIdentity{
		name: strings.TrimSpace(name),
		model: strings.TrimSpace(strings.Join([]string{
			nodePlatform(), release, nodeArch(),
		}, " ")),
		os: release,
		id: c.deviceID(),
	}
}

// promptCacheKey is the cache-routing hint sent as prompt_cache_key.
//
// The CLI sends "session_<uuid>", stable for the life of one conversation, so
// the vendor can keep a session's turns on the same prompt cache.  We mint one
// per process, which has the same shape and the same property a cache key needs
// -- constant while a conversation is running, different in a fresh process --
// and an operator can pin it with the prompt_cache_key config key.
func (c *Client) promptCacheKey() string {
	c.cacheKeyOnce.Do(func() {
		if k := strings.TrimSpace(c.cfg.PromptCacheKey); k != "" {
			c.cacheKeyVal = k
			return
		}
		c.cacheKeyVal = "session_" + newUUID()
	})
	return c.cacheKeyVal
}

// requestShapingFor returns the per-client request-shaping knobs: everything the
// vendor's request shape needs that is not in the caller's request.
func (c *Client) requestShapingFor() requestShaping {
	return requestShaping{
		cacheKey:  c.promptCacheKey(),
		thinking:  c.cfg.thinkingOn(),
		maxTokens: c.cfg.maxCompletionTokens(),
	}
}

// deviceID decides which id we present.
//
// The order matters.  The best answer is the id the installed CLI already uses,
// because then the gateway and the user's own CLI look like one device rather
// than two, which is what a real installation produces.  An explicit config
// value beats even that, and a self-minted persistent id is the fallback for a
// machine with no CLI.
func (c *Client) deviceID() string {
	if id := strings.TrimSpace(c.cfg.DeviceID); id != "" {
		return id
	}
	if id := readDeviceID(c.cliHome()); id != "" {
		c.deps.Log("kimi: presenting the device id the CLI persisted in %s", filepath.Join(c.cliHome(), cliDeviceName))
		return id
	}
	if id := readDeviceID(c.cfg.dataDir); id != "" {
		return id
	}
	id := newUUID()
	if err := writeDeviceID(c.cfg.dataDir, id); err != nil {
		c.deps.Log("kimi: could not persist a device id in %q: %v", c.cfg.dataDir, err)
	}
	return id
}

// cliHome is where the CLI keeps its state: the configured override, else the
// user's home directory.
func (c *Client) cliHome() string {
	if dir := strings.TrimSpace(c.cfg.HomeDir); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, cliHomeName)
}

// readDeviceID reads the CLI's plain-text device file.  Missing, unreadable and
// empty all mean "no id here", never an error: the caller moves on to the next
// source of truth.
func readDeviceID(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, cliDeviceName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeDeviceID stores an id in the same shape the CLI uses: the bare value,
// with no wrapper, readable by `cat`.
func writeDeviceID(dir, id string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("no data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cliDeviceName), []byte(id), 0o600)
}

// newUUID returns a random version-4 uuid in canonical form.  crypto/rand.Read
// is documented never to fail, but the fallback keeps this total rather than
// making every caller handle an impossible error.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
		for i := 8; i < 16; i++ {
			b[i] = byte(now >> (8 * (i - 8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// nodeArch maps Go's architecture names onto the ones Node reports, because the
// header carries process.arch and not runtime.GOARCH.
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		// arm64 and the names Go may add later already agree with Node's.
		return runtime.GOARCH
	}
}

// nodePlatform maps runtime.GOOS onto the process.platform spelling Node uses.
// It feeds both x-stainless-os and the leading word of x-msh-device-model,
// which is why "Windows" appears in both.
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "":
		return ""
	default:
		return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
	}
}

// cliUserAgent is the product User-Agent.  Go sets its own only when the header
// is absent, so setting this also removes the `Go-http-client/1.1` tell.
func cliUserAgent() string { return cliProduct + "/" + cliVersion }

// applyIdentityHeaders adds the block the CLI sends on every vendor call.
//
// withSDK adds the x-stainless-* set.  The CLI sends it on the calls its bundled
// OpenAI SDK builds (chat, models) and omits it on the OAuth form posts, which
// are hand-written; request shapes should mirror that.
func applyIdentityHeaders(h http.Header, id deviceIdentity, withSDK bool) {
	h.Set("User-Agent", cliUserAgent())
	h.Set("x-msh-platform", mshPlatform)
	h.Set("x-msh-version", mshVersion)
	h.Set("x-msh-device-name", id.name)
	h.Set("x-msh-device-model", id.model)
	h.Set("x-msh-os-version", id.os)
	h.Set("x-msh-device-id", id.id)
	h.Set("accept-language", "*")
	h.Set("sec-fetch-mode", "cors")

	if withSDK {
		h.Set("x-stainless-retry-count", "0")
		h.Set("x-stainless-lang", stainlessLang)
		h.Set("x-stainless-package-version", stainlessPackage)
		h.Set("x-stainless-os", nodePlatform())
		h.Set("x-stainless-arch", nodeArch())
		h.Set("x-stainless-runtime", stainlessRuntime)
		h.Set("x-stainless-runtime-version", stainlessRuntimeV)
	}
}

// applyChatHeaders adds the identity block plus the per-call headers observed on
// a chat completion.  The CLI's `accept` is application/json even though the
// body asks for a stream: its SDK sets that and the CLI never overrides it, so
// sending text/event-stream (as this module used to) was itself a mismatch.
func applyChatHeaders(h http.Header, id deviceIdentity) {
	applyIdentityHeaders(h, id, true)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
}
