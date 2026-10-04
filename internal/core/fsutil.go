package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureDir creates dir (and parents) with 0700 when missing.
func EnsureDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("EnsureDir: empty path")
	}
	return os.MkdirAll(dir, 0o700)
}

// WriteFileAtomic writes data to path through a temp file + rename, mode 0600.
// Readers therefore never observe a half-written file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// WriteJSONAtomic marshals v as indented JSON and writes it atomically.
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'))
}

// ReadJSON reads and unmarshals a JSON file.  os.ErrNotExist is returned
// unchanged so callers can test it with errors.Is.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// MaskSecret renders a credential for logs: an 8-character prefix plus its
// length, never the whole value.
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return fmt.Sprintf("***len=%d", len(s))
	}
	return fmt.Sprintf("%s…len=%d", s[:8], len(s))
}
