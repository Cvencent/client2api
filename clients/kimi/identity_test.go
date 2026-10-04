package kimi

// Tests for the identity block the module presents to the vendor, and for the
// request shape that goes with it.
//
// The expected values come from a live wire capture of `kimi.exe` 2.1.1 talking
// to api.kimi.com (client2api-lab/_sniff/kimi-cli-protocol.md and the raw logs
// under _sniff/work/).  Where the capture and an inference disagree, the capture
// wins: x-msh-device-model in particular is "<platform> <release> <arch>", not
// the "<release> <arch>" an early reading of the notes suggested.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

// clientWithHome builds a client whose CLI state directory is a fresh temp dir,
// so a test never reads the developer's real ~/.kimi-code.
func clientWithHome(t *testing.T, extra map[string]any) *Client {
	t.Helper()
	cfg := map[string]any{"home_dir": t.TempDir()}
	for k, v := range extra {
		cfg[k] = v
	}
	return newClientIn(t, t.TempDir(), cfg)
}

func TestTheChatHeadersLookLikeTheOfficialCLI(t *testing.T) {
	c := clientWithHome(t, nil)
	id := c.identity()

	h := http.Header{}
	applyChatHeaders(h, id)

	want := map[string]string{
		"User-Agent":                  "kimi-code-cli/2.1.1",
		"x-msh-platform":              "kimi_code_cli",
		"x-msh-version":               "2.1.1",
		"x-msh-device-name":           id.name,
		"x-msh-device-model":          id.model,
		"x-msh-os-version":            id.os,
		"x-msh-device-id":             id.id,
		"accept-language":             "*",
		"sec-fetch-mode":              "cors",
		"x-stainless-retry-count":     "0",
		"x-stainless-lang":            "js",
		"x-stainless-package-version": "6.34.0",
		"x-stainless-os":              nodePlatform(),
		"x-stainless-arch":            nodeArch(),
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v24.15.0",
		// The CLI's SDK sets application/json even though the body asks for a
		// stream, and never overrides it.  text/event-stream, which this module
		// used to send, was itself a mismatch.
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("header %q = %q, want %q", k, got, v)
		}
	}
	if got := h.Get("User-Agent"); got == "" || strings.HasPrefix(got, "Go-http-client") {
		t.Errorf("User-Agent = %q: the stock Go agent is the one tell we must never ship", got)
	}
}

func TestTheOAuthFormOmitsTheSDKHeaders(t *testing.T) {
	c := clientWithHome(t, nil)
	h := http.Header{}
	applyIdentityHeaders(h, c.identity(), false)

	if got := h.Get("User-Agent"); got != "kimi-code-cli/2.1.1" {
		t.Errorf("User-Agent = %q", got)
	}
	if h.Get("x-msh-device-id") == "" {
		t.Error("the device block must still be present on a form post")
	}
	for _, k := range []string{
		"x-stainless-retry-count", "x-stainless-lang", "x-stainless-package-version",
		"x-stainless-os", "x-stainless-arch", "x-stainless-runtime", "x-stainless-runtime-version",
	} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s = %q, want it absent: the CLI hand-writes its OAuth posts", k, v)
		}
	}
}

// The CLI mints a uuid once, keeps it at <home>/device_id, and reuses it for the
// life of the installation.  A gateway on the same machine should present that
// same id: two different ids from one machine is exactly what a real
// installation never produces.
func TestTheDeviceIDReusesWhatTheCLIPersisted(t *testing.T) {
	home := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	if err := os.WriteFile(filepath.Join(home, cliDeviceName), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := clientWithHome(t, map[string]any{"home_dir": home})
	if got := c.identity().id; got != id {
		t.Errorf("device id = %q, want the id the CLI persisted (%q)", got, id)
	}
}

func TestAnExplicitDeviceIDWins(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, cliDeviceName), []byte("from-the-cli"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := clientWithHome(t, map[string]any{"home_dir": home, "device_id": "from-the-config"})
	if got := c.identity().id; got != "from-the-config" {
		t.Errorf("device id = %q, want the configured value", got)
	}
}

func TestAMintedDeviceIDIsPersistedAndReused(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir() // deliberately empty: no CLI state to inherit

	first := newClientIn(t, dataDir, map[string]any{"home_dir": home}).identity().id
	if first == "" {
		t.Fatal("no device id was minted")
	}
	if !isUUIDv4(first) {
		t.Errorf("device id %q is not a version-4 uuid", first)
	}
	onDisk, err := os.ReadFile(filepath.Join(dataDir, cliDeviceName))
	if err != nil {
		t.Fatalf("the minted id was not persisted: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != first {
		t.Errorf("file holds %q, the client presents %q", strings.TrimSpace(string(onDisk)), first)
	}
	if second := newClientIn(t, dataDir, map[string]any{"home_dir": home}).identity().id; second != first {
		t.Errorf("a second client minted %q, want the persisted %q: an id must not change per process", second, first)
	}
}

func TestTheDeviceNameIsTheHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this machine: %v", err)
	}
	if got := clientWithHome(t, nil).identity().name; got != strings.TrimSpace(host) {
		t.Errorf("device name = %q, want the hostname %q", got, strings.TrimSpace(host))
	}
}

// The capture shows "Windows 10.0.26200 x64": three words, and the last one in
// Node's spelling rather than Go's.
func TestTheDeviceModelHasThreeWords(t *testing.T) {
	model := clientWithHome(t, nil).identity().model
	fields := strings.Fields(model)
	if len(fields) != 3 {
		t.Fatalf("device model = %q, want <platform> <release> <arch>", model)
	}
	if fields[0] != nodePlatform() {
		t.Errorf("platform word = %q, want %q", fields[0], nodePlatform())
	}
	if fields[2] != nodeArch() {
		t.Errorf("arch word = %q, want %q", fields[2], nodeArch())
	}
}

func TestTheArchIsSpelledTheWayNodeSpellsIt(t *testing.T) {
	want, ok := map[string]string{"amd64": "x64", "386": "ia32", "arm64": "arm64"}[runtime.GOARCH]
	if !ok {
		t.Skipf("no expected Node spelling recorded for GOARCH=%s", runtime.GOARCH)
	}
	if got := nodeArch(); got != want {
		t.Errorf("nodeArch() = %q on %s, want %q", got, runtime.GOARCH, want)
	}
}

func TestThePlatformWordIsCapitalised(t *testing.T) {
	got := nodePlatform()
	if got == "" {
		t.Fatal("nodePlatform() is empty")
	}
	if !strings.EqualFold(runtime.GOOS, got) {
		t.Errorf("nodePlatform() = %q, want the name of %s", got, runtime.GOOS)
	}
	if first := got[:1]; first != strings.ToUpper(first) {
		t.Errorf("nodePlatform() = %q, want a leading capital", got)
	}
}

// The CLI's body has an observable key order, because its SDK marshals a struct
// the same way encoding/json does.  Order costs nothing to get right, and a
// mismatch is a fingerprint in its own way, so pin it.
func TestTheChatBodyIsShapedLikeTheCLIs(t *testing.T) {
	req := &core.ChatRequest{
		Model:    "kimi-k2-thinking",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	got := buildOpenAIRequest(req, "fallback", requestShaping{cacheKey: "session_abc", thinking: true})

	if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage must be set: the CLI always asks for the usage block")
	}
	if got.PromptCacheKey != "session_abc" {
		t.Errorf("prompt_cache_key = %q", got.PromptCacheKey)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.Keep != "all" {
		t.Errorf("thinking = %#v, want {enabled all}", got.Thinking)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"model", "messages", "stream", "stream_options", "prompt_cache_key", "thinking"}
	if keys := topLevelKeys(t, b); !reflect.DeepEqual(keys, want) {
		t.Errorf("body key order = %v, want %v", keys, want)
	}
}

func TestTheChatBodyUsesTheModernTokenCeiling(t *testing.T) {
	ceiling := 4096
	b, err := json.Marshal(buildOpenAIRequest(&core.ChatRequest{MaxTokens: &ceiling}, "m", requestShaping{}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"max_tokens"`)) {
		t.Errorf(`body = %s: the CLI sends "max_completion_tokens", not "max_tokens"`, b)
	}
	if !bytes.Contains(b, []byte(`"max_completion_tokens":4096`)) {
		t.Errorf("body = %s, want the caller's ceiling under the modern name", b)
	}
}

// Shaping carries only what the caller could not know.  stream_options is not
// part of it: the CLI asks for usage on every request, so we do too.
func TestTheZeroShapingOmitsOnlyTheCallerOwnedFields(t *testing.T) {
	got := buildOpenAIRequest(&core.ChatRequest{}, "m", requestShaping{})
	if got.PromptCacheKey != "" || got.Thinking != nil {
		t.Errorf("zero shaping produced cache_key %q and thinking %#v", got.PromptCacheKey, got.Thinking)
	}
	if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage must be unconditional")
	}
}

func TestThePromptCacheKeyIsStablePerClientAndPrefixedLikeTheCLIs(t *testing.T) {
	c := clientWithHome(t, nil)
	key := c.promptCacheKey()
	if !strings.HasPrefix(key, "session_") {
		t.Errorf("cache key = %q, want the CLI's \"session_\" prefix", key)
	}
	if again := c.promptCacheKey(); again != key {
		t.Errorf("cache key changed from %q to %q inside one client", key, again)
	}
}

func TestThePromptCacheKeyCanBePinned(t *testing.T) {
	if got := clientWithHome(t, map[string]any{"prompt_cache_key": "session_pinned"}).promptCacheKey(); got != "session_pinned" {
		t.Errorf("cache key = %q, want the configured value", got)
	}
}

func TestThinkingDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	if !clientWithHome(t, nil).cfg.thinkingOn() {
		t.Error("thinking must default on: the CLI enables it on every request")
	}
	if clientWithHome(t, map[string]any{"thinking": "off"}).cfg.thinkingOn() {
		t.Error(`thinking="off" must disable the extension`)
	}
}

func TestTheTLSProfileKeysAreReadAndTrimmed(t *testing.T) {
	c := clientWithHome(t, map[string]any{"tls_profile": "  node  ", "tls_protocol": " http/1.1 "})
	if got := c.cfg.tlsProfile(); got != fingerprint.ProfileNode {
		t.Errorf("tls_profile = %q, want %q", got, fingerprint.ProfileNode)
	}
	if got := c.cfg.tlsProtocol(); got != fingerprint.ProtocolH1 {
		t.Errorf("tls_protocol = %q, want %q", got, fingerprint.ProtocolH1)
	}
	if c.vendorClient == nil {
		t.Error("a named tls_profile must produce a dedicated vendor client")
	}
}

// An unset profile is the off switch, and a profile we cannot build must not
// take the module down with it.
func TestAnUnsetOrUnknownTLSProfileLeavesTheSharedClient(t *testing.T) {
	if c := clientWithHome(t, nil); c.vendorClient != nil {
		t.Error("an unset tls_profile must leave the vendor client alone")
	}
	if c := clientWithHome(t, map[string]any{"tls_profile": "netscape"}); c.vendorClient != nil {
		t.Error("an unknown tls_profile must fall back rather than fail")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// topLevelKeys returns the keys of a JSON object in document order, which is the
// order encoding/json emitted them in.
func topLevelKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("payload is not a JSON object: %s", b)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("decoding %s: %v", b, err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("decoding %s: %v", b, err)
		}
	}
	return keys
}

func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return s[14] == '4' && strings.ContainsRune("89ab", rune(s[19]))
}
