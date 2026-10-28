package panel

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// DefaultTaskBoardCacheFileName is the relative path under the data directory.
const DefaultTaskBoardCacheFileName = "panel/task_board_cache.json"

type taskBoardCacheEntry struct {
	Tasks     []core.TaskInfo `json:"tasks"`
	FetchedAt int64           `json:"fetched_at"`
	Error     string          `json:"error,omitempty"`
}

type taskBoardCacheFile struct {
	Version int                                       `json:"version"`
	Clients map[string]map[string]taskBoardCacheEntry `json:"clients"`
}

// taskBoardCache remembers the last task list seen for an account.  The board
// uses it as a write-through read cache: a known account is rendered without
// another vendor call, while a new account is fetched once and then remembered.
type taskBoardCache struct {
	mu sync.Mutex
	// persistMu serialises disk writes only, so a reader rendering the board
	// never waits on a marshal plus a file create/write/rename.
	persistMu sync.Mutex
	path      string
	clients   map[string]map[string]taskBoardCacheEntry
}

func newTaskBoardCache(path string) *taskBoardCache {
	c := &taskBoardCache{
		path:    strings.TrimSpace(path),
		clients: map[string]map[string]taskBoardCacheEntry{},
	}
	c.load()
	return c
}

func (c *taskBoardCache) load() {
	if c == nil || c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var doc taskBoardCacheFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	if doc.Clients == nil {
		return
	}
	c.clients = doc.Clients
}

// taskBoardCacheTTL bounds how long a remembered task list is served without
// asking the module again.
//
// The cache is a write-through read cache and used to have no expiry at all,
// which meant an entry written once was served for ever: a board could still be
// rendering an account's state from weeks earlier, long after the vendor had
// released a new grant or the module had changed its mind.  A task row is
// vendor-derived and cheap to re-read (the module itself caches the underlying
// billing call), so a few minutes of staleness is the most this is worth.
const taskBoardCacheTTL = 5 * time.Minute

func (c *taskBoardCache) entry(client, id string) (taskBoardCacheEntry, bool) {
	if c == nil {
		return taskBoardCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byID, ok := c.clients[client]
	if !ok {
		return taskBoardCacheEntry{}, false
	}
	e, ok := byID[id]
	if !ok {
		return taskBoardCacheEntry{}, false
	}
	// A stale entry is reported as a miss so the caller fetches again; the
	// module decides what is current, this cache only avoids the round trip.
	if e.FetchedAt > 0 && time.Since(time.UnixMilli(e.FetchedAt)) > taskBoardCacheTTL {
		return taskBoardCacheEntry{}, false
	}
	if e.Tasks != nil {
		e.Tasks = append([]core.TaskInfo(nil), e.Tasks...)
	}
	return e, true
}

func (c *taskBoardCache) put(client, id string, tasks []core.TaskInfo, err error) {
	if c == nil {
		return
	}
	e := taskBoardCacheEntry{
		Tasks:     append([]core.TaskInfo(nil), tasks...),
		FetchedAt: time.Now().UnixMilli(),
	}
	if err != nil {
		e.Error = core.Redact(err.Error())
	}
	c.mu.Lock()
	byID, ok := c.clients[client]
	if !ok {
		byID = map[string]taskBoardCacheEntry{}
		c.clients[client] = byID
	}
	byID[id] = e
	c.mu.Unlock()

	c.persist()
}

// persist writes the cache to disk without holding c.mu.
//
// Two properties this has to keep, both of which the previous version lost:
//
//   - No reader blocks on I/O. The board renders straight from this cache, so
//     holding the mutex across a MarshalIndent of the whole document plus a
//     file rename stalled every panel read for the duration of the write.
//   - The file on disk is always complete. The fixed temp name "path.tmp" let
//     two concurrent writers interleave into one file and rename the result,
//     publishing exactly the half-written document the rename exists to
//     prevent. core.WriteFileAtomic gives each write its own temp name.
//
// The document is deep-copied under c.mu first: handing MarshalIndent the live
// maps would only freeze the top-level pointers while it walks into them.
func (c *taskBoardCache) persist() {
	if c == nil || c.path == "" {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()

	c.mu.Lock()
	doc := taskBoardCacheFile{Version: 1, Clients: copyTaskBoardClients(c.clients)}
	c.mu.Unlock()

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = core.WriteFileAtomic(c.path, append(raw, '\n'))
}

// copyTaskBoardClients clones the whole cache document.  It must be called with
// c.mu held; the result is what the encoder walks without the lock.
func copyTaskBoardClients(in map[string]map[string]taskBoardCacheEntry) map[string]map[string]taskBoardCacheEntry {
	out := make(map[string]map[string]taskBoardCacheEntry, len(in))
	for name, byID := range in {
		cp := make(map[string]taskBoardCacheEntry, len(byID))
		for id, e := range byID {
			if len(e.Tasks) > 0 {
				e.Tasks = append([]core.TaskInfo(nil), e.Tasks...)
			}
			cp[id] = e
		}
		out[name] = cp
	}
	return out
}
