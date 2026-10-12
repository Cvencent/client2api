package panel

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestTaskBoardCacheConcurrentWritesLeaveAReadableFile is the board's twin of
// TestBalanceCacheConcurrentWritesLeaveAReadableFile.
//
// The board renders straight from this cache, so the two properties the account
// page already relies on are load-bearing here too:
//
//   - the file on disk always parses, and
//   - it always contains every account that was written.
//
// The write used to run while the cache mutex was held and through one fixed
// temp name, so two writers could interleave into a single temp file and rename
// a half-written document into place -- exactly the failure the rename exists
// to prevent -- while every reader of the board waited on disk I/O.
func TestTaskBoardCacheConcurrentWritesLeaveAReadableFile(t *testing.T) {
	path := t.TempDir() + "/task_board_cache.json"
	const accounts = 24

	cache := newTaskBoardCache(path)

	var wg sync.WaitGroup
	// Writers: every account gets its task list, all at once.
	for i := range accounts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache.put("wb", "a"+strconv.Itoa(i), []core.TaskInfo{{
				Code:    "task-" + strconv.Itoa(i),
				Title:   "Task " + strconv.Itoa(i),
				Group:   "PC",
				Current: int64(i),
				Target:  10,
			}}, nil)
		}(i)
	}
	// Readers: the board keeps rendering from the cache throughout.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				cache.entry("wb", "a3")
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// Reload from disk: the document has to be complete and parseable.
	second := newTaskBoardCache(path)
	for i := range accounts {
		id := "a" + strconv.Itoa(i)
		e, ok := second.entry("wb", id)
		if !ok {
			t.Fatalf("account %s is missing from the persisted cache", id)
		}
		if len(e.Tasks) != 1 {
			t.Fatalf("%s has %d tasks, want 1", id, len(e.Tasks))
		}
		if e.Tasks[0].Code != "task-"+strconv.Itoa(i) {
			t.Errorf("%s task code = %q, want task-%d", id, e.Tasks[0].Code, i)
		}
		if e.FetchedAt == 0 {
			t.Errorf("%s has no fetched_at", id)
		}
	}
}
