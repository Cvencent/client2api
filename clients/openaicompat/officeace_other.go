//go:build !windows

package openaicompat

import (
	"errors"
	"os"
	"strings"
)

// officeAceLocalSupported is false off Windows: the OfficeAce desktop client
// protects its install secret with Windows DPAPI, so there is nothing to import
// on another platform and the panel hides the import tab entirely.
func officeAceLocalSupported() bool { return false }

// officeAceInstallDirs only honours the explicit override.  Without it there is
// no install to find, and the caller never reaches this path because the
// wrapper is not applied.
func officeAceInstallDirs() []string {
	if dir := strings.TrimSpace(os.Getenv(officeAceInstallEnv)); dir != "" {
		return []string{dir}
	}
	return nil
}

func unprotectOfficeAceDPAPI([]byte) ([]byte, error) {
	return nil, errors.New("officeace: DPAPI is available only on Windows")
}
