//go:build windows

package browser

import (
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// closeBrowserProfile is the Windows cleanup path for Chromium relaunches.
// The process exec.Cmd started is often only a launcher: Edge/Chrome hands
// the unique --user-data-dir to a child and exits.  Killing the original PID
// therefore leaves the real browser and its renderers behind under a new PID.
// The profile is the stable identity, so enumerate matching browser processes
// by command line and terminate every one of them.
func closeBrowserProfile(exe, profile string) {
	exe = strings.TrimSpace(exe)
	profile = strings.TrimSpace(profile)
	if exe == "" || profile == "" {
		return
	}
	image := strings.ToLower(filepath.Base(exe))
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids := browserProcessIDs(image, profile)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			terminateProcess(pid)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func browserProcessIDs(image, profile string) []uint32 {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var pids []uint32
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), image) {
			continue
		}
		if line, ok := processCommandLine(entry.ProcessID); ok && strings.Contains(strings.ToLower(line), strings.ToLower(profile)) {
			pids = append(pids, entry.ProcessID)
		}
	}
	return pids
}

func terminateProcess(pid uint32) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
	_, _ = windows.WaitForSingleObject(h, 1000)
}

// processCommandLine returns a process's full command line.  NtQuery writes a
// UNICODE_STRING at the front of the caller-owned buffer, with its Buffer
// field pointing immediately behind that header.  Converting the returned
// pointer directly is needlessly unsafe, so read the string by byte offset.
func processCommandLine(pid uint32) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)

	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
	const commandLineBufSize = 64 << 10
	buf := make([]byte, commandLineBufSize)
	base := uintptr(unsafe.Pointer(&buf[0]))
	var returned uint32
	if err := windows.NtQueryInformationProcess(
		h,
		windows.ProcessCommandLineInformation,
		unsafe.Pointer(&buf[0]),
		uint32(len(buf)),
		&returned,
	); err != nil {
		return "", false
	}
	us := (*unicodeString)(unsafe.Pointer(&buf[0]))
	if us.Length == 0 || us.Buffer < base {
		return "", false
	}
	off := int(us.Buffer - base)
	need := int(us.Length)
	if need%2 != 0 || off%2 != 0 || off > len(buf) || need > len(buf)-off {
		return "", false
	}
	q := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), need/2)
	return syscall.UTF16ToString(q), true
}
