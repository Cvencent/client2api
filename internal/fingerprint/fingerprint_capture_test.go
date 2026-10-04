package fingerprint

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The ClientHellos real desktop clients sent to the vendor hosts this project's
// modules actually talk to. They were captured by splitting the TLS record out
// of a blind TCP passthrough (client2api-lab/_sniff/work/proxy.py, SNIFF_MODE=tlsprobe),
// so the bytes are the client's own, not a proxy's rewriting of them.
//
// Every JA3 below is written out in full rather than as an md5 alone: a
// mismatch then fails with something an operator can diff.
const (
	// api.trae.com.cn, seen three times and identical from both the Trae CN and
	// the TRAE SOLO CN desktop applications — a stable, reproducible hello.
	// TLS 1.2 only (no supported_versions extension at all), no ALPN.
	captureTraeJA3 = "0303,49196-49195-49200-49199-49188-49187-49192-49191-49162-49161-49172-49171-157-156-61-60-53-47,0-10-11-13-35-23-65281,29-23-24,0"
	captureTraeMD5 = "3ad2731f6e4c8022713ec48eca82ade9"

	// zcode.z.ai: Chromium/BoringSSL shaped, GREASE in the cipher list and at
	// both ends of the extension list, so the raw string is not stable across
	// connections — compare the GREASE-free form.
	captureZCodeJA3 = "0303,27242-4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,2570-45-65037-43-23-10-5-11-51-27-35-0-13-17613-16-65281-18-60138,60138-4588-29-23-24,0"
	captureZCodeMD5 = "f5b1d00c0a22cc60e449764398576e50"

	// gateway.qoder.com.cn and static.qoder.com.cn shared this one. It is not
	// the host this project's qwenwork module uses (that is gateway.qwenwork.cn,
	// a different product), so it is corroborating evidence and nothing more.
	captureQoderJA3 = "0303,49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-4865-4866-4867,0-11-65281-23-18-5-10-13-16-43-51,4588-29-23-24-25,0"
	captureQoderMD5 = "0a0021494b3074afab4dd7a3d5caeffe"

	// bytegate-sg.byteintlapi.com, from inside the Trae CN application, and
	// mobile.events.data.microsoft.com. Both show Go's own crypto/tls shape:
	// no ALPN, TLS 1.3 suites first, and Go's fixed extension order. This is the
	// reference shape ProfileGolang exists to reproduce.
	captureGolangJA3 = "0303,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-13-51-45-43-21,29-23-24,0"
	captureGolangMD5 = "7e7f22c8ab1d793e7ed1fe2e53be8c42"
)

// surveyedProfiles is every named profile the package offers, minus the
// randomized one, whose output is not meant to be stable.
func surveyedProfiles() []Profile {
	return []Profile{
		ProfileNone,
		ProfileGolang,
		ProfileNode,
		ProfileTrae,
		ProfileChrome,
		ProfileChrome133,
		ProfileChrome131,
		ProfileChrome120,
		ProfileChrome106,
		ProfileFirefox,
		ProfileFirefox120,
		ProfileSafari,
		ProfileEdge,
		ProfileIOS,
		ProfileAndroid,
	}
}

// splitJA3 takes a JA3 string apart so its lists can be normalised.
func splitJA3(t *testing.T, s string) [5][]string {
	t.Helper()
	parts := strings.Split(s, ",")
	if len(parts) != 5 {
		t.Fatalf("JA3 %q has %d comma-separated fields, want 5", s, len(parts))
	}
	var out [5][]string
	for i, p := range parts {
		if p == "" {
			out[i] = nil
			continue
		}
		out[i] = strings.Split(p, "-")
	}
	return out
}

// greaseFreeJA3 drops GREASE values from a JA3 string's cipher, extension and
// group lists. A browser re-randomises GREASE on every connection, so an exact
// comparison against a profile can never succeed without this step — and the
// comparison is only meaningful once both sides are normalised the same way.
func greaseFreeJA3(t *testing.T, s string) string {
	t.Helper()
	f := splitJA3(t, s)
	keep := func(list []string, strip bool) []string {
		if !strip {
			return list
		}
		out := make([]string, 0, len(list))
		for _, v := range list {
			n, err := strconv.ParseUint(v, 10, 16)
			if err != nil {
				t.Fatalf("JA3 field %q is not a number: %v", v, err)
			}
			if uint16(n)&0x0f0f == 0x0a0a {
				continue
			}
			out = append(out, v)
		}
		return out
	}
	return strings.Join([]string{
		strings.Join(f[0], "-"),
		strings.Join(keep(f[1], true), "-"),
		strings.Join(keep(f[2], true), "-"),
		strings.Join(keep(f[3], true), "-"),
		strings.Join(f[4], "-"),
	}, ",")
}

// TestSurveyBuiltInProfiles is a diagnostic. It prints what every profile
// actually puts on the wire, normalised, so the profile-to-capture mapping is
// chosen from observed bytes instead of from a preset's name. It asserts only
// that the survey ran; the mapping itself is asserted by the tests below.
func TestSurveyBuiltInProfiles(t *testing.T) {
	for _, p := range surveyedProfiles() {
		got, ch := computedJA3(t, Options{Profile: p, Protocol: ProtocolH1})
		t.Logf("profile %-12q ciphers=%-3d exts=%-3d alpn=%-22v versions=%v groups=%v",
			p, len(ch.cipherSuites), len(ch.extensions), ch.alpn, ch.versions, ch.groups)
		t.Logf("    ja3      = %s", got)
		t.Logf("    md5      = %s", ja3MD5(got))
		t.Logf("    greasefree= %s", greaseFreeJA3(t, got))
		t.Logf("    gfmd5    = %s", ja3MD5(greaseFreeJA3(t, got)))
	}
}

// TestTheCapturesAgreeWithTheSurvey is a guard on the constants above: the
// recorded md5 has to be the md5 of the recorded string, or a later edit to one
// of them would silently stop describing the capture.
func TestTheCapturesAgreeWithTheSurvey(t *testing.T) {
	for _, c := range []struct{ name, ja3, md5 string }{
		{"trae", captureTraeJA3, captureTraeMD5},
		{"zcode", captureZCodeJA3, captureZCodeMD5},
		{"qoder", captureQoderJA3, captureQoderMD5},
		{"golang", captureGolangJA3, captureGolangMD5},
	} {
		if got := ja3MD5(c.ja3); got != c.md5 {
			t.Errorf("%s: md5 of the recorded JA3 is %s, but the capture md5 is %s", c.name, got, c.md5)
		}
	}
}

// TestTheCapturedTraeHelloIsTLS12Only pins the property that makes the Trae
// capture unusual and that a look-alike profile has to preserve: the client
// offers no supported_versions extension, so the handshake cannot upgrade to
// TLS 1.3.
func TestTheCapturedTraeHelloIsTLS12Only(t *testing.T) {
	f := splitJA3(t, captureTraeJA3)
	if slices.Contains(f[2], "43") {
		t.Errorf("the Trae capture must not offer supported_versions (43), got extensions %v", f[2])
	}
	if slices.Contains(f[2], "16") {
		t.Errorf("the Trae capture offers no ALPN (16), got extensions %v", f[2])
	}
	if got := f[0][0]; got != "0303" {
		t.Errorf("legacy version is %s, want 0303", got)
	}
}

// TestTheTraeProfileReproducesTheCapturedJA3 is the point of ProfileTrae: a real
// handshake against a raw socket, and a byte-for-byte match with what the Trae
// desktop applications put on the wire.
func TestTheTraeProfileReproducesTheCapturedJA3(t *testing.T) {
	got, ch := computedJA3(t, Options{Profile: ProfileTrae, Protocol: ProtocolH1})

	if got != captureTraeJA3 {
		t.Errorf("the emitted ClientHello is not the captured one:\n got %s\nwant %s", got, captureTraeJA3)
	}
	if sum := ja3MD5(got); sum != captureTraeMD5 {
		t.Errorf("JA3 md5 = %s, want %s", sum, captureTraeMD5)
	}
	t.Logf("emitted JA3: %s", got)
	t.Logf("emitted JA3 md5: %s", ja3MD5(got))

	if n := len(ch.cipherSuites); n != 18 {
		t.Errorf("the capture offers 18 cipher suites, got %d: %v", n, ch.cipherSuites)
	}
	if n := len(ch.extensions); n != 7 {
		t.Errorf("the capture offers 7 extensions, got %d: %v", n, ch.extensionTypes())
	}
	if !slices.Equal(ch.groups, []uint16{29, 23, 24}) {
		t.Errorf("supported_groups must be 29-23-24, got %v", ch.groups)
	}
	if !slices.Equal(ch.pointFormats, []uint16{0}) {
		t.Errorf("ec_point_formats must be 0 alone, got %v", ch.pointFormats)
	}
	if len(ch.alpn) != 0 {
		t.Errorf("the captured hello names no protocol at all, got %v", ch.alpn)
	}
	if _, ok := ch.extension(43); ok {
		t.Errorf("the captured hello offers no supported_versions, so TLS 1.3 must be unreachable: %v",
			ch.extensionTypes())
	}
	if ch.legacyVersion != 0x0303 {
		t.Errorf("legacy version = 0x%04x, want 0x0303", ch.legacyVersion)
	}

	// None of the curves that give away a modern Go or Chrome client may be
	// here: no post-quantum group, no FFDHE, no curve the capture did not list.
	for _, forbidden := range []uint16{4588, 4587, 4589, 30, 25, 256, 257} {
		if slices.Contains(ch.groups, forbidden) {
			t.Errorf("the capture does not offer group %d, got %v", forbidden, ch.groups)
		}
	}
	if hasGREASE(ch.cipherSuites) || hasGREASE(ch.extensionTypes()) || hasGREASE(ch.groups) {
		t.Errorf("the captured hello has no GREASE: %v / %v / %v",
			ch.cipherSuites, ch.extensionTypes(), ch.groups)
	}
}

// TestTheTraeProfileMatchesNoOtherProfile is the negative control: without it, a
// helper that returned the constant would pass the assertion above.
func TestTheTraeProfileMatchesNoOtherProfile(t *testing.T) {
	for _, p := range surveyedProfiles() {
		if p == ProfileTrae {
			continue
		}
		got, _ := computedJA3(t, Options{Profile: p, Protocol: ProtocolH1})
		if sum := ja3MD5(got); sum == captureTraeMD5 {
			t.Errorf("profile %q produced the Trae md5 %s, so the Trae assertion proves nothing",
				p, sum)
		}
	}
}

// sameStrings reports whether two lists hold the same values, ignoring order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, v := range a {
		count[v]++
	}
	for _, v := range b {
		count[v]--
		if count[v] < 0 {
			return false
		}
	}
	return true
}

// TestTheChromeProfileMatchesTheCapturedZCodeHello documents the limit of what
// a profile can achieve against a Chrome-family capture. ZCode's cipher list,
// group list and set of extension types are exactly the chrome preset's once
// GREASE is normalised away — but the extension *order* is not, and JA3 hashes
// that order, so the digests differ.
//
// Chrome permutes its extension order on every connection, so no fixed spec can
// reproduce a given captured digest; matching the three lists that JA3 shares
// with the preset is the whole of the available improvement, and this test
// fails if a future uTLS preset drifts out of that agreement.
func TestTheChromeProfileMatchesTheCapturedZCodeHello(t *testing.T) {
	got, _ := computedJA3(t, Options{Profile: ProfileChrome, Protocol: ProtocolH1})
	have := splitJA3(t, greaseFreeJA3(t, got))
	want := splitJA3(t, greaseFreeJA3(t, captureZCodeJA3))

	if !sameStrings(have[1], want[1]) {
		t.Errorf("cipher suites differ from the ZCode capture:\n got %v\nwant %v", have[1], want[1])
	}
	if !sameStrings(have[3], want[3]) {
		t.Errorf("supported_groups differ from the ZCode capture:\n got %v\nwant %v", have[3], want[3])
	}
	if !sameStrings(have[2], want[2]) {
		t.Errorf("extension types differ from the ZCode capture:\n got %v\nwant %v", have[2], want[2])
	}
	// The order is expected to differ. If it ever stops differing, this note
	// stops being true and the sentence above is wrong.
	if slices.Equal(have[2], want[2]) {
		t.Logf("the chrome preset now reproduces ZCode's extension order too: %v", have[2])
	}
	if ja3MD5(got) == captureZCodeMD5 {
		t.Errorf("the chrome preset reproduced the ZCode digest %s; update the comment above", captureZCodeMD5)
	}
}

// TestTheCapturedQoderHelloIsGoShaped records why no profile was built for the
// QoderWork capture: the host is a different product's gateway, and the hello it
// presents is Go's own shape, which this package already offers as
// ProfileGolang. The two differ only in the signature_algorithms_cert extension
// and in two post-quantum drafts, so the finding is "nothing to imitate".
func TestTheCapturedQoderHelloIsGoShaped(t *testing.T) {
	got, _ := computedJA3(t, Options{Profile: ProfileGolang, Protocol: ProtocolH1})
	have := splitJA3(t, got)
	want := splitJA3(t, captureQoderJA3)

	if !slices.Equal(have[1], want[1]) {
		t.Errorf("cipher suites:\n got %v\nwant %v", have[1], want[1])
	}
	// Extensions and groups are allowed to differ only by the additions below.
	extraExt := []string{"50"}             // signature_algorithms_cert
	extraGroup := []string{"4587", "4589"} // orphaned Kyber drafts
	var filtered []string
	for _, v := range have[2] {
		if !slices.Contains(extraExt, v) {
			filtered = append(filtered, v)
		}
	}
	if !slices.Equal(filtered, want[2]) {
		t.Errorf("extension types after dropping %v:\n got %v\nwant %v", extraExt, filtered, want[2])
	}
	filtered = nil
	for _, v := range have[3] {
		if !slices.Contains(extraGroup, v) {
			filtered = append(filtered, v)
		}
	}
	if !slices.Equal(filtered, want[3]) {
		t.Errorf("groups after dropping %v:\n got %v\nwant %v", extraGroup, filtered, want[3])
	}
}
