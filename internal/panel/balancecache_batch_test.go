package panel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"client2api/internal/core"
)

// blockingBalanceClient blocks one account's vendor read so the test can
// observe whether the cache has already written a partial document while a
// refresh pass is still in flight.
type blockingBalanceClient struct {
	*fakeQuotaClient
	blockID string
	started chan string
	release chan struct{}
}

func (c *blockingBalanceClient) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	if id == c.blockID {
		c.started <- id
		<-c.release
	}
	return c.fakeQuotaClient.AccountBalance(ctx, id, soon)
}

// TestBalanceRefreshWritesOnceAtPassEnd pins the write-amplification fix: a
// refresh pass over N accounts must not rewrite the whole JSON document after
// every account. The file must not exist while the pass is still running, then
// appear once with every row after the pass completes.
func TestBalanceRefreshWritesOnceAtPassEnd(t *testing.T) {
	accounts := []core.AccountRecord{{ID: "acct-0"}, {ID: "acct-1"}}
	inner := quotaClient("wb", accounts...)
	inner.balances["acct-0"] = core.Balance{Credits: 5, Total: 10}
	inner.balances["acct-1"] = core.Balance{Credits: 6, Total: 10}
	c := &blockingBalanceClient{
		fakeQuotaClient: inner,
		blockID:         "acct-1",
		started:         make(chan string, 1),
		release:         make(chan struct{}),
	}

	path := t.TempDir() + "/balance_cache.json"
	cache := newBalanceCache(registryOf(c), func() time.Duration { return 0 }, path)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.runRefresh(context.Background(), "wb", cache.targetsForClient("wb", 0), reviveOff, false)
	}()

	select {
	case <-c.started:
	case <-time.After(3 * time.Second):
		t.Fatal("second account was never reached")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file exists before the pass completed: err=%v", err)
	}
	close(c.release)
	<-done

	reloaded := newBalanceCache(registryOf(inner), func() time.Duration { return 0 }, path)
	for i := range accounts {
		id := fmt.Sprintf("acct-%d", i)
		if _, ok := reloaded.entry("wb", id); !ok {
			t.Fatalf("account %s missing from the persisted document", id)
		}
	}
}

// TestBalanceRefreshPersistsFailedRowsToo makes sure batching does not drop an
// error row: it still proves the account was tried and must survive a restart.
func TestBalanceRefreshPersistsFailedRowsToo(t *testing.T) {
	inner := quotaClient("wb", core.AccountRecord{ID: "bad"})
	inner.balErr["bad"] = errors.New("vendor refused")
	path := t.TempDir() + "/balance_cache.json"
	cache := newBalanceCache(registryOf(inner), func() time.Duration { return 0 }, path)

	cache.runRefresh(context.Background(), "wb", cache.targetsForClient("wb", 0), reviveOff, false)
	reloaded := newBalanceCache(registryOf(inner), func() time.Duration { return 0 }, path)
	e, ok := reloaded.entry("wb", "bad")
	if !ok || e.Error == "" {
		t.Fatalf("error row = %+v ok=%v, want it persisted", e, ok)
	}
}
