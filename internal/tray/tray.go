// Package tray owns the notification-area icon the gateway shows while it is
// running.
//
// It exists for the install the panel hands out: the shortcut launches the
// gateway with no console, so without an icon a running service is invisible --
// there is no window to close, no obvious way back to the panel, and stopping
// it means hunting for the process in Task Manager.  The icon is that missing
// handle: hover shows the address, double-click opens the panel, and the menu
// carries the panel, the data directory and a clean shutdown.
//
// The surface is deliberately one struct and two calls so that cmd/client2api
// needs no build tags.  Every non-Windows build -- the container image, the CI
// matrix -- gets the stub, where Start returns nil.
package tray

import (
	// Blank: required by the //go:embed directive below, which may only name
	// files when this package is in the build.
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Options describes the icon and what its menu offers.
type Options struct {
	// Title is the hover tooltip.  Windows keeps the first 127 characters.
	Title string
	// PanelURL is opened by a double click and by the menu's first item.
	PanelURL string
	// Version is what the About dialog reports.  Empty means "unknown",
	// and the dialog says so rather than showing a blank line where a
	// version belongs.
	Version string
	// DataDir adds an "open data directory" item when it is not empty.
	DataDir string
	// OnRestart restarts the process.  It adds a "restart" menu item, and
	// the settings dialog runs it after a save that only a restart applies.
	OnRestart func() error
	// LoadSettings and SaveSettings back the settings dialog.  Without a
	// loader there is no dialog and no menu item: the tray must not offer a
	// form it can neither fill nor persist.
	LoadSettings func() (Settings, error)
	SaveSettings func(Settings) error
	// FullSettingsURL opens the panel's own config page, which owns
	// everything the dialog deliberately does not: aliases, per-platform
	// policy, the timetable, credentials.
	FullSettingsURL string
	// OnExit runs when the operator picks Quit from the menu.  Shutdown is the
	// caller's job: the tray only reports the intent.
	OnExit func()
	// Logf receives setup failures.  A missing icon must never stop the
	// gateway, so every failure here is a log line, not an error return.
	Logf func(format string, args ...any)
}

// Settings is the slice of the configuration the tray's dialog edits: the
// three connection-level keys that have to be right before the panel is
// reachable at all.
//
// It is deliberately not the whole config.  Aliases, routing, the timetable
// and credentials live on the panel's config page, which has the room for
// them; the dialog carries a link to it.  What is here is the set that
// decides whether that page can be opened in the first place.
type Settings struct {
	// Listen is the bind address, "host:port" or a bare port.
	Listen string
	// DataDir is where accounts, caches and usage live.
	DataDir string
	// Proxy is the outbound HTTP proxy; empty means a direct connection.
	Proxy string
}

// ValidateSettings normalises what the dialog collected and refuses what the
// gateway could not act on.  It returns the values to save, which may differ
// from the input: a bare port is expanded to a loopback bind address.
func ValidateSettings(s Settings) (Settings, error) {
	listen, err := normalizeListen(s.Listen)
	if err != nil {
		return Settings{}, err
	}
	dataDir := strings.TrimSpace(s.DataDir)
	if dataDir == "" {
		return Settings{}, errors.New("data directory cannot be empty")
	}
	proxy := strings.TrimSpace(s.Proxy)
	if proxy != "" {
		if err := validateProxy(proxy); err != nil {
			return Settings{}, err
		}
	}
	return Settings{Listen: listen, DataDir: dataDir, Proxy: proxy}, nil
}

// normalizeListen accepts the two spellings an operator actually types.  A
// full "host:port" is passed through; a bare port is the first thing somebody
// who wants a different port types, so it becomes a loopback bind rather than
// an error about missing fields.
func normalizeListen(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("listen address cannot be empty")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if err := checkPort(n); err != nil {
			return "", err
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(n)), nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", errors.New("listen address must look like host:port, e.g. 127.0.0.1:8788 (a bare port also works)")
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("port %q is not a number", port)
	}
	if err := checkPort(n); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

func checkPort(n int) error {
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", n)
	}
	return nil
}

// validateProxy checks only what the gateway's own client would reject: the
// URL has to parse and carry a scheme the transport understands.  Whether
// the proxy answers is the operator's business, not this form's.
func validateProxy(proxy string) error {
	u, err := url.Parse(proxy)
	if err != nil {
		return errors.New("proxy must be a URL, e.g. http://127.0.0.1:8080")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("proxy must be a URL, e.g. http://127.0.0.1:8080")
	}
	if u.Host == "" {
		return errors.New("proxy must be a URL, e.g. http://127.0.0.1:8080")
	}
	return nil
}

// checkListenAvailable refuses a bind address that is already taken, so the
// settings dialog can say so before the change is written and the process is
// restarted into a port it cannot have.  The probe is skipped when the address
// keeps the current port: the running gateway is holding that port itself, so
// probing it would always report a conflict.
func checkListenAvailable(current, next string) error {
	if next == "" || sameListenPort(current, next) {
		return nil
	}
	ln, err := net.Listen("tcp", next)
	if err != nil {
		return fmt.Errorf("listen address %s is not available: %w", next, err)
	}
	return ln.Close()
}

// sameListenPort reports whether two spellings name the same port, which is
// the only part a second bind of our own would collide on.
func sameListenPort(a, b string) bool {
	_, ap, aerr := net.SplitHostPort(a)
	_, bp, berr := net.SplitHostPort(b)
	return aerr == nil && berr == nil && ap != "" && ap == bp
}

// aboutText is the body of the tray's About dialog.  It is a pure function of
// the Options so the version line is testable without a window station.
func aboutText(opts Options) string {
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "未知"
	}
	var b strings.Builder
	b.WriteString("client2api " + version + "\n")
	b.WriteString("一个把多个 AI 平台账号聚合成 OpenAI 兼容网关的本地服务。\n")
	if url := strings.TrimSpace(opts.PanelURL); url != "" {
		b.WriteString("\n面板：" + url + "\n")
	}
	if dir := strings.TrimSpace(opts.DataDir); dir != "" {
		b.WriteString("数据目录：" + dir + "\n")
	}
	return b.String()
}

// Icon is a live notification-area icon.  A nil *Icon is usable and means "no
// icon", which is what a non-Windows build and a failed setup both produce, so
// callers never branch on the platform.
type Icon struct {
	stop   func()
	notify func(title, message string)
}

type backend struct {
	stop   func()
	notify func(title, message string)
}

// Start shows the icon and returns immediately; message pumping happens on a
// goroutine of its own.  It returns nil when the platform has no notification
// area or the icon could not be created.
func Start(opts Options) *Icon {
	b, err := start(opts)
	if err != nil {
		if opts.Logf != nil {
			opts.Logf("tray icon: %v", err)
		}
		return nil
	}
	if b.stop == nil {
		return nil
	}
	return &Icon{stop: b.stop, notify: b.notify}
}

// Notify shows a balloon notification.  A nil Icon drops the notification,
// which keeps non-Windows and headless callers branch-free.
func (i *Icon) Notify(title, message string) {
	if i == nil || i.notify == nil {
		return
	}
	i.notify(title, message)
}

// Stop removes the icon and waits for its message loop to end.  Calling it on a
// nil Icon is a no-op, so "defer icon.Stop()" is safe on every platform.
func (i *Icon) Stop() {
	if i == nil || i.stop == nil {
		return
	}
	i.stop()
	i.stop = nil
}

// The installer's icon, embedded rather than read from disk: the gateway is
// also run straight out of the build tree and copied around by hand, and the
// icon should not depend on a sibling file surviving that.
//
//go:embed icon.ico
var iconBytes []byte

// iconFrame picks the frame closest to size x size out of an .ico container and
// returns its image data in the layout CreateIconFromResourceEx wants.  An .ico
// is a directory followed by per-size images; the API takes one of those images
// on its own, so the container has to be walked here.
func iconFrame(ico []byte, size int) ([]byte, error) {
	if len(ico) < 6 {
		return nil, errors.New("icon is truncated")
	}
	if binary.LittleEndian.Uint16(ico[0:2]) != 0 || binary.LittleEndian.Uint16(ico[2:4]) != 1 {
		return nil, errors.New("not an icon file")
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))

	// Prefer the smallest frame that is at least the requested size: Windows
	// scales a too-large frame down cleanly, while scaling a 16x16 up to the
	// notification area leaves it visibly soft.
	best, bestScore := -1, 0
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(ico) {
			break
		}
		w, h := frameExtent(ico[off]), frameExtent(ico[off+1])
		length := int(binary.LittleEndian.Uint32(ico[off+8 : off+12]))
		start := int(binary.LittleEndian.Uint32(ico[off+12 : off+16]))
		if length <= 0 || start < 0 || start+length > len(ico) {
			continue
		}
		score := abs(w-size) + abs(h-size)
		if w < size || h < size {
			score += 1000
		}
		if best < 0 || score < bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return nil, errors.New("icon has no usable frame")
	}
	off := 6 + best*16
	length := int(binary.LittleEndian.Uint32(ico[off+8 : off+12]))
	start := int(binary.LittleEndian.Uint32(ico[off+12 : off+16]))
	return ico[start : start+length], nil
}

// frameExtent decodes one dimension of an icon directory entry, where 0 stands
// for 256 -- the largest value a single byte cannot hold.
func frameExtent(b byte) int {
	if b == 0 {
		return 256
	}
	return int(b)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// maxTooltip is szTip's capacity in UTF-16 units, minus the terminator.
const maxTooltip = 127

// Balloon text capacities, also excluding their terminators.
const (
	maxBalloonText  = 255
	maxBalloonTitle = 63
)

// tooltip renders s into an szTip buffer, clipping rather than failing: a long
// address should shrink the tooltip, not cost the icon.
func tooltip(s string) [maxTooltip + 1]uint16 {
	var out [maxTooltip + 1]uint16
	if s == "" {
		s = "client2api"
	}
	units, err := utf16Encode(s)
	if err != nil {
		return out
	}
	if len(units) > maxTooltip {
		units = units[:maxTooltip]
	}
	copy(out[:], units)
	return out
}

// balloonText clips a string to a Win32 fixed-width buffer and returns it in
// UTF-16 with the required terminating NUL.
func balloonText(s string, limit int) []uint16 {
	units, err := utf16Encode(s)
	if err != nil || limit <= 0 {
		return make([]uint16, 1)
	}
	if len(units) > limit {
		units = units[:limit]
	}
	out := make([]uint16, len(units)+1)
	copy(out, units)
	return out
}

// utf16Encode is split out so Win32 buffer logic stays testable without the
// Windows syscalls.

func utf16Encode(s string) ([]uint16, error) {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x10000:
			out = append(out, uint16(r))
		case r <= 0x10FFFF:
			r -= 0x10000
			out = append(out, 0xD800+uint16(r>>10), 0xDC00+uint16(r&0x3FF))
		default:
			return nil, fmt.Errorf("tray: %q is not a Unicode scalar value", r)
		}
	}
	return out, nil
}
