//go:build windows

package tray

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- Win32 constants -------------------------------------------------------

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmApp     = 0x8000

	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo = 0x00000001

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	idiApplication = 32512

	swShownormal = 1

	// trayCallback is the private window message the shell posts the icon's
	// mouse events to.  WM_APP and up belong to the application.
	trayCallback = wmApp + 1

	// trayIconSize is the frame the notification area wants at 100% scaling.
	// Windows scales a larger frame down, which is why iconFrame prefers one
	// at least this size rather than the closest match.
	trayIconSize = 32

	// Menu command ids.  TrackPopupMenu is called with TPM_RETURNCMD, so the
	// chosen id comes back as the return value and no WM_COMMAND arrives.
	cmdPanel    = 1
	cmdData     = 2
	cmdQuit     = 3
	cmdRestart  = 4
	cmdSettings = 5
)

// ---- Win32 procs -----------------------------------------------------------

var (
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modShell32  = windows.NewLazySystemDLL("shell32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW          = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW           = modUser32.NewProc("DefWindowProcW")
	procDestroyWindow            = modUser32.NewProc("DestroyWindow")
	procGetMessageW              = modUser32.NewProc("GetMessageW")
	procTranslateMessage         = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW         = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage          = modUser32.NewProc("PostQuitMessage")
	procPostMessageW             = modUser32.NewProc("PostMessageW")
	procRegisterWindowMessageW   = modUser32.NewProc("RegisterWindowMessageW")
	procLoadIconW                = modUser32.NewProc("LoadIconW")
	procCreateIconFromResourceEx = modUser32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = modUser32.NewProc("DestroyIcon")
	procCreatePopupMenu          = modUser32.NewProc("CreatePopupMenu")
	procAppendMenuW              = modUser32.NewProc("AppendMenuW")
	procDestroyMenu              = modUser32.NewProc("DestroyMenu")
	procTrackPopupMenu           = modUser32.NewProc("TrackPopupMenu")
	procSetForegroundWindow      = modUser32.NewProc("SetForegroundWindow")
	procGetCursorPos             = modUser32.NewProc("GetCursorPos")

	procShellNotifyIconW = modShell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = modShell32.NewProc("ShellExecuteW")

	procGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
)

// ---- Win32 structs ---------------------------------------------------------

// notifyIconData mirrors the Windows NOTIFYICONDATAW layout through
// hBalloonIcon.  The older V1 prefix ended at szTip, but balloon messages
// need szInfo, szInfoTitle and dwInfoFlags, so the full structure is kept
// here and cbSize is its actual size.
type notifyIconData struct {
	cbSize           uint32
	hwnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [maxTooltip + 1]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [maxBalloonText + 1]uint16
	uTimeoutVersion  uint32
	szInfoTitle      [maxBalloonTitle + 1]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

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

// msgW mirrors the Win32 MSG layout.  x/sys/windows does not export one, and
// the fields are only ever handed back to the message loop unread, so the names
// carry no meaning beyond the layout.
type msgW struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type point struct{ x, y int32 }

// ---- Service ---------------------------------------------------------------

// service is one icon and the hidden window its messages arrive on.
type service struct {
	opts Options

	hwnd uintptr
	icon uintptr
	// ownIcon records whether the HICON came from CreateIconFromResourceEx and
	// therefore has to be destroyed; the shared IDI_APPLICATION must not be.
	ownIcon bool

	taskbarCreated uint32
	done           chan struct{}

	// settingsMu guards settingsHwnd, the settings dialog the menu keeps at
	// most one of.  The dialog lives on its own thread; this only records
	// its handle so a second click raises it instead of stacking a copy.
	settingsMu   sync.Mutex
	settingsHwnd uintptr

	exitOnce sync.Once
}

// start brings the icon up on a dedicated OS thread and returns the function
// that takes it down.  A nil stop with a nil error means the platform declined
// to provide an icon, which is not worth a log line.
func start(opts Options) (backend, error) {
	s := &service{opts: opts, done: make(chan struct{})}
	ready := make(chan error, 1)
	go s.loop(ready)

	select {
	case err := <-ready:
		if err != nil {
			return backend{}, err
		}
	case <-time.After(10 * time.Second):
		return backend{}, errors.New("timed out waiting for the message loop")
	}
	return backend{stop: s.stop, notify: s.notify}, nil
}

// loop owns the window for its whole life.  A window belongs to the thread that
// created it, so the thread has to stay pinned: if the scheduler moved this
// goroutine mid-loop, messages would be queued for a thread nobody is reading
// and the menu would stop appearing.
func (s *service) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	// A panic in here must cost the icon, never the gateway: the icon is a
	// convenience, and the process it decorates is serving requests.
	defer s.guard(ready)

	// Claim DPI awareness before any window exists.  A process that stays
	// unaware has its windows laid out in a virtualised, scaled coordinate
	// space, which both blurs the settings form and makes the menu's own
	// item rectangles disagree with the pixels on screen.
	procSetProcessDPIAware.Call()

	className, err := windows.UTF16PtrFromString("Client2apiTrayIcon")
	if err != nil {
		ready <- err
		return
	}
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   windows.NewCallback(s.proc),
		hInstance:     hInstance(),
		lpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		ready <- fmt.Errorf("RegisterClassExW: %v", callErr)
		return
	}

	// A plain hidden top-level window: no style bits means it is never shown
	// and never appears in the taskbar, but it still owns a message queue and
	// can receive the shell's callbacks.
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0,
		0, 0, hInstance(), 0)
	runtime.KeepAlive(className)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowExW: %v", callErr)
		return
	}
	s.hwnd = hwnd

	s.registerTaskbarCreated()
	s.icon, s.ownIcon = loadIcon()
	if err := s.add(); err != nil {
		procDestroyWindow.Call(hwnd)
		ready <- err
		return
	}

	ready <- nil

	var msg msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	s.remove()
	if s.ownIcon && s.icon != 0 {
		procDestroyIcon.Call(s.icon)
	}
}

// guard turns a panic on the UI thread into a log line.  The message loop is
// started here rather than through cmd/client2api's GoSafe helpers, so
// without this a Win32 surprise would take the whole gateway down over a
// cosmetic icon.
func (s *service) guard(ready chan<- error) {
	r := recover()
	if r == nil {
		return
	}
	if s.opts.Logf != nil {
		s.opts.Logf("tray icon: %v (icon disabled)", r)
	}
	select {
	case ready <- fmt.Errorf("tray message loop panicked: %v", r):
	default:
	}
}

// stop closes the window and waits for the loop to unwind.  It is the half the
// caller holds, so it never runs OnExit: a shutdown that already started must
// not be told to start again.
func (s *service) stop() {
	if s.hwnd != 0 {
		procPostMessageW.Call(s.hwnd, wmClose, 0, 0)
	}
	<-s.done
}

// quit is the menu's half.  It runs on the UI thread, so it reports the intent
// and posts the close, leaving the caller's Stop to do the waiting.
func (s *service) quit() {
	s.exitOnce.Do(func() {
		if s.opts.OnExit != nil {
			// Off the UI thread: the caller's shutdown may run the panel's
			// part of the teardown, and blocking here would wedge the loop.
			go s.opts.OnExit()
		}
	})
	if s.hwnd != 0 {
		procPostMessageW.Call(s.hwnd, wmClose, 0, 0)
	}
}

// restart asks the caller to replace this process.  The hand-off is owned by
// cmd/client2api -- it starts the replacement, watches it survive startup
// and only then lets this process exit -- so the tray only reports the
// intent and surfaces a failure.  It runs off the UI thread because the
// hand-off blocks while it watches the replacement.
func (s *service) restart() {
	if s.opts.OnRestart == nil {
		return
	}
	go func() {
		if err := s.opts.OnRestart(); err != nil {
			s.errorBox("client2api", "重启失败：\n"+err.Error())
		}
	}()
}

// setSettings records the open dialog's window handle; currentSettings
// reports it so a second menu click raises the dialog instead of opening a
// twin.  The tray thread writes one and the dialog's own thread the other,
// which is why the handle sits behind a mutex.
func (s *service) setSettings(hwnd uintptr) {
	s.settingsMu.Lock()
	s.settingsHwnd = hwnd
	s.settingsMu.Unlock()
}

func (s *service) currentSettings() uintptr {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.settingsHwnd
}

func (s *service) proc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case trayCallback:
		switch uint32(lParam) {
		case wmLButtonDblClk:
			s.openURL(s.opts.PanelURL)
			return 0
		case wmRButtonUp:
			s.showMenu()
			return 0
		}
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	default:
		// Explorer restarting destroys every icon and broadcasts a registered
		// message so applications can put theirs back.
		if s.taskbarCreated != 0 && uint32(msg) == s.taskbarCreated {
			_ = s.add()
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func (s *service) registerTaskbarCreated() {
	name, err := windows.UTF16PtrFromString("TaskbarCreated")
	if err != nil {
		return
	}
	msg, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	s.taskbarCreated = uint32(msg)
}

func (s *service) nid() notifyIconData {
	return notifyIconData{
		cbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		hwnd:             s.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: trayCallback,
		hIcon:            s.icon,
		szTip:            tooltip(s.opts.Title),
	}
}

func (s *service) add() error {
	nid := s.nid()
	if ok, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ok == 0 {
		return fmt.Errorf("Shell_NotifyIcon: %v", err)
	}
	return nil
}

func (s *service) remove() {
	nid := s.nid()
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

// notify displays a balloon without replacing the icon's tooltip or menu.
// Shell_NotifyIcon is called on whichever goroutine raises the alert; the API
// serialises access to the shell's own icon state.
func (s *service) notify(title, message string) {
	if s.hwnd == 0 || s.icon == 0 {
		return
	}
	nid := notifyIconData{
		cbSize:      uint32(unsafe.Sizeof(notifyIconData{})),
		hwnd:        s.hwnd,
		uID:         1,
		uFlags:      nifInfo,
		szInfo:      [maxBalloonText + 1]uint16{},
		szInfoTitle: [maxBalloonTitle + 1]uint16{},
		dwInfoFlags: niifInfo,
	}
	copy(nid.szInfo[:], balloonText(message, maxBalloonText))
	copy(nid.szInfoTitle[:], balloonText(title, maxBalloonTitle))
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func (s *service) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	if s.opts.PanelURL != "" {
		appendMenuItem(menu, cmdPanel, "打开面板")
	}
	if s.opts.DataDir != "" {
		appendMenuItem(menu, cmdData, "打开数据目录")
	}
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	if s.opts.OnRestart != nil {
		appendMenuItem(menu, cmdRestart, "重启")
	}
	if s.opts.LoadSettings != nil && s.opts.SaveSettings != nil {
		appendMenuItem(menu, cmdSettings, "设置…")
	}
	if s.opts.OnRestart != nil || (s.opts.LoadSettings != nil && s.opts.SaveSettings != nil) {
		procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	}
	appendMenuItem(menu, cmdQuit, "退出")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without foreground ownership the menu would not dismiss when the
	// operator clicks elsewhere, which is the classic tray-menu bug.
	procSetForegroundWindow.Call(s.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmRightButton|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y),
		0, s.hwnd, 0)

	switch cmd {
	case cmdPanel:
		s.openURL(s.opts.PanelURL)
	case cmdData:
		s.openURL(s.opts.DataDir)
	case cmdRestart:
		s.restart()
	case cmdSettings:
		// Off the UI thread: the dialog runs its own message loop, and
		// nesting it inside the menu's would wedge the menu.
		go s.showSettings()
	case cmdQuit:
		s.quit()
	}
}

func appendMenuItem(menu, id uintptr, label string) {
	text, err := windows.UTF16PtrFromString(label)
	if err != nil {
		return
	}
	procAppendMenuW.Call(menu, mfString, id, uintptr(unsafe.Pointer(text)))
	runtime.KeepAlive(text)
}

// openURL hands the target to the shell so it opens in whatever the operator
// uses -- a path to the data directory works through the same call as the panel
// URL, which is why one helper covers both menu items.
func (s *service) openURL(target string) {
	if target == "" {
		return
	}
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return
	}
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return
	}
	procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0, 0, swShownormal)
	runtime.KeepAlive(verb)
	runtime.KeepAlive(file)
}

// loadIcon builds the HICON, falling back to the generic application icon when
// the embedded one cannot be decoded.  own reports whether the result has to be
// destroyed by the caller.
func loadIcon() (h uintptr, own bool) {
	if frame, err := iconFrame(iconBytes, trayIconSize); err == nil && len(frame) > 0 {
		icon, _, _ := procCreateIconFromResourceEx.Call(
			uintptr(unsafe.Pointer(&frame[0])),
			uintptr(len(frame)),
			1,          // fIcon
			0x00030000, // version
			0, 0,       // keep the frame's own dimensions
			0) // LR_DEFAULTCOLOR
		if icon != 0 {
			return icon, true
		}
	}
	shared, _, _ := procLoadIconW.Call(0, idiApplication)
	return shared, false
}

// hInstance returns the module handle of the running executable, which
// RegisterClassExW and CreateWindowExW both want.
func hInstance() uintptr {
	h, _, _ := procGetModuleHandleW.Call(0)
	return h
}
