package lobsterai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The last-known-good client version persists next to the credential store
// (issue #178).  The in-memory cache dies with the process, and the version
// endpoint is a third-party manifest service that goes unreachable for days --
// a cold start during such an outage then spent the whole process lifetime
// answering "0.1.0", a version that was never released, which is more
// suspicious to the vendor than a stale one.  One small JSON file closes that
// window: a process that ever fetched a real version keeps speaking it across
// restarts.

const versionFile = "version.json"

type persistedVersion struct {
	Version string `json:"version"`
	SavedAt string `json:"saved_at"`
}

// versionPath is where the last-known-good version lives.  An empty DataDir
// means "do not persist", matching the credential store's own rule: a file
// written into the working directory would be worse than losing the value.
func (c *Client) versionPath() string {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, versionFile)
}

// loadPersistedVersion seeds the in-memory cache from disk at construction.
// A corrupt or stale-shaped file is ignored, not an error: the fallback chain
// ends at the configured constant either way.
func (c *Client) loadPersistedVersion() {
	path := c.versionPath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pv persistedVersion
	if err := json.Unmarshal(raw, &pv); err != nil {
		return
	}
	v := strings.TrimSpace(pv.Version)
	if !validVersion(v) {
		return
	}
	c.versionMu.Lock()
	if c.version == "" {
		c.version = v
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(pv.SavedAt)); err == nil {
			c.versionAt = ts
		} else {
			c.versionAt = c.now()
		}
	}
	c.versionMu.Unlock()
}

// persistVersion writes the last-known-good version.  A failure is noted, not
// returned: the in-memory cache still serves this process.
func (c *Client) persistVersion(v string) {
	path := c.versionPath()
	if path == "" {
		return
	}
	doc, err := json.Marshal(persistedVersion{Version: v, SavedAt: c.now().UTC().Format(time.RFC3339)})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, doc, 0o644); err != nil {
		c.deps.Log("lobsterai: saving version: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		c.deps.Log("lobsterai: saving version: %v", err)
	}
}
