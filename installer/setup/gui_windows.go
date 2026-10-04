//go:build windows

package main

// The installer's visual face.  Run with no command-line arguments -- which is
// what happens when the user double-clicks the setup .exe -- and the console
// flow in install.go is replaced by a three-step wizard: pick the target
// directory, choose the shortcuts, then watch the copy.  Flags keep the
// scriptable behaviour, so -silent and the CI path are untouched.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- Win32 constants -------------------------------------------------------

const (
	wsChild       = 0x40000000
	wsVisible     = 0x10000000
	wsTabStop     = 0x00010000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000

	wsExClientEdge = 0x00000200

	esAutoHScroll = 0x00000080

	bsDefPushButton = 0x00000001
	bsAutoCheckBox  = 0x00000003

	ssEtchedHorz = 0x00000010

	swHide = 0
	swShow = 5

	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmCommand = 0x0111
	wmTimer   = 0x0113
	wmSetFont = 0x0030
	wmKeyDown = 0x0100

	vkTab    = 9
	vkReturn = 13
	vkEscape = 27
	vkShift  = 0x10

	bmClick = 0x00F5

	bmGetCheck = 0x00F0

	pbmSetPos = 0x0402

	iccProgressClass = 0x00000020

	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040

	smCXScreen = 0
	smCYScreen = 1

	logPixelsX = 88

	fwNormal = 400
	fwBold   = 700

	mfYesNo        = 0x00000004
	mfIconQuestion = 0x00000020
	idYes          = 6

	// Control ids.  Only the buttons need one: WM_COMMAND reports it, while
	// the checkboxes and the edit are read through their handles.
	idBrowse     = 1001
	idNext       = 1002
	idBack       = 1003
	idCancel     = 1004
	idChkDesktop = 1010
	idChkStartup = 1011
	idChkLaunch  = 1012

	timerInstall = 1
)

// ---- Win32 procs -----------------------------------------------------------

var (
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modGdi32    = windows.NewLazySystemDLL("gdi32.dll")
	modShell32  = windows.NewLazySystemDLL("shell32.dll")
	modOle32    = windows.NewLazySystemDLL("ole32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	modComctl32 = windows.NewLazySystemDLL("comctl32.dll")

	procRegisterClassExW     = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW      = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW       = modUser32.NewProc("DefWindowProcW")
	procDestroyWindow        = modUser32.NewProc("DestroyWindow")
	procShowWindow           = modUser32.NewProc("ShowWindow")
	procSetForegroundWindow  = modUser32.NewProc("SetForegroundWindow")
	procUpdateWindow         = modUser32.NewProc("UpdateWindow")
	procGetMessageW          = modUser32.NewProc("GetMessageW")
	procTranslateMessage     = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW     = modUser32.NewProc("DispatchMessageW")
	procIsDialogMessageW     = modUser32.NewProc("IsDialogMessageW")
	procPostQuitMessage      = modUser32.NewProc("PostQuitMessage")
	procSendMessageW         = modUser32.NewProc("SendMessageW")
	procSetWindowTextW       = modUser32.NewProc("SetWindowTextW")
	procGetWindowTextW       = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
	procEnableWindow         = modUser32.NewProc("EnableWindow")
	procSetFocus             = modUser32.NewProc("SetFocus")
	procGetFocus             = modUser32.NewProc("GetFocus")
	procGetKeyState          = modUser32.NewProc("GetKeyState")
	procSetTimer             = modUser32.NewProc("SetTimer")
	procKillTimer            = modUser32.NewProc("KillTimer")
	procMessageBoxW          = modUser32.NewProc("MessageBoxW")
	procLoadCursorW          = modUser32.NewProc("LoadCursorW")
	procLoadIconW            = modUser32.NewProc("LoadIconW")
	procGetDC                = modUser32.NewProc("GetDC")
	procReleaseDC            = modUser32.NewProc("ReleaseDC")
	procGetSystemMetrics     = modUser32.NewProc("GetSystemMetrics")
	procAdjustWindowRectEx   = modUser32.NewProc("AdjustWindowRectEx")
	procSetProcessDPIAware   = modUser32.NewProc("SetProcessDPIAware")

	procCreateFontW   = modGdi32.NewProc("CreateFontW")
	procGetDeviceCaps = modGdi32.NewProc("GetDeviceCaps")

	procSHBrowseForFolderW   = modShell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = modShell32.NewProc("SHGetPathFromIDListW")

	procCoInitializeEx = modOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree  = modOle32.NewProc("CoTaskMemFree")

	procGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = modKernel32.NewProc("GetConsoleWindow")

	procInitCommonControlsEx = modComctl32.NewProc("InitCommonControlsEx")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type initCommonControlsEx struct {
	size uint32
	icc  uint32
}

// msgW mirrors the Win32 MSG layout.  x/sys/windows does not export one, and
// only the message queue ever holds it, so the field names are never read.
type msgW struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

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

// ---- Progress shared between the copy goroutine and the UI thread ----------

var (
	guiProgress int32
	guiDone     int32
	guiStatusMu sync.Mutex
	guiStatus   string
	guiErr      string
	guiFail     bool
)

// guiReport is handed to install() through options.report; it only records, so
// the copy goroutine never touches a window handle.
func guiReport(percent int, status string) {
	atomic.StoreInt32(&guiProgress, int32(percent))
	guiStatusMu.Lock()
	guiStatus = status
	guiStatusMu.Unlock()
}

// ---- Wizard ----------------------------------------------------------------

type wizard struct {
	hwnd    uintptr
	font    uintptr
	bigFont uintptr
	dpi     int

	page1 []uintptr
	page2 []uintptr
	page3 []uintptr

	dirEdit    uintptr
	oldHint    uintptr
	chkDesktop uintptr
	chkStartup uintptr
	chkLaunch  uintptr
	pageTitle  uintptr
	pageBody   uintptr
	status     uintptr
	progress   uintptr
	info       uintptr
	btnBrowse  uintptr
	btnBack    uintptr
	btnNext    uintptr
	btnCancel  uintptr

	page       int
	installing bool
	finished   bool
	dir        string
}

var gWiz *wizard

func (w *wizard) px(v int) int { return v * w.dpi / 96 }

func wizardProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	if gWiz != nil {
		if r, handled := gWiz.handle(msg, wparam, lparam); handled {
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (w *wizard) handle(msg, wparam, lparam uintptr) (uintptr, bool) {
	switch msg {
	case wmCommand:
		switch wparam & 0xffff {
		case idBrowse:
			w.browse()
		case idBack:
			if w.page == 2 {
				w.showPage(1)
			}
		case idNext:
			w.next()
		case idCancel:
			w.quit()
		}
		return 0, true
	case wmTimer:
		if wparam == timerInstall {
			w.tick()
			return 0, true
		}
	case wmClose:
		if w.installing {
			// Closing mid-copy would leave a half-written install behind.
			return 0, true
		}
		procDestroyWindow.Call(w.hwnd)
		return 0, true
	case wmDestroy:
		procKillTimer.Call(w.hwnd, timerInstall)
		procPostQuitMessage.Call(0)
		return 0, true
	}
	return 0, false
}

func (w *wizard) quit() {
	if w.installing {
		return
	}
	procDestroyWindow.Call(w.hwnd)
}

func (w *wizard) next() {
	switch w.page {
	case 1:
		dir := w.dirText()
		if dir == "" {
			messageBox("client2api 安装程序", "请先选择安装目录。")
			procSetFocus.Call(w.dirEdit)
			return
		}
		w.dir = dir
		w.showPage(2)
	case 2:
		w.showPage(3)
		w.startInstall()
	case 3:
		w.quit()
	}
}

func (w *wizard) tick() {
	procSendMessageW.Call(w.progress, pbmSetPos, uintptr(atomic.LoadInt32(&guiProgress)), 0)
	guiStatusMu.Lock()
	status := guiStatus
	guiStatusMu.Unlock()
	if status != "" {
		w.setText(w.status, status)
	}
	if atomic.LoadInt32(&guiDone) == 0 {
		return
	}
	procKillTimer.Call(w.hwnd, timerInstall)
	w.installing = false
	w.finished = true
	procSendMessageW.Call(w.progress, pbmSetPos, 100, 0)
	if guiFail {
		guiStatusMu.Lock()
		msg := guiErr
		guiStatusMu.Unlock()
		writeInstallErrorLog("安装失败", msg)
		w.setText(w.pageTitle, "安装失败")
		w.setText(w.status, "安装没有完成。")
		w.setText(w.info, msg)
		// Update the button before the modal dialog: the dialog blocks this
		// goroutine (the UI thread), so anything after it would only run once
		// the operator dismisses it.
		w.setText(w.btnNext, "关闭")
		procEnableWindow.Call(w.btnNext, 1)
		procSetFocus.Call(w.btnNext)
		messageBox("client2api 安装程序", "安装失败：\n"+msg)
	} else {
		w.setText(w.pageTitle, "安装完成")
		w.setText(w.status, "client2api 已安装到：")
		info := w.dir
		if panel, ok := configuredPanelURL(w.dir); ok {
			info += "\r\n\r\n控制面板：" + panel
		} else {
			info += "\r\n\r\n控制面板：请使用配置中的监听端口访问"
		}
		w.setText(w.info, info)
		w.setText(w.btnNext, "完成")
		procEnableWindow.Call(w.btnNext, 1)
		procSetFocus.Call(w.btnNext)
	}
}

// ---- Controls --------------------------------------------------------------

func (w *wizard) ctl(id int, class, text string, style uint32, exStyle uint32, x, y, cx, cy int) uintptr {
	cls, _ := windows.UTF16PtrFromString(class)
	txt, _ := windows.UTF16PtrFromString(text)
	h, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(txt)),
		uintptr(style),
		uintptr(w.px(x)), uintptr(w.px(y)), uintptr(w.px(cx)), uintptr(w.px(cy)),
		w.hwnd, uintptr(id), hInstance(), 0)
	procSendMessageW.Call(h, wmSetFont, w.font, 1)
	runtime.KeepAlive(cls)
	runtime.KeepAlive(txt)
	return h
}

func (w *wizard) label(id int, text string, x, y, cx, cy int) uintptr {
	return w.ctl(id, "STATIC", text, wsChild|wsVisible, 0, x, y, cx, cy)
}

func (w *wizard) setText(h uintptr, text string) {
	t, _ := windows.UTF16PtrFromString(text)
	procSetWindowTextW.Call(h, uintptr(unsafe.Pointer(t)))
	runtime.KeepAlive(t)
}

func (w *wizard) checked(h uintptr) bool {
	r, _, _ := procSendMessageW.Call(h, bmGetCheck, 0, 0)
	return r == 1
}

func (w *wizard) dirText() string {
	n, _, _ := procGetWindowTextLengthW.Call(w.dirEdit)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	procGetWindowTextW.Call(w.dirEdit, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

func (w *wizard) setDir(dir string) {
	w.setText(w.dirEdit, dir)
}

func (w *wizard) browse() {
	var display [windows.MAX_PATH]uint16
	title, _ := windows.UTF16PtrFromString("选择安装位置")
	bi := browseInfoW{
		hwndOwner:      w.hwnd,
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
	w.setDir(windows.UTF16ToString(path[:]))
}

// showPage swaps the visible control set and re-labels the navigation buttons.
func (w *wizard) showPage(n int) {
	w.page = n
	for _, group := range [][]uintptr{w.page1, w.page2, w.page3} {
		for _, h := range group {
			procShowWindow.Call(h, swHide)
		}
	}
	var show []uintptr
	switch n {
	case 1:
		show = w.page1
	case 2:
		show = w.page2
	default:
		show = w.page3
	}
	for _, h := range show {
		procShowWindow.Call(h, swShow)
	}

	switch n {
	case 1:
		w.setText(w.pageTitle, "选择安装位置")
		procShowWindow.Call(w.btnBack, swHide)
		procShowWindow.Call(w.btnCancel, swShow)
		w.setText(w.btnNext, "下一步")
		procEnableWindow.Call(w.btnNext, 1)
		procSetFocus.Call(w.dirEdit)
	case 2:
		w.setText(w.pageTitle, "安装选项")
		procShowWindow.Call(w.btnBack, swShow)
		procShowWindow.Call(w.btnCancel, swShow)
		w.setText(w.btnNext, "安装")
		procEnableWindow.Call(w.btnNext, 1)
		procSetFocus.Call(w.chkDesktop)
	default:
		w.setText(w.pageTitle, "正在安装")
		procShowWindow.Call(w.btnBack, swHide)
		procShowWindow.Call(w.btnCancel, swHide)
		w.setText(w.btnNext, "请稍候…")
		procEnableWindow.Call(w.btnNext, 0)
	}
}

// tabOrder lists the controls Tab should visit on the current page.  This is a
// plain window rather than a dialog, so IsDialogMessageW will not move focus
// for us and the key handling in the message loop does it instead.
func (w *wizard) tabOrder() []uintptr {
	switch w.page {
	case 1:
		return []uintptr{w.dirEdit, w.btnBrowse, w.btnCancel, w.btnNext}
	case 2:
		return []uintptr{w.chkDesktop, w.chkStartup, w.chkLaunch, w.btnBack, w.btnCancel, w.btnNext}
	default:
		return []uintptr{w.btnNext}
	}
}

func (w *wizard) moveFocus() {
	order := w.tabOrder()
	if len(order) == 0 {
		return
	}
	focus, _, _ := procGetFocus.Call()
	idx := -1
	for i, h := range order {
		if h == focus {
			idx = i
			break
		}
	}
	state, _, _ := procGetKeyState.Call(vkShift)
	back := int16(state) < 0
	next := 0
	if idx >= 0 {
		if back {
			next = (idx - 1 + len(order)) % len(order)
		} else {
			next = (idx + 1) % len(order)
		}
	}
	procSetFocus.Call(order[next])
}

func (w *wizard) activateDefault() {
	if w.installing {
		return
	}
	procSendMessageW.Call(w.btnNext, bmClick, 0, 0)
}

func (w *wizard) startInstall() {
	atomic.StoreInt32(&guiProgress, 0)
	atomic.StoreInt32(&guiDone, 0)
	guiStatusMu.Lock()
	guiStatus = "准备安装…"
	guiErr = ""
	guiFail = false
	guiStatusMu.Unlock()

	w.installing = true
	opt := options{
		dir:       w.dir,
		noDesktop: !w.checked(w.chkDesktop),
		startup:   w.checked(w.chkStartup),
		noLaunch:  !w.checked(w.chkLaunch),
		silent:    true,
		report:    guiReport,
	}
	go func() {
		err := install(opt)
		if err != nil {
			guiStatusMu.Lock()
			guiErr = err.Error()
			guiFail = true
			guiStatusMu.Unlock()
		}
		atomic.StoreInt32(&guiProgress, 100)
		atomic.StoreInt32(&guiDone, 1)
	}()
	procSetTimer.Call(w.hwnd, timerInstall, 80, 0)
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

func hInstance() uintptr {
	h, _, _ := procGetModuleHandleW.Call(0)
	return h
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

// hideConsoleWindow removes the console the console-subsystem image gets when
// Explorer launches it.  The CLI path never calls this, so scripted use keeps
// its stdout.
func hideConsoleWindow() {
	h, _, _ := procGetConsoleWindow.Call()
	if h != 0 {
		procShowWindow.Call(h, swHide)
	}
}

// ---- Entry points ----------------------------------------------------------

// runWizard shows the graphical installer and returns a process exit code.
// runWizard shows the graphical installer and returns a process exit code.
//
// The whole window life -- RegisterClass/CreateWindow/GetMessage/Dispatch --
// has to stay on one OS thread.  A window belongs to the thread that created
// it, so if the Go scheduler moved this goroutine mid-loop the messages would
// be queued for a thread nobody is reading and the window would freeze with
// "not responding".  LockOSThread pins it for the duration.
func runWizard() int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procSetProcessDPIAware.Call()
	procCoInitializeEx.Call(0, coinitApartmentThreaded)

	icc := initCommonControlsEx{size: uint32(unsafe.Sizeof(initCommonControlsEx{})), icc: iccProgressClass}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))

	w := &wizard{dpi: systemDPI(), page: 1}
	gWiz = w
	defer func() { gWiz = nil }()

	w.font = createUIFont(w.dpi, 9, false)
	w.bigFont = createUIFont(w.dpi, 15, true)

	className, _ := windows.UTF16PtrFromString("Client2apiSetupWizard")
	windowTitle, _ := windows.UTF16PtrFromString("client2api 安装程序")
	cursor, _, _ := procLoadCursorW.Call(0, 32512 /* IDC_ARROW */)
	icon, _, _ := procLoadIconW.Call(hInstance(), 1)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   windows.NewCallback(wizardProc),
		hInstance:     hInstance(),
		hIcon:         icon,
		hCursor:       cursor,
		hbrBackground: 5, // COLOR_WINDOW + 1
		lpszClassName: className,
	}
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		messageBox("client2api 安装程序", "创建窗口失败："+err.Error())
		return 1
	}

	style := uint32(wsCaption | wsSysMenu | wsMinimizeBox)
	clientW, clientH := w.px(620), w.px(370)
	rect := windows.Rect{Right: int32(clientW), Bottom: int32(clientH)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rect)), uintptr(style), 0, 0)
	winW := int(rect.Right - rect.Left)
	winH := int(rect.Bottom - rect.Top)
	sw, _, _ := procGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCYScreen)
	x := (int(sw) - winW) / 2
	y := (int(sh) - winH) / 2

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowTitle)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, hInstance(), 0)
	runtime.KeepAlive(className)
	runtime.KeepAlive(windowTitle)
	if hwnd == 0 {
		messageBox("client2api 安装程序", "创建窗口失败："+err.Error())
		return 1
	}
	w.hwnd = hwnd

	w.build()
	procShowWindow.Call(hwnd, swShow)
	procSetForegroundWindow.Call(hwnd)
	procUpdateWindow.Call(hwnd)

	var msg msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		// Keyboard navigation has to be wired by hand: IsDialogMessageW only
		// services real dialogs, and this window is created with CreateWindowEx.
		if msg.message == wmKeyDown {
			switch msg.wParam {
			case vkTab:
				w.moveFocus()
				continue
			case vkReturn:
				w.activateDefault()
				continue
			case vkEscape:
				w.quit()
				continue
			}
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return 0
}

// build lays out every control once; pages are shown and hidden afterwards.
func (w *wizard) build() {
	w.hwnd = gWiz.hwnd

	heading := w.ctl(0, "STATIC", "client2api", wsChild|wsVisible, 0, 24, 18, 400, w.px(26))
	procSendMessageW.Call(heading, wmSetFont, w.bigFont, 1)
	w.label(0, "本地网关与账号池  ·  安装程序 "+version, 26, 50, 420, 18)

	line := w.ctl(0, "STATIC", "", wsChild|wsVisible|ssEtchedHorz, 0, 24, 78, w.px(572), w.px(2))
	_ = line

	w.pageTitle = w.label(0, "选择安装位置", 24, 96, 560, 22)

	// Page 1 -- target directory.
	p1a := w.label(0, "安装程序会把 client2api 安装到下面的文件夹。", 24, 128, 560, 20)
	p1b := w.label(0, "如果要装到别处，点「浏览」选择。", 24, 150, 560, 20)
	w.dirEdit = w.ctl(0, "EDIT", "", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, 24, 180, 460, 26)
	w.btnBrowse = w.ctl(idBrowse, "BUTTON", "浏览…", wsChild|wsVisible|wsTabStop, 0, 494, 180, 102, 26)
	p1c := w.label(0, "需要约 "+fmt.Sprintf("%.0f MB", float64(embeddedSize())/(1024*1024))+" 可用空间。", 24, 218, 560, 20)
	w.oldHint = w.label(0, "", 24, 240, 560, 20)
	prev, hasPrev := readRegistryString(defaultRoot(), installDirValue)
	if hasPrev && prev != "" {
		w.setText(w.oldHint, "检测到已安装的 client2api，将覆盖安装到该目录。")
	}
	w.page1 = []uintptr{p1a, p1b, w.dirEdit, w.btnBrowse, p1c, w.oldHint}

	// Page 2 -- options.  The labels reuse the page-1 body labels for nothing;
	// each page owns its controls so showing a page is a plain show/hide.
	w.chkDesktop = w.ctl(idChkDesktop, "BUTTON", "创建桌面快捷方式", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, 30, 132, 560, 22)
	w.chkStartup = w.ctl(idChkStartup, "BUTTON", "开机自动启动 client2api", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, 30, 162, 560, 22)
	w.chkLaunch = w.ctl(idChkLaunch, "BUTTON", "安装完成后启动 client2api 并打开控制面板", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, 30, 192, 560, 22)
	procSendMessageW.Call(w.chkDesktop, 0x00F1 /* BM_SETCHECK */, 1, 0)
	procSendMessageW.Call(w.chkLaunch, 0x00F1, 1, 0)
	p2note := w.label(0, "安装会保留现有监听端口，并按配置地址打开控制面板。", 30, 232, 560, 20)
	w.page2 = []uintptr{w.chkDesktop, w.chkStartup, w.chkLaunch, p2note}

	// Page 3 -- progress and result.
	w.status = w.label(0, "准备安装…", 30, 128, 560, 20)
	w.progress = w.ctl(0, "msctls_progress32", "", wsChild|wsVisible, 0, 30, 154, 560, 18)
	w.info = w.label(0, "", 30, 184, 560, 120)
	w.page3 = []uintptr{w.status, w.progress, w.info}

	// Bottom navigation.
	w.btnBack = w.ctl(idBack, "BUTTON", "上一步", wsChild|wsVisible|wsTabStop, 0, 324, 318, 90, 30)
	w.btnCancel = w.ctl(idCancel, "BUTTON", "取消", wsChild|wsVisible|wsTabStop, 0, 420, 318, 90, 30)
	w.btnNext = w.ctl(idNext, "BUTTON", "下一步", wsChild|wsVisible|wsTabStop|bsDefPushButton, 0, 516, 318, 90, 30)

	w.setDir(installDir(options{}))
	w.showPage(1)
}

// confirmYesNo is the MessageBox pair used by the uninstaller.
func confirmYesNo(title, text string) bool {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mfYesNo|mfIconQuestion)
	runtime.KeepAlive(t)
	runtime.KeepAlive(m)
	return r == idYes
}

// runUninstallGUI is what the Start Menu shortcut runs.  It replaces the
// console confirm-and-wait with a question box and a final report.
func runUninstallGUI() int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procSetProcessDPIAware.Call()
	if !confirmYesNo("卸载 client2api",
		"确定要卸载 client2api 吗？\n\n程序文件、快捷方式和账号数据都会被删除。") {
		return 0
	}
	opt := options{silent: true}
	if err := uninstall(opt); err != nil {
		messageBox("卸载 client2api", "卸载失败：\n"+err.Error())
		return 1
	}
	messageBox("client2api", "卸载完成。")
	return 0
}
