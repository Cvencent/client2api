package tabbit

import (
	"testing"

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
}
