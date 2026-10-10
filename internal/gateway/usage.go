package gateway

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
)

// DefaultUsageRecords is how many completed chat requests the per-request
// journal behind Snapshot keeps.  The aggregate history is NOT bounded by this:
// it lives in buckets (see UsageStore).
const DefaultUsageRecords = 100

// DefaultRecentRecords is how many per-request records are written to the
// recent.json journal.
// The live page and the restart journal use the same small window.
const DefaultRecentRecords = 100

// DefaultRecentFileName is the per-request journal written next to usage.json.
const DefaultRecentFileName = "recent.json"

// Retention and persistence tuning.  The values match the reference panel
// implementation (workbuddy2api-panel internal/usage) so both dashboards
// describe the same history.
const (
	// HourlyKeep is how long a bucket stays at hour granularity.  Rollup folds
	// anything older into per-day buckets, which are kept forever, so the
	// bucket count stays bounded without losing the long-term trend.
	HourlyKeep = 90 * 24 * time.Hour
	// FlushInterval is the debounce interval of the background flush.
	FlushInterval = 30 * time.Second
	// MaxUsageBuckets is the hard cap on live buckets.  See SetMaxBuckets for
	// the overflow policy.
	MaxUsageBuckets = 400_000
	// DefaultUsageFileName is the file a persistent store writes inside the
	// data directory.  The path itself is always supplied by the caller.
	DefaultUsageFileName = "usage.json"
)

// Bucket scopes.  The reference folds local-time scopes (internal/usage/
// usage.go:38-42), and that is the behaviour an operator expects: a "day" in
// the dashboard ends at the operator's midnight, not at 08:00 local because the
// box happens to store UTC.  The wall-clock string is therefore local time, and
// scopeTime parses it back in the same location so a bucket round-trips to the
// instant it was written at.
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
	hourScope  = "h:"
	dayScope   = "d:"
)

// Stats holds the gateway's lifetime counters.  It is created by the caller and
// handed to both the gateway (which increments it) and the panel (which
// renders it), so /panel/api/overview reports exactly the numbers /v1/status
// reports.
type Stats struct {
	requests atomic.Int64
	failures atomic.Int64
}

// NewStats returns zeroed counters.
func NewStats() *Stats { return &Stats{} }

// Requests is the number of chat requests accepted since start.
func (s *Stats) Requests() int64 {
	if s == nil {
		return 0
	}
	return s.requests.Load()
}

// Failures is the number of chat requests that failed since start.
func (s *Stats) Failures() int64 {
	if s == nil {
		return 0
	}
	return s.failures.Load()
}

func (s *Stats) addRequest() { s.requests.Add(1) }
func (s *Stats) addFailure() { s.failures.Add(1) }

// UsageRecord is one completed chat request.  It is deliberately tiny: the
// panel aggregates it, nothing else reads it.
type UsageRecord struct {
	At        time.Time `json:"at"`
	StartedAt time.Time `json:"started_at,omitempty"` // optional; Record derives latency/TPS from it
	Client    string    `json:"client,omitempty"`
	Realm     string    `json:"realm,omitempty"`
	Account   string    `json:"account,omitempty"`
	// SessionID names the conversation that produced this call, when the
	// caller named one: the conversation id the gateway resolved, else the
	// option spellings the router accepts, else the turn id the caller handed
	// over.  It is deliberately not derived from message content and never
	// borrows the request's user field, so a row can only claim a session the
	// client actually named.  See sessionIDFor.
	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`
	// ReasoningEffort is the thinking level the caller asked for, in the
	// vocabulary the caller used (low/medium/high/xhigh/...).  It stays empty
	// when the request named none: the row reports what was sent rather than
	// the module's configured default.  See reasoningEffortFor.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Candidate       int    `json:"candidate,omitempty"` // 1-based position in a bare-model route
	Failed          bool   `json:"failed,omitempty"`
	// Attempt marks a candidate that failed before the request moved on.
	// It is a recent-list row, not an extra inbound request, so it must not
	// inflate the aggregate totals.
	Attempt          bool    `json:"attempt,omitempty"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	LatencyMs        int64   `json:"latency_ms,omitempty"`
	HasLatency       bool    `json:"has_latency,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second,omitempty"`
	HasTPS           bool    `json:"has_tps,omitempty"`
}

// setUsage copies the module-reported token counts onto the record.  It is
// nil-safe because a request that never reached the module reports no usage at
// all, and such a request still belongs in the store (as a failure).
func (rec *UsageRecord) setUsage(u *core.Usage) {
	if rec == nil || u == nil {
		return
	}
	rec.PromptTokens = u.PromptTokens
	rec.CompletionTokens = u.CompletionTokens
	rec.TotalTokens = u.TotalTokens
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
}

// ---------------------------------------------------------------------------
// Buckets
// ---------------------------------------------------------------------------

// sessionIDFor resolves the session id a usage row records.  The order mirrors
// what the router already treats as a conversation: the id the gateway
// resolved from metadata or the top level, then the option spellings
// (conversation_id / conversationId / prompt_cache_key), then the per-turn id
// the caller handed over in X-Conversation-Request-ID.  The request's user
// field is deliberately not consulted -- an end-user id is not a conversation,
// and writing it into a column called 会话ID would claim a session the caller
// never named.
//
// When the caller names nothing at all, fall back to the content-derived key
// instead of leaving the cell empty.  An empty cell was not a neutral "unknown":
// the same conversation showed up as some rows named and some not depending on
// which of the several entry points that turn came in through, so one conversation
// read as several.  DeriveConversationKey is the same key the router already uses
// for stickiness, so the id in this column and the conversation the gateway
// actually kept together cannot drift apart.  It carries core.DerivedKeyPrefix so
// a reader can still tell a derived id from one the caller sent, and a request
// with nothing signable to derive from still degrades to empty rather than
// inventing a session.
func sessionIDFor(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		return id
	}
	if id := core.ConversationKey(req.Options, ""); id != "" {
		return id
	}
	if id := strings.TrimSpace(req.ConversationRequestID); id != "" {
		return id
	}
	return derivedSessionID(req)
}

// derivedSessionID is the last resort of sessionIDFor: the caller's request
// names no session anywhere, so the conversation is identified by what it said.
// It is a thin wrapper so the derivation rule lives in exactly one place: core,
// where the stickiness key is derived.  Re-deriving it here would let the usage
// journal and the router disagree about which calls belong to one conversation.
func derivedSessionID(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return core.DeriveConversationKey(req.Messages)
}

// usageBucket is one (time scope, client, realm, account, model) accumulator.
// The JSON tags are short because the number of buckets grows with time.
type usageBucket struct {
	Scope   string  `json:"s"` // "h:2006-01-02T15" (hour) or "d:2006-01-02" (day)
	Client  string  `json:"c"`
	Realm   string  `json:"r"`
	Account string  `json:"a"`
	Model   string  `json:"m"`
	Req     int64   `json:"q"`  // requests, failures included
	Err     int64   `json:"e"`  // failed attempts
	PT      int64   `json:"p"`  // prompt tokens
	CT      int64   `json:"ct"` // completion tokens
	TT      int64   `json:"t"`  // total tokens
	LatMs   int64   `json:"l"`  // latency sum, milliseconds
	LatN    int64   `json:"ln"` // latency samples
	TPS     float64 `json:"v"`  // tokens-per-second sum
	TPSN    int64   `json:"vn"` // tokens-per-second samples
}

func (b *usageBucket) key() string {
	return bucketKey(b.Scope, b.Client, b.Realm, b.Account, b.Model)
}

// bucketKey joins the identity of a bucket.  The key is only ever used as a
// map key — nothing parses it back — so a "|" inside a field cannot corrupt
// anything.
func bucketKey(scope, client, realm, account, model string) string {
	return scope + "|" + client + "|" + realm + "|" + account + "|" + model
}

// mergeFrom folds src into b.  Both buckets must already share a scope.
func (b *usageBucket) mergeFrom(src *usageBucket) {
	if b == nil || src == nil || b == src {
		return
	}
	b.Req += src.Req
	b.Err += src.Err
	b.PT += src.PT
	b.CT += src.CT
	b.TT += src.TT
	b.LatMs += src.LatMs
	b.LatN += src.LatN
	b.TPS += src.TPS
	b.TPSN += src.TPSN
}

func hourScopeOf(t time.Time) string { return hourScope + t.Local().Format(hourLayout) }
func dayScopeOf(t time.Time) string  { return dayScope + t.Local().Format(dayLayout) }

// scopeTime parses a bucket scope back into its start instant.  An unparseable
// scope yields the zero time, which sorts first and is therefore the first
// thing a capacity overflow drops.
func scopeTime(scope string) time.Time {
	if rest, ok := strings.CutPrefix(scope, hourScope); ok {
		if ts, err := time.ParseInLocation(hourLayout, rest, time.Local); err == nil {
			return ts
		}
		return time.Time{}
	}
	if rest, ok := strings.CutPrefix(scope, dayScope); ok {
		if ts, err := time.ParseInLocation(dayLayout, rest, time.Local); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// UsageStore records gateway traffic twice over, on purpose:
//
//   - Buckets aggregate every request by (hour, client, realm, account, model).
//     They are the durable record: they are flushed to disk, survive a restart,
//     and roll up from hour to day granularity so the long-term totals stay
//     available without unbounded growth.
//   - A bounded ring keeps the most recent DefaultUsageRecords *requests*, which
//     is what Snapshot serves (one record per chat request, with its outcome).
//     The ring is a window onto the newest traffic, not a second source of
//     truth: aggregate views are computed from the buckets.
//
// A store with an empty path is purely in memory (tests, and the default when
// the process was started without a data directory).
type UsageStore struct {
	max        int
	maxBuckets int
	path       string

	mu      sync.Mutex
	recs    []UsageRecord // ring storage, exactly max long
	start   int           // index of the oldest retained record
	count   int           // retained records (<= max)
	total   int64         // requests recorded since start, evicted included
	buckets map[string]*usageBucket
	// lastOK remembers the newest successful request per account id.  Buckets
	// are hour-granular, so they can say "this account succeeded during this
	// hour" but never "at 14:03:07"; the account table wants the latter.  Like
	// the ring, it is in-memory only and bounded by the number of accounts.
	lastOK  map[string]time.Time
	dirty   bool  // buckets changed since the last successful flush
	evicted int64 // buckets dropped by the capacity policy
	lastErr error // most recent persistence error
	logger  UsageLogger
	// lastRollup throttles the age-based rollup check to at most once an hour.
	lastRollup time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
}

// NewUsageStore returns an in-memory store retaining at most max records in its
// per-request ring.  A non-positive max selects DefaultUsageRecords.  Use
// NewPersistentUsageStore when the history must survive a restart.
func NewUsageStore(max int) *UsageStore { return newUsageStore(max, "") }

func newUsageStore(max int, path string) *UsageStore {
	if max <= 0 {
		max = DefaultUsageRecords
	}
	return &UsageStore{
		max:        max,
		maxBuckets: MaxUsageBuckets,
		path:       path,
		recs:       make([]UsageRecord, max),
		buckets:    make(map[string]*usageBucket),
		lastOK:     make(map[string]time.Time),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Add records one request attempt by its full identity.
//
// now selects the hour bucket the attempt lands in.  client, realm, account and
// model are stored verbatim: the store invents no label, so the panel can
// render its own placeholder for a request that was never routed.
//
// ok=false marks the attempt as failed (transport error, upstream >= 400,
// unparseable body).  A failed attempt usually carries no token counts but is
// still counted in Requests — retry amplification is only visible that way.
func (s *UsageStore) Add(now time.Time, client, realm, account, model string, d UsageDelta, ok bool) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Every recorded request counts, whether or not the ring kept a copy: this
	// is the process-lifetime request count, not the retained-window size.
	s.total++
	s.addLocked(now, client, realm, account, model, d, ok)
}

// UsageDelta is the per-attempt increment for one request.  A zero value with
// its Has* flag false means "the upstream did not report this", which is not
// the same as a reported zero: a missing measurement must never be averaged in
// as 0.
type UsageDelta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool
}

func (s *UsageStore) addLocked(now time.Time, client, realm, account, model string, d UsageDelta, ok bool) {
	scope := hourScopeOf(now)
	key := bucketKey(scope, client, realm, account, model)

	b := s.buckets[key]
	if b == nil {
		b = &usageBucket{Scope: scope, Client: client, Realm: realm, Account: account, Model: model}
		s.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	} else if account != "" {
		// Only a successful attempt moves the "last worked at" stamp, and only
		// when the gateway actually knew which account served it: an unrouted
		// failure must not invent an account, and a failure must not refresh a
		// success time.
		if prev, seen := s.lastOK[account]; !seen || now.After(prev) {
			s.lastOK[account] = now
		}
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// The upstream reported no total: fall back to pt+ct so the total
		// column stays continuous.
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}

	s.dirty = true
	// The cap is enforced against wall-clock time, never against the event
	// timestamp: a backdated or replayed request must not change how much
	// history the store folds away.
	s.enforceCapLocked()
}

// Record stores one completed chat request.  This is the shape the gateway has
// always produced, so it stays the entry point for the HTTP path; internally it
// is normalized into a UsageDelta and lands in a bucket.
//
// Latency and throughput are derived only from something the record actually
// carries: either an explicit HasLatency/HasTPS pair, or StartedAt.  With
// neither, the bucket receives no latency/TPS sample at all and the report
// omits those columns rather than reporting a fabricated 0.
func (s *UsageStore) Record(rec UsageRecord) {
	if s == nil {
		return
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}

	latencyMs, hasLatency := rec.LatencyMs, rec.HasLatency
	if !hasLatency && !rec.StartedAt.IsZero() && !at.Before(rec.StartedAt) {
		latencyMs = at.Sub(rec.StartedAt).Milliseconds()
		hasLatency = true
	}
	tps, hasTPS := rec.TokensPerSecond, rec.HasTPS
	if !hasTPS && hasLatency && latencyMs > 0 && rec.CompletionTokens > 0 {
		// A real measurement over the observed request duration (a lower bound
		// on pure decode speed), never an invented number.
		tps = float64(rec.CompletionTokens) * 1000 / float64(latencyMs)
		hasTPS = true
	}

	d := UsageDelta{
		PromptTokens:     int64(rec.PromptTokens),
		HasPromptTokens:  true,
		CompletionTokens: int64(rec.CompletionTokens),
		HasCompletion:    true,
		TotalTokens:      int64(rec.TotalTokens),
		HasTotal:         rec.TotalTokens != 0,
		LatencyMs:        latencyMs,
		HasLatency:       hasLatency,
		TokensPerSecond:  tps,
		HasTPS:           hasTPS,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec.At = at
	rec.LatencyMs = latencyMs
	rec.HasLatency = hasLatency
	rec.TokensPerSecond = tps
	rec.HasTPS = hasTPS
	s.appendLocked(rec)
	s.addLocked(at, rec.Client, rec.Realm, rec.Account, rec.Model, d, !rec.Failed)
}

// RecordAttempt stores one failed candidate attempt in the recent-call ring
// without touching the aggregate buckets.  The inbound request is counted once
// by the final Record; this method exists so failover is visible in the
// per-request view without turning one caller request into several.
func (s *UsageStore) RecordAttempt(rec UsageRecord) {
	if s == nil {
		return
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	rec.Attempt = true
	rec.Failed = true
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == s.max {
		s.recs[s.start] = rec
		s.start = (s.start + 1) % s.max
	} else {
		s.recs[(s.start+s.count)%s.max] = rec
		s.count++
	}
}

// appendLocked adds one record to the bounded recent-request ring.
func (s *UsageStore) appendLocked(rec UsageRecord) {
	if s.count == s.max {
		s.recs[s.start] = rec
		s.start = (s.start + 1) % s.max
	} else {
		s.recs[(s.start+s.count)%s.max] = rec
		s.count++
	}
	s.total++
}

// Snapshot returns the retained recent requests, oldest first.
func (s *UsageStore) Snapshot() []UsageRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]UsageRecord, 0, s.count)
	for i := 0; i < s.count; i++ {
		out = append(out, s.recs[(s.start+i)%s.max])
	}
	return out
}

// Total is the number of requests recorded since start, including requests
// already evicted from the ring.
func (s *UsageStore) Total() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// Capacity is the maximum number of requests the ring retains.
func (s *UsageStore) Capacity() int {
	if s == nil {
		return 0
	}
	return s.max
}

// Buckets is the number of live aggregation buckets.
func (s *UsageStore) Buckets() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}

// Evicted is the number of buckets dropped because the store was over
// MaxUsageBuckets and Rollup could not free enough.
func (s *UsageStore) Evicted() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

// MaxBuckets is the current bucket cap.
func (s *UsageStore) MaxBuckets() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxBuckets
}

// SetMaxBuckets changes the bucket cap.  A non-positive n restores
// MaxUsageBuckets.  An existing oversized store is trimmed on the next Add, or
// immediately by calling Rollup.
func (s *UsageStore) SetMaxBuckets(n int) {
	if s == nil {
		return
	}
	if n <= 0 {
		n = MaxUsageBuckets
	}
	s.mu.Lock()
	s.maxBuckets = n
	s.enforceCapLocked()
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Rollup and the bucket cap
// ---------------------------------------------------------------------------

// Rollup folds hour buckets older than HourlyKeep into day buckets (by UTC
// calendar day).  It is idempotent: folding the same hour twice cannot double
// count, because the source bucket is deleted after its counters are merged.
func (s *UsageStore) Rollup(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollupLocked(now)
}

// rollupLocked is Rollup with the store lock already held.  It returns how many
// hour buckets were folded.
func (s *UsageStore) rollupLocked(now time.Time) int {
	cutoff := now.Add(-HourlyKeep)
	if now.IsZero() {
		cutoff = time.Now().Add(-HourlyKeep)
	}

	type move struct{ from, to string }
	moves := make([]move, 0, 8)
	for k, b := range s.buckets {
		if b == nil || !strings.HasPrefix(b.Scope, hourScope) {
			continue
		}
		ts := scopeTime(b.Scope)
		if ts.IsZero() || !ts.Before(cutoff) {
			continue
		}
		to := bucketKey(dayScopeOf(ts), b.Client, b.Realm, b.Account, b.Model)
		moves = append(moves, move{from: k, to: to})
	}
	for _, m := range moves {
		src := s.buckets[m.from]
		if src == nil {
			continue
		}
		if dst := s.buckets[m.to]; dst != nil {
			dst.mergeFrom(src)
		} else {
			cp := *src
			cp.Scope, _, _ = strings.Cut(m.to, "|")
			dst := cp
			s.buckets[m.to] = &dst
		}
		delete(s.buckets, m.from)
	}
	if len(moves) > 0 {
		s.dirty = true
	}
	return len(moves)
}

// enforceCapLocked implements the overflow policy: fold hour buckets into day
// buckets first, and only when that is not enough drop the oldest buckets until
// the cap is met.  Dropping is counted in Evicted so a lossy trim is never
// silent.
//
// Both decisions are made against wall-clock time, not against the timestamp of
// whatever request happened to trigger them: a backdated or replayed request
// must not change how much history is retained.  The lock must be held.
func (s *UsageStore) enforceCapLocked() {
	if len(s.buckets) <= s.maxBuckets {
		return
	}
	s.rollupLocked(time.Now())
	if len(s.buckets) <= s.maxBuckets {
		return
	}

	type aged struct {
		key string
		ts  time.Time
	}
	all := make([]aged, 0, len(s.buckets))
	for k, b := range s.buckets {
		all = append(all, aged{key: k, ts: scopeTime(b.Scope)})
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].ts.Equal(all[j].ts) {
			return all[i].ts.Before(all[j].ts)
		}
		return all[i].key < all[j].key
	})
	for i := 0; i < len(all) && len(s.buckets) > s.maxBuckets; i++ {
		delete(s.buckets, all[i].key)
		s.evicted++
	}
	s.dirty = true
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// UsageTotals is one aggregated bucket of traffic.
//
// The two averages are pointers on purpose: a nil average means "no sample
// taken", and omitempty keeps it out of the JSON entirely.  0 would be a lie,
// because an unreported latency and a measured 0 ms are different facts.
type UsageTotals struct {
	Requests           int64    `json:"requests"`
	Failures           int64    `json:"failures"`
	PromptTokens       int64    `json:"prompt_tokens"`
	CompletionTokens   int64    `json:"completion_tokens"`
	TotalTokens        int64    `json:"total_tokens"`
	AvgLatencyMs       *float64 `json:"avg_latency_ms,omitempty"`
	AvgTokensPerSecond *float64 `json:"avg_tokens_per_second,omitempty"`
}

// UsageBucket is one named row of the traffic breakdown.
type UsageBucket struct {
	Name  string `json:"name"`
	Realm string `json:"realm,omitempty"`
	// Extra carries a display-only label; for an account row the panel puts
	// the account's nickname here.
	Extra string `json:"extra,omitempty"`
	// LastSuccess is the newest successful request served by this name, as
	// RFC3339.  Only account rows fill it: the account table wants "when did
	// this credential last work", which no token count can express.  It is
	// process-lifetime and in memory, so it resets on restart — the same
	// lifetime as the health state it sits next to.
	LastSuccess string `json:"last_success,omitempty"`
	UsageTotals
}

// UsagePoint is one time-slice of the series.
type UsagePoint struct {
	T                string `json:"t"`
	Scope            string `json:"scope,omitempty"` // "hour" | "day"
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Requests         int64  `json:"requests"`
	Failures         int64  `json:"failures,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
}

// UsageReport is the whole /panel/api/usage payload.
//
// The by_* lists and totals all describe the SAME window: switching the window
// changes every number, which is what the UI implies.  Series points are hour
// slices inside the window plus the day slices Rollup produced from older
// traffic.
type UsageReport struct {
	WindowHours int           `json:"window_hours"`
	Totals      UsageTotals   `json:"totals"`
	ByClient    []UsageBucket `json:"by_client"`
	ByRealm     []UsageBucket `json:"by_realm"`
	ByAccount   []UsageBucket `json:"by_account"`
	ByModel     []UsageBucket `json:"by_model"`
	Series      []UsagePoint  `json:"series"`
	// Recent is the per-request journal, newest first.  It is deliberately
	// outside the bucket aggregation so the usage page can answer "which
	// platform and account served this call?" without changing the long-term
	// totals.
	Recent []UsageRecord `json:"recent"`
	// Buckets is how many buckets the window matched, not the store's total.
	Buckets   int    `json:"buckets"`
	Since     string `json:"since,omitempty"`
	Generated string `json:"generated"`
	FileBytes int64  `json:"file_bytes"`
}

// UsageReport aggregates the bucket history into the panel's traffic view.
//
// windowHours bounds the history: 0 means "everything retained, including the
// day buckets".  A positive window is hour-aligned (the current hour counts as
// the last of the N), matching the reference panel.  Rows are sorted by name so
// the legacy view never reorders under a client's feet; every list is non-nil
// so the JSON carries [] rather than null.
func (s *UsageStore) UsageReport(windowHours int) UsageReport {
	return s.report(windowHours, nil, false)
}

// UsageSnapshot is UsageReport for the reference-shaped dashboard: accounts get
// their nickname through nicks (account id -> display name) and rows are sorted
// heaviest-first instead of by name.  Everything else, including the window
// semantics, is identical.
func (s *UsageStore) UsageSnapshot(windowHours int, nicks map[string]string) UsageReport {
	return s.report(windowHours, nicks, true)
}

func (s *UsageStore) report(windowHours int, nicks map[string]string, byWeight bool) UsageReport {
	rep := UsageReport{
		WindowHours: windowHours,
		ByClient:    []UsageBucket{},
		ByRealm:     []UsageBucket{},
		ByAccount:   []UsageBucket{},
		ByModel:     []UsageBucket{},
		Series:      []UsagePoint{},
		Generated:   time.Now().UTC().Format(time.RFC3339),
	}
	if s == nil {
		return rep
	}

	var from time.Time
	if windowHours > 0 {
		from = time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(windowHours-1) * time.Hour)
	}

	s.mu.Lock()
	bs := make([]usageBucket, 0, len(s.buckets))
	since := ""
	for _, b := range s.buckets {
		if b == nil {
			continue
		}
		bs = append(bs, *b)
		if since == "" || b.Scope < since {
			since = b.Scope
		}
	}
	// Copy the success stamps under the same lock: the account rows are built
	// outside it, and a concurrent Add must not be able to tear the map.
	lastOK := make(map[string]time.Time, len(s.lastOK))
	for id, ts := range s.lastOK {
		lastOK[id] = ts
	}
	path := s.path
	s.mu.Unlock()

	total := &usageAgg{}
	byClient := map[string]*usageAgg{}
	byRealm := map[string]*usageAgg{}
	byAccount := map[string]*usageAgg{}
	accountRealm := map[string]string{}
	byModel := map[string]*usageAgg{}
	hourSeries := map[string]*usageAgg{}
	daySeries := map[string]*usageAgg{}

	for i := range bs {
		b := &bs[i]
		if !from.IsZero() {
			ts := scopeTime(b.Scope)
			// An unparseable bucket belongs to no window (and to no series).
			if ts.IsZero() || ts.Before(from) {
				continue
			}
		}
		rep.Buckets++
		total.add(b)
		usageGroup(byClient, b.Client).add(b)
		usageGroup(byRealm, b.Realm).add(b)
		usageGroup(byAccount, b.Account).add(b)
		if accountRealm[b.Account] == "" {
			accountRealm[b.Account] = b.Realm
		}
		usageGroup(byModel, b.Model).add(b)

		if strings.HasPrefix(b.Scope, hourScope) {
			h, _ := strings.CutPrefix(b.Scope, hourScope)
			usageGroup(hourSeries, h).add(b)
		} else {
			d, _ := strings.CutPrefix(b.Scope, dayScope)
			usageGroup(daySeries, d).add(b)
		}
	}

	rep.Totals = total.finish()
	rep.ByClient = usageRows(byClient, nil, byWeight)
	rep.ByRealm = usageRows(byRealm, nil, byWeight)
	rep.ByAccount = usageRows(byAccount, nicks, byWeight)
	for i := range rep.ByAccount {
		name := rep.ByAccount[i].Name
		rep.ByAccount[i].Realm = accountRealm[name]
		// The stamp is not windowed on purpose: "last succeeded" is a property
		// of the credential, not of the window the numbers above describe.  A
		// narrower window would blank it for an account that simply has not
		// been used lately, which reads as "never worked".
		if ts, ok := lastOK[name]; ok && !ts.IsZero() {
			rep.ByAccount[i].LastSuccess = ts.UTC().Format(time.RFC3339)
		}
	}
	for i := range rep.ByRealm {
		// The realm column of a realm row is the realm itself; the frontend
		// renders both tables with one row shape.
		rep.ByRealm[i].Realm = rep.ByRealm[i].Name
	}
	rep.ByModel = usageRows(byModel, nil, byWeight)

	// Day slices first, then hour slices: a day bucket can only be older than
	// the hour buckets (Rollup folds 90-day-old hours into days), so the series
	// stays chronological while remaining strictly "old, coarse" then "new, fine".
	rep.Series = append(rep.Series, seriesPoints(daySeries, dayScope, "day")...)
	rep.Series = append(rep.Series, seriesPoints(hourSeries, hourScope, "hour")...)

	if since != "" {
		if ts := scopeTime(since); !ts.IsZero() {
			rep.Since = ts.Format(time.RFC3339)
		}
	}
	rep.FileBytes = usageFileSize(path)
	return rep
}

// usageAgg is the aggregation accumulator: UsageTotals holds finished values,
// while the samples needed to weight the averages correctly live here.
type usageAgg struct {
	UsageTotals
	latSum int64
	latN   int64
	tpsSum float64
	tpsN   int64
}

func (a *usageAgg) add(b *usageBucket) {
	a.Requests += b.Req
	a.Failures += b.Err
	a.PromptTokens += b.PT
	a.CompletionTokens += b.CT
	a.TotalTokens += b.TT
	a.latSum += b.LatMs
	a.latN += b.LatN
	a.tpsSum += b.TPS
	a.tpsN += b.TPSN
}

func (a *usageAgg) finish() UsageTotals {
	t := a.UsageTotals
	if a.latN > 0 {
		v := float64(a.latSum) / float64(a.latN)
		t.AvgLatencyMs = &v
	}
	if a.tpsN > 0 {
		v := a.tpsSum / float64(a.tpsN)
		t.AvgTokensPerSecond = &v
	}
	return t
}

func usageGroup(m map[string]*usageAgg, key string) *usageAgg {
	a, ok := m[key]
	if !ok {
		a = &usageAgg{}
		m[key] = a
	}
	return a
}

// usageRows turns one grouping map into the row list the frontend renders.
// nicks, when non-nil, supplies display labels for the account grouping.
func usageRows(m map[string]*usageAgg, nicks map[string]string, byWeight bool) []UsageBucket {
	out := make([]UsageBucket, 0, len(m))
	for key, a := range m {
		out = append(out, UsageBucket{Name: key, Extra: nicks[key], UsageTotals: a.finish()})
	}
	sort.Slice(out, func(i, j int) bool {
		if byWeight {
			if out[i].TotalTokens != out[j].TotalTokens {
				return out[i].TotalTokens > out[j].TotalTokens
			}
			if out[i].Requests != out[j].Requests {
				return out[i].Requests > out[j].Requests
			}
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// seriesPoints renders one scope's time slices in chronological order.
func seriesPoints(m map[string]*usageAgg, prefix, scope string) []UsagePoint {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]UsagePoint, 0, len(keys))
	for _, k := range keys {
		t := scopeTime(prefix + k)
		if t.IsZero() {
			continue
		}
		agg := m[k].finish()
		out = append(out, UsagePoint{
			T:                t.Format(time.RFC3339),
			Scope:            scope,
			PromptTokens:     agg.PromptTokens,
			CompletionTokens: agg.CompletionTokens,
			Requests:         agg.Requests,
			Failures:         agg.Failures,
			TotalTokens:      agg.TotalTokens,
		})
	}
	return out
}
