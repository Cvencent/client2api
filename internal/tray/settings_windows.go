//go:build windows

package tray

import (
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The tray's settings window.  It exists because "设置" has to mean a small
// native form, not a browser tab: the shortcut-launched gateway deliberately
// runs without a console, and the three keys this form edits are exactly the
// ones that decide whether the panel can be opened at all.  Sending the
// operator to a web page that may not be reachable is the one answer that
// cannot work when it is needed most.
//
// The form is deliberately narrow.  Aliases, routing policy, the timetable and
// credentials stay on the panel's own config page, which has the room for them
// and is one button away; what lives here is the connection-level set plus the
// data directory, which is the most a native dialog can do well.

// ---- Win32 constants -------------------------------------------------------

const (
	wmCommand = 0x0111
	wmSetFont = 0x0030
	wmKeyDown = 0x0100

	wsChild   = 0x40000000
	wsVisible = 0x10000000
	wsTabStop = 0x00010000
	wsCaption = 0x00C00000
	wsSysMenu = 0x00080000

	wsExClientEdge = 0x00000200

	esAutoHScroll = 0x00000080

	bsDefPushButton = 0x00000001

	ssEtchedHorz = 0x00000010

	// SetWindowPos flags for showing the settings form.  SWP_SHOWWINDOW
	// sets WS_VISIBLE without going through ShowWindow.
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040
	hwndTop       = 0

	vkReturn = 13
	vkEscape = 27

	logPixelsX = 88
	fwNormal   = 400
	fwBold     = 700

	// colorWindow is COLOR_WINDOW; the window class wants it pre-incremented.
	colorWindow = 5

	// idcArrow is IDC_ARROW, loaded from the shared system resources.
	idcArrow = 32512

	smCXScreen = 0
	smCYScreen = 1

	coinitApartmentThreaded = 0x2

	mfYesNo        = 0x00000004
	mfIconQuestion = 0x00000020
	mfIconError    = 0x00000010
	mbOK           = 0x00000000
	idYes          = 6

	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040

	// Control ids.  The buttons report through WM_COMMAND; the edits are read
	// through their handles and need no id.
	idBtnBrowse = 2001
	idBtnSave   = 2002
	idBtnCancel = 2003
	idBtnFull   = 2004
)

// ---- Win32 procs -----------------------------------------------------------

var (
	modGdi32 = windows.NewLazySystemDLL("gdi32.dll")
	modOle32 = windows.NewLazySystemDLL("ole32.dll")

	procSendMessageW         = modUser32.NewProc("SendMessageW")
	procSetWindowTextW       = modUser32.NewProc("SetWindowTextW")
	procGetWindowTextW       = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
	procSetWindowPos         = modUser32.NewProc("SetWindowPos")
	procUpdateWindow         = modUser32.NewProc("UpdateWindow")
	procSetFocus             = modUser32.NewProc("SetFocus")
	procMessageBoxW          = modUser32.NewProc("MessageBoxW")
	procLoadCursorW          = modUser32.NewProc("LoadCursorW")
	procGetDC                = modUser32.NewProc("GetDC")
	procReleaseDC            = modUser32.NewProc("ReleaseDC")
	procGetSystemMetrics     = modUser32.NewProc("GetSystemMetrics")
	procAdjustWindowRectEx   = modUser32.NewProc("AdjustWindowRectEx")
	procSetProcessDPIAware   = modUser32.NewProc("SetProcessDPIAware")

	procCreateFontW   = modGdi32.NewProc("CreateFontW")
	procGetDeviceCaps = modGdi32.NewProc("GetDeviceCaps")
	procDeleteObject  = modGdi32.NewProc("DeleteObject")

	procSHBrowseForFolderW   = modShell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = modShell32.NewProc("SHGetPathFromIDListW")

	procCoInitializeEx = modOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree  = modOle32.NewProc("CoTaskMemFree")
)

type browseInfoW struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

// ---- Dialog -----------------------------------------------------------------

// settingsDialog is one open settings window.  The message loop owns it for
// its whole life, so every field is touched from that one thread; the service
// only ever holds the window handle, to raise an already-open dialog instead of
// stacking a second one.
type settingsDialog struct {
	owner *service
	hwnd  uintptr
	font  uintptr
	big   uintptr
	dpi   int

	editListen uintptr
	editData   uintptr
	editProxy  uintptr
	status     uintptr

	// openedListen is the address the running process is bound to, kept so a
	// save can tell a real port change from a re-spelling of the same one.
	openedListen string
}

// gSettings is the window the settings message procedure dispatches into.
// There is at most one dialog per process, and only its own thread reads this.
var gSettings *settingsDialog

// showSettings opens the dialog on a thread of its own.  It is called from a
// goroutine rather than the tray thread: the tray's message loop is busy
// dispatching the menu click that got us here, and a nested modal loop on top
// of it would deadlock the menu.
func (s *service) showSettings() {
	if s.opts.LoadSettings == nil || s.opts.SaveSettings == nil {
		return
	}
	if hwnd := s.currentSettings(); hwnd != 0 {
		// Already open: raise it rather than stacking a second copy.
		procSetForegroundWindow.Call(hwnd)
		return
	}

	current, err := s.opts.LoadSettings()
	if err != nil {
		s.errorBox("client2api 设置", "读取当前设置失败：\n"+err.Error())
		return
	}

	d := &settingsDialog{owner: s}
	d.run(current)
}

func (d *settingsDialog) px(v int) int { return v * d.dpi / 96 }

// run owns the window for its whole life.  A window belongs to the thread that
// created it, so the thread is pinned; without that the scheduler could move
// this goroutine mid-loop and the window would freeze with "not responding".
func (d *settingsDialog) run(current Settings) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procSetProcessDPIAware.Call()
	procCoInitializeEx.Call(0, coinitApartmentThreaded)

	d.dpi = systemDPI()
	d.openedListen = current.Listen
	d.font = createUIFont(d.dpi, 9, false)
	d.big = createUIFont(d.dpi, 12, true)
	defer d.releaseFonts()

	className, err := windows.UTF16PtrFromString("Client2apiTraySettings")
	if err != nil {
		d.owner.errorBox("client2api 设置", "创建窗口失败："+err.Error())
		return
	}
	title, err := windows.UTF16PtrFromString("client2api 设置")
	if err != nil {
		d.owner.errorBox("client2api 设置", "创建窗口失败："+err.Error())
		return
	}
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   windows.NewCallback(settingsProc),
		hInstance:     hInstance(),
		hCursor:       cursor,
		hbrBackground: colorWindow + 1,
		lpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		d.owner.errorBox("client2api 设置", "创建窗口失败："+callErr.Error())
		return
	}

	style := uint32(wsCaption | wsSysMenu)
	clientW, clientH := d.px(460), d.px(350)
	rect := windows.Rect{Right: int32(clientW), Bottom: int32(clientH)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rect)), uintptr(style), 0, 0)
	winW := int(rect.Right - rect.Left)
	winH := int(rect.Bottom - rect.Top)
	sw, _, _ := procGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCYScreen)
	x := (int(sw) - winW) / 2
	y := (int(sh) - winH) / 2

	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, hInstance(), 0)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		d.owner.errorBox("client2api 设置", "创建窗口失败："+callErr.Error())
		return
	}
	d.hwnd = hwnd

	gSettings = d
	d.owner.setSettings(hwnd)

	d.build(current)
	// ShowWindow is avoided on purpose.  When the process is started with
	// STARTF_USESHOWWINDOW -- an installer or shortcut set to run hidden,
	// which is how a console-less gateway is usually launched -- Windows
	// substitutes that startup value for the nCmdShow of this process's
	// FIRST ShowWindow call.  SW_HIDE would then keep this form hidden
	// forever, with no error to show for it.  Setting WS_VISIBLE through
	// SetWindowPos is not subject to that substitution.
	procSetWindowPos.Call(hwnd, hwndTop, 0, 0, 0, 0,
		uintptr(swpNoMove|swpNoSize|swpShowWindow))
	procSetForegroundWindow.Call(hwnd)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(d.editListen)

	var msg msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	gSettings = nil
	d.owner.setSettings(0)
}

func (d *settingsDialog) releaseFonts() {
	if d.font != 0 {
		procDeleteObject.Call(d.font)
	}
	if d.big != 0 {
		procDeleteObject.Call(d.big)
	}
}

func settingsProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	if d := gSettings; d != nil {
		if r, handled := d.handle(hwnd, msg, wparam, lparam); handled {
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (d *settingsDialog) handle(hwnd, msg, wparam, lparam uintptr) (uintptr, bool) {
	switch msg {
	case wmCommand:
		switch wparam & 0xffff {
		case idBtnFull:
			d.owner.openURL(d.owner.opts.FullSettingsURL)
		case idBtnBrowse:
			d.browse()
		case idBtnSave:
			d.save()
		case idBtnCancel:
			procDestroyWindow.Call(hwnd)
		}
		return 0, true
	case wmKeyDown:
		switch wparam {
		case vkEscape:
			procDestroyWindow.Call(hwnd)
			return 0, true
		case vkReturn:
			d.save()
			return 0, true
		}
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0, true
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0, true
	}
	return 0, false
}

// save validates, persists and then offers the restart the change needs.  The
// gateway reads its listen address, data directory and proxy once at startup,
// so a save that skipped the restart would look successful and change nothing.
func (d *settingsDialog) save() {
	next, err := ValidateSettings(Settings{
		Listen:  d.text(d.editListen),
		DataDir: d.text(d.editData),
		Proxy:   d.text(d.editProxy),
	})
	if err != nil {
		d.setStatus(err.Error())
		return
	}
	if err := checkListenAvailable(d.openedListen, next.Listen); err != nil {
		d.setStatus(err.Error())
		return
	}
	if err := d.owner.opts.SaveSettings(next); err != nil {
		d.setStatus("保存失败：" + err.Error())
		return
	}

	// Echo the normalised values back, so a bare port the operator typed shows
	// as the address that was actually written.
	d.setText(d.editListen, next.Listen)
	d.setText(d.editData, next.DataDir)
	d.setText(d.editProxy, next.Proxy)
	d.setStatus("已保存。")

	if d.owner.opts.OnRestart == nil {
		d.owner.infoBox("client2api 设置", "设置已保存。重启网关后生效。")
		procDestroyWindow.Call(d.hwnd)
		return
	}
	if !d.confirmRestart() {
		procDestroyWindow.Call(d.hwnd)
		return
	}
	restart := d.owner.opts.OnRestart
	procDestroyWindow.Call(d.hwnd)
	go func() {
		if err := restart(); err != nil {
			d.owner.errorBox("client2api 设置", "重启失败：\n"+err.Error())
		}
	}()
}

func (d *settingsDialog) confirmRestart() bool {
	title, _ := windows.UTF16PtrFromString("client2api 设置")
	text, _ := windows.UTF16PtrFromString(
		"设置已保存。\n\n修改监听地址、数据目录或代理需要重启网关才能生效。\n\n现在重启吗？")
	r, _, _ := procMessageBoxW.Call(d.hwnd,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		mfYesNo|mfIconQuestion)
	runtime.KeepAlive(title)
	runtime.KeepAlive(text)
	return r == idYes
}

// build lays out every control once.  All coordinates are in 96-DPI units and
// scaled through px, so the form keeps its proportions on a high-DPI display.
func (d *settingsDialog) build(cur Settings) {
	d.ctl(0, "STATIC", "client2api 设置", wsChild|wsVisible, 0, 22, 14, 416, 26, d.big)
	d.ctl(0, "STATIC", "", wsChild|wsVisible|ssEtchedHorz, 0, 22, 48, 416, 2, 0)

	d.label("监听地址", 22, 62, 416, 18)
	d.editListen = d.ctl(0, "EDIT", cur.Listen, wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, 22, 82, 416, 24, 0)
	d.label("端口或 host:port，例如 8788 或 127.0.0.1:8788", 22, 110, 416, 16)

	d.label("数据目录", 22, 136, 416, 18)
	d.editData = d.ctl(0, "EDIT", cur.DataDir, wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, 22, 156, 326, 24, 0)
	d.ctl(idBtnBrowse, "BUTTON", "浏览…", wsChild|wsVisible|wsTabStop, 0, 356, 156, 82, 24, 0)

	d.label("出站代理（可留空）", 22, 188, 416, 18)
	d.editProxy = d.ctl(0, "EDIT", cur.Proxy, wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, 22, 208, 416, 24, 0)
	d.label("留空表示直连；支持 http、https、socks5", 22, 236, 416, 16)

	d.status = d.ctl(0, "STATIC", "", wsChild|wsVisible, 0, 22, 262, 416, 30, 0)

	d.ctl(idBtnFull, "BUTTON", "打开完整设置…", wsChild|wsVisible|wsTabStop, 0, 22, 302, 118, 28, 0)
	d.ctl(idBtnCancel, "BUTTON", "取消", wsChild|wsVisible|wsTabStop, 0, 256, 302, 84, 28, 0)
	d.ctl(idBtnSave, "BUTTON", "保存", wsChild|wsVisible|wsTabStop|bsDefPushButton, 0, 348, 302, 90, 28, 0)
}

func (d *settingsDialog) ctl(id int, class, text string, style, exStyle uint32, x, y, cx, cy int, font uintptr) uintptr {
	cls, _ := windows.UTF16PtrFromString(class)
	txt, _ := windows.UTF16PtrFromString(text)
	h, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(txt)),
		uintptr(style),
		uintptr(d.px(x)), uintptr(d.px(y)), uintptr(d.px(cx)), uintptr(d.px(cy)),
		d.hwnd, uintptr(id), hInstance(), 0)
	if font != 0 {
		procSendMessageW.Call(h, wmSetFont, font, 1)
	}
	runtime.KeepAlive(cls)
	runtime.KeepAlive(txt)
	return h
}

func (d *settingsDialog) label(text string, x, y, cx, cy int) uintptr {
	return d.ctl(0, "STATIC", text, wsChild|wsVisible, 0, x, y, cx, cy, d.font)
}

func (d *settingsDialog) text(h uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(h)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

func (d *settingsDialog) setText(h uintptr, text string) {
	t, _ := windows.UTF16PtrFromString(text)
	procSetWindowTextW.Call(h, uintptr(unsafe.Pointer(t)))
	runtime.KeepAlive(t)
}

func (d *settingsDialog) setStatus(text string) {
	d.setText(d.status, text)
}

func (d *settingsDialog) browse() {
	var display [windows.MAX_PATH]uint16
	title, _ := windows.UTF16PtrFromString("选择数据目录")
	bi := browseInfoW{
		hwndOwner:      d.hwnd,
		pszDisplayName: &display[0],
		lpszTitle:      title,
		ulFlags:        bifReturnOnlyFSDirs | bifEditBox | bifNewDialogStyle,
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	runtime.KeepAlive(title)
	if pidl == 0 {
		return
	}
	var path [windows.MAX_PATH]uint16
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	procCoTaskMemFree.Call(pidl)
	if ok == 0 {
		return
	}
	d.setText(d.editData, windows.UTF16ToString(path[:]))
}

// ---- Fonts / DPI -----------------------------------------------------------

func systemDPI() int {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 96
	}
	dpi, _, _ := procGetDeviceCaps.Call(hdc, logPixelsX)
	procReleaseDC.Call(0, hdc)
	if int(dpi) < 96 {
		return 96
	}
	return int(dpi)
}

func createUIFont(dpi, sizePt int, bold bool) uintptr {
	face, _ := windows.UTF16PtrFromString("Microsoft YaHei UI")
	weight := fwNormal
	if bold {
		weight = fwBold
	}
	height := -(sizePt * dpi) / 72
	h, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight),
		0, 0, 0, 0x86 /* GB2312_CHARSET */, 0, 0, 0x5 /* CLEARTYPE_QUALITY */, 0,
		uintptr(unsafe.Pointer(face)))
	runtime.KeepAlive(face)
	return h
}

// ---- Message boxes ---------------------------------------------------------

func (s *service) errorBox(title, text string) {
	s.messageBox(title, text, mbOK|mfIconError)
}

func (s *service) infoBox(title, text string) {
	s.messageBox(title, text, mbOK)
}

func (s *service) messageBox(title, text string, flags uint32) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(flags))
	runtime.KeepAlive(t)
	runtime.KeepAlive(m)
}
