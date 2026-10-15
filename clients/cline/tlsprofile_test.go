package cline

// tlsprofile_test.go pins the transport-choice rules for the module's own
// client. No network is touched: the only property under test is which
// *http.Client httpClient() hands back.

import (
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func clientWithProfile(profile string, shared *http.Client) *Client {
	cfg := config{}
	if profile != "" {
		cfg.TLSProfile = profile
	}
	return &Client{deps: core.Deps{HTTPClient: shared}, cfg: cfg}
}

func TestAnUnsetTLSProfileKeepsTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := clientWithProfile("", shared)
	if c.httpClient() != shared {
		t.Fatal("an unset tls_profile must keep the shared client so the default behaves as before")
	}
	if got := c.cfg.tlsProfile(); got != fingerprint.ProfileNone {
		t.Fatalf("tlsProfile() = %q, want the off switch", string(got))
	}
}

func TestANamedTLSProfileReplacesTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c := clientWithProfile("chrome", shared)
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
	c := clientWithProfile("netscape", shared)
	if c.httpClient() != shared {
		t.Fatal("an unbuildable profile must fall back to the shared client, not take the module down")
	}
}
