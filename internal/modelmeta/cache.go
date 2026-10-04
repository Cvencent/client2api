package modelmeta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cacheVersion is written into the cache document so a future format change can be
// detected instead of misread.
const cacheVersion = 1

// cacheFileNamePrefix names the files this package owns inside a cache directory.
const cacheFileNamePrefix = "modelmeta-"

// cacheEntry is one model's persisted metadata.
//
// FetchedAt and Source are recorded for the operator: the file on disk explains
// itself. A cache hit always reports SourceCache to the caller, because "this came
// from my local cache" is the resolution path the caller actually took; the
// original layer stays visible here in the file.
type cacheEntry struct {
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"supported_efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	Source          string   `json:"source,omitempty"`
	FetchedAt       string   `json:"fetched_at,omitempty"`
}

type cacheDoc struct {
	Version int                   `json:"version"`
	Client  string                `json:"client"`
	Models  map[string]cacheEntry `json:"models"`
}

// Store is a last-resort metadata cache beside the vendor, static and models.dev
// layers: it lets a value learned once survive a restart, and it is the weakest
// layer in the chain, so it can only ever fill a field nothing else filled.
//
// Robustness rules, all of them load-bearing:
//
//   - a missing cache file is simply an empty cache;
//   - a corrupt cache file is treated as absent, never a panic and never a
//     poisoned answer — the worst case is that we re-learn the values;
//   - writes are atomic (temp file in the target directory + rename) so a crash
//     mid-write cannot leave a half-written cache behind;
//   - the cache is memory-only when no directory was configured, which keeps a
//     module that was not wired for persistence working unchanged.
type Store struct {
	dir    string
	client string
	path   string

	mu      sync.Mutex
	loaded  bool
	entries map[string]cacheEntry
	warnf   func(format string, args ...any)
}

// CacheFileName returns the file name a Store uses for a client inside a cache
// directory.
func CacheFileName(client string) string {
	name := strings.TrimSpace(client)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	safe := b.String()
	if safe == "" || safe == "." || safe == ".." {
		safe = "default"
	}
	return cacheFileNamePrefix + safe + ".json"
}

// OpenStore returns a cache Store backed by dir. An empty dir yields a working but
// memory-only Store, so callers never have to nil-check.
func OpenStore(dir, client string) *Store {
	s := &Store{dir: strings.TrimSpace(dir), client: strings.TrimSpace(client)}
	if s.dir != "" {
		s.path = filepath.Join(s.dir, CacheFileName(s.client))
	}
	return s
}

// SetWarnf installs a warning sink for unreadable or unwritable cache files. The
// default is silence: this library never writes to the process's stdio on its own.
func (s *Store) SetWarnf(fn func(format string, args ...any)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.warnf = fn
	s.mu.Unlock()
}

// Path returns the cache file path, or "" for a memory-only Store.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// warn reports a problem to the configured logger. Every caller already holds
// s.mu (which is what keeps the reported state consistent), so this must not
// take the lock again — doing so self-deadlocks the Store.
func (s *Store) warn(format string, args ...any) {
	if fn := s.warnf; fn != nil {
		fn(format, args...)
	}
}

// Len returns the number of cached models.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return len(s.entries)
}

// Get returns the cached metadata for a model. A missing, unreadable or corrupt
// cache is reported as "absent", never as an error and never as a panic: a cache is
// an optimisation and must not be able to break a request.
func (s *Store) Get(model string) (Meta, bool) {
	if s == nil || model == "" {
		return Meta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	e, ok := s.entries[model]
	if !ok {
		return Meta{}, false
	}
	m := e.meta()
	if m.IsZero() {
		return Meta{}, false
	}
	return m, true
}

// put reports whether a value was actually stored, so callers can avoid pointless
// disk writes.
func (e cacheEntry) meta() Meta {
	m := Meta{Source: SourceCache, FieldSources: map[string]string{}}
	if e.ContextLength > 0 {
		m.ContextLength = e.ContextLength
		m.FieldSources[FieldContextLength] = SourceCache
	}
	if e.MaxOutputTokens > 0 {
		m.MaxOutputTokens = e.MaxOutputTokens
		m.FieldSources[FieldMaxOutputTokens] = SourceCache
	}
	if efforts := dedupeStrings(e.Efforts); len(efforts) > 0 {
		m.Efforts = efforts
		m.FieldSources[FieldEfforts] = SourceCache
		if e.DefaultEffort != "" && containsString(efforts, e.DefaultEffort) {
			m.DefaultEffort = e.DefaultEffort
			m.FieldSources[FieldDefaultEffort] = SourceCache
		}
	}
	return m
}

// Put stores metadata for a model. It returns true when the cache changed and was
// (or did not need to be) written. Failures to persist are reported through the
// warning sink and never to the caller: an unwritable cache directory must not
// break a lookup.
func (s *Store) Put(model string, m Meta) bool {
	if s == nil || model == "" || m.IsZero() {
		return false
	}
	entry := cacheEntry{
		ContextLength:   m.ContextLength,
		MaxOutputTokens: m.MaxOutputTokens,
		Efforts:         dedupeStrings(m.Efforts),
		DefaultEffort:   m.DefaultEffort,
		Source:          m.Source,
		FetchedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.loadLocked()
	if s.entries == nil {
		s.entries = map[string]cacheEntry{}
	}
	if prev, ok := s.entries[model]; ok && sameCacheEntry(prev, entry) {
		s.mu.Unlock()
		return false
	}
	s.entries[model] = entry
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		s.warn("modelmeta: cannot write cache %s: %v", s.path, err)
		return true
	}
	return true
}

// Delete forgets one model, reporting whether anything was removed.
func (s *Store) Delete(model string) bool {
	if s == nil || model == "" {
		return false
	}
	s.mu.Lock()
	s.loadLocked()
	_, ok := s.entries[model]
	if ok {
		delete(s.entries, model)
		if err := s.saveLocked(); err != nil {
			s.warn("modelmeta: cannot write cache %s: %v", s.path, err)
		}
	}
	s.mu.Unlock()
	return ok
}

// loadLocked reads the cache file once per process (or per Store). Every failure
// mode — no file, no permission, truncated JSON, wrong shape — collapses to an
// empty cache with a warning.
func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[string]cacheEntry{}
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warn("modelmeta: cannot read cache %s: %v", s.path, err)
		}
		return
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return
	}
	var doc cacheDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Corrupt cache: treat as absent rather than failing the lookup, so a bad
		// file cannot make a model permanently unresolvable.
		s.warn("modelmeta: ignoring corrupt cache %s: %v", s.path, err)
		return
	}
	if doc.Version != 0 && doc.Version != cacheVersion {
		s.warn("modelmeta: ignoring cache %s with version %d", s.path, doc.Version)
		return
	}
	for id, entry := range doc.Models {
		s.entries[id] = entry
	}
}

// saveLocked writes the whole cache atomically. Callers must hold s.mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := cacheDoc{Version: cacheVersion, Client: s.client, Models: s.entries}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, cacheFileNamePrefix+"*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	// 0600: the cache is local state, not something to share between accounts.
	if err := tmp.Chmod(0o600); err != nil {
		discard()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		discard()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func sameCacheEntry(a, b cacheEntry) bool {
	if a.ContextLength != b.ContextLength || a.MaxOutputTokens != b.MaxOutputTokens ||
		a.DefaultEffort != b.DefaultEffort || a.Source != b.Source {
		return false
	}
	if len(a.Efforts) != len(b.Efforts) {
		return false
	}
	for i := range a.Efforts {
		if a.Efforts[i] != b.Efforts[i] {
			return false
		}
	}
	return true
}
