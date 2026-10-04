package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken sink") }

type bufferWriter struct{ data []byte }

func (w *bufferWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func TestFanoutAttemptsEverySinkEvenAfterOneFails(t *testing.T) {
	var after bufferWriter
	if _, err := Fanout(failWriter{}, &after).Write([]byte("kept")); err == nil {
		t.Fatal("Fanout hid the failing sink")
	}
	if got := string(after.data); got != "kept" {
		t.Fatalf("later sink received %q, want %q", got, "kept")
	}
}

func TestWriterRotatesAtConfiguredSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client2api.log")
	w, err := Open(path, 10, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("first-line\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := w.Write([]byte("second-line\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if !strings.Contains(string(current), "second-line") || strings.Contains(string(current), "first-line") {
		t.Fatalf("current log = %q, want only the second line", current)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if !strings.Contains(string(rotated), "first-line") {
		t.Fatalf("rotated log = %q, want the first line", rotated)
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "client2api.log")
	w, err := Open(path, 1024, 1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
}
