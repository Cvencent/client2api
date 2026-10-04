//go:build windows

package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// payload is staged by installer/build.ps1 and mirrors the installed layout
// one-for-one, so writing the install is a directory copy and nothing has to
// keep a second file list in sync.
//
//go:embed all:payload
var payload embed.FS

const (
	appKey       = `Software\client2api`
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\client2api`
)

func install(opt options) error {
	dir := installDir(opt)

	step(opt, "%s 安装程序 %s", appName, version)
	step(opt, "")

	step(opt, "[1/6] 安装目录：%s", dir)
	reportf(opt, 4, "准备安装目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建安装目录失败：%w", err)
	}

	step(opt, "[2/6] 停止正在运行的 client2api")
	reportf(opt, 12, "停止正在运行的 client2api")
	stopRunningApp()

	step(opt, "[3/6] 写入程序文件")
	reportf(opt, 22, "正在写入程序文件")
	written, preserved, err := writePayload(dir, !opt.noData)
	if err != nil {
		return fmt.Errorf("写入程序文件失败：%w", err)
	}
	step(opt, "        已写入 %d 个文件", written)
	if preserved > 0 {
		step(opt, "        保留已有配置与账号数据 %d 个文件", preserved)
	}
	reportf(opt, 62, "已写入 %d 个文件", written)

	if err := writePanelShortcutFile(dir); err != nil {
		return fmt.Errorf("写入面板快捷方式失败：%w", err)
	}

	step(opt, "[4/6] 生成卸载程序")
	reportf(opt, 70, "生成卸载程序")
	if err := copyFile(selfPath(), filepath.Join(dir, uninstallExe)); err != nil {
		return fmt.Errorf("生成卸载程序失败：%w", err)
	}

	step(opt, "[5/6] 创建快捷方式")
	reportf(opt, 78, "创建快捷方式")
	if err := createShortcuts(opt, dir); err != nil {
		// A locked-down or redirected Start Menu must not cost the user the
		// whole install: the program files, uninstaller and registry entry are
		// the parts that make it usable and removable.
		warn(opt, "创建快捷方式失败（安装继续）：%v", err)
	}

	step(opt, "[6/6] 注册卸载信息")
	reportf(opt, 88, "注册卸载信息")
	if err := writeRegistry(dir); err != nil {
		warn(opt, "写入注册表失败（安装继续）：%v", err)
	}

	step(opt, "")
	step(opt, "安装完成：%s", dir)
	panel, hasPanel := configuredPanelURL(dir)
	if hasPanel {
		step(opt, "控制面板：%s", panel)
	} else {
		step(opt, "控制面板：配置中没有可用的监听地址，本次不会自动打开页面")
	}
	reportf(opt, 95, "配置完成")

	if !opt.noLaunch {
		if hasPanel {
			step(opt, "正在启动网关并打开控制面板...")
			reportf(opt, 97, "正在启动 client2api 并打开控制面板")
		} else {
			step(opt, "正在启动网关（未打开控制面板）...")
			reportf(opt, 97, "正在启动 client2api")
		}
		launchApp(dir)
	}
	reportf(opt, 100, "安装完成：%s", dir)
	if !opt.silent {
		waitForEnter()
	}
	return nil
}

// uninstall runs in two hops.  The copy inside the install directory cannot
// delete itself, so the first invocation copies this binary to %TEMP% and hands
// over; the temp copy owns the directory and can remove it wholesale.
func uninstall(opt options) error {
	dir := uninstallDir(opt)

	if !opt.fromTemp {
		tmp, err := os.MkdirTemp("", "client2api-uninstall-")
		if err != nil {
			return fmt.Errorf("创建临时目录失败：%w", err)
		}
		tmpExe := filepath.Join(tmp, uninstallExe)
		if err := copyFile(selfPath(), tmpExe); err != nil {
			return fmt.Errorf("复制卸载程序失败：%w", err)
		}
		args := []string{"--uninstall", "--from-temp", "--dir", dir,
			"--parent-pid", strconv.Itoa(os.Getpid())}
		if opt.silent {
			args = append(args, "--silent")
		}
		// Deliberately not waited on: the helper has to be able to delete
		// this process's own image, which Windows keeps locked while it runs.
		if err := runDetached(tmpExe, args...); err != nil {
			return fmt.Errorf("启动卸载程序失败：%w", err)
		}
		step(opt, "卸载进行中：%s", dir)
		return nil
	}

	// The %TEMP% copy runs while the install-dir one is still mapped, and a
	// running image cannot be deleted; wait for it to go before touching dir.
	if opt.parentPID > 0 {
		waitForProcessExit(opt.parentPID, 60*time.Second)
	}

	step(opt, "卸载 client2api：%s", dir)
	stopRunningApp()
	removeShortcuts(opt, dir)
	if err := removeRegistry(); err != nil {
		return fmt.Errorf("清理注册表失败：%w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除安装目录失败：%w", err)
	}

	// Best effort: the temp copy is usually locked, and only an elevated
	// process may queue a delete-on-reboot.  A leftover file in %TEMP% is a
	// normal outcome for a per-user uninstaller.
	scheduleSelfDelete(selfPath())

	step(opt, "卸载完成")
	if !opt.silent {
		// Launched from the Start Menu or "Apps & features" there is no
		// console to print to, so the only visible confirmation is a dialog.
		if !hasConsole() {
			messageBox("client2api", "卸载完成。")
		}
		waitForEnter()
	}
	return nil
}

// writePayload copies the embedded tree into dir.  Files under data/ are
// skipped when the operator asked for a program-only install.
//
// The paths that hold operator state -- configs/client2api.json and
// everything under data/ -- are written only when they are not already
// there.  An upgrade replaces the program and leaves the live config, the
// account pool and the usage history alone: without this, installing a
// package staged with -no-data would reset the gateway to the example
// config, and reinstalling a full package would drop the packager's own
// credentials over the operator's.
func writePayload(dir string, includeData bool) (written, preserved int, err error) {
	return copyPayload(payload, "payload", dir, includeData)
}

// copyPayload is writePayload's body, split out so tests can drive it with an
// in-memory tree.  A payload carries data/ only when the build staged one, so
// the rule that an upgrade leaves the account pool alone has to hold in both
// shapes.
func copyPayload(fsys fs.FS, root, dir string, includeData bool) (written, preserved int, err error) {
	err = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.FromSlash(rel)

		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		// The placeholder keeps the embedded directory non-empty in a fresh
		// checkout; it is not part of the product.
		if filepath.Base(rel) == ".gitkeep" {
			return nil
		}
		if !includeData && isAccountData(rel) {
			return nil
		}

		dst := filepath.Join(dir, rel)
		if isOperatorState(rel) {
			if _, statErr := os.Stat(dst); statErr == nil {
				preserved++
				return nil
			}
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return err
		}
		written++
		return nil
	})
	return written, preserved, err
}

// isAccountData is the part of the tree that -no-data drops: the account pool
// and the usage history.  The config is separate -- it is staged either way,
// but never written over an existing one.
func isAccountData(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == "data" || strings.HasPrefix(rel, "data/")
}

// isOperatorState reports whether a payload path holds state the operator
// owns.  An upgrade leaves these exactly as it found them.
func isOperatorState(rel string) bool {
	return isAccountData(rel) || filepath.ToSlash(rel) == "configs/client2api.json"
}

func writePanelShortcutFile(dir string) error {
	panel, ok := configuredPanelURL(dir)
	if !ok {
		_ = os.Remove(filepath.Join(dir, panelLNK))
		return nil
	}
	body := "[InternetShortcut]\r\n" +
		"URL=" + panel + "\r\n" +
		"IconFile=" + filepath.Join(dir, appName+".ico") + "\r\n" +
		"IconIndex=0\r\n"
	return os.WriteFile(filepath.Join(dir, panelLNK), []byte(body), 0o644)
}

func createShortcuts(opt options, dir string) error {
	exe := filepath.Join(dir, appExe)
	args := fmt.Sprintf("-config %q -data-dir %q",
		filepath.Join(dir, "configs", "client2api.json"),
		filepath.Join(dir, "data"))
	icon := filepath.Join(dir, appName+".ico")
	if _, err := os.Stat(icon); err != nil {
		icon = exe
	}

	// Every shortcut is independent: a redirected Start Menu should still
	// leave the desktop shortcut (and vice versa).  The caller turns the
	// joined error into a warning instead of aborting the install.
	var errs []error

	programs, _ := knownFolder(windows.FOLDERID_Programs,
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs"))
	menu := filepath.Join(programs, appName)
	if err := os.MkdirAll(menu, 0o755); err != nil {
		errs = append(errs, err)
	} else {
		if err := createShortcut(filepath.Join(menu, "client2api 服务器.lnk"), exe, args, dir, icon,
			"启动 client2api 本地网关"); err != nil {
			errs = append(errs, err)
		}
		if err := createShortcut(filepath.Join(menu, "卸载 client2api.lnk"),
			filepath.Join(dir, uninstallExe), "", dir, icon, "卸载 client2api"); err != nil {
			errs = append(errs, err)
		}
		panelShortcut := filepath.Join(dir, panelLNK)
		if _, statErr := os.Stat(panelShortcut); statErr == nil {
			if err := copyFile(panelShortcut, filepath.Join(menu, "打开控制面板.url")); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if opt.startup {
		startupDir := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
		if err := createShortcut(filepath.Join(startupDir, appName+".lnk"), exe, args, dir, icon,
			"开机启动 client2api 本地网关"); err != nil {
			errs = append(errs, err)
		}
	}
	if !opt.noDesktop {
		desktop, _ := knownFolder(windows.FOLDERID_Desktop, filepath.Join(os.Getenv("USERPROFILE"), "Desktop"))
		if err := createShortcut(filepath.Join(desktop, appName+".lnk"), exe, args, dir, icon,
			"启动 client2api 本地网关"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeShortcuts(opt options, dir string) {
	programs, _ := knownFolder(windows.FOLDERID_Programs,
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs"))
	menu := filepath.Join(programs, appName)
	for _, name := range []string{"client2api 服务器.lnk", "打开控制面板.url", "卸载 client2api.lnk"} {
		_ = os.Remove(filepath.Join(menu, name))
	}
	_ = os.Remove(menu)

	desktop, _ := knownFolder(windows.FOLDERID_Desktop, filepath.Join(os.Getenv("USERPROFILE"), "Desktop"))
	_ = os.Remove(filepath.Join(desktop, appName+".lnk"))

	startupDir := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	_ = os.Remove(filepath.Join(startupDir, appName+".lnk"))
}

func writeRegistry(dir string) error {
	app, _, err := registry.CreateKey(registry.CURRENT_USER, appKey, registry.WRITE)
	if err != nil {
		return err
	}
	defer app.Close()
	if err := app.SetStringValue(installDirValue, dir); err != nil {
		return err
	}
	if err := app.SetStringValue("Version", version); err != nil {
		return err
	}

	un, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.WRITE)
	if err != nil {
		return err
	}
	defer un.Close()

	exe := filepath.Join(dir, appExe)
	unst := `"` + filepath.Join(dir, uninstallExe) + `"`
	values := map[string]string{
		"DisplayName":          appName + " " + version,
		"DisplayVersion":       version,
		"Publisher":            appName,
		"InstallLocation":      dir,
		"DisplayIcon":          exe,
		"UninstallString":      unst,
		"QuietUninstallString": unst + " --silent",
	}
	for k, v := range values {
		if err := un.SetStringValue(k, v); err != nil {
			return err
		}
	}
	for _, k := range []string{"NoModify", "NoRepair"} {
		if err := un.SetDWordValue(k, 1); err != nil {
			return err
		}
	}
	// "Apps & features" shows this in KB.
	return un.SetDWordValue("EstimatedSize", uint32(embeddedSize()/1024))
}

func removeRegistry() error {
	if err := registry.DeleteKey(registry.CURRENT_USER, uninstallKey); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, appKey); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readRegistryString(root registry.Key, name string) (string, bool) {
	k, err := registry.OpenKey(root, appKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	return v, true
}

func defaultRoot() registry.Key { return registry.CURRENT_USER }

// embeddedSize is the uncompressed payload size, used for the "Apps &
// features" size column.
func embeddedSize() int64 {
	var total int64
	_ = fs.WalkDir(payload, "payload", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// stopRunningApp kills any running gateway so the copy in step 3 can replace a
// locked client2api.exe.  taskkill is used rather than a toolhelp walk: it is
// always present and its exit code is not interesting here.
func stopRunningApp() {
	_ = runHidden("taskkill", "/F", "/IM", appExe)
	time.Sleep(400 * time.Millisecond)
}

func launchApp(dir string) {
	exe := filepath.Join(dir, appExe)
	cmd := exec.Command(exe,
		"-config", filepath.Join(dir, "configs", "client2api.json"),
		"-data-dir", filepath.Join(dir, "data"))
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
	if err := cmd.Start(); err != nil {
		return
	}
	panel, ok := configuredPanelURL(dir)
	if !ok {
		return
	}
	// Give the listener a moment so the browser does not land on a connection
	// error; the gateway is usually accepting well inside this.
	time.Sleep(3 * time.Second)
	_ = runHidden("rundll32", "url.dll,FileProtocolHandler", panel)
}

func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

// runDetached starts a helper without waiting for it, so the caller is free
// to exit and release the lock Windows holds on its own executable image.
func runDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// waitForProcessExit blocks until pid is gone or the timeout elapses.  It
// deliberately ignores errors: a process that already exited (or that we
// may not open) needs no waiting.
func waitForProcessExit(pid int, timeout time.Duration) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_, _ = windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
}

// hasConsole reports whether this process has a console window to talk to.
func hasConsole() bool {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	h, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	return h != 0
}

func knownFolder(id *windows.KNOWNFOLDERID, fallback string) (string, error) {
	p, err := windows.KnownFolderPath(id, windows.KF_FLAG_DEFAULT)
	if err != nil || p == "" {
		return fallback, err
	}
	return p, nil
}

func messageBox(title, text string) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x00000040)
	runtime.KeepAlive(t)
	runtime.KeepAlive(m)
}

// scheduleSelfDelete queues the temp copy for deletion at the next reboot.
// This needs HKLM access, so it silently does nothing for a non-elevated
// uninstall -- by design, not by oversight.
func scheduleSelfDelete(path string) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("MoveFileExW")
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	const moveFileDelayUntilReboot = 0x4
	_, _, _ = proc.Call(uintptr(unsafe.Pointer(p)), 0, moveFileDelayUntilReboot)
	runtime.KeepAlive(p)
}
