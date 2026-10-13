package panel

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

// balancecache_test.go pins the two properties the accounts page depends on:
// the cache survives a restart, and a refresh pass touches the oldest accounts
// first instead of the whole pool at once.

func TestBalanceCachePersistsAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel", "balance_cache.json")
	reg := registryOf(quotaClient("wb", core.AccountRecord{ID: "a1"}))
	used, remaining := 15.0, 85.0

	first := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	first.put("wb", "a1", core.Balance{
		Credits: 42,
		Total:   100,
		Unit:    "credits",
		Lines: []core.BalanceLine{{
			Label:            "循环额度",
			UsedPercent:      &used,
			RemainingPercent: &remaining,
			ResetsAt:         "2026-10-14T20:20:26+08:00",
		}},
	})

	// A second cache over the same file is what a gateway restart builds: the
	// numbers the operator last saw have to come back before any vendor call.
	second := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	e, ok := second.entry("wb", "a1")
	if !ok {
		t.Fatal("the persisted entry did not come back")
	}
	if e.Credits != 42 || e.Total != 100 || e.Unit != "credits" {
		t.Errorf("reloaded entry = %+v, want the stored numbers", e)
	}
	if len(e.Lines) != 1 || e.Lines[0].Label != "循环额度" ||
		e.Lines[0].UsedPercent == nil || *e.Lines[0].UsedPercent != 15 {
		t.Errorf("reloaded lines = %+v, want the quota meter", e.Lines)
	}
	if e.FetchedAt == 0 {
		t.Error("fetched_at did not survive the round trip")
	}
}

// A scheduled sweep lives outside the panel and cannot call its private
// refresh. It has to feed the very cache the accounts page renders, or it can
// read every account successfully while the column still says "last read 8
// hours ago".
func TestBalanceStoreFeedsThePanelColumn(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1", State: "ready"})
	reg := registryOf(c)
	store := NewBalanceStore(reg, func() time.Duration { return 0 }, filepath.Join(t.TempDir(), "panel", "balance_cache.json"))
	store.Publish("wb", "a1", core.Balance{Credits: 42, Total: 100, Unit: "credits"})

	p, h := balanceHandler(Options{Registry: reg, Balances: store, Started: time.Now()})
	// The GET schedules its own gentle pass; throttle it so the published read
	// is the only possible source of the number under assertion.
	p.balanceCache.lastSoft = time.Now()

	body := decodeMap(t, get(t, h, "/panel/api/clients/wb/balances"))
	rows := rowsOf(t, body["accounts"])
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want one", len(rows))
	}
	if rows[0]["credits"] != float64(42) || rows[0]["credits_total"] != float64(100) {
		t.Fatalf("published row = %v/%v, want 42/100", rows[0]["credits"], rows[0]["credits_total"])
	}
	if body["unit"] != "credits" {
		t.Fatalf("unit = %v, want the published label", body["unit"])
	}
	if got, ok := rows[0]["fetched_at"].(float64); !ok || got <= 0 {
		t.Fatalf("fetched_at = %v, want the stamp the publish just wrote", rows[0]["fetched_at"])
	}
	if soon := c.seenSoon(); len(soon) != 0 {
		t.Fatalf("/balances asked the vendor %v; the published store already had the number", soon)
	}
}

func TestBalanceCacheMissesNoErrorRow(t *testing.T) {
	// An error row has no credits but still proves the account was tried; it has
	// to persist too, or the row falls back to "never read" after a restart.
	path := filepath.Join(t.TempDir(), "balance_cache.json")
	reg := registryOf(quotaClient("wb", core.AccountRecord{ID: "a1"}))

	first := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	first.putError("wb", "a1", context.Canceled)

	second := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	e, ok := second.entry("wb", "a1")
	if !ok || e.Error == "" {
		t.Fatalf("error row = %+v ok=%v, want it to survive", e, ok)
	}
}

func TestBalanceCacheTargetsTheOldestAndTheUnknownFirst(t *testing.T) {
	c := newBalanceCache(registryOf(quotaClient("wb",
		core.AccountRecord{ID: "fresh"},
		core.AccountRecord{ID: "stale"},
		core.AccountRecord{ID: "never"},
	)), func() time.Duration { return 0 }, "")

	now := time.Now()
	c.putEntry("wb", "fresh", balanceEntry{Credits: 1, FetchedAt: now.UnixMilli()})
	c.putEntry("wb", "stale", balanceEntry{Credits: 1, FetchedAt: now.Add(-time.Hour).UnixMilli()})

	got := c.targets(2)
	if len(got) != 2 {
		t.Fatalf("targets = %d, want the requested 2", len(got))
	}
	// A row nobody ever read has zero fetched time, so it sorts ahead of the
	// stale one; the fresh row waits for a later pass.
	if got[0].id != "never" || got[1].id != "stale" {
		t.Fatalf("order = %s/%s, want never/stale", got[0].id, got[1].id)
	}
}

func TestBalanceCacheRefusesToOverlapPasses(t *testing.T) {
	c := newBalanceCache(registryOf(quotaClient("wb",
		core.AccountRecord{ID: "a1"}, core.AccountRecord{ID: "a2"},
		core.AccountRecord{ID: "a3"}, core.AccountRecord{ID: "a4"},
	)), func() time.Duration { return 0 }, "")

	if !c.refresh(context.Background(), balanceRefreshBatch, true) {
		t.Fatal("the first pass did not start")
	}
	// Overlapping passes are exactly the burst the cache exists to prevent, so
	// the second one has to be refused even with force.
	if c.refresh(context.Background(), balanceRefreshBatch, true) {
		t.Fatal("a second pass started while the first was in flight")
	}
}
