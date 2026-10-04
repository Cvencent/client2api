package logfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	DefaultMaxBytes = 10 << 20
	DefaultBackups  = 3
)

// Fanout writes to every sink before returning an error.  io.MultiWriter stops
// at the first failure, which is wrong for a windowsgui process: an invalid
// stdout handle must not prevent the panel ring and the log file from seeing
// the line.
func Fanout(writers ...io.Writer) io.Writer {
	return fanout(writers)
}

type fanout []io.Writer

func (w fanout) Write(p []byte) (int, error) {
	var firstErr error
	for _, dst := range w {
		if dst == nil {
			continue
		}
		n, err := dst.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(p), firstErr
}

// Writer is a size-rotating UTF-8 log file.  It is intentionally small: the
// process only needs one log stream and a bounded set of rotated files.
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

// Open opens path for appending, creating its parent directory when needed.
func Open(path string, maxBytes int64, backups int) (*Writer, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("log path cannot be empty")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if backups < 0 {
		backups = 0
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Writer{path: path, maxBytes: maxBytes, backups: backups, file: f, size: fi.Size()}, nil
}

// Write appends one record, rotating first when the record would exceed the
// configured size.  A single oversized record is still written rather than
// dropped or split.
func (w *Writer) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if w.backups == 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		_ = os.Remove(w.path + "." + strconv.Itoa(w.backups))
		for i := w.backups - 1; i >= 1; i-- {
			src := w.path + "." + strconv.Itoa(i)
			dst := w.path + "." + strconv.Itoa(i+1)
			if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}

// Close flushes and closes the underlying file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
