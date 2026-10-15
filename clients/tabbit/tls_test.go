package tabbit

import (
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

// tls_test.go pins the transport-fingerprint config keys: the web transport
// talks to the vendor's own site with a real session cookie, so the handshake
// must be able to imitate a browser instead of Go.

func TestTabbitTLSProfileKeysAreRead(t *testing.T) {
	cfg := Config{
		TLSProfile:  " chrome ",
		TLSProtocol: " http/1.1 ",
	}.normalize()
	if got := cfg.tlsProfile(); got != fingerprint.ProfileChrome {
		t.Errorf("tlsProfile = %q, want %q", got, fingerprint.ProfileChrome)
	}
	if got := cfg.tlsProtocol(); got != fingerprint.ProtocolH1 {
		t.Errorf("tlsProtocol = %q, want %q", got, fingerprint.ProtocolH1)
	}
}

func TestTabbitTLSProfileDefaultsOff(t *testing.T) {
	cfg := Config{}.normalize()
	if got := cfg.tlsProfile(); got != fingerprint.ProfileNone {
		t.Errorf("an unset tls_profile = %q, want the off switch", got)
	}
}

func TestTabbitNamedTLSProfileBuildsItsOwnClient(t *testing.T) {
	c := newTestClient(t, `{"transport":"web","tls_profile":"chrome"}`, nil)
	if c.cfg.tlsProfile() != fingerprint.ProfileChrome {
		t.Fatalf("tls_profile was not read: %q", c.cfg.tlsProfile())
	}
	if c.vendorClient == nil {
		t.Fatal("a named tls_profile must build the dedicated vendor client")
	}
}

func TestTabbitAnUnsetTLSProfileKeepsTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	got, err := New(core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: shared,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	if c.vendorClient != nil {
		t.Fatal("an unset tls_profile must not build a replacement client")
	}
	if c.httpClient() != shared {
		t.Fatal("an unset tls_profile must keep the shared client so the default behaves as before")
	}
}

func TestTabbitAnUnknownTLSProfileFallsBackToTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	got, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     []byte(`{"tls_profile":"netscape"}`),
		HTTPClient: shared,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	if c.vendorClient != nil {
		t.Fatal("an unbuildable profile must not leave a replacement client installed")
	}
	if c.httpClient() != shared {
		t.Fatal("an unbuildable profile must fall back to the shared client")
	}
}
