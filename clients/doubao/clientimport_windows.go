//go:build windows

package doubao

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// clientimport_windows.go unwraps Chromium's app-bound cookie values with
// Windows DPAPI.  The Doubao client runs the same Chromium that encrypts
// cookie values as AES-256-GCM(App-Bound-Key) and then DPAPI-protects that key,
// so the key unwrap is the only platform-specific step; the AES work and the
// SQLite read live in portable files.

func clientImportSupported() bool { return true }

// unprotectClientDPAPI unwraps one DPAPI blob.
func unprotectClientDPAPI(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("doubao: empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("doubao: CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	if out.Size == 0 || out.Data == nil {
		return nil, fmt.Errorf("doubao: DPAPI returned an empty key")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
