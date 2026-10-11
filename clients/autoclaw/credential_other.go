//go:build !windows

package autoclaw

import "errors"

// safeStorageSupported is false off Windows: the AutoClaw desktop credential
// is an Electron safeStorage blob whose key is wrapped with DPAPI.  The pasted
// token path keeps working on those platforms.
func safeStorageSupported() bool { return false }

func unprotectDPAPI([]byte) ([]byte, error) {
	return nil, errors.New("autoclaw: DPAPI is available only on Windows")
}
