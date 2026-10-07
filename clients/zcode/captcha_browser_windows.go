//go:build windows

package zcode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// browserCandidates lists where Windows keeps Edge and Chrome.  Edge ships
// with Windows 10 and 11, which is why the JWT channel can work out of the box
// without bundling a runtime.
func browserCandidates() []string {
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		root := strings.TrimSpace(os.Getenv(env))
		if root == "" {
			continue
		}
		out = append(out,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	return append(out, "msedge.exe", "chrome.exe")
}

// captchaBrowserDesktopName is the private desktop Chromium windows are
// created on.  The operator's shell (Explorer and the taskbar) is attached to
// the normal desktop, so a window that exists only on this desktop cannot
// flash into the taskbar while it starts.
const captchaBrowserDesktopName = "client2api-zcode-captcha"

// desktopAllAccess is DESKTOP_ALL_ACCESS: the new desktop must be able to
// create the browser's windows and accept the process created on it.
const desktopAllAccess = 0x000F01FF

var (
	desktopOnce   sync.Once
	desktopHandle uintptr
	desktopErr    error
)

// hiddenCaptchaDesktop creates the private desktop once per process.  Keeping
// the handle open keeps the desktop alive for the lifetime of the gateway;
// Windows destroys it with the window station when the process exits.
func hiddenCaptchaDesktop() (uintptr, error) {
	desktopOnce.Do(func() {
		name, err := syscall.UTF16PtrFromString(captchaBrowserDesktopName)
		if err != nil {
			desktopErr = err
			return
		}
		h, _, callErr := procCreateDesktopW.Call(
			uintptr(unsafe.Pointer(name)),
			0,
			0,
			0,
			uintptr(desktopAllAccess),
			0,
		)
		if h == 0 {
			if callErr != syscall.Errno(0) {
				desktopErr = callErr
			} else {
				desktopErr = syscall.EINVAL
			}
			return
		}
		desktopHandle = h
	})
	return desktopHandle, desktopErr
}

// startCaptchaBrowser asks CreateProcess for a browser on the private desktop.
// StartupInfo.Desktop is the important part: exec.Cmd cannot express it, while
// passing it here means the first Chromium window is born where Explorer and
// the taskbar cannot see it.
//
// If the private desktop cannot be created (for example a non-interactive
// service session), fail instead of launching a visible browser and flashing
// the taskbar.  captcha_command remains the headless fallback.
func startCaptchaBrowser(exe string, args []string) (captchaBrowserProcess, error) {
	if _, err := hiddenCaptchaDesktop(); err != nil {
		return nil, fmt.Errorf("cannot create the private captcha desktop: %w", err)
	}
	app, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return nil, err
	}
	commandLine, err := syscall.UTF16PtrFromString(windowsCommandLine(exe, args))
	if err != nil {
		return nil, err
	}
	desktop, err := syscall.UTF16PtrFromString(captchaBrowserDesktopName)
	if err != nil {
		return nil, err
	}

	startup := syscall.StartupInfo{
		Cb:      uint32(unsafe.Sizeof(syscall.StartupInfo{})),
		Desktop: desktop,
	}
	var info syscall.ProcessInformation
	if err := syscall.CreateProcess(app, commandLine, nil, nil, false, 0, nil, nil, &startup, &info); err != nil {
		return nil, err
	}
	_ = syscall.CloseHandle(info.Thread)
	return &windowsCaptchaBrowserProcess{pid: int(info.ProcessId), handle: info.Process}, nil
}

func windowsCommandLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, syscall.EscapeArg(exe))
	for _, arg := range args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

type windowsCaptchaBrowserProcess struct {
	pid    int
	handle syscall.Handle
	once   sync.Once
	err    error
}

func (p *windowsCaptchaBrowserProcess) PID() int { return p.pid }

func (p *windowsCaptchaBrowserProcess) Wait() error {
	p.once.Do(func() {
		defer syscall.CloseHandle(p.handle)
		status, err := syscall.WaitForSingleObject(p.handle, syscall.INFINITE)
		if err != nil {
			p.err = err
			return
		}
		if status != syscall.WAIT_OBJECT_0 {
			p.err = fmt.Errorf("unexpected process wait status %#x", status)
			return
		}
		var code uint32
		if err := syscall.GetExitCodeProcess(p.handle, &code); err != nil {
			p.err = err
			return
		}
		if code != 0 {
			p.err = fmt.Errorf("browser exited with code %d", code)
		}
	})
	return p.err
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procCreateDesktopW           = user32.NewProc("CreateDesktopW")

	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess          = kernel32.NewProc("OpenProcess")
	procTerminateProcess     = kernel32.NewProc("TerminateProcess")
	procCloseHandle          = kernel32.NewProc("CloseHandle")
	procWaitForSingleObject  = kernel32.NewProc("WaitForSingleObject")
	ntdll                    = syscall.NewLazyDLL("ntdll.dll")
	procQueryCommandlineInfo = ntdll.NewProc("NtQueryInformationProcess")

	// processCommandLineClass is ProcessCommandLineInformation; it asks the
	// kernel for a UNICODE_STRING holding the process's full command line,
	// which is what the profile match is built on.
	processCommandLineClass = 60

	// The callback is registered once: syscall.NewCallback leaks a slot per
	// call, and guardBrowserWindow polls many times per mint, so building it
	// per poll would exhaust the table after a few captchas.
	guardMu      sync.Mutex
	guardProfile string
	guardCB      = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if guardProfile == "" {
			return 1
		}
		var pid uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid == 0 {
			return 1
		}
		line, ok := processCommandLine(uintptr(pid))
		if !ok || !strings.Contains(line, guardProfile) {
			return 1
		}
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		const swpNoSize, swpNoZOrder, swpNoActivate = 0x0001, 0x0004, 0x0010
		// int32 through a variable: converting a negative constant straight to
		// uintptr is a compile error, and the API reads the low 32 bits as int.
		offscreen := int32(-32000)
		procSetWindowPos.Call(hwnd, 0,
			uintptr(offscreen), uintptr(offscreen), 0, 0,
			swpNoSize|swpNoZOrder|swpNoActivate)
		return 1
	})
)

// The kernel wants a process HANDLE, and it writes a UNICODE_STRING into the
// front of the caller's buffer whose Buffer member points at the command line
// stored right behind it in that same buffer.
const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION.
	// It is the access right that still works across integrity levels, which
	// is what enumerating other people's processes needs.
	processQueryLimitedInformation = 0x1000
	// processTerminateAccess is PROCESS_TERMINATE, used to close a browser
	// this module started.
	processTerminateAccess = 0x0001
)

// commandLineBuf recycles the read buffer.  The off-screen guard asks for the
// command line once per top-level window, twenty times a second, and a fresh
// 64 KiB allocation per window is a lot of garbage for a background poll.
var commandLineBuf = sync.Pool{
	New: func() any {
		b := make([]byte, 64<<10)
		return &b
	},
}

// processCommandLine returns the full command line of pid.  Chromium relaunches
// its browser process, so the pid the gateway started is not the pid that owns
// the window later; the unique --user-data-dir in the command line is the only
// stable handle.
//
// NtQueryInformationProcess takes a HANDLE and a caller-owned buffer.  The
// earlier version passed the bare pid and a zero-length buffer, so the kernel
// answered STATUS_INVALID_HANDLE every single time and both callers silently
// gave up: the guard never moved a window and the profile-based kill never
// matched a process.  That is why the throwaway captcha browsers stayed on the
// operator's desktop and piled up run after run.
func processCommandLine(pid uintptr) (string, bool) {
	if pid == 0 {
		return "", false
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, pid)
	if h == 0 {
		return "", false
	}
	defer procCloseHandle.Call(h)

	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
	// 64 KiB holds the header plus the longest command line Windows can
	// report (32767 UTF-16 units).
	bufp := commandLineBuf.Get().(*[]byte)
	defer commandLineBuf.Put(bufp)
	buf := *bufp

	base := uintptr(unsafe.Pointer(&buf[0]))
	status, _, _ := procQueryCommandlineInfo.Call(
		h,
		uintptr(processCommandLineClass),
		base,
		uintptr(len(buf)),
	)
	// NTSTATUS: 0 or the informational band (0x40000000..0x7FFFFFFF) is a
	// success; anything in the error bands is a failed read (system
	// processes, closed races), which is the common case while enumerating.
	if status != 0 && (status&0xC0000000) != 0 {
		return "", false
	}
	us := (*unicodeString)(unsafe.Pointer(&buf[0]))
	if us.Length == 0 || us.Buffer < base {
		return "", false
	}
	// Read the string by offset instead of converting the kernel's uintptr
	// back into a pointer: same bytes, and nothing for vet to flag.
	off := int(us.Buffer - base)
	need := int(us.Length)
	if need%2 != 0 || off > len(buf) || need > len(buf)-off {
		return "", false
	}
	q := (*[32768]uint16)(unsafe.Pointer(&buf[off]))[:need/2]
	return syscall.UTF16ToString(q), true
}

// guardBrowserWindow keeps the throwaway captcha window off the operator's
// desktop.  It walks the top-level windows and parks every visible one whose
// process command line carries this mint's profile -- which keeps matching
// after Chromium relaunches its browser process under a different pid.  It
// runs in its own goroutine and never blocks the mint.
func guardBrowserWindow(profile string, stop <-chan struct{}, logf func(string, ...any)) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return
	}
	go func() {
		_ = logf
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			moveWindowsOffscreen(profile)
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
}

func moveWindowsOffscreen(profile string) {
	guardMu.Lock()
	guardProfile = profile
	_, _, _ = procEnumWindows.Call(guardCB, 0)
	guardProfile = ""
	guardMu.Unlock()
}

// matchingProcessIDs lists the browser processes whose command line carries
// profile.  Only Chromium-family image names are considered, so a malformed
// profile string cannot reach unrelated processes.
func matchingProcessIDs(profile string, names []string) []uintptr {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil
	}
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	return enumProcessIDs(func(pid uint32, image string) bool {
		if !want[strings.ToLower(image)] {
			return false
		}
		line, ok := processCommandLine(uintptr(pid))
		return ok && strings.Contains(line, profile)
	})
}

// browserProcessNames are the executables findBrowser may have launched.
func browserProcessNames() []string {
	out := make([]string, 0, 2)
	for _, p := range browserCandidates() {
		base := strings.ToLower(filepath.Base(strings.TrimSpace(p)))
		if base != "" && base != "." {
			out = append(out, base)
		}
	}
	return out
}

// killBrowserProfile closes every process whose command line names this mint's
// profile, then waits for them to exit.  The unique --user-data-dir is the
// safety boundary: the operator's own browser runs without it, so it can never
// match.  This is the primary cleanup because the started pid stops matching
// as soon as Edge relaunches its browser process; killBrowserTree stays as the
// fallback for a mint that fails before the relaunch.
func killBrowserProfile(profile string, logf func(string, ...any)) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return
	}
	pids := matchingProcessIDs(profile, browserProcessNames())
	if len(pids) == 0 {
		return
	}
	const waitTimeout = 5000 // ms
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := false
		for _, pid := range pids {
			h, _, _ := procOpenProcess.Call(processTerminateAccess, 0, uintptr(pid))
			if h == 0 {
				continue // already gone, or nothing this process can touch
			}
			done, _, _ := procWaitForSingleObject.Call(h, waitTimeout)
			if done != 0 {
				// Still alive after the wait: terminate and let the next
				// sweep confirm.
				procTerminateProcess.Call(h, 1)
				alive = true
			}
			procCloseHandle.Call(h)
		}
		if !alive || time.Now().After(deadline) {
			break
		}
		if len(pids) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
		time.Sleep(100 * time.Millisecond)
	}
	if logf != nil {
		logf("zcode: closed %d captcha browser process(es) for %s", len(pids), profile)
	}
}

// captchaProfileToken pulls the throwaway profile name out of a command line.
var captchaProfileToken = regexp.MustCompile(`zcode-captcha-\d+`)

// reapStaleCaptchaBrowsers closes browsers an earlier run left behind: a crash
// or a hard kill between launching the window and its cleanup leaves the
// process, and sometimes its profile, in %TEMP% forever.  A live mint always
// has a freshly touched profile directory, so an untouched directory -- or a
// missing one, which is what a failed cleanup leaves behind -- is what
// separates debris from work in progress.  The zcode-captcha- prefix is the
// safety boundary: the operator's own browser never runs with it.
//
// It runs once while the module starts, before this process can mint its own
// captcha, so every matching process and profile directory is debris by
// definition.  The zcode-captcha- prefix is the safety boundary: the
// operator's own browser never runs with it.
func reapStaleCaptchaBrowsers(logf func(string, ...any)) {
	tmp := os.TempDir()
	closing := 0
	for _, pid := range matchingProcessIDs(captchaProfilePrefix, browserProcessNames()) {
		line, ok := processCommandLine(pid)
		if !ok {
			continue
		}
		name := captchaProfileToken.FindString(line)
		if name == "" {
			continue
		}
		h, _, _ := procOpenProcess.Call(processTerminateAccess, 0, pid)
		if h == 0 {
			continue
		}
		procTerminateProcess.Call(h, 1)
		procCloseHandle.Call(h)
		closing++
	}
	if closing > 0 && logf != nil {
		logf("zcode: closed %d leftover captcha browser process(es) from an earlier run", closing)
	}

	// Fold in any profile directory whose browser is already gone.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), captchaProfilePrefix) {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		if os.RemoveAll(dir) == nil {
			removed++
		}
	}
	if removed > 0 && logf != nil {
		logf("zcode: removed %d leftover captcha browser profile(s)", removed)
	}
}

// killBrowserTree is the fallback cleanup for a mint that dies before
// Chromium relaunches its browser process: the original pid still names the
// right tree in that window.  The profile-based killBrowserProfile is the
// primary path.
func killBrowserTree(pid int, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err == nil || logf == nil {
		return
	}
	// Edge often exits by itself as soon as the page has reported.  taskkill
	// then answers 128 ("process not found"), which is success for this
	// cleanup rather than a problem worth a log line.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 128 {
		return
	}
	msg := strings.TrimSpace(string(out))
	logf("zcode: could not close the captcha browser (pid %d): %v %s", pid, err, msg)
}

// enumProcessIDs walks the Toolhelp32 process snapshot and keeps every entry
// the filter accepts.  The filter receives the pid and the image file name.
func enumProcessIDs(keep func(pid uint32, image string) bool) []uintptr {
	const (
		th32csSnapProcess  = 0x00000002
		invalidHandleValue = ^uintptr(0)
	)
	type processEntry32 struct {
		dwSize              uint32
		cntUsage            uint32
		th32ProcessID       uint32
		th32DefaultHeapID   uintptr
		th32ModuleID        uint32
		cntThreads          uint32
		th32ParentProcessID uint32
		pcPriClassBase      int32
		dwFlags             uint32
		szExeFile           [260]uint16
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procCreateSnapshot := kernel32.NewProc("CreateToolhelp32Snapshot")
	procFirst := kernel32.NewProc("Process32FirstW")
	procNext := kernel32.NewProc("Process32NextW")
	procClose := kernel32.NewProc("CloseHandle")

	snap, _, _ := procCreateSnapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == invalidHandleValue {
		return nil
	}
	defer procClose.Call(snap)

	var out []uintptr
	var entry processEntry32
	entry.dwSize = uint32(unsafe.Sizeof(entry))
	first := true
	for {
		var rc uintptr
		if first {
			rc, _, _ = procFirst.Call(snap, uintptr(unsafe.Pointer(&entry)))
			first = false
		} else {
			rc, _, _ = procNext.Call(snap, uintptr(unsafe.Pointer(&entry)))
		}
		if rc == 0 {
			break
		}
		image := syscall.UTF16ToString(entry.szExeFile[:])
		if keep(entry.th32ProcessID, image) {
			out = append(out, uintptr(entry.th32ProcessID))
		}
		entry.dwSize = uint32(unsafe.Sizeof(entry))
	}
	return out
}
