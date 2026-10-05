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

	first := newBalanceCache(reg, func() time.Duration { return 0 }, path)
	first.put("wb", "a1", core.Balance{Credits: 42, Total: 100, Unit: "credits"})

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
	if e.FetchedAt == 0 {
		t.Error("fetched_at did not survive the round trip")
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
