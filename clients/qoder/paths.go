package qoder

import (
	"path/filepath"
	"strings"
)

// paths.go holds the small filesystem predicates the CLI bridge needs to be
// safe with an operator-supplied path.

// pathWithin reports whether child is the same path as parent or lives under
// it.  It is what keeps a config-root cleanup from ever pointing outside the
// module's own data directory, however a stored leaf was spelled.
//
// Both sides are made absolute first, and the comparison uses the OS's own
// case folding, so a Windows path that differs only in case is still within.
func pathWithin(parent, child string) bool {
	parent, child = strings.TrimSpace(parent), strings.TrimSpace(child)
	if parent == "" || child == "" {
		return false
	}
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absParent, absChild)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
