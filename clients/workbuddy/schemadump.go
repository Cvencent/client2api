package workbuddy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Diagnosing a 11129 that survives normalization.
//
// body.go rewrites every construct the vendor's tool validator is known to
// refuse, so a 11129 that still arrives means the validator rejects something
// nobody has seen yet.  The vendor's answer names no field -- it is a bare
// "invalid function call parameters" -- so there is nothing in the response to
// work from, and the request body is the only evidence that exists.
//
// A 11129 must therefore leave the offending schema behind.  Two constraints
// shape how:
//
//   - one dump per process, not one per failure.  A caller retrying through a
//     broken tool definition produces the same 11129 every turn; writing each
//     one would turn a diagnostic into a disk-filling loop.
//   - the schema only, never the conversation.  `tools` is what the validator
//     reads, and it carries no caller content.  Messages stay out of the file
//     even though they are in the same body.
//
// The dump is written under <data_dir>/workbuddy/schema-rejected.json, which is
// the same directory the module already owns, so an operator can find it
// without being told a path.
const schemaDumpName = "schema-rejected.json"

var (
	schemaDumpMu   sync.Mutex
	schemaDumpDone bool
)

// dumpRejectedSchema writes the tools array of a body whose schema the vendor
// just refused, once per process.  A failure to write is logged and otherwise
// ignored: a diagnostic must never change the outcome of the request.
func (c *Client) dumpRejectedSchema(body []byte, msg string) {
	if c == nil {
		return
	}
	schemaDumpMu.Lock()
	if schemaDumpDone {
		schemaDumpMu.Unlock()
		return
	}
	schemaDumpDone = true
	schemaDumpMu.Unlock()

	var wire struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	// A body that does not decode is still worth reporting: the vendor refused
	// it, and the fact that it is not JSON is itself the finding.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	_ = dec.Decode(&wire)

	record := map[string]any{
		"at":     time.Now().UTC().Format(time.RFC3339),
		"model":  wire.Model,
		"reason": msg,
		"tools":  wire.Tools,
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		c.logf("[workbuddy] 11129 diagnostic: marshal: %v", err)
		return
	}
	dir := filepath.Join(c.deps.DataDir, "workbuddy")
	if c.deps.DataDir == "" {
		c.logf("[workbuddy] 11129 diagnostic: no data dir, schema not saved")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.logf("[workbuddy] 11129 diagnostic: mkdir: %v", err)
		return
	}
	path := filepath.Join(dir, schemaDumpName)
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		c.logf("[workbuddy] 11129 diagnostic: write %s: %v", path, err)
		return
	}
	c.logf("[workbuddy] 11129 diagnostic: the refused tools schema is in %s", path)
}
