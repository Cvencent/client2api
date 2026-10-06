//go:build windows

package zcode

import (
	"os"
	"os/exec"
	"path/filepath"
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

// configureBrowserCommand asks Windows to create the process without showing a
// window.  Chromium often ignores the flag for its own browser window, which is
// why guardBrowserWindow exists as well, but when it is honoured the operator
// never sees even a flash.
func configureBrowserCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetWindowPos             = user32.NewProc("SetWindowPos")

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

// processCommandLine returns the full command line of pid.  Chromium relaunches
// its browser process, so the pid the gateway started is not the pid that owns
// the window later; the unique --user-data-dir in the command line is the only
// stable handle.
func processCommandLine(pid uintptr) (string, bool) {
	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
	var buf unicodeString
	status, _, _ := procQueryCommandlineInfo.Call(
		pid,
		uintptr(processCommandLineClass),
		uintptr(unsafe.Pointer(&buf)),
		0,
	)
	// NTSTATUS: 0 or the informational band (0x40000000..0x7FFFFFFF) is a
	// success; anything in the error bands is a failed read (system
	// processes, closed races), which is the common case while enumerating.
	if status != 0 && (status&0xC0000000) != 0 {
		return "", false
	}
	if buf.Length == 0 || buf.Buffer == 0 {
		return "", false
	}
	// Converting the kernel-returned pointer in two steps keeps vet's
	// unsafe.Pointer rules happy: the syscall wrote it, we only read it.
	q := (*[32768]uint16)(unsafe.Pointer(&struct{ a uintptr }{buf.Buffer}))[:buf.Length/2]
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
	const processTerminate = 0x0001
	const waitTimeout = 5000 // ms
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := false
		for _, pid := range pids {
			h, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
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
