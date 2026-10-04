package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// DefaultLimit is how many notifications the panel retains.
const DefaultLimit = 200

// Alert is one operator-facing notification.
type Alert struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Client  string    `json:"client,omitempty"`
	Model   string    `json:"model,omitempty"`
	Account string    `json:"account,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Count   int       `json:"count,omitempty"`
}

// Store is a bounded, persistent alert journal.  Alerts are low-frequency, so
// Add writes synchronously; a failed write is retained in LastError rather
// than losing the in-memory notification.
type Store struct {
	limit int
	path  string

	mu      sync.Mutex
	alerts  []Alert // oldest first
	lastErr error
	// readAt is the timestamp of the newest alert the operator has marked
	// read.  It is a watermark rather than a per-row flag so the file format
	// stays small and a new alert is unread by construction.
	readAt time.Time
}

type alertFile struct {
	Version int       `json:"version"`
	Saved   time.Time `json:"saved"`
	ReadAt  time.Time `json:"read_at,omitempty"`
	Alerts  []Alert   `json:"alerts"`
}

// NewStore returns an in-memory store when path is empty.
func NewStore(limit int, path string) *Store {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Store{limit: limit, path: strings.TrimSpace(path)}
}

// Add appends one alert, evicts the oldest beyond the cap, and persists it.
func (s *Store) Add(a Alert) {
	if s == nil {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	s.mu.Lock()
	s.alerts = append(s.alerts, a)
	if len(s.alerts) > s.limit {
		copy(s.alerts, s.alerts[len(s.alerts)-s.limit:])
		s.alerts = s.alerts[:s.limit]
	}
	path := s.path
	raw := s.marshalLocked()
	s.mu.Unlock()

	if path == "" {
		return
	}
	if err := core.WriteFileAtomic(path, raw); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
	}
}

// List returns alerts newest first.
func (s *Store) List() []Alert {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alert, len(s.alerts))
	for i := range s.alerts {
		out[i] = s.alerts[len(s.alerts)-1-i]
	}
	return out
}

// Unread reports how many alerts arrived after the operator's read watermark.
func (s *Store) Unread() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.alerts {
		if s.readAt.IsZero() || a.At.After(s.readAt) {
			n++
		}
	}
	return n
}

// MarkAllRead advances the read watermark past the newest alert and persists
// it.  History is kept; only the unread count changes.
func (s *Store) MarkAllRead() {
	if s == nil {
		return
	}
	s.mu.Lock()
	mark := s.readAt
	for _, a := range s.alerts {
		if a.At.After(mark) {
			mark = a.At
		}
	}
	s.readAt = mark
	path := s.path
	raw := s.marshalLocked()
	s.mu.Unlock()
	if path != "" {
		if err := core.WriteFileAtomic(path, raw); err != nil {
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
		}
	}
}

// Clear drops every retained alert and persists the empty journal.  The read
// watermark is cleared too, so a later alert starts from a clean slate.
func (s *Store) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.alerts = nil
	s.readAt = time.Time{}
	path := s.path
	raw := s.marshalLocked()
	s.mu.Unlock()
	if path == "" {
		return
	}
	if err := core.WriteFileAtomic(path, raw); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
	}
}

// LastError is the most recent persistence error, if any.
func (s *Store) LastError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Save writes the current journal.
func (s *Store) Save() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	path := s.path
	raw := s.marshalLocked()
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := core.WriteFileAtomic(path, raw); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.lastErr = nil
	s.mu.Unlock()
	return nil
}

func (s *Store) marshalLocked() []byte {
	f := alertFile{Version: 1, Saved: time.Now().UTC(), ReadAt: s.readAt, Alerts: make([]Alert, len(s.alerts))}
	copy(f.Alerts, s.alerts)
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		// Alert contains only JSON-safe scalar fields, but keep Add panic-free.
		s.lastErr = err
		return []byte("{}")
	}
	return append(raw, '\n')
}

// Load replaces the in-memory journal with the file's contents.
func (s *Store) Load() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	var f alertFile
	if err := json.Unmarshal(raw, &f); err != nil {
		err = fmt.Errorf("alerts file %s: %w", path, err)
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	if len(f.Alerts) > s.limit {
		f.Alerts = f.Alerts[len(f.Alerts)-s.limit:]
	}
	s.mu.Lock()
	s.alerts = append([]Alert(nil), f.Alerts...)
	s.readAt = f.ReadAt
	s.lastErr = nil
	s.mu.Unlock()
	return nil
}
