//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"golang.org/x/sys/windows"
)

// Shortcuts are created through IShellLink rather than by writing a .lnk by
// hand or shelling out to a script host: the COM object is the documented
// owner of the format, and it keeps the installer to plain file I/O plus a
// handful of vtable calls with no temp files for an antivirus engine to
// second-guess.

type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

var (
	clsidShellLink = guid{0x00021401, 0x0000, 0x0000,
		[8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIShellLinkW = guid{0x000214F9, 0x0000, 0x0000,
		[8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIPersistFile = guid{0x0000010B, 0x0000, 0x0000,
		[8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

const (
	clsctxInprocServer      = 0x1
	coinitApartmentThreaded = 0x2
	swShowNormal            = 1
)

const (
	// startupLNK is the shortcut the installer owns in the user's Startup
	// folder.  Its name is also the marker older installs left behind, so an
	// upgrade can recover the operator's choice even before the registry key
	// existed.
	startupLNK = appName + ".lnk"
	// startupValue remembers the checkbox across upgrades.  It lives beside
	// InstallDir under HKLM/HKCU\Software\client2api.
	startupValue = "Startup"
)

// startupDir returns the per-user Startup folder.  A redirected profile
// falls back to the path the shell uses when the known-folder query fails.
func startupDir() string {
	dir, _ := knownFolder(windows.FOLDERID_Startup,
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup"))
	return dir
}

// startupShortcutExistsIn reports whether the startup link is present in a
// specific directory.  Kept separate from startupShortcutExists so tests can
// point it at a temp directory.
func startupShortcutExistsIn(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, startupLNK))
	return err == nil && !info.IsDir()
}

func startupShortcutExists() bool {
	return startupShortcutExistsIn(startupDir())
}

// clearStartupShortcutIn removes the link from dir.  The wizard uses it when
// the operator unticks autostart during an upgrade, so the remembered "off"
// actually takes effect instead of leaving the old link behind.
func clearStartupShortcutIn(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, startupLNK))
}

func clearStartupShortcut() {
	clearStartupShortcutIn(startupDir())
}

// startupDefault decides whether the wizard's autostart box starts checked.
// An explicit remembered value wins; otherwise the presence of the shortcut
// is treated as "on", which is how installs made before the preference was
// persisted migrate without the operator having to tick the box again.
func startupDefault(stored, hasStored, hasShortcut bool) bool {
	if hasStored {
		return stored
	}
	return hasShortcut
}

// readRegistryBool reads a stored boolean; a missing value reports has=false
// so callers can tell "never set" apart from an explicit "off".
func readRegistryBool(root registry.Key, name string) (value, has bool) {
	k, err := registry.OpenKey(root, appKey, registry.QUERY_VALUE)
	if err != nil {
		return false, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return false, false
	}
	return v != 0, true
}

// writeStartupPreference records the operator's checkbox so the next upgrade
// can restore it instead of starting unchecked.
func writeStartupPreference(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, appKey, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()
	v := uint32(0)
	if enabled {
		v = 1
	}
	return k.SetDWordValue(startupValue, v)
}

// ishellLinkWVtbl mirrors IShellLinkWVtbl from the Windows SDK's shobjidl.h.
// The slot order there is neither the alphabetical one the documentation
// page lists nor a getter/setter pairing, so the slots run, in order:
//
// GetPath, GetIDList, SetIDList, GetDescription, SetDescription,
// GetWorkingDirectory, SetWorkingDirectory, GetArguments, SetArguments,
// GetHotkey, SetHotkey, GetShowCmd, SetShowCmd, GetIconLocation,
// SetIconLocation, SetRelativePath, Resolve, SetPath.
//
// Declaring the
// members in any other order silently routes every call to a neighbouring
// method: the shortcut still gets written and still looks plausible, but it
// points at the wrong file and drops its arguments.
type ishellLinkWVtbl struct {
	queryInterface      uintptr
	addRef              uintptr
	release             uintptr
	getPath             uintptr
	getIDList           uintptr
	setIDList           uintptr
	getDescription      uintptr
	setDescription      uintptr
	getWorkingDirectory uintptr
	setWorkingDirectory uintptr
	getArguments        uintptr
	setArguments        uintptr
	getHotkey           uintptr
	setHotkey           uintptr
	getShowCmd          uintptr
	setShowCmd          uintptr
	getIconLocation     uintptr
	setIconLocation     uintptr
	setRelativePath     uintptr
	resolve             uintptr
	setPath             uintptr
}

type ishellLinkW struct{ vtbl *ishellLinkWVtbl }

type iPersistFileVtbl struct {
	queryInterface uintptr
	addRef         uintptr
	release        uintptr
	getClassID     uintptr
	isDirty        uintptr
	load           uintptr
	save           uintptr
	saveCompleted  uintptr
	getCurFile     uintptr
}

type iPersistFile struct{ vtbl *iPersistFileVtbl }

// createShortcut writes one .lnk pointing at target with the given arguments,
// working directory, icon and description.
func createShortcut(link, target, args, workDir, icon, description string) error {
	// The shortcut COM objects are apartment-threaded; the goroutine that
	// calls CoInitializeEx must be the one that makes every later call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ole32 := windows.NewLazySystemDLL("ole32.dll")
	coInitializeEx := ole32.NewProc("CoInitializeEx")
	coCreateInstance := ole32.NewProc("CoCreateInstance")
	coUninitialize := ole32.NewProc("CoUninitialize")

	// S_OK (0) and S_FALSE (1, already initialised on this thread) are both
	// usable; anything else means COM will not work at all.
	if hr, _, _ := coInitializeEx.Call(0, coinitApartmentThreaded); hr != 0 && hr != 1 {
		return fmt.Errorf("CoInitializeEx 失败：0x%08X", hr)
	}
	defer coUninitialize.Call()

	var linkObj *ishellLinkW
	hr, _, _ := coCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIShellLinkW)),
		uintptr(unsafe.Pointer(&linkObj)),
	)
	if hr != 0 || linkObj == nil {
		return fmt.Errorf("创建 IShellLink 失败：0x%08X", hr)
	}
	defer comCall(linkObj.vtbl.release, uintptr(unsafe.Pointer(linkObj)))

	targetP, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	argsP, err := syscall.UTF16PtrFromString(args)
	if err != nil {
		return err
	}
	workP, err := syscall.UTF16PtrFromString(workDir)
	if err != nil {
		return err
	}
	descP, err := syscall.UTF16PtrFromString(description)
	if err != nil {
		return err
	}
	iconP, err := syscall.UTF16PtrFromString(icon)
	if err != nil {
		return err
	}
	linkP, err := syscall.UTF16PtrFromString(link)
	if err != nil {
		return err
	}

	self := uintptr(unsafe.Pointer(linkObj))
	comCallStr(linkObj.vtbl.setPath, self, targetP)
	comCallStr(linkObj.vtbl.setArguments, self, argsP)
	comCallStr(linkObj.vtbl.setWorkingDirectory, self, workP)
	comCallStr(linkObj.vtbl.setDescription, self, descP)
	comCallNum(linkObj.vtbl.setIconLocation, self, uintptr(unsafe.Pointer(iconP)), 0)
	comCall(linkObj.vtbl.setShowCmd, self, swShowNormal)

	var persist *iPersistFile
	hr = comCall(linkObj.vtbl.queryInterface,
		self,
		uintptr(unsafe.Pointer(&iidIPersistFile)),
		uintptr(unsafe.Pointer(&persist)))
	if hr != 0 || persist == nil {
		return fmt.Errorf("获取 IPersistFile 失败：0x%08X", hr)
	}
	defer comCall(persist.vtbl.release, uintptr(unsafe.Pointer(persist)))

	// TRUE asks IPersistFile::Save to remember the path for later Save() calls.
	hr = comCallNum(persist.vtbl.save, uintptr(unsafe.Pointer(persist)), uintptr(unsafe.Pointer(linkP)), 1)
	if hr != 0 {
		return fmt.Errorf("保存快捷方式失败：0x%08X", hr)
	}

	// Every UTF-16 buffer above must outlive its COM call; the compiler keeps
	// them live to here.
	runtime.KeepAlive(targetP)
	runtime.KeepAlive(argsP)
	runtime.KeepAlive(workP)
	runtime.KeepAlive(descP)
	runtime.KeepAlive(iconP)
	runtime.KeepAlive(linkP)
	return nil
}

func comCall(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

func comCallStr(fn uintptr, self uintptr, s *uint16) uintptr {
	r, _, _ := syscall.SyscallN(fn, self, uintptr(unsafe.Pointer(s)))
	runtime.KeepAlive(s)
	return r
}

func comCallNum(fn uintptr, self uintptr, a, b uintptr) uintptr {
	return comCall(fn, self, a, b)
}
