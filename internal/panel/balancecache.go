package panel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// The balances column used to cost one vendor API call per account every time
// the accounts page was opened: /balances fanned out to every account on every
// load.  That is exactly the pattern that trips a vendor's abuse detection on a
// large pool, and it also made the column blank for a moment each time.
//
// This cache changes the contract: the read is served from the last known
// numbers, and refreshing moves to the background a few accounts at a time.
// The cache is persisted to the data directory so it survives a restart, and
// the panel only ever *reports* what the vendor last said -- it never invents a
// number for an account it has not read yet.

const (
	// balanceRefreshBatch is how many accounts one background pass asks about.
	// Deliberately small: the point of the feature is not to fan out over the
	// whole fleet at once.
	balanceRefreshBatch = 3
	// balanceRefreshGap spaces the accounts inside that pass.  Three calls back
	// to back still look like a burst to some vendors, so leave a gap between
	// them.
	balanceRefreshGap = 2 * time.Second
	// balanceSoftInterval throttles the gentle refresh that every account-page
	// open performs, so idle polling cannot restart it continuously.
	balanceSoftInterval = 45 * time.Second
	// balanceForceInterval throttles the explicit 刷新余额 button.  Even a
	// deliberate click should not queue a second pass while the first is
	// still in flight.
	balanceForceInterval = 5 * time.Second
	// balanceFetchedTTL is when a cached number is old enough to earn a
	// "数据较旧" hint in the row tooltip.
	balanceFetchedTTL = 10 * time.Minute
)

// DefaultBalanceCacheFileName is the relative path under the data directory.
const DefaultBalanceCacheFileName = "panel/balance_cache.json"

// balanceEntry is one account's last known balance.  FetchedAt is stored in
// unix milliseconds, matching the unit the rest of the panel's JSON uses.
type balanceEntry struct {
	Credits           int64   `json:"credits"`
	Used              float64 `json:"used,omitempty"`
	Total             int64   `json:"credits_total"`
	Unlimited         bool    `json:"unlimited,omitempty"`
	Expiring          int64   `json:"expiring,omitempty"`
	EarliestAt        string  `json:"earliest_at,omitempty"`
	EarliestRemaining int64   `json:"earliest_remaining,omitempty"`
	Unit              string  `json:"unit,omitempty"`
	FetchedAt         int64   `json:"fetched_at"`
	Error             string  `json:"error,omitempty"`
}

type balanceCacheFile struct {
	Version int                                `json:"version"`
	Clients map[string]map[string]balanceEntry `json:"clients"`
}

// balanceProvider is the slice of the registry this cache needs.  It is an
// interface so tests can drive refreshes without a real module.
type balanceProvider interface {
	All() []core.Client
}

// balanceCache keeps the last known balance of every account and refreshes it
// a few accounts at a time.  The zero value is not usable; call
// newBalanceCache.
type balanceCache struct {
	mu        sync.Mutex
	all       balanceProvider
	soon      func() time.Duration
	path      string
	clients   map[string]map[string]balanceEntry
	lastSoft  time.Time
	lastForce time.Time
	inflight  bool
}

func newBalanceCache(all balanceProvider, soon func() time.Duration, path string) *balanceCache {
	c := &balanceCache{
		all:     all,
		soon:    soon,
		path:    strings.TrimSpace(path),
		clients: map[string]map[string]balanceEntry{},
	}
	c.load()
	return c
}

// load reads the persisted snapshot.  A missing or corrupt file is not an
// error: the first refresh will repopulate the cache, and refusing to start
// the panel over a stale cache would be worse than an empty column for a
// moment.
func (c *balanceCache) load() {
	if c == nil || c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var doc balanceCacheFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	if doc.Clients == nil {
		return
	}
	c.clients = doc.Clients
}

// persistLocked writes the cache atomically.  The caller must hold c.mu.
func (c *balanceCache) persistLocked() {
	if c == nil || c.path == "" {
		return
	}
	doc := balanceCacheFile{Version: 1, Clients: c.clients}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}

// entry returns one account's cached balance and whether it exists.
func (c *balanceCache) entry(client, id string) (balanceEntry, bool) {
	if c == nil {
		return balanceEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byID, ok := c.clients[client]
	if !ok {
		return balanceEntry{}, false
	}
	e, ok := byID[id]
	return e, ok
}

// put stores a balance read.  Errors are stored too, with no numbers, so a
// later successful read can replace them and the UI can see that the account
// was tried.
func (c *balanceCache) put(client, id string, bal core.Balance) {
	c.putEntry(client, id, balanceEntry{
		Credits:           bal.Credits,
		Used:              bal.Used,
		Total:             bal.Total,
		Unlimited:         bal.Unlimited,
		Expiring:          bal.Expiring,
		EarliestAt:        formatEarliestAt(bal.EarliestAt),
		EarliestRemaining: bal.EarliestRemaining,
		Unit:              bal.Unit,
		FetchedAt:         time.Now().UnixMilli(),
	})
}

func (c *balanceCache) putError(client, id string, err error) {
	c.putEntry(client, id, balanceEntry{
		FetchedAt: time.Now().UnixMilli(),
		Error:     core.Redact(err.Error()),
	})
}

func (c *balanceCache) putEntry(client, id string, e balanceEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byID, ok := c.clients[client]
	if !ok {
		byID = map[string]balanceEntry{}
		c.clients[client] = byID
	}
	byID[id] = e
	c.persistLocked()
}

func formatEarliestAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// refreshTarget is one (client, account) pair whose value is stale or unknown.
type refreshTarget struct {
	client  string
	id      string
	label   string
	state   string
	fetched time.Time
}

// refresh asks up to limit accounts for a fresh balance, oldest first.  force
// bypasses the soft throttle but never the in-flight guard: two passes must
// not overlap, because that would be the burst this cache exists to prevent.
func (c *balanceCache) refresh(ctx context.Context, limit int, force bool) bool {
	if c == nil {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	if c.inflight {
		c.mu.Unlock()
		return false
	}
	gap := balanceSoftInterval
	if force {
		gap = balanceForceInterval
	}
	last := c.lastSoft
	if force {
		last = c.lastForce
	}
	if !last.IsZero() && now.Sub(last) < gap {
		c.mu.Unlock()
		return false
	}
	if limit <= 0 {
		limit = balanceRefreshBatch
	}
	c.inflight = true
	if force {
		c.lastForce = now
	} else {
		c.lastSoft = now
	}
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			c.inflight = false
			c.mu.Unlock()
		}()
		targets := c.targets(limit)
		for i, tgt := range targets {
			if i > 0 {
				select {
				case <-time.After(balanceRefreshGap):
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
			c.refreshOne(ctx, tgt)
		}
	}()
	return true
}

func (c *balanceCache) targets(limit int) []refreshTarget {
	var out []refreshTarget
	for _, client := range c.all.All() {
		_, ok := core.AsBalanceProvider(client)
		if !ok {
			continue
		}
		am, ok := core.AsAccountManager(client)
		if !ok {
			continue
		}
		lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		recs, err := am.Accounts(lctx)
		cancel()
		if err != nil {
			continue
		}
		for _, rec := range recs {
			e, known := c.entry(client.Name(), rec.ID)
			fetched := time.Time{}
			if known && e.FetchedAt > 0 {
				fetched = time.UnixMilli(e.FetchedAt)
			}
			out = append(out, refreshTarget{
				client:  client.Name(),
				id:      rec.ID,
				label:   rec.Label,
				state:   rec.State,
				fetched: fetched,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].fetched.Before(out[j].fetched)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (c *balanceCache) refreshOne(ctx context.Context, tgt refreshTarget) {
	var client core.Client
	for _, cand := range c.all.All() {
		if cand.Name() == tgt.client {
			client = cand
			break
		}
	}
	if client == nil {
		return
	}
	bp, ok := core.AsBalanceProvider(client)
	if !ok {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bal, err := bp.AccountBalance(bctx, tgt.id, c.soon())
	if err != nil {
		c.putError(tgt.client, tgt.id, err)
		return
	}
	c.put(tgt.client, tgt.id, bal)
}

// snapshot returns a copy of one client's cache, safe to hand to a handler.
func (c *balanceCache) snapshot(client string) map[string]balanceEntry {
	out := map[string]balanceEntry{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byID, ok := c.clients[client]
	if !ok {
		return out
	}
	for id, e := range byID {
		out[id] = e
	}
	return out
}

// unitOf picks the unit label for a module from whatever rows named one.
func unitOf(rows map[string]balanceEntry) string {
	for _, e := range rows {
		if strings.TrimSpace(e.Unit) != "" {
			return e.Unit
		}
	}
	return ""
}
