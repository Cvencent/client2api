//go:build windows

// Command setup builds client2api-setup-<version>.exe, a self-contained
// Windows installer for the client2api gateway.
//
// Why a Go installer instead of NSIS or Inno Setup: NSIS keeps the whole
// installer payload in one buffer and, past 16 MiB, hands that buffer to a
// memory-mapped scratch file.  On this machine (and any host where a security
// product or a nearly-full system volume gets in the way) creating that
// mapping fails with "Internal compiler error #12345: error creating mmap",
// and the payload here is ~23 MiB.  Inno Setup would need a toolchain the
// machine does not have.  A Go program embeds the payload in its own image and
// copies it out with ordinary file I/O, so none of that applies -- and the
// whole thing still builds with nothing but the Go toolchain.
//
// Layout it produces under the install directory:
//
//	client2api.exe          the gateway
//	probe.exe               the diagnostic probe
//	panelsmoke.exe          the panel self-check (run before and after an install)
//	uninstall.exe           this same binary, re-run with --uninstall
//	client2api-panel.url    internet shortcut to the web panel
//	client2api.ico          shortcut icon
//	configs/                live config plus the example
//	data/                   account pool and usage history (optional)
//	README.md, README.zh-CN.md, LICENSE
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// version is the packaging default; build.ps1 injects the authoritative value
// from cmd/client2api/main.go with -X main.version=... .  installer's guard
// test fails if this literal ever drifts from the source of truth.
var version = "0.1.35"

const (
	appName      = "client2api"
	appExe       = "client2api.exe"
	uninstallExe = "uninstall.exe"
	panelLNK     = "client2api-panel.url"
	// legacyInstallDirKey remembers where the last install went, so an
	// upgrade lands in the same place even though the default moved.
	installDirValue = "InstallDir"
)

type options struct {
	dir       string
	silent    bool
	noData    bool
	noLaunch  bool
	noDesktop bool
	startup   bool
	uninstall bool
	fromTemp  bool
	parentPID int
	// skipSelfCheck turns off the real-browser panel check that normally runs
	// before and after an upgrade.  It exists for machines with no Chrome or
	// Edge at all; the recommended answer when a check fails is to fix the
	// package, not to skip the check.
	skipSelfCheck bool
	// report receives coarse progress from install so a front-end (the GUI
	// wizard) can drive a progress bar.  A nil report means console only.
	report func(percent int, status string)
}

func main() {
	opt := parseFlags()

	if err := enableUTF8Console(); err != nil {
		// Cosmetic only: a redirected stdout or an unusual console host is
		// not a reason to refuse the install.
		_ = err
	}

	// No arguments means the user double-clicked the setup .exe, so show the
	// wizard instead of the console flow.  Any flag keeps the scriptable
	// behaviour that -silent and CI depend on.
	if len(os.Args) == 1 {
		hideConsoleWindow()
		if strings.EqualFold(filepath.Base(selfPath()), uninstallExe) {
			os.Exit(runUninstallGUI())
		}
		os.Exit(runWizard())
	}

	// This image is GUI-subsystem so the wizard never flashes a console, which
	// means a scripted invocation has no console of its own.  Attach to the
	// launcher's when there is one so -silent / -dir keep their progress output.
	attachParentConsole()

	if opt.uninstall || strings.EqualFold(filepath.Base(selfPath()), uninstallExe) {
		if err := uninstall(opt); err != nil {
			fail(opt, "卸载失败", err)
		}
		return
	}

	if err := install(opt); err != nil {
		fail(opt, "安装失败", err)
	}
}

func parseFlags() options {
	var opt options
	flag.StringVar(&opt.dir, "dir", "", "安装目录（默认 %LOCALAPPDATA%\\Programs\\client2api）")
	flag.BoolVar(&opt.silent, "silent", false, "不输出进度、不等待按键")
	flag.BoolVar(&opt.noData, "no-data", false, "不写入 data/ 账号数据")
	flag.BoolVar(&opt.noLaunch, "no-launch", false, "安装完成后不启动网关")
	flag.BoolVar(&opt.noDesktop, "no-desktop", false, "不创建桌面快捷方式")
	flag.BoolVar(&opt.startup, "startup", false, "创建开机自启快捷方式")
	flag.BoolVar(&opt.skipSelfCheck, "skip-self-check", false, "跳过安装前后的真实浏览器面板自检（仅在机器没有浏览器时使用，不推荐）")
	flag.BoolVar(&opt.uninstall, "uninstall", false, "卸载 client2api")
	flag.BoolVar(&opt.fromTemp, "from-temp", false, "内部使用：卸载器已由 %TEMP% 中的副本接管")
	flag.IntVar(&opt.parentPID, "parent-pid", 0, "内部使用：先等该进程退出，再删除安装目录")
	flag.Parse()
	return opt
}

// installDir resolves the target directory: an explicit -dir wins, then a
// previous install recorded in the registry, then the per-user default.
func installDir(opt options) string {
	if opt.dir != "" {
		if abs, err := filepath.Abs(opt.dir); err == nil {
			return abs
		}
		return opt.dir
	}
	if prev, ok := readRegistryString(defaultRoot(), installDirValue); ok && prev != "" {
		return prev
	}
	return filepath.Join(localAppData(), "Programs", appName)
}

// uninstallDir finds what to remove.  An explicit -dir wins, then the
// directory the running uninstaller itself lives in -- which is the only
// answer that still works when the registry write was skipped or the
// install was moved -- and only then the recorded/default location.
func uninstallDir(opt options) string {
	if opt.dir != "" {
		if abs, err := filepath.Abs(opt.dir); err == nil {
			return abs
		}
		return opt.dir
	}
	self := selfPath()
	if strings.EqualFold(filepath.Base(self), uninstallExe) {
		candidate := filepath.Dir(self)
		if _, err := os.Stat(filepath.Join(candidate, appExe)); err == nil {
			return candidate
		}
	}
	if prev, ok := readRegistryString(defaultRoot(), installDirValue); ok && prev != "" {
		return prev
	}
	return filepath.Join(localAppData(), "Programs", appName)
}

// localAppData returns %LOCALAPPDATA%, falling back to the path it almost
// always is so a stripped environment cannot send the install to the root of
// the system drive.
func localAppData() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return appName + "-setup.exe"
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func step(opt options, format string, args ...any) {
	if opt.silent {
		return
	}
	fmt.Printf(format+"\n", args...)
}

// reportf forwards coarse install progress to a front-end that attached one
// (the GUI wizard), and is a no-op for the console flow.
func reportf(opt options, percent int, format string, args ...any) {
	if opt.report == nil {
		return
	}
	opt.report(percent, fmt.Sprintf(format, args...))
}

// warn reports a non-fatal problem.  It writes to stderr even in silent mode
// so an unattended install still leaves a trace of what was skipped.
func warn(opt options, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "警告："+format+"\n", args...)
}

func fail(opt options, title string, err error) {
	writeInstallErrorLog(title, err.Error())
	fmt.Fprintf(os.Stderr, "%s: %v\n", title, err)
	if !opt.silent {
		messageBox(title, fmt.Sprintf("%v", err))
		waitForEnter()
	}
	os.Exit(1)
}

// enableUTF8Console switches the console to code page 65001 so the Chinese
// progress lines above survive a default zh-CN console (which is GBK).
func enableUTF8Console() error {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	outputCP := kernel32.NewProc("SetConsoleOutputCP")
	inputCP := kernel32.NewProc("SetConsoleCP")
	if r, _, err := outputCP.Call(65001); r == 0 {
		return err
	}
	if r, _, err := inputCP.Call(65001); r == 0 {
		return err
	}
	return nil
}

func waitForEnter() {
	fmt.Print("按回车键退出...")
	_, _ = fmt.Scanln()
}

// attachParentConsole reconnects a GUI-subsystem image to the console that
// launched it, so console-mode flags keep working after the installer was
// switched to the windowsgui subsystem.  It is a no-op when there is no
// parent console (Explorer, the normal double-click path) and never causes a
// window to appear.
func attachParentConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel32.NewProc("AttachConsole")
	if r, _, _ := attach.Call(^uintptr(0)); r == 0 {
		return // no parent console, or it was already attached
	}
	// Go captured the standard handles before main(), and a GUI image starts
	// with them null; reopen them onto the console we just attached to.
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stderr = f
	}
}
