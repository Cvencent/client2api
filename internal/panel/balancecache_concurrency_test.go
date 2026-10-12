package panel

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestBalanceCacheConcurrentWritesLeaveAReadableFile pins the two properties the
// account page depends on when a refresh pass and an explicit button press land
// on the same cache at once:
//
//   - the file on disk always parses, and
//   - it always contains every account that was written.
//
// The write used to run while the cache mutex was held and through a single
// fixed temp name, so two writers could interleave into one temp file and rename
// a half-written document into place -- the same failure the usage store's own
// comment records having already been fixed there. It also meant every reader
// blocked on disk I/O.
func TestBalanceCacheConcurrentWritesLeaveAReadableFile(t *testing.T) {
	path := t.TempDir() + "/balance_cache.json"
	const accounts = 24

	var clients []core.Client
	for i := range accounts {
		clients = append(clients, quotaClient("wb", core.AccountRecord{ID: "a" + strconv.Itoa(i)}))
	}
	reg := registryOf(clients...)
	cache := newBalanceCache(reg, func() time.Duration { return 0 }, path)

	var wg sync.WaitGroup
	// Writers: every account gets a balance, all at once.
	for i := range accounts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache.put("wb", "a"+strconv.Itoa(i), core.Balance{
				Credits: int64(i),
				Total:   100,
				Unit:    "credits",
			})
		}(i)
	}
	// Readers: an explicit refresh reads the same cache throughout.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				cache.snapshot("wb")
				cache.statusForClient("wb")
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// Reload from disk: the document has to be complete and parseable.
	second := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	rows := second.snapshot("wb")
	if len(rows) != accounts {
		t.Fatalf("persisted rows = %d, want %d", len(rows), accounts)
	}
	for i := range accounts {
		e, ok := rows["a"+strconv.Itoa(i)]
		if !ok {
			t.Fatalf("account a%d is missing from the persisted cache", i)
		}
		if e.Credits != int64(i) || e.Total != 100 || e.Unit != "credits" {
			t.Errorf("a%d = %+v, want the values that were written", i, e)
		}
		if e.FetchedAt == 0 {
			t.Errorf("a%d has no fetched_at", i)
		}
	}
}
