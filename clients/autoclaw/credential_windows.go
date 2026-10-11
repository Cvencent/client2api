//go:build windows

package autoclaw

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// safeStorageSupported reports whether this build can unwrap the AutoClaw
// desktop client's credential blob.  The desktop build is Windows-only, so the
// Electron safeStorage key is protected with DPAPI.
func safeStorageSupported() bool { return true }

func unprotectDPAPI(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("autoclaw: empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("autoclaw: CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	if out.Size == 0 || out.Data == nil {
		return nil, errors.New("autoclaw: DPAPI returned an empty key")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
