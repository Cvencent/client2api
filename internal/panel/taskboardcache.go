package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	mu      sync.Mutex
	path    string
	clients map[string]map[string]taskBoardCacheEntry
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
	defer c.mu.Unlock()
	byID, ok := c.clients[client]
	if !ok {
		byID = map[string]taskBoardCacheEntry{}
		c.clients[client] = byID
	}
	byID[id] = e
	c.persistLocked()
}

func (c *taskBoardCache) persistLocked() {
	if c == nil || c.path == "" {
		return
	}
	doc := taskBoardCacheFile{Version: 1, Clients: c.clients}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}
