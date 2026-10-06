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

const (
	// balanceRefreshBatch is how many accounts one gentle background pass asks
	// about. Explicit refreshes use all enabled accounts in the selected client.
	balanceRefreshBatch = 3
	// balanceRefreshGap spaces accounts inside one pass. Three calls back to
	// back still look like a burst to some vendors, so leave a gap between them.
	balanceRefreshGap = 2 * time.Second
	// balanceSoftInterval throttles the gentle refresh that every account-page
	// open performs.
	balanceSoftInterval = 45 * time.Second
	// balanceForceInterval throttles an explicit refresh button.
	balanceForceInterval = 5 * time.Second
	// balanceFetchedTTL is when a cached number is old enough to earn a
	// "data is stale" hint in the row tooltip.
	balanceFetchedTTL = 10 * time.Minute
)

// DefaultBalanceCacheFileName is the relative path under the data directory.
const DefaultBalanceCacheFileName = "panel/balance_cache.json"

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

type balanceProvider interface {
	All() []core.Client
}

// balanceRefreshStatus is the progress contract returned by GET and POST
// /balances* routes. The browser uses it to keep polling until the selected
// client's pass really finishes, and to explain a throttle instead of silently
// doing nothing.
type balanceRefreshStatus struct {
	Client       string `json:"client,omitempty"`
	Running      bool   `json:"running"`
	Total        int    `json:"total"`
	Done         int    `json:"done"`
	Succeeded    int    `json:"succeeded"`
	Failed       int    `json:"failed"`
	Revived      int    `json:"revived"`
	Remaining    int    `json:"remaining"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	Message      string `json:"message,omitempty"`
	StartedAt    int64  `json:"started_at,omitempty"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
}

type balanceClientRefresh struct {
	running    bool
	lastSoft   time.Time
	lastForce  time.Time
	total      int
	done       int
	succeeded  int
	failed     int
	revived    int
	startedAt  time.Time
	finishedAt time.Time
	message    string
}

type balanceCache struct {
	mu        sync.Mutex
	all       balanceProvider
	soon      func() time.Duration
	path      string
	clients   map[string]map[string]balanceEntry
	refreshes map[string]*balanceClientRefresh

	// Legacy fields keep the original global refresh() helper usable for
	// embedders and tests that call it directly. Panel routes use per-client
	// state and never let one client block another.
	lastSoft  time.Time
	lastForce time.Time
	inflight  bool
}

func newBalanceCache(all balanceProvider, soon func() time.Duration, path string) *balanceCache {
	c := &balanceCache{
		all:       all,
		soon:      soon,
		path:      strings.TrimSpace(path),
		clients:   map[string]map[string]balanceEntry{},
		refreshes: map[string]*balanceClientRefresh{},
	}
	c.load()
	return c
}

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

type refreshTarget struct {
	client  string
	id      string
	label   string
	state   string
	enabled bool
	fetched time.Time
}

// statusForClient returns the current progress for one client. It is safe to
// hand to an HTTP handler.
func (c *balanceCache) statusForClient(client string) balanceRefreshStatus {
	if c == nil {
		return balanceRefreshStatus{Client: client}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked(strings.TrimSpace(client))
}

func (c *balanceCache) statusLocked(client string) balanceRefreshStatus {
	st := c.refreshes[client]
	if st == nil {
		return balanceRefreshStatus{Client: client}
	}
	return balanceRefreshStatus{
		Client:     client,
		Running:    st.running,
		Total:      st.total,
		Done:       st.done,
		Succeeded:  st.succeeded,
		Failed:     st.failed,
		Revived:    st.revived,
		Remaining:  maxInt(st.total-st.done, 0),
		Message:    st.message,
		StartedAt:  unixMilli(st.startedAt),
		FinishedAt: unixMilli(st.finishedAt),
	}
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *balanceCache) refreshStateLocked(client string) *balanceClientRefresh {
	if c.refreshes == nil {
		c.refreshes = map[string]*balanceClientRefresh{}
	}
	st := c.refreshes[client]
	if st == nil {
		st = &balanceClientRefresh{}
		c.refreshes[client] = st
	}
	return st
}

// refreshClient starts one refresh pass for one client. limit <= 0 means all
// enabled accounts in that client; a positive limit keeps the gentle
// background sweep small. revive is true only for explicit refreshes, where a
// successful funded balance read is evidence that a cooling account can be
// brought back without waiting for the operator to press revive too.
func (c *balanceCache) refreshClient(ctx context.Context, client string, limit int, force, revive bool) (balanceRefreshStatus, bool) {
	if c == nil {
		return balanceRefreshStatus{Client: client}, false
	}
	client = strings.TrimSpace(client)
	targets := c.targetsForClient(client, limit)
	now := time.Now()

	c.mu.Lock()
	st := c.refreshStateLocked(client)
	gap := balanceSoftInterval
	last := st.lastSoft
	if force {
		gap = balanceForceInterval
		last = st.lastForce
	}
	// Existing tests and older embedders used these fields to suppress a pass.
	if legacy := c.lastSoft; !force && !legacy.IsZero() {
		last = legacy
	}
	if legacy := c.lastForce; force && !legacy.IsZero() {
		last = legacy
	}
	if st.running {
		snap := c.statusLocked(client)
		snap.Message = "正在刷新"
		c.mu.Unlock()
		return snap, false
	}
	if !last.IsZero() && now.Sub(last) < gap {
		snap := c.statusLocked(client)
		snap.RetryAfterMS = int64(gap-now.Sub(last)) / int64(time.Millisecond)
		if snap.RetryAfterMS < 1 {
			snap.RetryAfterMS = 1
		}
		snap.Message = "刷新太频繁，请稍后再试"
		c.mu.Unlock()
		return snap, false
	}

	st.running = true
	st.lastSoft = now
	if force {
		st.lastForce = now
	}
	st.total = len(targets)
	st.done = 0
	st.succeeded = 0
	st.failed = 0
	st.revived = 0
	st.startedAt = now
	st.finishedAt = time.Time{}
	st.message = ""
	if len(targets) == 0 {
		st.running = false
		st.finishedAt = now
		st.message = "没有可刷新的账号"
	}
	snap := c.statusLocked(client)
	c.mu.Unlock()

	if len(targets) > 0 {
		go c.runRefresh(ctx, client, targets, revive)
	}
	return snap, true
}

// refresh preserves the original global helper for tests and embedders. Panel
// routes use refreshClient so two clients cannot consume each other's slots.
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
	last := c.lastSoft
	if force {
		gap = balanceForceInterval
		last = c.lastForce
	}
	if !last.IsZero() && now.Sub(last) < gap {
		c.mu.Unlock()
		return false
	}
	c.inflight = true
	if force {
		c.lastForce = now
	} else {
		c.lastSoft = now
	}
	c.mu.Unlock()

	targets := c.targets(limit)
	if len(targets) > 0 {
		go func() {
			defer func() {
				c.mu.Lock()
				c.inflight = false
				c.mu.Unlock()
			}()
			c.runRefresh(ctx, "", targets, false)
		}()
	} else {
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}
	return true
}

func (c *balanceCache) runRefresh(ctx context.Context, client string, targets []refreshTarget, revive bool) {
	for i, tgt := range targets {
		if i > 0 {
			select {
			case <-time.After(balanceRefreshGap):
			case <-ctx.Done():
				c.finishRefresh(client, ctx.Err())
				return
			}
		}
		if ctx.Err() != nil {
			c.finishRefresh(client, ctx.Err())
			return
		}
		revived, err := c.refreshOne(ctx, tgt, revive)
		c.mu.Lock()
		st := c.refreshStateLocked(client)
		st.done++
		if err != nil {
			st.failed++
		} else {
			st.succeeded++
		}
		if revived {
			st.revived++
		}
		c.mu.Unlock()
	}
	c.finishRefresh(client, nil)
}

func (c *balanceCache) finishRefresh(client string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.refreshStateLocked(client)
	st.running = false
	st.finishedAt = time.Now()
	switch {
	case err != nil:
		st.message = "刷新已取消"
	case st.total == 0:
		st.message = "没有可刷新的账号"
	case st.failed > 0:
		st.message = "刷新完成，部分账号失败"
	default:
		st.message = "刷新完成"
	}
}

func (c *balanceCache) targets(limit int) []refreshTarget {
	return c.targetsForClient("", limit)
}

func (c *balanceCache) targetsForClient(client string, limit int) []refreshTarget {
	var out []refreshTarget
	client = strings.TrimSpace(client)
	for _, cand := range c.all.All() {
		if client != "" && cand.Name() != client {
			continue
		}
		_, ok := core.AsBalanceProvider(cand)
		if !ok {
			continue
		}
		am, ok := core.AsAccountManager(cand)
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
			e, known := c.entry(cand.Name(), rec.ID)
			fetched := time.Time{}
			if known && e.FetchedAt > 0 {
				fetched = time.UnixMilli(e.FetchedAt)
			}
			out = append(out, refreshTarget{
				client:  cand.Name(),
				id:      rec.ID,
				label:   rec.Label,
				state:   rec.State,
				enabled: rec.Enabled,
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

func (c *balanceCache) refreshOne(ctx context.Context, tgt refreshTarget, revive bool) (bool, error) {
	var client core.Client
	for _, cand := range c.all.All() {
		if cand.Name() == tgt.client {
			client = cand
			break
		}
	}
	if client == nil {
		return false, nil
	}
	bp, ok := core.AsBalanceProvider(client)
	if !ok {
		return false, nil
	}
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bal, err := bp.AccountBalance(bctx, tgt.id, c.soon())
	if err != nil {
		c.putError(tgt.client, tgt.id, err)
		return false, err
	}
	c.put(tgt.client, tgt.id, bal)

	if !revive || !balanceIsFunded(bal) || !balanceCanReviveState(tgt.state) {
		return false, nil
	}
	if !tgt.enabled {
		// 人工禁用的账号不能仅凭余额恢复，避免覆盖运营者的显式操作。
		return false, nil
	}
	rv, ok := core.AsReviver(client)
	if !ok {
		return false, nil
	}
	if err := rv.ReviveAccount(bctx, tgt.id); err != nil {
		return false, err
	}
	return true, nil
}

func balanceIsFunded(bal core.Balance) bool {
	return bal.Unlimited || bal.Credits > 0
}

func balanceCanReviveState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "cooling", "exhausted", "unknown", "low_credit", "quota_exceeded", "rate_limited":
		return true
	default:
		return false
	}
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

func unitOf(rows map[string]balanceEntry) string {
	for _, e := range rows {
		if strings.TrimSpace(e.Unit) != "" {
			return e.Unit
		}
	}
	return ""
}
