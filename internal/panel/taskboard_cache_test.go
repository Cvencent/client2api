package panel

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestTaskListAllCachesKnownAccountsAndFetchesOnlyNewOnes(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"), liveAccount("a2"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true}}
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := c.accountsAsked(); len(got) != 2 {
		t.Fatalf("first scan asked %v, want both accounts", got)
	}

	// Opening the board again must serve the remembered task state. Only an
	// account the cache has never seen may cost another upstream call.
	rec, _ = doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cached scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := c.accountsAsked(); len(got) != 2 {
		t.Fatalf("cached scan asked upstream again: %v", got)
	}

	c.accounts = append(c.accounts, liveAccount("a3"))
	rec, _ = doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("new-account scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := c.accountsAsked()
	if len(got) != 3 {
		t.Fatalf("new-account scan asked %v, want only a3 to be new", got)
	}
	if got[len(got)-1] != "a3" {
		t.Fatalf("new-account scan asked %v, want a3 last", got)
	}

	rec, _ = doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1&refresh=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("forced scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := c.accountsAsked(); len(got) != 6 {
		t.Fatalf("forced scan asked %v, want every account again", got)
	}
}

func TestTaskListAllReportsWhetherRowsCameFromCache(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true, Claimed: true}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ := out["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", accounts)
	}
	first, _ := accounts[0].(map[string]any)
	if first["cached"] == true {
		t.Fatalf("first response claimed a cache hit: %v", first)
	}

	rec, out = doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cached scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ = out["accounts"].([]any)
	second, _ := accounts[0].(map[string]any)
	if second["cached"] != true {
		t.Fatalf("second response was not marked cached: %v", second)
	}
	if _, ok := second["fetched_at"].(float64); !ok {
		t.Fatalf("cached response has no fetched_at: %v", second)
	}
}

func TestTaskListAllCacheSurvivesPanelRestart(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true, Claimed: true}}
	dir := t.TempDir()

	first := taskPanel(t, c)
	first.opts.DataDir = dir
	first.initTaskBoardCache()
	rec, _ := doTask(t, first, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first panel: status=%d body=%s", rec.Code, rec.Body.String())
	}
	before := len(c.accountsAsked())

	second := taskPanel(t, c)
	second.opts.DataDir = dir
	second.initTaskBoardCache()
	rec, out := doTask(t, second, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second panel: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := len(c.accountsAsked()); got != before {
		t.Fatalf("second panel asked upstream again: calls %d -> %d", before, got)
	}
	accounts, _ := out["accounts"].([]any)
	row, _ := accounts[0].(map[string]any)
	if row["cached"] != true {
		t.Fatalf("persisted row was not marked cached: %v", row)
	}
}

// TestTaskBoardCacheExpires covers the bug that made a board render state from
// weeks earlier: the cache had no expiry, so an entry written once was served
// for ever.  A task row is vendor-derived and the module caches the billing
// call behind it, so an old entry must be treated as a miss.
func TestTaskBoardCacheExpires(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true}}
	p := taskPanel(t, c)
	p.initTaskBoardCache()

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := len(c.accountsAsked()); n != 1 {
		t.Fatalf("first scan asked %d accounts, want 1", n)
	}

	// Age the entry past the TTL without touching the other fields.
	p.taskBoardCache.mu.Lock()
	byID := p.taskBoardCache.clients["loomy"]
	e := byID["a1"]
	e.FetchedAt = time.Now().Add(-taskBoardCacheTTL - time.Minute).UnixMilli()
	byID["a1"] = e
	p.taskBoardCache.mu.Unlock()

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expired scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := len(c.accountsAsked()); n != 2 {
		t.Fatalf("a stale entry was served from cache: asked %d accounts, want 2", n)
	}
	accounts, _ := out["accounts"].([]any)
	row, _ := accounts[0].(map[string]any)
	if row["cached"] == true {
		t.Fatalf("the refetched row was still marked cached: %v", row)
	}
}

// The vendor behind a task board can slow to tens of seconds and the browser
// cancels an in-flight scan when it re-polls or navigates.  One cancelled read
// used to render "读取失败" on the row AND poison the cache with the error, so
// the next poll served the failure too.  A failed read now leaves the last
// good list in place: the row keeps showing it (cached=true) instead of an
// error, and the error never enters the cache while a good entry exists.
func TestAFailedBoardReadKeepsTheLastGoodList(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true, Claimed: true}}
	p := taskPanel(t, c)
	p.initTaskBoardCache()

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1&refresh=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first scan: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The upstream goes dark exactly the way a cancelled browser read does.
	c.fakeTaskClient.tasks = nil
	c.fakeTaskClient.listErr = context.Canceled
	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1&refresh=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("failed scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ := out["accounts"].([]any)
	row, _ := accounts[0].(map[string]any)
	if row["error"] != nil && row["error"] != "" {
		t.Fatalf("a row with a last-good list reported an error: %v", row)
	}
	tasks, _ := row["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("row tasks = %v, want the remembered share_soul row", tasks)
	}
	if row["cached"] != true {
		t.Fatalf("the fallback row was not marked cached: %v", row)
	}

	// The next non-forced poll must still serve the good list, not the error.
	rec, out = doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll after failure: status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ = out["accounts"].([]any)
	row, _ = accounts[0].(map[string]any)
	if row["error"] != nil && row["error"] != "" {
		t.Fatalf("the error leaked into the cache and the next poll: %v", row)
	}
	tasks, _ = row["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks after the failure = %v, want the good list still served", tasks)
	}
}

// When there is NO last good list the error stays visible: a board that has
// never answered must not pretend to be empty.
func TestAFailedBoardReadWithoutHistoryStillReportsTheError(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"))
	c.fakeTaskClient.listErr = context.DeadlineExceeded
	p := taskPanel(t, c)
	p.initTaskBoardCache()

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1&refresh=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan: status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ := out["accounts"].([]any)
	row, _ := accounts[0].(map[string]any)
	if row["error"] == nil || row["error"] == "" {
		t.Fatalf("a first-time failure hid the error: %v", row)
	}
}
