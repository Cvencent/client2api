package workbuddy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The device-token file is read on the chat hot path, so the reader caches by
// path and refuses to treat an oversized file as a token.  Both behaviours are
// ports of the reference (internal/upstream/device_token.go): the file has a
// five-minute TTL, and anything over a kilobyte is a mistake (a log, a keychain
// dump) whose first bytes must never be sent as X-Device-Token.

func writeDeviceTokenFile(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "device-token")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write device token file: %v", err)
	}
	return path
}

func TestWorkbuddyDeviceTokenFileIsTrimmed(t *testing.T) {
	path := writeDeviceTokenFile(t, t.TempDir(), "  tok-abc\n")

	u := &Upstream{}
	if got := u.readDeviceTokenFile(path); got != "tok-abc" {
		t.Fatalf("token = %q, want %q", got, "tok-abc")
	}
}

func TestWorkbuddyDeviceTokenFileIsCached(t *testing.T) {
	dir := t.TempDir()
	path := writeDeviceTokenFile(t, dir, "first")

	u := &Upstream{}
	if got := u.readDeviceTokenFile(path); got != "first" {
		t.Fatalf("first read = %q, want %q", got, "first")
	}

	// Rewriting the file must not be visible until the TTL expires; if the
	// reader went back to the filesystem on every call this would return
	// "second" and the cache would be doing nothing.
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rewrite device token file: %v", err)
	}
	if got := u.readDeviceTokenFile(path); got != "first" {
		t.Fatalf("second read = %q, want the cached %q", got, "first")
	}
}

func TestWorkbuddyDeviceTokenFileCacheIsPerPath(t *testing.T) {
	dir := t.TempDir()
	first := writeDeviceTokenFile(t, dir, "one")
	second := filepath.Join(dir, "other-token")
	if err := os.WriteFile(second, []byte("two"), 0o600); err != nil {
		t.Fatalf("write second device token file: %v", err)
	}

	u := &Upstream{}
	if got := u.readDeviceTokenFile(first); got != "one" {
		t.Fatalf("first path = %q, want %q", got, "one")
	}
	if got := u.readDeviceTokenFile(second); got != "two" {
		t.Fatalf("second path = %q, want %q; the cache must key on the path", got, "two")
	}
}

func TestWorkbuddyDeviceTokenFileOverTheCapIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := writeDeviceTokenFile(t, dir, strings.Repeat("x", deviceTokenFileMaxLen+1))

	u := &Upstream{DeviceTokenFile: path}
	if got := u.readDeviceTokenFile(path); got != "" {
		t.Fatalf("oversized file yielded %q, want no token at all", got)
	}

	// The point of dropping the read is that no header goes out: truncating the
	// file into a header would send the first kilobyte of a log file upstream.
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	u.injectDeviceToken(req, nil)
	if got := req.Header.Get("X-Device-Token"); got != "" {
		t.Fatalf("X-Device-Token = %q, want the header to be omitted", got)
	}
}

func TestWorkbuddyDeviceTokenFileAtTheCapIsAccepted(t *testing.T) {
	path := writeDeviceTokenFile(t, t.TempDir(), strings.Repeat("x", deviceTokenFileMaxLen))

	u := &Upstream{}
	got := u.readDeviceTokenFile(path)
	if len(got) != deviceTokenFileMaxLen {
		t.Fatalf("token length = %d, want %d; the cap is a maximum, not an exclusive bound", len(got), deviceTokenFileMaxLen)
	}
}

func TestWorkbuddyDeviceTokenFileMissingIsQuiet(t *testing.T) {
	u := &Upstream{}
	if got := u.readDeviceTokenFile(filepath.Join(t.TempDir(), "not-there")); got != "" {
		t.Fatalf("missing file yielded %q, want no token", got)
	}
}

func TestWorkbuddyDeviceTokenEmptyPathReadsNothing(t *testing.T) {
	u := &Upstream{}
	if got := u.readDeviceTokenFile(""); got != "" {
		t.Fatalf("empty path yielded %q, want no token", got)
	}
}

func TestWorkbuddyResolveDeviceTokenPrefersTheAccountThenTheConfig(t *testing.T) {
	path := writeDeviceTokenFile(t, t.TempDir(), "from-file")

	u := &Upstream{DeviceToken: "from-config", DeviceTokenFile: path}
	if got := u.resolveDeviceToken(&Auth{DeviceToken: "from-account"}); got != "from-account" {
		t.Fatalf("token = %q, want the account value", got)
	}
	if got := u.resolveDeviceToken(&Auth{}); got != "from-config" {
		t.Fatalf("token = %q, want the config value", got)
	}
	if got := u.resolveDeviceToken(nil); got != "from-config" {
		t.Fatalf("token = %q, want the config value for a nil account", got)
	}

	u.DeviceToken = ""
	if got := u.resolveDeviceToken(nil); got != "from-file" {
		t.Fatalf("token = %q, want the file value", got)
	}
}
