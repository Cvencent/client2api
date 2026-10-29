package fingerprint

import (
	"slices"
	"testing"
)

// The captured fingerprint this profile exists to reproduce. The capture is the
// Bun runtime the OpenCode CLI is packaged with (v2.2.0, node_modules/bun), taken
// through an openssl s_server handshake on localhost — a real ClientHello, the
// client's own bytes. The emitted hello keeps Bun's cipher list and its
// extension set, and departs from the capture where opencode.ai's edge requires
// it: the key share offers X25519MLKEM768 before X25519 (an X25519-only share is
// refused with a TLS handshake_failure), so the JA3 below is the captured one
// with the group list and key-share extension widened. See the bunSpec comment.
const (
	bunJA3    = "0303,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,65281-0-11-10-35-16-22-23-13-43-45-51-5-18,4588-29-23-24,0-1-2"
	bunJA3MD5 = "1b70e35f13bd1edd9648c62fa8c7e4df"
)

// TestTheBunProfileReproducesTheEmittedJA3 pins the profile's wire bytes: a real
// handshake, a raw socket, and a byte-for-byte JA3 match. The constant moves only
// with a deliberate change to the spec, so an accidental edit to the cipher list
// or the extension order fails here first.
func TestTheBunProfileReproducesTheEmittedJA3(t *testing.T) {
	got, ch := computedJA3(t, Options{Profile: ProfileBun, Protocol: ProtocolH1})

	if got != bunJA3 {
		t.Errorf("the emitted ClientHello is not the pinned one:\n got %s\nwant %s", got, bunJA3)
	}
	if sum := ja3MD5(got); sum != bunJA3MD5 {
		t.Errorf("JA3 md5 = %s, want %s", sum, bunJA3MD5)
	}
	t.Logf("emitted JA3: %s", got)
	t.Logf("emitted JA3 md5: %s", ja3MD5(got))

	if n := len(ch.cipherSuites); n != 17 {
		t.Errorf("the spec offers 17 cipher suites, got %d: %v", n, ch.cipherSuites)
	}
	if !slices.Equal(ch.alpn, []string{"http/1.1"}) {
		t.Errorf("ProtocolH1 offers http/1.1 alone, got %v", ch.alpn)
	}
	for _, want := range []uint16{0x0304, 0x0303} {
		if !slices.Contains(ch.versions, want) {
			t.Errorf("supported_versions must offer 0x%04x, got %v", want, ch.versions)
		}
	}
	// 4588 is X25519MLKEM768 — the group the gate's edge requires in the key
	// share. Not a GREASE value.
	if !slices.Contains(ch.groups, 4588) {
		t.Errorf("supported_groups must offer X25519MLKEM768 (4588) first, got %v", ch.groups)
	}

	// No GREASE anywhere: the capture has none in either list.
	if hasGREASE(ch.cipherSuites) {
		t.Errorf("the emitted hello has no GREASE cipher suite, got %v", ch.cipherSuites)
	}
	if hasGREASE(ch.extensionTypes()) || hasGREASE(ch.groups) {
		t.Errorf("the emitted hello has no GREASE in its extensions or groups: %v / %v",
			ch.extensionTypes(), ch.groups)
	}
}

// TestTheBunJA3IsNotAnyOtherProfiles is the negative control: without it, a
// helper that returned the constant would pass the assertion above. The node
// profile is the closest relative — both are JS runtimes with http/1.1 ALPN —
// so it must produce a different digest for the bun assertion to mean anything.
func TestTheBunJA3IsNotAnyOtherProfiles(t *testing.T) {
	for _, profile := range []Profile{ProfileNode, ProfileChrome, ProfileFirefox, ProfileSafari} {
		got, _ := computedJA3(t, Options{Profile: profile, Protocol: ProtocolH1})
		if sum := ja3MD5(got); sum == bunJA3MD5 {
			t.Errorf("profile %q produced the bun md5 %s, so the bun assertion proves nothing",
				profile, sum)
		}
	}
}

// TestTheBunProfileWidensALPNForHTTP2 covers the protocol contract: the captured
// hello offers http/1.1 alone, but a transport that means to speak h2 has to
// offer h2, so the profile rewrites its own ALPN extension rather than pinning
// the captured list. JA3 carries the ALPN extension's type and not its contents,
// so the digest must not move when it widens.
func TestTheBunProfileWidensALPNForHTTP2(t *testing.T) {
	got, ch := computedJA3(t, Options{Profile: ProfileBun, Protocol: ProtocolH2})

	if !slices.Contains(ch.alpn, "h2") {
		t.Errorf("ProtocolH2 must offer h2 for this profile, got %v", ch.alpn)
	}
	if !slices.Contains(ch.alpn, "http/1.1") {
		t.Errorf("the widened list should keep http/1.1 as the fallback, got %v", ch.alpn)
	}
	if sum := ja3MD5(got); sum != bunJA3MD5 {
		t.Errorf("widening ALPN moved the JA3 md5 to %s, want %s; JA3 has no ALPN contents",
			sum, bunJA3MD5)
	}
}
