//go:build windows

package qoder

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func safeStorageSupported() bool { return true }

func unprotectDPAPI(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("qoder: empty DPAPI blob")
	}

	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("qoder: CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	if out.Size == 0 || out.Data == nil {
		return nil, errors.New("qoder: DPAPI returned an empty key")
	}
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
