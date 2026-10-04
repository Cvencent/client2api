//go:build !windows

package kimi

import "runtime"

// osRelease stands in for Node's os.release() on platforms the CLI does not
// ship for.
//
// Node reports the kernel version there ("6.8.0-45-generic", "23.6.0"); Go
// exposes no portable equivalent, and reading it would mean a per-OS syscall
// for a header no supported deployment sends.  The OS name is what we have, and
// it is still better than an omitted header, because the CLI always sends all
// six x-msh-* fields.
func osRelease() string { return runtime.GOOS }
