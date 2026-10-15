package lobsterai

// tlsprofile_test.go pins the transport-choice rules for the module's own
// client. No network is touched: the only property under test is which
// *http.Client httpClient() returns.

import (
	"encoding/json"
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func newClientWithProfile(t *testing.T, profile string, shared *http.Client) *Client {
	t.Helper()
	raw := `{}`
	if profile != "" {
		raw = `{"tls_profile":"` + profile + `"}`
	}
	got, err := New(core.Deps{
		Config:     json.RawMessage(raw),
		HTTPClient: shared,
		DataDir:    t.TempDir(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New(tls_profile=%q): %v", profile, err)
	}
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	return c
}

func TestAnUnsetTLSProfileKeepsTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "", shared)
	if got := c.httpClient(); got != shared {
		t.Fatal("an unset tls_profile must keep the shared client so the default behaves as before")
	}
	if got := c.cfg.tlsProfile(); got != fingerprint.ProfileNone {
		t.Fatalf("tlsProfile() = %q, want the off switch", string(got))
	}
}

func TestANamedTLSProfileReplacesTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "chrome", shared)
	got := c.httpClient()
	if got == shared {
		t.Fatal("a named tls_profile must not reuse the shared client: the handshake imitation is the whole point")
	}
	if got == nil {
		t.Fatal("httpClient() returned nil for a named profile")
	}
	if c.cfg.tlsProfile() != fingerprint.ProfileChrome {
		t.Fatalf("tlsProfile() = %q, want chrome", string(c.cfg.tlsProfile()))
	}
}

func TestAnUnknownTLSProfileFallsBackToTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := newClientWithProfile(t, "netscape", shared)
	if got := c.httpClient(); got != shared {
		t.Fatal("an unbuildable profile must fall back to the shared client, not take the module down")
	}
}
