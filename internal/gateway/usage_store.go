package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// usageFileVersion is the on-disk schema version.
const usageFileVersion = 1

// usagePricingVersion is bumped when a published price change makes historical
// bucket costs stale. The migration is one-way and is persisted in usage.json.
const usagePricingVersion = 2

const recentFileVersion = 1

// recentFile is the bounded per-request journal.  It is separate from
// usage.json so the 30s flush rewrites only the recent-call window.
type recentFile struct {
	Version int           `json:"version"`
	Saved   time.Time     `json:"saved"`
	Records []UsageRecord `json:"records"`
}

func recentPathFor(usagePath string) string {
	if strings.TrimSpace(usagePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(usagePath), DefaultRecentFileName)
}

// usageFile is the aggregate JSON document written to usage.json.  Recent
// requests use a separate bounded recent.json so each flush rewrites at most
// DefaultRecentRecords entries instead of the full in-memory ring.
type usageFile struct {
	Version        int           `json:"version"`
	PricingVersion int           `json:"pricing_version,omitempty"`
	Saved          time.Time     `json:"saved"`
	Buckets        []usageBucket `json:"buckets"`
	// LastOK is the per-account last-success stamp.  It is stored next to the
	// buckets because it cannot be derived from them: a bucket is hour
	// granular, so it can say "a1 succeeded during this hour" but never
	// "at 09:00:00".  The key is optional — a file written before the account
	// view existed simply does not have it, and absent means "no account has
	// been seen to succeed yet".
	LastOK map[string]time.Time `json:"last_ok,omitempty"`
}

// UsageLogger receives non-fatal persistence errors.  A failed flush must never
// take the gateway down, but it must not be silent either.  A nil logger
// discards them; LastError still reports the most recent one.
type UsageLogger func(msg string, err error)

// NewPersistentUsageStore returns an in-memory store that also keeps its bucket
// history in path.
//
// The path is supplied by the caller — the config layer owns data_dir, and this
// package must not go looking for it.  An empty path degrades to a purely
// in-memory store, which is what happens when the process has no data
// directory.  Call Start to enable the 30s background flush (and the initial
// load), or Load/Save explicitly.
//
// Expected path for this application: filepath.Join(cfg.DataDir, gateway.
// DefaultUsageFileName), i.e. data/usage.json.
func NewPersistentUsageStore(max int, path string) *UsageStore {
	s := newUsageStore(max, strings.TrimSpace(path))
	return s
}

// Path is the file the store persists to, or "" when it is memory-only.
func (s *UsageStore) Path() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// SetPath points the store at a different file.  A non-empty path is loaded
// immediately (missing file is not an error: a first run simply has no
// history).  SetPath("") makes the store memory-only again without discarding
// the buckets already in memory; an explicit Save is still performed against
// the old path by the caller if it wants one.
func (s *UsageStore) SetPath(path string) error {
	if s == nil {
		return nil
	}
	path = strings.TrimSpace(path)

	s.mu.Lock()
	s.path = path
	s.mu.Unlock()

	if path == "" {
		return nil
	}
	return s.Load()
}

// LastError is the most recent persistence error (flush or load), or nil.
func (s *UsageStore) LastError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// SetLogger installs the sink for non-fatal persistence errors.
func (s *UsageStore) SetLogger(fn UsageLogger) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.logger = fn
	s.mu.Unlock()
}

// reportError records err and hands it to the logger, if any.
func (s *UsageStore) reportError(msg string, err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastErr = err
	fn := s.logger
	s.mu.Unlock()

	if fn != nil {
		fn(msg, err)
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// Load reads the persisted buckets, replacing the in-memory ones.  Missing file
// is not an error — the first run of a fresh install has no history yet.
func (s *UsageStore) Load() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	return s.load(path)
}

func (s *UsageStore) load(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.reportError("usage load "+path, err)
		return err
	}

	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		err = fmt.Errorf("usage file %s: %w", path, err)
		s.reportError("usage load", err)
		return err
	}
	migrated := s.migratePricing(&f)

	buckets := make(map[string]*usageBucket, len(f.Buckets))
	for i := range f.Buckets {
		b := f.Buckets[i]
		if b.Scope == "" {
			// A bucket without a scope cannot be placed in time; a corrupt
			// file must not be able to invent history.
			continue
		}
		key := b.key()
		if cur := buckets[key]; cur != nil {
			cur.mergeFrom(&b)
			continue
		}
		cp := b
		buckets[key] = &cp
	}

	s.mu.Lock()
	s.buckets = buckets
	// The document is the truth for this path, so the stamps are replaced
	// rather than merged: a reload must not keep a success that the file no
	// longer records.  An older file has no last_ok key at all, which decodes
	// to nil and means "nothing has succeeded yet".
	s.lastOK = f.LastOK
	if s.lastOK == nil {
		s.lastOK = make(map[string]time.Time)
	}
	s.dirty = false
	s.lastErr = nil
	if migrated {
		s.dirty = true
	}
	s.enforceCapLocked()
	s.mu.Unlock()
	if err := s.loadRecent(path); err != nil {
		return err
	}
	return nil
}

// migratePricing repairs historical bucket costs after a published price
// correction. Only hourly buckets are eligible: a day bucket has lost the time
// needed to choose DeepSeek's peak or off-peak rate, so inventing one would be
// worse than leaving its existing estimate alone. The returned flag makes the
// repaired document flush on the next Save/tick.
func (s *UsageStore) migratePricing(f *usageFile) bool {
	if f == nil || f.PricingVersion >= usagePricingVersion {
		return false
	}
	changed := false
	for i := range f.Buckets {
		b := &f.Buckets[i]
		if !strings.HasPrefix(b.Scope, hourScope) || b.Model == "" {
			continue
		}
		client, model := splitBucketModel(b)
		if !modelmeta.IsOfficialDeepSeekModel(client, model) {
			continue
		}
		previous, ok := modelmeta.DeepSeekPreviousPrice(client, model)
		if !ok {
			continue
		}
		prompt := b.PT
		cached := b.Cached
		if cached > prompt {
			cached = prompt
		}
		if !b.HasCost || math.Abs(b.Cost-gatewayCost(previous, prompt-cached, cached, b.CT)) > 1e-9 {
			// A bucket whose stored total does not match the old built-in
			// aggregate was priced another way (most importantly, a manual
			// override) and must stay untouched.
			continue
		}
		price, ok := modelmeta.DefaultPriceAt(client, model, scopeTime(b.Scope))
		if !ok {
			continue
		}
		cost := gatewayCost(price, prompt-cached, cached, b.CT)
		if !b.HasCost || b.Cost != cost {
			b.Cost = cost
			b.HasCost = true
			changed = true
		}
	}
	if f.PricingVersion < usagePricingVersion {
		f.PricingVersion = usagePricingVersion
		changed = true
	}
	return changed
}

// splitBucketModel accepts both the canonical client/model value and a bare
// model id. The bucket's Client column is authoritative when the two disagree.
func splitBucketModel(b *usageBucket) (string, string) {
	if b == nil {
		return "", ""
	}
	client, model := b.Client, b.Model
	if client == "" {
		if prefix, rest, ok := strings.Cut(model, "/"); ok {
			client, model = prefix, rest
		}
	}
	return client, model
}

func gatewayCost(price modelmeta.Price, uncached, cached, completion int64) float64 {
	cacheRate := price.InputPerMillion
	if price.HasCacheRead {
		cacheRate = price.CacheReadPerMillion
	}
	return (float64(uncached)*price.InputPerMillion +
		float64(cached)*cacheRate +
		float64(completion)*price.OutputPerMillion) / 1_000_000
}

func (s *UsageStore) loadRecent(usagePath string) error {
	path := recentPathFor(usagePath)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		s.reportError("recent load "+path, err)
		return err
	}
	var f recentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		err = fmt.Errorf("recent file %s: %w", path, err)
		s.reportError("recent load", err)
		return err
	}
	records := f.Records
	if len(records) > s.max {
		records = records[len(records)-s.max:]
	}
	s.mu.Lock()
	s.start, s.count = 0, len(records)
	s.recs = make([]UsageRecord, s.max)
	copy(s.recs, records)
	s.mu.Unlock()
	return nil
}

// Save writes the bucket history to the configured path, atomically.  It is the
// explicit counterpart of the background flush, and does nothing when there is
// neither a path nor anything new to write.
func (s *UsageStore) Save() error { return s.flush(true) }

// flush writes the bucket history when the store is dirty (or force is set).
// The write is atomic: the document goes to a temporary file next to the
// target, is flushed and closed, and only then is renamed over the target.  A
// reader therefore sees either the previous complete file or the new complete
// file, never a half-written one.
func (s *UsageStore) flush(force bool) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	path := s.path
	if path == "" {
		s.mu.Unlock()
		return nil
	}
	if !force && !s.dirty {
		s.mu.Unlock()
		return nil
	}
	f := usageFile{
		Version:        usageFileVersion,
		PricingVersion: usagePricingVersion,
		Saved:          time.Now().UTC(),
		Buckets:        make([]usageBucket, 0, len(s.buckets)),
	}
	now := time.Now().UTC()
	rf := recentFile{Version: recentFileVersion, Saved: now}
	keep := s.count
	if keep > DefaultRecentRecords {
		keep = DefaultRecentRecords
	}
	if keep > 0 {
		rf.Records = make([]UsageRecord, 0, keep)
		for i := s.count - keep; i < s.count; i++ {
			rf.Records = append(rf.Records, s.recs[(s.start+i)%s.max])
		}
	}
	for _, b := range s.buckets {
		if b != nil {
			f.Buckets = append(f.Buckets, *b)
		}
	}
	if len(s.lastOK) > 0 {
		f.LastOK = make(map[string]time.Time, len(s.lastOK))
		for id, ts := range s.lastOK {
			f.LastOK[id] = ts
		}
	}
	s.mu.Unlock()

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		s.reportError("usage encode", err)
		return err
	}
	raw = append(raw, '\n')

	// core.WriteFileAtomic performs the same temp-file-then-rename dance this
	// used to hand-roll, with one important difference: every call gets its own
	// temp name (os.CreateTemp).  The hand-rolled version wrote each flush to
	// the single fixed name "path.tmp", so a panel Save arriving on top of a
	// background flush could interleave two writers into one temp file and then
	// rename the result -- publishing exactly the half-written document the
	// rename exists to prevent.  It also creates the parent directory itself.
	if err := core.WriteFileAtomic(path, raw); err != nil {
		s.reportError("usage write "+path, err)
		return err
	}

	if len(rf.Records) > 0 {
		rraw, err := json.MarshalIndent(rf, "", "  ")
		if err != nil {
			s.reportError("recent encode", err)
			return err
		}
		rraw = append(rraw, '\n')
		recentPath := recentPathFor(path)
		if err := core.WriteFileAtomic(recentPath, rraw); err != nil {
			s.reportError("recent write "+recentPath, err)
			return err
		}
	}
	s.mu.Lock()
	s.dirty = false
	s.lastErr = nil
	s.mu.Unlock()
	return nil
}

// usageFileSize is the size in bytes of the persisted history, 0 when there is
// none (or when it cannot be stat'ed).
func usageFileSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0
	}
	return fi.Size()
}

// ---------------------------------------------------------------------------
// Background lifecycle
// ---------------------------------------------------------------------------

// Start loads the persisted history and starts the 30s flush loop.  It is
// idempotent; call Stop (typically deferred from main) to stop it and perform
// the final flush.
func (s *UsageStore) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if err := s.Load(); err != nil {
			// Already reported; a store that cannot read its history still
			// serves live traffic.
			_ = err
		}
		s.started.Store(true)
		// GoSafe, not a bare "go": this loop outlives every request, so a
		// panic in it would otherwise take the whole gateway down and drop
		// whatever is in flight.  The trace lands in lastErr as well as the
		// log, because the panel is where an operator will look.
		core.GoSafe("usage flush loop", func(msg string) {
			s.reportError("usage flush loop", errors.New(msg))
		}, s.loop)
	})
}

func (s *UsageStore) loop() {
	defer close(s.done)

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

// tick performs one flush cycle: fold hour buckets that aged out (at most once
// an hour; the cap check in Add already handles the "buckets grew too fast"
// case), then persist whatever changed.
func (s *UsageStore) tick(now time.Time) {
	s.mu.Lock()
	due := s.lastRollup.IsZero() || now.Sub(s.lastRollup) >= time.Hour
	if due {
		s.lastRollup = now
	}
	over := len(s.buckets) > s.maxBuckets
	s.mu.Unlock()

	if due || over {
		s.Rollup(now)
	}
	if err := s.flush(false); err != nil {
		s.reportError("usage flush", err)
	}
}

// Stop ends the flush loop and performs a final flush.  It is idempotent and
// safe to call when Start never ran.  It never blocks on a caller-supplied
// timeout: the loop exits as soon as it observes the stop signal.
func (s *UsageStore) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
	if s.started.Load() {
		<-s.done
	}
	if err := s.Save(); err != nil {
		s.reportError("usage final save", err)
	}
}

// Describe is a one-line human summary for startup logs and /v1/status.
func (s *UsageStore) Describe() string {
	if s == nil {
		return "usage: disabled"
	}
	s.mu.Lock()
	buckets := len(s.buckets)
	evicted := s.evicted
	path := s.path
	total := s.total
	lastErr := s.lastErr
	since := ""
	for _, b := range s.buckets {
		if b == nil {
			continue
		}
		if since == "" || b.Scope < since {
			since = b.Scope
		}
	}
	s.mu.Unlock()

	where := "in-memory (no data directory)"
	if path != "" {
		where = fmt.Sprintf("%s, %d bytes", path, usageFileSize(path))
	}
	out := fmt.Sprintf("usage: %d buckets, %d requests, %d buckets evicted, %s", buckets, total, evicted, where)
	if ts := scopeTime(since); !ts.IsZero() {
		out += ", since " + ts.Format(time.RFC3339)
	} else {
		out += ", no history yet"
	}
	if lastErr != nil {
		out += ", last error: " + lastErr.Error()
	}
	return out
}
