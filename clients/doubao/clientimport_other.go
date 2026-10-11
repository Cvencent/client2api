//go:build !windows

package doubao

import "fmt"

// clientimport_other.go: the Doubao desktop client is a Windows application,
// so on any other platform the import path reports that it cannot read it.

func clientImportSupported() bool { return false }

func unprotectClientDPAPI([]byte) ([]byte, error) {
	return nil, fmt.Errorf("doubao: reading the desktop client is supported only on Windows")
}
