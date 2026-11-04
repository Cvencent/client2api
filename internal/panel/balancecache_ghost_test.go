package panel

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"client2api/internal/core"
)

// These tests pin the ghost-entry treatment behind issue #171: the balance
// cache used to grow forever, so a deleted account's stale credits were
// re-served the moment the same credential key was re-added, and twenty-five
// days of removed accounts kept riding along in the document.

func ghostCache(t *testing.T, clients ...core.Client) (*balanceCache, string) {
	t.Helper()
	path := t.TempDir() + "/balance_cache.json"
	c := newBalanceCache(registryOf(clients...), func() time.Duration { return 0 }, path)
	return c, path
}

func TestReconcilePrunesEntriesForAccountsTheModuleNoLongerHolds(t *testing.T) {
	keeper := quotaClient("wb", core.AccountRecord{ID: "kept"})
	c, path := ghostCache(t, keeper)

	c.put("wb", "kept", core.Balance{Credits: 5, Total: 10, Unit: "credits"})
	// The ghost: an entry the module's pool can no longer explain.
	c.put("wb", "gone", core.Balance{Credits: 9, Total: 10, Unit: "credits"})

	if !c.reconcile() {
		t.Fatal("reconcile reported nothing dropped, want the ghost pruned")
	}
	if _, ok := c.snapshot("wb")["gone"]; ok {
		t.Fatal("the ghost survived reconcile (issue #171)")
	}
	if _, ok := c.snapshot("wb")["kept"]; !ok {
		t.Fatal("reconcile ate a live entry: it must only remove what no account holds")
	}

	// The pruning reached the disk: a restart must not resurrect the ghost.
	second := newBalanceCache(registryOf(keeper), func() time.Duration { return 0 }, path)
	if _, ok := second.snapshot("wb")["gone"]; ok {
		t.Fatal("the ghost was persisted back by the pruned document")
	}
}

func TestReconcileKeepsEntriesWhenTheModuleReadFails(t *testing.T) {
	// A transient Accounts failure must not be read as "the pool is empty":
	// the module gets skipped this round, entries stay put.
	sick := quotaClient("wb", core.AccountRecord{ID: "kept"})
	sick.accountsErr = errors.New("vendor timed out")
	c, _ := ghostCache(t, sick)

	c.put("wb", "kept", core.Balance{Credits: 5, Total: 10, Unit: "credits"})
	c.put("wb", "unknown", core.Balance{Credits: 7, Total: 10, Unit: "credits"})

	if c.reconcile() {
		t.Fatal("reconcile dropped entries while the module's read failed")
	}
	if _, ok := c.snapshot("wb")["kept"]; !ok {
		t.Fatal("a failed read wiped a live entry")
	}
	if _, ok := c.snapshot("wb")["unknown"]; !ok {
		t.Fatal("a failed read wiped an unverifiable entry: skip, not prune")
	}
}

func TestReconcileDropsEntriesOfAModuleTheRegistryLost(t *testing.T) {
	// bcsn/vsllm: the cache outlived its source.  No module can resurrect
	// those entries and no page can render them, so they are ghosts by
	// definition even though there is no Accounts call to contradict them.
	keeper := quotaClient("wb", core.AccountRecord{ID: "kept"})
	c, path := ghostCache(t, keeper)

	c.put("wb", "kept", core.Balance{Credits: 5, Total: 10, Unit: "credits"})
	c.put("bcsn", "key-gone", core.Balance{Credits: 3, Total: 10, Unit: "credits"})

	if !c.reconcile() {
		t.Fatal("reconcile reported nothing dropped, want the dead module's entries gone")
	}
	if rows := c.snapshot("bcsn"); len(rows) != 0 {
		t.Fatalf("the sourceless module kept %d entries", len(rows))
	}
	if _, ok := c.snapshot("wb")["kept"]; !ok {
		t.Fatal("the live module was collateral damage")
	}

	second := newBalanceCache(registryOf(keeper), func() time.Duration { return 0 }, path)
	if rows := second.snapshot("bcsn"); len(rows) != 0 {
		t.Fatalf("the dead module's entries were persisted back: %d rows", len(rows))
	}
}

func TestDropForgetsOneEntryAndPersistsIt(t *testing.T) {
	c, path := ghostCache(t, quotaClient("wb", core.AccountRecord{ID: "kept"}))
	c.put("wb", "kept", core.Balance{Credits: 5, Total: 10, Unit: "credits"})
	c.put("wb", "doomed", core.Balance{Credits: 9, Total: 10, Unit: "credits"})

	if !c.drop("wb", "doomed") {
		t.Fatal("drop reported nothing removed")
	}
	if _, ok := c.snapshot("wb")["doomed"]; ok {
		t.Fatal("drop left the entry in place")
	}
	if _, ok := c.snapshot("wb")["kept"]; !ok {
		t.Fatal("drop ate its neighbour")
	}
	if c.drop("wb", "doomed") {
		t.Fatal("dropping an absent entry again must be a no-op")
	}

	// The removal persisted: a fresh reader must not see the doomed row.
	raw, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := io.ReadAll(raw)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc, []byte(`"doomed"`)) {
		t.Fatal("the doomed row survived in the document on disk")
	}
	if !bytes.Contains(doc, []byte(`"kept"`)) {
		t.Fatal("the kept row vanished from the document on disk")
	}
}

func TestDeleteAccountRouteDropsTheCacheEntry(t *testing.T) {
	// The DELETE route used to only call am.RemoveAccount: the cache kept the
	// row, so re-adding the same credential key (the documented recovery step
	// for a misclassified account) served last month's credits as this
	// month's until the next sweep.
	inner := quotaClient("wb", core.AccountRecord{ID: "acc-1"})
	p := taskPanel(t, inner)
	p.initBalanceCache()
	p.balanceCache.put("wb", "acc-1", core.Balance{Credits: 42, Total: 100, Unit: "credits"})

	rec := httptest.NewRecorder()
	p.accountByID(rec, httptest.NewRequest(http.MethodDelete, "/panel/api/clients/wb/accounts/acc-1", nil), inner, "acc-1")
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE account = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := p.balanceCache.snapshot("wb")["acc-1"]; ok {
		t.Fatal("the DELETE route left the account's balance entry cached (issue #171)")
	}
}
