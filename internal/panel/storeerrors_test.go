package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// TestStoreWriteFailureSurfacesInOverview pins the issue #34 contract: a
// persisted store whose disk write fails must (a) keep serving from memory,
// (b) remember the failure, (c) expose it through /panel/api/overview's
// store_errors, and (d) ring the panel log ring at the moment it happens --
// instead of silently showing data the next restart erases.
func TestStoreWriteFailureSurfacesInOverview(t *testing.T) {
	dir := t.TempDir()
	// chat_history.json as a DIRECTORY makes os.Rename inside
	// core.WriteFileAtomic fail with a path error, the same way a read-only
	// directory or a stray file would.
	blocked := filepath.Join(dir, "panel", "chat_history.json")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("preparing the blocked path: %v", err)
	}
	c := &fakeClient{name: "zcode", models: []core.Model{{ID: "m1", OwnedBy: "zcode"}}}
	h := New(Options{
		Registry: registryOf(c),
		Started:  time.Now(),
		DataDir:  dir,
		Logs:     gateway.NewLogRing(64),
	})

	// One create: it lands in memory (the handler answers 200 with the new
	// id) even though the disk write fails.
	payload, _ := json.Marshal(map[string]any{"title": "probe", "messages": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/chat/conversations", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d (%s), want 200 -- the in-memory path must keep working", rec.Code, rec.Body.String())
	}

	// Overview now carries the store error.
	oreq := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	orec := httptest.NewRecorder()
	h.ServeHTTP(orec, oreq)
	if orec.Code != http.StatusOK {
		t.Fatalf("overview = %d", orec.Code)
	}
	var body struct {
		StoreErrors map[string]string `json:"store_errors"`
	}
	if err := json.Unmarshal(orec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding overview: %v", err)
	}
	msg, ok := body.StoreErrors["chat history"]
	if !ok || msg == "" {
		t.Fatalf("store_errors = %v, want a chat history entry", body.StoreErrors)
	}

	// The panel log ring recorded the failure at write time.
	lreq := httptest.NewRequest(http.MethodGet, "/panel/api/logs", nil)
	lrec := httptest.NewRecorder()
	h.ServeHTTP(lrec, lreq)
	if !strings.Contains(lrec.Body.String(), "chat history") {
		t.Fatalf("logs = %.400s, want a chat history write-failure line", lrec.Body.String())
	}
}
