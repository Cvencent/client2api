package zcode

// tlsprofile_test.go — the transport-fingerprint config keys.  Nothing here
// touches the network: the point is only that an unset key keeps whatever
// client the core handed in, so the default behaves exactly as it did before
// the key existed, and that a named one replaces it.

import (
	"encoding/json"
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

// newClientWithProfile builds a client with auto-discovery off, so the test
// never reads the operator's real credential store.
func newClientWithProfile(t *testing.T, profile string, shared *http.Client) *Client {
	t.Helper()
	raw := `{"auto_discover":false}`
	if profile != "" {
		raw = `{"auto_discover":false,"tls_profile":"` + profile + `"}`
	}
	c, err := New(core.Deps{
		Config:     json.RawMessage(raw),
		HTTPClient: shared,
		DataDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New(tls_profile=%q): %v", profile, err)
	}
	zc, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", c)
	}
	return zc
}

func TestAnUnsetTLSProfileKeepsTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "", shared)
	if c.http != shared {
		t.Fatal("an unset tls_profile must keep the client the core supplied: the default has to behave exactly as it did before the key existed")
	}
	if got := c.cfg.tlsProfile(); got != fingerprint.ProfileNone {
		t.Fatalf("tlsProfile() = %q, want the off switch", string(got))
	}
}

func TestANamedTLSProfileBuildsItsOwnClient(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "chrome", shared)
	if c.http == shared {
		t.Fatal("a named tls_profile must not reuse the shared client: replacing the handshake is the entire point of the key")
	}
	if got := c.cfg.tlsProfile(); got != fingerprint.ProfileChrome {
		t.Fatalf("tlsProfile() = %q, want chrome", string(got))
	}
}

func TestAnUnknownTLSProfileIsIgnoredRatherThanFatal(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "netscape", shared)
	if c.http != shared {
		t.Fatal("a profile uTLS cannot build must fall back to the shared client instead of taking the module down")
	}
}

func TestTheTLSProtocolKeyIsReadAndTrimmed(t *testing.T) {
	cfg := loadConfig(json.RawMessage(`{"tls_protocol":"  http/1.1  "}`), nil)
	if got := cfg.tlsProtocol(); got != fingerprint.ProtocolH1 {
		t.Fatalf("tlsProtocol() = %q, want http/1.1 after trimming", string(got))
	}
	if got := loadConfig(json.RawMessage(`{}`), nil).tlsProtocol(); got != fingerprint.ProtocolAuto {
		t.Fatalf("an unset tls_protocol = %q, want auto", string(got))
	}
}
