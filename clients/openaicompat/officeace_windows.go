//go:build windows

package openaicompat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// officeAceLocalSupported reports whether this build can unwrap the OfficeAce
// client's install secret.  The desktop client is Windows-only and wraps that
// secret with DPAPI in the current user's scope.
func officeAceLocalSupported() bool { return true }

// officeAceInstallDirs resolves OfficeAce install roots from the environment
// and from the uninstall entry the installer writes.  The registry is the only
// location-independent source: the client itself lives wherever the operator
// installed it.
func officeAceInstallDirs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		key := strings.ToLower(dir)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dir)
	}
	add(os.Getenv(officeAceInstallEnv))
	for _, dir := range officeAceUninstallDirs() {
		add(dir)
	}
	for _, candidate := range [][2]string{
		{os.Getenv("LOCALAPPDATA"), filepath.Join("Programs", "OfficeAce")},
		{os.Getenv("ProgramFiles"), "OfficeAce"},
		{os.Getenv("ProgramFiles(x86)"), "OfficeAce"},
	} {
		if strings.TrimSpace(candidate[0]) == "" {
			continue
		}
		add(filepath.Join(candidate[0], candidate[1]))
	}
	return out
}

// officeAceUninstallDirs walks the Windows uninstall keys for the OfficeAce
// entry and returns its install location.  A missing key, an unreadable value
// or a foreign product just yields no candidate.
func officeAceUninstallDirs() []string {
	var out []string
	paths := []string{
		`Software\Microsoft\Windows\CurrentVersion\Uninstall`,
		`Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	}
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		for _, path := range paths {
			key, err := registry.OpenKey(root, path, registry.READ)
			if err != nil {
				continue
			}
			names, err := key.ReadSubKeyNames(-1)
			if err != nil {
				key.Close()
				continue
			}
			for _, name := range names {
				sub, err := registry.OpenKey(key, name, registry.READ)
				if err != nil {
					continue
				}
				display, _, _ := sub.GetStringValue("DisplayName")
				if officeAceDisplayName(display) {
					if loc, _, err := sub.GetStringValue("InstallLocation"); err == nil {
						out = append(out, loc)
					}
					if icon, _, err := sub.GetStringValue("DisplayIcon"); err == nil {
						if dir, ok := officeAceIconDir(icon); ok {
							out = append(out, dir)
						}
					}
				}
				sub.Close()
			}
			key.Close()
		}
	}
	return out
}

func officeAceDisplayName(display string) bool {
	name := strings.ToLower(strings.TrimSpace(display))
	return strings.Contains(name, "officeace") || strings.Contains(name, "office claw")
}

// officeAceIconDir accepts only an executable path.  The same value is often an
// icon file, whose directory is not the install root.
func officeAceIconDir(icon string) (string, bool) {
	icon = strings.TrimSpace(icon)
	if icon == "" {
		return "", false
	}
	if i := strings.Index(icon, ","); i >= 0 {
		icon = strings.TrimSpace(icon[:i])
	}
	icon = strings.Trim(icon, `"`)
	if !strings.EqualFold(filepath.Ext(icon), ".exe") {
		return "", false
	}
	return filepath.Dir(icon), true
}

// unprotectOfficeAceDPAPI unwraps the current user's DPAPI blob, which is how
// the OfficeAce client protects .oauth-profile-encryption-key.
func unprotectOfficeAceDPAPI(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("officeace: empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("officeace: CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	if out.Size == 0 || out.Data == nil {
		return nil, errors.New("officeace: DPAPI returned an empty secret")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
