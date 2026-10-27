package raccoon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Token-imported credentials (origin "stored") can arrive without a device id.
// The desktop-grant endpoint is answered granted:false for any session that
// never carries X-Client-Device-ID, while sessions whose account has one are
// granted daily by the same gateway call (measured 2026-10-27 across four
// accounts: the two with a device id were granted, the two without were not).
// Loading the pool must therefore mint AND PERSIST a device id for a stored
// credential that lacks one, so the header goes out on the very first request.
func TestLoadMintsAndPersistsDeviceIDForStoredAccounts(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "accounts.json")
	statePath := filepath.Join(dir, "state.json")
	write := func(v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := os.WriteFile(storePath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write([]account{{
		AccessToken: "tok-1",
		UserID:      "u-1",
		Label:       "no-device",
	}})

	p := &pool{storePath: storePath, statePath: statePath, now: time.Now}
	p.reload(nil)

	rows := readStoreForTest(t, storePath)
	if len(rows) != 1 {
		t.Fatalf("stored rows = %d", len(rows))
	}
	if strings.TrimSpace(rows[0].DeviceID) == "" {
		t.Fatalf("device id was not minted+persisted for a stored credential: %+v", rows[0])
	}

	// A second load must keep the SAME id (stable identity across restarts).
	first := rows[0].DeviceID
	p2 := &pool{storePath: storePath, statePath: statePath, now: time.Now}
	p2.reload(nil)
	rows2 := readStoreForTest(t, storePath)
	if rows2[0].DeviceID != first {
		t.Fatalf("device id changed across loads: %q -> %q", first, rows2[0].DeviceID)
	}
}

func readStoreForTest(t *testing.T, path string) []account {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []account
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
