//go:build windows

package kimi

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// osRelease mirrors the value Node's os.release() reports on Windows: the
// major.minor.build version, e.g. "10.0.26200".  That is what the CLI puts in
// x-msh-os-version, and (with the platform and arch in front of it) in
// x-msh-device-model.
//
// Node gets it from RtlGetVersion, which is the only API that tells the truth
// under a compatibility shim.  The registry key below carries the same numbers
// and needs no syscall trampoline, which keeps this file free of unsafe code.
//
// An empty result is a legitimate answer, not an error: the header is then sent
// empty rather than omitted, because the CLI always sends all six.
func osRelease() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = k.Close() }()

	build, _, err := k.GetStringValue("CurrentBuildNumber")
	if err != nil || strings.TrimSpace(build) == "" {
		// Layouts older than Windows 10 keep the whole version in CurrentVersion.
		if v, _, verr := k.GetStringValue("CurrentVersion"); verr == nil {
			return strings.TrimSpace(v)
		}
		return ""
	}
	build = strings.TrimSpace(build)

	major, _, _ := k.GetIntegerValue("CurrentMajorVersionNumber")
	minor, _, _ := k.GetIntegerValue("CurrentMinorVersionNumber")
	if major == 0 && minor == 0 {
		// Same pre-Windows-10 layouts as above: the build number is all there is.
		return build
	}
	return fmt.Sprintf("%d.%d.%s", major, minor, build)
}
