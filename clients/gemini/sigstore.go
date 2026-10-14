package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// sigStore caches the upstream's thoughtSignature values.
//
// When a thinking-enabled model answers with a functionCall it attaches a
// thoughtSignature, and the next turn is expected to send it back verbatim or
// the reasoning chain is lost (quality quietly degrades).  The core.Message
// type has nowhere to carry it, so it is keyed by (tool name, canonical
// arguments) and persisted beside the credentials.
//
// Only functionCall signatures are stored: the vendor validates that one and
// ignores a signature on a plain text part, so storing those would just evict
// useful entries.
type sigStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]sigEntry
	dirty   bool
}

type sigEntry struct {
	Sig  string `json:"sig"`
	At   int64  `json:"at"`
	Name string `json:"name,omitempty"`
}

// sigKeyBodyLimit is the prefix of a body that participates in the key: a long
// answer's tail changes every time, so a whole-body key would never hit.
const sigKeyBodyLimit = 512

// maxSigEntries bounds the cache; a signature is a few hundred bytes.
const maxSigEntries = 2_000

func newSigStore(path string) *sigStore {
	s := &sigStore{path: path, entries: map[string]sigEntry{}}
	s.load()
	return s
}

// sigKey is sha256(role + "\0" + body[:512])[:16] in hex.
func sigKey(role, text string) string {
	body := text
	if len(body) > sigKeyBodyLimit {
		body = body[:sigKeyBodyLimit]
	}
	sum := sha256.Sum256([]byte(role + "\x00" + body))
	return hex.EncodeToString(sum[:])[:16]
}

// toolSigKey is the tool-call key.  Both sides must use canonicalArgs.
func toolSigKey(name, argsJSON string) string { return sigKey("tool:"+name, argsJSON) }

// Get returns the signature for one tool call, falling back to the most recent
// signature for the same tool name when the exact key misses (arguments drift
// between turns; an approximate signature beats none).
func (s *sigStore) Get(name, argsJSON string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[toolSigKey(name, argsJSON)]; ok && e.Sig != "" {
		return e.Sig
	}
	return s.latestForToolLocked(name)
}

func (s *sigStore) latestForToolLocked(name string) string {
	best := ""
	bestAt := int64(-1)
	for _, e := range s.entries {
		if e.Name == name && e.Sig != "" && e.At >= bestAt {
			best = e.Sig
			bestAt = e.At
		}
	}
	return best
}

// Put records a signature.  Empty keys or signatures are dropped.
func (s *sigStore) Put(name, argsJSON, signature string) {
	if s == nil {
		return
	}
	sig := strings.TrimSpace(signature)
	if sig == "" {
		return
	}
	key := toolSigKey(name, argsJSON)
	if key == "" {
		return
	}
	s.mu.Lock()
	s.entries[key] = sigEntry{Sig: sig, At: time.Now().Unix(), Name: name}
	if len(s.entries) > maxSigEntries {
		s.evictLocked()
	}
	s.dirty = true
	s.mu.Unlock()
}

// evictLocked drops the oldest half.
func (s *sigStore) evictLocked() {
	type kv struct {
		key string
		at  int64
	}
	all := make([]kv, 0, len(s.entries))
	for k, e := range s.entries {
		all = append(all, kv{k, e.At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	for i := 0; i < len(all)/2; i++ {
		delete(s.entries, all[i].key)
	}
}

// Flush writes the cache atomically.  A failure is not fatal.
func (s *sigStore) Flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	payload := make(map[string]sigEntry, len(s.entries))
	for k, v := range s.entries {
		payload[k] = v
	}
	s.mu.Unlock()
	return core.WriteJSONAtomic(s.path, payload)
}

// load reads the cache; a missing or corrupt file is treated as empty.
func (s *sigStore) load() {
	if s.path == "" {
		return
	}
	var payload map[string]sigEntry
	if err := core.ReadJSON(s.path, &payload); err != nil {
		return
	}
	for k, e := range payload {
		if e.Sig == "" {
			continue
		}
		s.entries[k] = e
	}
}

// sigStorePath is where the cache lives under the module's data dir.
func sigStorePath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, sigFile)
}
