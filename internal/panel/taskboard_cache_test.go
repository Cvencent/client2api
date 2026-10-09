package panel

import (
	"net/http"
	"testing"

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
