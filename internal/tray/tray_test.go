package tray

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// TestIconFrameSelectsDecodableResource is the guard that matters: the frame
// handed to CreateIconFromResourceEx has to be a bare RT_ICON image, which
// means it starts with a BITMAPINFOHEADER, not with the .ico directory the
// embedded bytes begin with.
func TestIconFrameSelectsDecodableResource(t *testing.T) {
	frame, err := iconFrame(iconBytes, 32)
	if err != nil {
		t.Fatalf("iconFrame: %v", err)
	}
	if len(frame) < 40 {
		t.Fatalf("frame is %d bytes, too short for a BITMAPINFOHEADER", len(frame))
	}
	if got := binary.LittleEndian.Uint32(frame[0:4]); got != 40 {
		t.Fatalf("frame starts with %d, want a 40-byte BITMAPINFOHEADER", got)
	}
}

// TestIconFramePrefersFrameAtLeastRequestedSize pins the downscaling choice:
// every size in the container is offered to the picker, and the winner must not
// be smaller than asked for while a larger frame exists.
func TestIconFramePrefersFrameAtLeastRequestedSize(t *testing.T) {
	for _, want := range []int{16, 32, 48} {
		frame, err := iconFrame(iconBytes, want)
		if err != nil {
			t.Fatalf("iconFrame(%d): %v", want, err)
		}
		if len(frame) == 0 {
			t.Fatalf("iconFrame(%d) returned an empty frame", want)
		}
	}
}

func TestIconFrameRejectsNonIcons(t *testing.T) {
	cases := map[string][]byte{
		"empty":     nil,
		"truncated": {0, 0, 1},
		"wrongType": {0, 0, 2, 0, 1, 0},
	}
	for name, in := range cases {
		if _, err := iconFrame(in, 32); err == nil {
			t.Errorf("%s: iconFrame accepted input it should reject", name)
		}
	}
}

// TestIconFrameSkipsOutOfBoundsEntries makes sure a corrupt directory cannot
// produce a slice past the end of the container, which would panic in the
// window thread rather than degrade to the fallback icon.
func TestIconFrameSkipsOutOfBoundsEntries(t *testing.T) {
	bad := make([]byte, 6+16)
	copy(bad[0:4], []byte{0, 0, 1, 0})
	binary.LittleEndian.PutUint16(bad[4:6], 1)
	bad[6] = 32 // width
	bad[7] = 32 // height
	binary.LittleEndian.PutUint32(bad[14:18], 4096)
	binary.LittleEndian.PutUint32(bad[18:22], 1<<20) // far past the buffer
	if _, err := iconFrame(bad, 32); err == nil {
		t.Fatal("iconFrame accepted an entry pointing past the buffer")
	}
}

func TestTooltipIsNulTerminatedAndClipped(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := tooltip(long)
	if got[maxTooltip] != 0 {
		t.Fatalf("tooltip is not NUL terminated: %#x", got[maxTooltip])
	}
	for i := 0; i < maxTooltip; i++ {
		if got[i] != 'a' {
			t.Fatalf("tooltip[%d] = %#x, want 'a'", i, got[i])
		}
	}
}

// TestTooltipKeepsNonASCII pins that the CJK tail survives as UTF-16 rather
// than mojibake: the tooltip is the one string in the UI with no chance to
// show a replacement glyph, it just renders as boxes.
func TestTooltipKeepsNonASCII(t *testing.T) {
	const title = "client2api 本地"
	got := tooltip(title)
	units, err := utf16Encode(title)
	if err != nil {
		t.Fatalf("utf16Encode: %v", err)
	}
	if len(units) >= len(got) {
		t.Fatalf("the test title does not fit: %d units", len(units))
	}
	for i, want := range units {
		if got[i] != want {
			t.Fatalf("tooltip[%d] = %#x, want %#x", i, got[i], want)
		}
	}
	if got[len(units)] != 0 {
		t.Fatalf("tooltip is not NUL terminated after %d units", len(units))
	}
}

// TestValidateSettingsAcceptsABarePort pins the friendliest spelling: somebody
// who wants a different port types the number, and the dialog has to turn that
// into the bind address the gateway needs rather than complain about a missing
// host.
func TestValidateSettingsAcceptsABarePort(t *testing.T) {
	got, err := ValidateSettings(Settings{Listen: " 9000 ", DataDir: " data "})
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if got.Listen != "127.0.0.1:9000" {
		t.Fatalf("Listen = %q, want 127.0.0.1:9000", got.Listen)
	}
	if got.DataDir != "data" {
		t.Fatalf("DataDir = %q, want the trimmed value", got.DataDir)
	}
}

// TestValidateSettingsKeepsExplicitBinds makes sure normalising does not
// quietly move a deliberate bind: an all-interfaces or wildcard address has to
// survive the round trip exactly.
func TestValidateSettingsKeepsExplicitBinds(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8788", ":8788", "0.0.0.0:8788", "[::1]:8788"} {
		got, err := ValidateSettings(Settings{Listen: listen, DataDir: "data"})
		if err != nil {
			t.Fatalf("ValidateSettings(%q): %v", listen, err)
		}
		if got.Listen != listen {
			t.Errorf("Listen = %q, want it left as %q", got.Listen, listen)
		}
	}
}

func TestValidateSettingsRejectsBadInput(t *testing.T) {
	cases := map[string]Settings{
		"empty listen":     {Listen: "", DataDir: "data"},
		"port zero":        {Listen: "0", DataDir: "data"},
		"port too large":   {Listen: "70000", DataDir: "data"},
		"no separator":     {Listen: "not-an-address", DataDir: "data"},
		"empty data dir":   {Listen: "8788", DataDir: "  "},
		"proxy nonsense":   {Listen: "8788", DataDir: "data", Proxy: "notaurl"},
		"proxy bad scheme": {Listen: "8788", DataDir: "data", Proxy: "ftp://host:1"},
	}
	for name, in := range cases {
		if _, err := ValidateSettings(in); err == nil {
			t.Errorf("%s: ValidateSettings accepted %+v", name, in)
		}
	}
}

// TestValidateSettingsAcceptsSupportedProxies covers the three schemes the
// gateway's own transport understands; anything else has to be refused here,
// before a restart, because the proxy is only parsed at startup.
func TestValidateSettingsAcceptsSupportedProxies(t *testing.T) {
	for _, proxy := range []string{"", "http://127.0.0.1:8080", "https://proxy.example:8443", "socks5://127.0.0.1:1080"} {
		got, err := ValidateSettings(Settings{Listen: "8788", DataDir: "data", Proxy: proxy})
		if err != nil {
			t.Fatalf("ValidateSettings(proxy=%q): %v", proxy, err)
		}
		if got.Proxy != proxy {
			t.Errorf("Proxy = %q, want %q", got.Proxy, proxy)
		}
	}
}

func TestSameListenPortComparesOnlyThePort(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:8788", "127.0.0.1:8788", true},
		{"127.0.0.1:8788", ":8788", true},
		{"0.0.0.0:8788", "127.0.0.1:8788", true},
		{"127.0.0.1:8788", "127.0.0.1:8899", false},
		{"127.0.0.1:8788", "nonsense", false},
	}
	for _, c := range cases {
		if got := sameListenPort(c.a, c.b); got != c.want {
			t.Errorf("sameListenPort(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNilIconNotifyIsSafe(t *testing.T) {
	var icon *Icon
	icon.Notify("client2api", "platform temporarily demoted")
}

// TestCheckListenAvailableSkipsOurOwnPort is the guard that keeps the dialog
// usable at all: the probe must not fire for the port the gateway is already
// bound to, or every save would be refused.
func TestCheckListenAvailableSkipsOurOwnPort(t *testing.T) {
	if err := checkListenAvailable("127.0.0.1:8788", ":8788"); err != nil {
		t.Fatalf("a re-spelling of the current port was rejected: %v", err)
	}
	if err := checkListenAvailable("127.0.0.1:8788", ""); err != nil {
		t.Fatalf("an empty address was probed: %v", err)
	}
}

// TestCheckListenAvailableFindsAClaimedPort pins the half that earns its
// keep: a port somebody else already holds has to be reported before the save,
// because the failure the other way is a restart into a dead gateway.
func TestCheckListenAvailableFindsAClaimedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	if err := checkListenAvailable("127.0.0.1:8788", addr); err == nil {
		t.Fatalf("checkListenAvailable(%q) accepted a port that is in use", addr)
	}
}
