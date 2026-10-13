package panel

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

var checkinTestCST = time.FixedZone("CST", 8*60*60)

func TestCheckinStoreRecordsOnlyDefinitiveDailyOutcomes(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore(
		filepath.Join(t.TempDir(), "panel", "checkin_state.json"),
		func() time.Time { return now },
	)

	store.Record("qwenwork", "acct-1", "daily", core.CheckinResult{
		OK:      true,
		Message: "checked in",
	})
	done, ok := store.Lookup("qwenwork", "acct-1", "daily")
	if !ok || done.State != core.CheckinStateDone || done.Message != "checked in" {
		t.Fatalf("done status = %+v, ok=%v; want done with the vendor message", done, ok)
	}

	store.Record("raccoon", "acct-2", "login-points", core.CheckinResult{
		Skipped: true,
		Error:   "granted:false",
	})
	unavailable, ok := store.Lookup("raccoon", "acct-2", "login-points")
	if !ok || unavailable.State != core.CheckinStateUnavailable || unavailable.Message != "granted:false" {
		t.Fatalf("unavailable status = %+v, ok=%v; want unavailable with the vendor message", unavailable, ok)
	}

	store.Record("qwenwork", "acct-3", "daily", core.CheckinResult{
		Error: "network timeout",
	})
	if got, ok := store.Lookup("qwenwork", "acct-3", "daily"); ok {
		t.Fatalf("non-definitive failure was recorded as %+v", got)
	}
}

func TestCheckinStoreResetsAtTheNextShanghaiDay(t *testing.T) {
	now := time.Date(2026, 10, 14, 23, 59, 0, 0, checkinTestCST)
	store := NewCheckinStore("", func() time.Time { return now })
	store.Record("qwenwork", "acct-1", "daily", core.CheckinResult{OK: true})

	if _, ok := store.Lookup("qwenwork", "acct-1", "daily"); !ok {
		t.Fatal("today's status was not visible before midnight")
	}

	now = time.Date(2026, 10, 15, 0, 1, 0, 0, checkinTestCST)
	if got, ok := store.Lookup("qwenwork", "acct-1", "daily"); ok {
		t.Fatalf("yesterday's status leaked into the next day: %+v", got)
	}
}

func TestCheckinStorePersistsAcrossInstances(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	path := filepath.Join(t.TempDir(), "panel", "checkin_state.json")
	first := NewCheckinStore(path, func() time.Time { return now })
	first.Record("raccoon", "acct-1", "login-points", core.CheckinResult{Skipped: true, Error: "granted:false"})

	second := NewCheckinStore(path, func() time.Time { return now })
	got, ok := second.Lookup("raccoon", "acct-1", "login-points")
	if !ok || got.State != core.CheckinStateUnavailable {
		t.Fatalf("persisted status = %+v, ok=%v; want unavailable", got, ok)
	}
}

func TestCheckinStoreIsSafeForConcurrentRecords(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore("", func() time.Time { return now })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.Record("platform", "account-"+string(rune('a'+i%26)), "daily", core.CheckinResult{OK: true})
		}(i)
	}
	wg.Wait()

	for i := 0; i < 26; i++ {
		id := "account-" + string(rune('a'+i))
		if _, ok := store.Lookup("platform", id, "daily"); !ok {
			t.Fatalf("missing concurrently recorded status for %s", id)
		}
	}
}

func TestPanelExposesTodayCheckinStatusInAccountList(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore("", func() time.Time { return now })
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "ci"},
		accounts:       []core.AccountRecord{liveAccount("a1")},
		res:            core.CheckinResult{OK: true, Message: "checked in"},
	}
	p := taskPanel(t, c)
	p.checkins = store

	rec, _ := doTask(t, p, "POST", "/panel/api/clients/ci/accounts/a1/checkin", `{"action":"daily"}`)
	if rec.Code != 200 {
		t.Fatalf("checkin = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	rec, out := doTask(t, p, "GET", "/panel/api/clients/ci/accounts", "")
	if rec.Code != 200 {
		t.Fatalf("accounts = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	accounts, _ := out["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", out["accounts"])
	}
	account, _ := accounts[0].(map[string]any)
	checkins, _ := account["checkins"].(map[string]any)
	daily, _ := checkins["daily"].(map[string]any)
	if daily["state"] != core.CheckinStateDone || daily["message"] != "checked in" {
		t.Fatalf("account checkins = %v, want done with the vendor message", account["checkins"])
	}
}

func TestPanelExposesVendorUnavailableCheckinStatus(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore("", func() time.Time { return now })
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "ci"},
		accounts:       []core.AccountRecord{liveAccount("a1")},
		res:            core.CheckinResult{Skipped: true, Error: "the vendor has not enabled daily check-in"},
	}
	p := taskPanel(t, c)
	p.checkins = store

	rec, _ := doTask(t, p, "POST", "/panel/api/clients/ci/accounts/a1/checkin", `{"action":"daily"}`)
	if rec.Code != 200 {
		t.Fatalf("checkin = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	_, out := doTask(t, p, "GET", "/panel/api/clients/ci/accounts", "")
	accounts, _ := out["accounts"].([]any)
	account, _ := accounts[0].(map[string]any)
	checkins, _ := account["checkins"].(map[string]any)
	daily, _ := checkins["daily"].(map[string]any)
	if daily["state"] != core.CheckinStateUnavailable {
		t.Fatalf("account checkins = %v, want unavailable", account["checkins"])
	}
}

func TestPanelDoesNotGrayOnTransportFailure(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, checkinTestCST)
	store := NewCheckinStore("", func() time.Time { return now })
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "ci"},
		accounts:       []core.AccountRecord{liveAccount("a1")},
		err:            errors.New("network timeout"),
	}
	p := taskPanel(t, c)
	p.checkins = store

	rec, _ := doTask(t, p, "POST", "/panel/api/clients/ci/accounts/a1/checkin", `{"action":"daily"}`)
	if rec.Code != 400 {
		t.Fatalf("checkin = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	_, out := doTask(t, p, "GET", "/panel/api/clients/ci/accounts", "")
	accounts, _ := out["accounts"].([]any)
	account, _ := accounts[0].(map[string]any)
	if checkins, ok := account["checkins"].(map[string]any); ok && len(checkins) > 0 {
		t.Fatalf("transport failure was exposed as a check-in state: %v", checkins)
	}
}
