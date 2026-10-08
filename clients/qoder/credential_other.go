//go:build !windows

package qoder

import "errors"

func safeStorageSupported() bool { return false }

func unprotectDPAPI([]byte) ([]byte, error) {
	return nil, errors.New("qoder: DPAPI is available only on Windows")
}
