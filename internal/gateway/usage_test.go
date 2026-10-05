package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestUsageStoreCapsRetainedRecords(t *testing.T) {
	s := NewUsageStore(3)
	for i := 0; i < 5; i++ {
		s.Record(UsageRecord{At: time.Now(), Client: "c", Model: "m", PromptTokens: i})
	}

	got := s.Snapshot()
	if len(got) != 3 {
		t.Fatalf("retained %d records, want 3", len(got))
	}
	if got[0].PromptTokens != 2 || got[2].PromptTokens != 4 {
		t.Errorf("oldest records were not evicted first: %+v", got)
	}
	if s.Total() != 5 {
		t.Errorf("Total() = %d, want 5", s.Total())
	}
	if s.Capacity() != 3 {
		t.Errorf("Capacity() = %d, want 3", s.Capacity())
	}
}

func TestUsageStoreDefaultCapacity(t *testing.T) {
	if got := NewUsageStore(0).Capacity(); got != DefaultUsageRecords {
		t.Errorf("NewUsageStore(0).Capacity() = %d, want %d", got, DefaultUsageRecords)
	}
}

func TestUsageStoreDefaultRecentLimit(t *testing.T) {
	s := NewUsageStore(0)
	if got := s.Capacity(); got != 100 {
		t.Fatalf("Capacity() = %d, want 100", got)
	}
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 150; i++ {
		s.Record(UsageRecord{At: base.Add(time.Duration(i) * time.Second), Client: "c", Model: "m", PromptTokens: i})
	}
	got := s.Snapshot()
	if len(got) != 100 {
		t.Fatalf("Snapshot() retained %d records, want 100", len(got))
	}
	if got[0].PromptTokens != 50 || got[99].PromptTokens != 149 {
		t.Fatalf("Snapshot() = first %d / last %d, want 50 / 149", got[0].PromptTokens, got[99].PromptTokens)
	}
}

func TestUsageRecordWritesDerivedTimingToSnapshot(t *testing.T) {
	s := NewUsageStore(4)
	started := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s.Record(UsageRecord{
		At:               started.Add(1500 * time.Millisecond),
		StartedAt:        started,
		Client:           "c",
		Model:            "m",
		CompletionTokens: 30,
		TotalTokens:      30,
	})

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() returned %d records, want 1", len(got))
	}
	rec := got[0]
	if !rec.HasLatency || rec.LatencyMs != 1500 {
		t.Fatalf("latency = has:%v value:%d, want has:true value:1500", rec.HasLatency, rec.LatencyMs)
	}
	if !rec.HasTPS || rec.TokensPerSecond != 20 {
		t.Fatalf("tps = has:%v value:%v, want has:true value:20", rec.HasTPS, rec.TokensPerSecond)
	}
}

// localHourStart and localDayStart name the instants the dashboard calls the
// start of the hour / day containing t.  Bucket scopes are local-time (see
// usage.go), so a test that wants to pin a series point has to express the
// expectation in the same terms rather than assuming UTC.
func localHourStart(t time.Time) time.Time {
	l := t.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, time.Local)
}

func localDayStart(t time.Time) time.Time {
	l := t.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local)
}

// seriesTime parses a report point's RFC3339 timestamp.
func seriesTime(t *testing.T, raw string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("series timestamp %q is not RFC3339: %v", raw, err)
	}
	return ts
}

func TestUsageReportBuckets(t *testing.T) {
	s := NewUsageStore(100)
	base := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	s.Record(UsageRecord{At: base, Client: "trae", Model: "trae/a", PromptTokens: 100, CompletionTokens: 40, TotalTokens: 140})
	s.Record(UsageRecord{At: base.Add(2 * time.Minute), Client: "trae", Model: "trae/a", PromptTokens: 200, CompletionTokens: 60, TotalTokens: 260})
	s.Record(UsageRecord{At: base.Add(90 * time.Minute), Client: "kimi", Model: "kimi/b", Failed: true})

	rep := s.UsageReport(0)

	if rep.Totals.Requests != 3 || rep.Totals.Failures != 1 {
		t.Errorf("totals = %+v, want 3 requests / 1 failure", rep.Totals)
	}
	if rep.Totals.PromptTokens != 300 || rep.Totals.CompletionTokens != 100 || rep.Totals.TotalTokens != 400 {
		t.Errorf("token totals = %+v, want 300/100/400", rep.Totals)
	}

	if len(rep.ByClient) != 2 {
		t.Fatalf("by_client has %d rows, want 2: %+v", len(rep.ByClient), rep.ByClient)
	}
	// sorted by name: kimi before trae
	if rep.ByClient[0].Name != "kimi" || rep.ByClient[1].Name != "trae" {
		t.Errorf("by_client not sorted by name: %+v", rep.ByClient)
	}
	if rep.ByClient[1].Requests != 2 || rep.ByClient[1].PromptTokens != 300 {
		t.Errorf("trae bucket wrong: %+v", rep.ByClient[1])
	}

	if len(rep.ByModel) != 2 {
		t.Fatalf("by_model has %d rows, want 2: %+v", len(rep.ByModel), rep.ByModel)
	}
	if rep.ByModel[0].Name != "kimi/b" {
		t.Errorf("by_model[0] = %q, want kimi/b", rep.ByModel[0].Name)
	}

	// Two hourly buckets: the hours containing base and base+90m.  Scopes are
	// local-time, so compare the instants rather than the rendered text.
	if len(rep.Series) != 2 {
		t.Fatalf("series has %d points, want 2: %+v", len(rep.Series), rep.Series)
	}
	want0 := localHourStart(base)
	want1 := localHourStart(base.Add(90 * time.Minute))
	got0 := seriesTime(t, rep.Series[0].T)
	got1 := seriesTime(t, rep.Series[1].T)
	if !got0.Equal(want0) || !got1.Equal(want1) {
		t.Errorf("series buckets are not hourly and sorted: %+v (want %s, %s)", rep.Series,
			want0.Format(time.RFC3339), want1.Format(time.RFC3339))
	}
	if rep.Series[0].Requests != 2 || rep.Series[0].PromptTokens != 300 || rep.Series[0].CompletionTokens != 100 {
		t.Errorf("first series point wrong: %+v", rep.Series[0])
	}
	if rep.Series[1].Requests != 1 {
		t.Errorf("second series point wrong: %+v", rep.Series[1])
	}
}

func TestUsageReportWindow(t *testing.T) {
	s := NewUsageStore(100)
	now := time.Now()
	s.Record(UsageRecord{At: now.Add(-100 * time.Hour), Client: "old", Model: "old/m"})
	s.Record(UsageRecord{At: now.Add(-1 * time.Hour), Client: "new", Model: "new/m"})

	rep := s.UsageReport(72)
	if rep.WindowHours != 72 {
		t.Errorf("WindowHours = %d, want 72", rep.WindowHours)
	}
	if rep.Totals.Requests != 1 {
		t.Errorf("window=72 counted %d requests, want 1: %+v", rep.Totals.Requests, rep.ByClient)
	}
	if len(rep.ByClient) != 1 || rep.ByClient[0].Name != "new" {
		t.Errorf("window=72 buckets = %+v, want only new", rep.ByClient)
	}

	all := s.UsageReport(0)
	if all.Totals.Requests != 2 {
		t.Errorf("window=0 counted %d requests, want all 2", all.Totals.Requests)
	}
}

// TestUsageReportEmptyListsAreArrays guards the JSON contract: the panel
// iterates these, so they must never serialise as null.
func TestUsageReportEmptyListsAreArrays(t *testing.T) {
	rep := NewUsageStore(4).UsageReport(72)
	if rep.ByClient == nil || rep.ByModel == nil || rep.Series == nil {
		t.Fatalf("empty report has nil lists: %+v", rep)
	}
}

func TestUsageRecordSetUsage(t *testing.T) {
	var rec UsageRecord
	rec.setUsage(nil)
	if rec.TotalTokens != 0 {
		t.Errorf("nil usage changed the record: %+v", rec)
	}

	rec.setUsage(&core.Usage{PromptTokens: 7, CompletionTokens: 3})
	if rec.PromptTokens != 7 || rec.CompletionTokens != 3 || rec.TotalTokens != 10 {
		t.Errorf("setUsage did not default total: %+v", rec)
	}

	rec.setUsage(&core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 99})
	if rec.TotalTokens != 99 {
		t.Errorf("setUsage overrode a reported total: %+v", rec)
	}
}

func TestStatsNilSafeAndCounters(t *testing.T) {
	var nilStats *Stats
	if nilStats.Requests() != 0 || nilStats.Failures() != 0 {
		t.Error("nil Stats must report zero")
	}

	s := NewStats()
	s.addRequest()
	s.addRequest()
	s.addFailure()
	if s.Requests() != 2 || s.Failures() != 1 {
		t.Errorf("counters = %d/%d, want 2/1", s.Requests(), s.Failures())
	}
}

// TestUsageStoreConcurrent exercises Record/Snapshot under -race.
func TestUsageStoreConcurrent(t *testing.T) {
	s := NewUsageStore(32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.Record(UsageRecord{At: time.Now(), Client: "c", Model: "c/m"})
		}
	}()
	for i := 0; i < 200; i++ {
		if got := len(s.Snapshot()); got > 32 {
			t.Fatalf("snapshot grew past capacity: %d", got)
		}
	}
	<-done
}

// ---------------------------------------------------------------------------
// Bucket aggregation
// ---------------------------------------------------------------------------

// TestUsageAddAggregatesByFullIdentity pins the bucketing key: repeats of the
// same identity accumulate, and every difference in realm/account/model/client
// splits into its own bucket.
func TestUsageAddAggregatesByFullIdentity(t *testing.T) {
	s := NewUsageStore(16)
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	d := UsageDelta{
		PromptTokens: 10, HasPromptTokens: true,
		CompletionTokens: 5, HasCompletion: true,
		TotalTokens: 15, HasTotal: true,
		LatencyMs: 100, HasLatency: true,
	}

	s.Add(at, "c", "r1", "a1", "c/m", d, true)
	s.Add(at.Add(time.Minute), "c", "r1", "a1", "c/m", d, true) // same key: accumulates
	s.Add(at, "c", "r2", "a2", "c/m", d, true)                  // another realm+account
	s.Add(at, "c", "r1", "a1", "c/m2", d, true)                 // another model
	s.Add(at, "c", "r1", "a1", "c/m", d, false)                 // failure, same key

	// Four identities, one hour.
	if got := s.Buckets(); got != 3 {
		t.Fatalf("Buckets() = %d, want 3", got)
	}

	rep := s.UsageReport(0)
	if rep.Totals.Requests != 5 || rep.Totals.Failures != 1 {
		t.Errorf("totals = %+v, want 5 requests / 1 failure", rep.Totals)
	}
	if rep.Totals.PromptTokens != 50 || rep.Totals.CompletionTokens != 25 || rep.Totals.TotalTokens != 75 {
		t.Errorf("token totals = %+v, want 50/25/75", rep.Totals)
	}
	if rep.Buckets != 3 {
		t.Errorf("report buckets = %d, want 3", rep.Buckets)
	}

	if len(rep.ByRealm) != 2 || rep.ByRealm[0].Name != "r1" || rep.ByRealm[0].Requests != 4 {
		t.Errorf("by_realm = %+v, want r1 with 4 requests", rep.ByRealm)
	}
	if len(rep.ByAccount) != 2 || rep.ByAccount[0].Name != "a1" || rep.ByAccount[0].Requests != 4 {
		t.Errorf("by_account = %+v, want a1 with 4 requests", rep.ByAccount)
	}
	if len(rep.ByModel) != 2 || rep.ByModel[0].Name != "c/m" || rep.ByModel[0].Requests != 4 {
		t.Errorf("by_model = %+v, want c/m with 4 requests", rep.ByModel)
	}

	// The accumulated row carries the merged latency samples, weighted by count:
	// 100ms twice for c/m plus 100ms once for c/m2, and nothing from the failure.
	if got := rep.ByClient[0].AvgLatencyMs; got == nil {
		t.Error("measured row lost its average latency")
	} else if want := 100.0; *got != want {
		t.Errorf("avg latency = %v, want %v", *got, want)
	}

	// The realm row mirrors its key so one table shape renders both.
	if rep.ByRealm[0].Realm != "r1" {
		t.Errorf("realm row realm = %q, want r1", rep.ByRealm[0].Realm)
	}

	snap := s.UsageSnapshot(0, map[string]string{"a1": "prod-1"})
	found := false
	for _, row := range snap.ByAccount {
		if row.Name == "a1" {
			found = true
			if row.Extra != "prod-1" {
				t.Errorf("account nick = %q, want prod-1", row.Extra)
			}
		} else if row.Extra != "" {
			t.Errorf("account %q got a nick it has none for: %q", row.Name, row.Extra)
		}
	}
	if !found {
		t.Error("a1 is missing from the account rows")
	}
}

func TestUsageSnapshotSortsByWeight(t *testing.T) {
	s := NewUsageStore(16)
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for name, tokens := range map[string]int64{"zebra": 10, "alpha": 90, "kiwi": 50} {
		s.Add(at, name, "", "", "m", UsageDelta{TotalTokens: tokens, HasTotal: true}, true)
	}

	// UsageReport keeps the legacy name order...
	rep := s.UsageReport(0)
	if len(rep.ByClient) != 3 || rep.ByClient[0].Name != "alpha" || rep.ByClient[2].Name != "zebra" {
		t.Errorf("UsageReport by_client = %+v, want name order", rep.ByClient)
	}
	// ...while the dashboard snapshot ranks heaviest first.
	snap := s.UsageSnapshot(0, nil)
	if len(snap.ByClient) != 3 || snap.ByClient[0].Name != "alpha" || snap.ByClient[1].Name != "kiwi" || snap.ByClient[2].Name != "zebra" {
		t.Errorf("UsageSnapshot by_client = %+v, want alpha/kiwi/zebra", snap.ByClient)
	}
}

// ---------------------------------------------------------------------------
// Latency and throughput
// ---------------------------------------------------------------------------

func TestUsageDerivesLatencyAndTPS(t *testing.T) {
	s := NewUsageStore(16)
	at := time.Date(2026, 4, 5, 8, 30, 0, 0, time.UTC)

	// Measured: 100 completion tokens over 2000 ms = 50 tok/s.
	s.Record(UsageRecord{At: at, Client: "measured", Model: "measured/m", CompletionTokens: 100, TotalTokens: 100, LatencyMs: 2000, HasLatency: true})
	// Enqueued but never measured.
	s.Record(UsageRecord{At: at, Client: "unmeasured", Model: "unmeasured/m", PromptTokens: 5, TotalTokens: 5})

	rep := s.UsageReport(0)
	if len(rep.ByClient) != 2 {
		t.Fatalf("by_client = %+v, want 2 rows", rep.ByClient)
	}
	measured, unmeasured := rep.ByClient[0], rep.ByClient[1]
	if measured.Name != "measured" || unmeasured.Name != "unmeasured" {
		t.Fatalf("by_client order = %+v", rep.ByClient)
	}
	if measured.AvgLatencyMs == nil || *measured.AvgLatencyMs != 2000 {
		t.Errorf("measured avg latency = %v, want 2000", measured.AvgLatencyMs)
	}
	if measured.AvgTokensPerSecond == nil || *measured.AvgTokensPerSecond != 50 {
		t.Errorf("measured avg tps = %v, want 50", measured.AvgTokensPerSecond)
	}
	if unmeasured.AvgLatencyMs != nil || unmeasured.AvgTokensPerSecond != nil {
		t.Errorf("unmeasured row invented averages: %+v", unmeasured)
	}

	// A missing measurement must leave the field out of the JSON entirely: a
	// 0 the frontend would render as "0 ms" is a lie about what was observed.
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Totals   map[string]any   `json:"totals"`
		ByClient []map[string]any `json:"by_client"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wire.ByClient[1]["avg_latency_ms"]; ok {
		t.Error(`unmeasured row serialised "avg_latency_ms"`)
	}
	if _, ok := wire.ByClient[1]["avg_tokens_per_second"]; ok {
		t.Error(`unmeasured row serialised "avg_tokens_per_second"`)
	}
	if got := wire.ByClient[0]["avg_tokens_per_second"]; got != 50.0 {
		t.Errorf("measured avg_tokens_per_second = %v, want 50", got)
	}
	// The report totals contain one real sample, so they keep the field.
	if got := wire.Totals["avg_latency_ms"]; got != 2000.0 {
		t.Errorf("totals avg_latency_ms = %v, want 2000", got)
	}
}

// TestUsageDerivesLatencyFromStartedAt covers the timing the gateway can
// observe itself, including the two boundaries: a zero duration is a real
// latency with no derivable throughput, and clock skew is not a measurement.
func TestUsageDerivesLatencyFromStartedAt(t *testing.T) {
	s := NewUsageStore(16)
	start := time.Date(2026, 4, 5, 8, 0, 0, 0, time.UTC)

	s.Record(UsageRecord{At: start.Add(1500 * time.Millisecond), StartedAt: start, Client: "timed", Model: "timed/m", CompletionTokens: 30, TotalTokens: 30})
	s.Record(UsageRecord{At: start, StartedAt: start, Client: "instant", Model: "instant/m", CompletionTokens: 10, TotalTokens: 10})
	s.Record(UsageRecord{At: start, StartedAt: start.Add(time.Second), Client: "skewed", Model: "skewed/m", CompletionTokens: 10, TotalTokens: 10})

	rows := map[string]UsageBucket{}
	for _, row := range s.UsageReport(0).ByClient {
		rows[row.Name] = row
	}

	if got := rows["timed"].AvgLatencyMs; got == nil || *got != 1500 {
		t.Errorf("timed avg latency = %v, want 1500", got)
	}
	if got := rows["timed"].AvgTokensPerSecond; got == nil || *got != 20 {
		t.Errorf("timed avg tps = %v, want 20", got)
	}
	if got := rows["instant"].AvgLatencyMs; got == nil || *got != 0 {
		t.Errorf("instant avg latency = %v, want a measured 0", got)
	}
	if got := rows["instant"].AvgTokensPerSecond; got != nil {
		t.Errorf("instant row fabricated a throughput from a 0ms duration: %v", *got)
	}
	if got := rows["skewed"].AvgLatencyMs; got != nil {
		t.Errorf("skewed row reported a negative measurement as %v", *got)
	}
	if got := rows["skewed"].AvgTokensPerSecond; got != nil {
		t.Errorf("skewed row reported a throughput: %v", *got)
	}
}

func TestUsageAddRecordsExplicitTPSWithoutLatency(t *testing.T) {
	s := NewUsageStore(8)
	at := time.Date(2026, 4, 5, 8, 0, 0, 0, time.UTC)
	s.Add(at, "c", "", "", "c/m", UsageDelta{TokensPerSecond: 12.5, HasTPS: true}, true)

	row := s.UsageReport(0).ByClient[0]
	if row.AvgTokensPerSecond == nil || *row.AvgTokensPerSecond != 12.5 {
		t.Errorf("avg tps = %v, want 12.5", row.AvgTokensPerSecond)
	}
	if row.AvgLatencyMs != nil {
		t.Errorf("avg latency = %v, want absent", *row.AvgLatencyMs)
	}
}

// ---------------------------------------------------------------------------
// Rollup and the bucket cap
// ---------------------------------------------------------------------------

func TestUsageRollupFoldsAgedHoursIntoDays(t *testing.T) {
	s := NewUsageStore(64)
	now := time.Now().UTC().Truncate(time.Hour)
	old := now.Add(-100 * 24 * time.Hour)
	d := UsageDelta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 10, HasTotal: true}

	s.Add(old, "c", "", "", "c/m", d, true)
	s.Add(now, "c", "", "", "c/m", d, true)
	if got := s.Buckets(); got != 2 {
		t.Fatalf("Buckets() = %d, want 2", got)
	}

	s.Rollup(time.Now())

	// One hour bucket became one day bucket: same count, coarser scope.
	if got := s.Buckets(); got != 2 {
		t.Errorf("Buckets() after rollup = %d, want 2", got)
	}
	rep := s.UsageSnapshot(0, nil)
	if len(rep.Series) != 2 {
		t.Fatalf("series = %+v, want 2 points", rep.Series)
	}
	if rep.Series[0].Scope != "day" || rep.Series[1].Scope != "hour" {
		t.Fatalf("series scopes = %q/%q, want day/hour", rep.Series[0].Scope, rep.Series[1].Scope)
	}
	wantDay := localDayStart(old).Format(time.RFC3339)
	if rep.Series[0].T != wantDay {
		t.Errorf("day point T = %q, want %q", rep.Series[0].T, wantDay)
	}
	if rep.Series[0].PromptTokens != 10 || rep.Series[0].Requests != 1 {
		t.Errorf("day point lost the aged traffic: %+v", rep.Series[0])
	}
	wantHour := localHourStart(now).Format(time.RFC3339)
	if rep.Series[1].T != wantHour {
		t.Errorf("hour point T = %q, want %q", rep.Series[1].T, wantHour)
	}

	// Nothing may be counted twice by a second pass.
	if rep.Totals.Requests != 2 || rep.Totals.PromptTokens != 20 {
		t.Errorf("totals after rollup = %+v, want 2 requests / 20 prompt tokens", rep.Totals)
	}
	s.Rollup(time.Now())
	again := s.UsageSnapshot(0, nil)
	if again.Totals != rep.Totals || len(again.Series) != len(rep.Series) {
		t.Errorf("rollup is not idempotent: %+v vs %+v", again.Totals, rep.Totals)
	}
}

// TestUsageCapRollsUpBeforeEvicting pins the overflow policy: an oversized
// store folds aged hour buckets into day buckets first, and only drops buckets
// when folding is not enough.
func TestUsageCapRollsUpBeforeEvicting(t *testing.T) {
	s := NewUsageStore(64)
	s.SetMaxBuckets(2)
	if got := s.MaxBuckets(); got != 2 {
		t.Fatalf("MaxBuckets() = %d, want 2", got)
	}

	d := UsageDelta{TotalTokens: 1, HasTotal: true}
	old := time.Now().UTC().Truncate(time.Hour).Add(-100 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		s.Add(old.Add(-time.Duration(i)*time.Hour), "a", "", "", "m", d, true)
	}

	if got := s.Buckets(); got > 2 {
		t.Errorf("Buckets() = %d, want <= 2", got)
	}
	if got := s.Evicted(); got != 0 {
		t.Errorf("Evicted() = %d, want 0: folding should have freed enough room", got)
	}
	if got := s.UsageReport(0).Totals.Requests; got != 3 {
		t.Errorf("requests = %d, want 3: folding must not lose traffic", got)
	}
}

func TestUsageCapEvictsTheOldestBuckets(t *testing.T) {
	s := NewUsageStore(64)
	s.SetMaxBuckets(3)
	base := time.Now().UTC().Truncate(time.Hour)

	for i := 0; i < 10; i++ {
		s.Add(base.Add(time.Duration(i)*time.Hour), fmt.Sprintf("c%d", i), "", "", "m", UsageDelta{TotalTokens: 1, HasTotal: true}, true)
	}

	if got := s.Buckets(); got != 3 {
		t.Fatalf("Buckets() = %d, want 3", got)
	}
	if got := s.Evicted(); got != 7 {
		t.Errorf("Evicted() = %d, want 7", got)
	}
	rep := s.UsageSnapshot(0, nil)
	kept := map[string]bool{}
	for _, row := range rep.ByClient {
		kept[row.Name] = true
	}
	for _, name := range []string{"c7", "c8", "c9"} {
		if !kept[name] {
			t.Errorf("bucket %s was dropped instead of an older one: %+v", name, rep.ByClient)
		}
	}
	if rep.Totals.Requests != 3 {
		t.Errorf("retained requests = %d, want the 3 surviving buckets", rep.Totals.Requests)
	}
	// The ring still knows how many requests the process actually saw.
	if got := s.Total(); got != 10 {
		t.Errorf("Total() = %d, want 10", got)
	}

	// Raising the cap again stops the bleeding.
	s.SetMaxBuckets(0)
	if got := s.MaxBuckets(); got != MaxUsageBuckets {
		t.Errorf("MaxBuckets() = %d, want the default %d", got, MaxUsageBuckets)
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func TestUsagePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	base := time.Date(2026, 2, 3, 9, 0, 0, 0, time.UTC)

	s := NewPersistentUsageStore(64, path)
	if got := s.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	s.Add(base, "trae", "r1", "a1", "trae/m", UsageDelta{
		PromptTokens: 100, HasPromptTokens: true,
		CompletionTokens: 50, HasCompletion: true,
		TotalTokens: 150, HasTotal: true,
		LatencyMs: 800, HasLatency: true,
		TokensPerSecond: 62.5, HasTPS: true,
	}, true)
	s.Add(base.Add(time.Hour), "kimi", "r2", "a2", "kimi/m", UsageDelta{HasTotal: true}, false)
	s.Add(base.Add(48*time.Hour), "trae", "r1", "a1", "trae/m", UsageDelta{TotalTokens: 7, HasTotal: true}, true)

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file is the documented schema, complete and parseable.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	if f.Version != usageFileVersion {
		t.Errorf("file version = %d, want %d", f.Version, usageFileVersion)
	}
	if len(f.Buckets) != 3 {
		t.Errorf("file holds %d buckets, want 3", len(f.Buckets))
	}
	if f.Saved.IsZero() {
		t.Error("file records no save time")
	}

	reloaded := NewPersistentUsageStore(64, path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Buckets(); got != s.Buckets() {
		t.Errorf("reloaded %d buckets, want %d", got, s.Buckets())
	}

	want := s.UsageSnapshot(0, nil)
	got := reloaded.UsageSnapshot(0, nil)
	want.Generated, got.Generated = "", ""
	if !reflect.DeepEqual(want, got) {
		t.Errorf("reloaded report differs\n want %+v\n  got %+v", want, got)
	}
	if got.FileBytes == 0 {
		t.Error("reloaded report reports file_bytes = 0")
	}
	if got.Since != want.Since || got.Since == "" {
		t.Errorf("since = %q, want %q", got.Since, want.Since)
	}

	// SetPath on an in-memory store loads the same history.  The comparison is
	// made on the wire form: the averages are pointers, so DeepEqual would
	// compare addresses rather than values.
	other := NewUsageStore(64)
	if err := other.SetPath(path); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	// Both sides use the same view, so the row ordering is comparable.
	otherRep := other.UsageSnapshot(0, nil)
	otherRep.Generated, want.Generated = "", ""
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(otherRep)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("SetPath report differs\n want %s\n  got %s", wantJSON, gotJSON)
	}
}

func TestUsageRecentRecordsPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	started := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)

	s := NewPersistentUsageStore(10, path)
	s.Record(UsageRecord{At: started, StartedAt: started, Client: "alpha", Account: "a1", SessionID: "conv-alpha-1", Model: "alpha/m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, LatencyMs: 250, HasLatency: true})
	s.Record(UsageRecord{At: started.Add(time.Second), Client: "beta", Account: "b1", Model: "beta/m", Failed: true})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recent.json")); err != nil {
		t.Fatalf("recent journal was not written: %v", err)
	}

	reloaded := NewPersistentUsageStore(10, path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := reloaded.Snapshot()
	if len(got) != 2 {
		t.Fatalf("reloaded %d recent records, want 2", len(got))
	}
	if got[0].Client != "alpha" || got[0].Account != "a1" || got[0].TotalTokens != 15 || !got[0].HasLatency {
		t.Fatalf("first recent record = %+v, want the successful alpha call", got[0])
	}
	if got[0].SessionID != "conv-alpha-1" {
		t.Errorf("first recent record session = %q, want conv-alpha-1: the 会话ID column must survive a reload", got[0].SessionID)
	}
	if got[1].SessionID != "" {
		t.Errorf("second recent record session = %q, want empty", got[1].SessionID)
	}
	if got[1].Client != "beta" || !got[1].Failed {
		t.Fatalf("second recent record = %+v, want the failed beta call", got[1])
	}
}

func TestUsageRecentJournalKeepsNewestHundred(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	started := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	s := NewPersistentUsageStore(0, path)
	for i := 0; i < 150; i++ {
		s.Record(UsageRecord{At: started.Add(time.Duration(i) * time.Second), Client: "c", Model: "m", PromptTokens: i})
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewPersistentUsageStore(0, path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := reloaded.Snapshot()
	if len(got) != 100 {
		t.Fatalf("reloaded %d recent records, want 100", len(got))
	}
	if got[0].PromptTokens != 50 || got[99].PromptTokens != 149 {
		t.Fatalf("reloaded first/last prompt tokens = %d/%d, want 50/149", got[0].PromptTokens, got[99].PromptTokens)
	}
}

// TestUsageSaveIsAtomic pins the tmp+rename contract: after a save the target
// holds one complete document, and no temporary file is left behind.
func TestUsageSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	at := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)

	s := NewPersistentUsageStore(8, path)
	s.Add(at, "c", "", "", "c/m", UsageDelta{TotalTokens: 3, HasTotal: true}, true)
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// A second save replaces the whole document rather than appending to it.
	s.Add(at.Add(time.Hour), "c", "", "", "c/m", UsageDelta{TotalTokens: 4, HasTotal: true}, true)
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("file after the second save is not valid JSON: %v", err)
	}
	if len(f.Buckets) != 2 {
		t.Errorf("file holds %d buckets, want 2", len(f.Buckets))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "usage.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data dir holds %v, want only usage.json", names)
	}

	// A dirty save creates the directory it needs.
	nested := filepath.Join(dir, "deep", "er", "usage.json")
	s2 := NewPersistentUsageStore(8, nested)
	s2.Add(at, "c", "", "", "c/m", UsageDelta{HasTotal: true}, true)
	if err := s2.Save(); err != nil {
		t.Fatalf("Save into a missing directory: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
}

// TestUsageConcurrentSavesNeverPublishAPartialDocument pins why flush calls
// core.WriteFileAtomic instead of hand-rolling "path + .tmp".
//
// The panel's Save and the 30s background flush are two goroutines writing the
// same file, and they snapshot the store at different moments, so they write
// documents of different lengths.  With one fixed temp name both writers land
// in the same temp file: the second write starts at offset 0 of a file the
// first writer already filled, so the longer writer's tail survives past the
// shorter document's closing brace and the following rename publishes that
// hybrid.  Giving every save its own temp file makes the rename the only
// publication step, so a reader can only ever observe a whole document.
func TestUsageConcurrentSavesNeverPublishAPartialDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	at := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)

	// Distinct lengths, so an interleaved write cannot coincidentally look
	// like either writer's document.
	sizes := []int{200, 260, 320, 380, 440, 500}
	valid := make(map[int]bool, len(sizes))
	stores := make([]*UsageStore, 0, len(sizes))
	for _, n := range sizes {
		valid[n] = true
		s := NewPersistentUsageStore(1<<16, path)
		for i := 0; i < n; i++ {
			s.Add(at, "c", "", "", fmt.Sprintf("c/model-%04d-with-a-fairly-long-name", i),
				UsageDelta{
					PromptTokens:     int64(i),
					HasPromptTokens:  true,
					CompletionTokens: int64(i),
					HasCompletion:    true,
					TotalTokens:      int64(2 * i),
					HasTotal:         true,
				}, true)
		}
		stores = append(stores, s)
	}

	stop := make(chan struct{})
	bad := make(chan error, 1)
	fail := func(err error) {
		select {
		case bad <- err:
		default:
		}
	}

	var reader sync.WaitGroup
	reads := 0
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				// Not the failure this test is about: Windows returns a
				// sharing violation when a rename lands while the target is
				// open, and the file legitimately does not exist before the
				// first save completes.
				continue
			}
			var f usageFile
			if err := json.Unmarshal(raw, &f); err != nil {
				fail(fmt.Errorf("published a torn document (%d bytes): %w", len(raw), err))
				return
			}
			if !valid[len(f.Buckets)] {
				fail(fmt.Errorf("published %d buckets, which is no writer's document (want one of %v)",
					len(f.Buckets), sizes))
				return
			}
			reads++
		}
	}()

	var writers sync.WaitGroup
	var saved, saveErrs atomic.Int64
	for _, s := range stores {
		writers.Add(1)
		go func(s *UsageStore) {
			defer writers.Done()
			for i := 0; i < 200; i++ {
				if err := s.Save(); err != nil {
					// A rename that loses a race with the reader on Windows
					// fails with a sharing violation.  It publishes nothing,
					// so it is not the corruption this test hunts for.
					saveErrs.Add(1)
					continue
				}
				saved.Add(1)
			}
		}(s)
	}
	writers.Wait()
	close(stop)
	reader.Wait()

	// A run where nothing happened would pass vacuously.
	if reads == 0 || saved.Load() == 0 {
		t.Fatalf("nothing actually ran (reads=%d saves=%d); the test proves nothing", reads, saved.Load())
	}
	t.Logf("read %d whole documents across %d successful saves (%d lost to OS races)",
		reads, saved.Load(), saveErrs.Load())

	select {
	case err := <-bad:
		t.Fatalf("concurrent saves corrupted the published file: %v", err)
	default:
	}
}

func TestUsageSaveWithoutPathIsANoOp(t *testing.T) {
	s := NewUsageStore(8)
	s.Add(time.Now(), "c", "", "", "c/m", UsageDelta{HasTotal: true}, true)
	if err := s.Save(); err != nil {
		t.Errorf("in-memory Save = %v, want nil", err)
	}
	if got := s.Path(); got != "" {
		t.Errorf("Path() = %q, want empty", got)
	}
}

func TestUsageLoadMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-written.json")
	s := NewPersistentUsageStore(8, path)
	if err := s.Load(); err != nil {
		t.Errorf("Load of a missing file = %v, want nil", err)
	}
	if got := s.UsageReport(0).Totals.Requests; got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

// TestUsageLoadCorruptFileKeepsLiveHistory: an unreadable file is reported, but
// it must not wipe the buckets the running process already collected.
func TestUsageLoadCorruptFileKeepsLiveHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s := NewPersistentUsageStore(8, path)
	s.Add(time.Now(), "keep", "", "", "keep/m", UsageDelta{PromptTokens: 7, HasPromptTokens: true, TotalTokens: 7, HasTotal: true}, true)

	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if err := s.Load(); err == nil {
		t.Fatal("Load of a corrupt file returned nil, want an error")
	}
	if s.LastError() == nil {
		t.Error("LastError() is nil after a failed load")
	}
	if got := s.UsageReport(0).Totals.Requests; got != 1 {
		t.Errorf("requests after a failed load = %d, want the live 1", got)
	}
	if !strings.Contains(s.Describe(), "last error") {
		t.Errorf("Describe() hides the failure: %q", s.Describe())
	}
}

func TestUsageStartStopFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	at := time.Date(2026, 6, 7, 8, 0, 0, 0, time.UTC)

	s := NewPersistentUsageStore(8, path)
	s.Start()
	s.Start() // idempotent
	s.Add(at, "c", "r", "a", "c/m", UsageDelta{TotalTokens: 5, HasTotal: true}, true)
	s.Stop()
	s.Stop() // idempotent

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Stop did not flush: %v", err)
	}
	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("flushed file is not valid JSON: %v", err)
	}
	if len(f.Buckets) != 1 {
		t.Fatalf("flushed %d buckets, want 1", len(f.Buckets))
	}
	if f.Buckets[0].Client != "c" || f.Buckets[0].Account != "a" || f.Buckets[0].TT != 5 {
		t.Errorf("flushed bucket = %+v", f.Buckets[0])
	}

	// Stop is safe on a store that never started, and on a memory-only store.
	NewPersistentUsageStore(8, filepath.Join(t.TempDir(), "x.json")).Stop()
	NewUsageStore(8).Stop()

	// A store that already has the file loads it on Start.
	s2 := NewPersistentUsageStore(8, path)
	s2.Start()
	defer s2.Stop()
	if got := s2.UsageReport(0).Totals.Requests; got != 1 {
		t.Errorf("Start did not load the history: requests = %d, want 1", got)
	}
}

func TestUsageDescribe(t *testing.T) {
	if got := NewUsageStore(4).Describe(); !strings.Contains(got, "in-memory") {
		t.Errorf("Describe() = %q, want it to say in-memory", got)
	}

	path := filepath.Join(t.TempDir(), "usage.json")
	s := NewPersistentUsageStore(4, path)
	s.Add(time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC), "c", "", "", "c/m", UsageDelta{HasTotal: true}, true)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := s.Describe()
	if !strings.Contains(got, path) || !strings.Contains(got, "1 buckets") {
		t.Errorf("Describe() = %q, want the path and one bucket", got)
	}
	if !strings.Contains(got, "2026-07-08") {
		t.Errorf("Describe() = %q, want the history start", got)
	}
}

// TestUsageNilStoreIsInert keeps the panel's nil-store fallback honest: every
// entry point must tolerate a nil receiver instead of panicking.
func TestUsageNilStoreIsInert(t *testing.T) {
	var s *UsageStore
	s.Add(time.Now(), "c", "", "", "c/m", UsageDelta{HasTotal: true}, true)
	s.Record(UsageRecord{})
	s.Rollup(time.Now())
	if err := s.Save(); err != nil {
		t.Errorf("nil Save = %v", err)
	}
	if err := s.Load(); err != nil {
		t.Errorf("nil Load = %v", err)
	}
	s.Stop()
	s.Start()
	if s.Buckets() != 0 || s.Evicted() != 0 || s.MaxBuckets() != 0 || s.Total() != 0 || s.Capacity() != 0 {
		t.Error("nil store reported non-zero state")
	}
	if got := s.UsageSnapshot(72, nil); got.Totals.Requests != 0 || got.ByClient == nil {
		t.Errorf("nil store report = %+v", got)
	}
	if got := s.Describe(); got == "" {
		t.Error("nil store Describe() is empty")
	}
}

// TestUsageStoreConcurrentAddsAndSaves exercises the whole store under -race:
// writers, a saver, and readers at once.  Nothing may be lost.
func TestUsageStoreConcurrentAddsAndSaves(t *testing.T) {
	const (
		workers   = 8
		perWorker = 200
	)
	path := filepath.Join(t.TempDir(), "usage.json")
	s := NewPersistentUsageStore(64, path)
	at := time.Now().UTC().Truncate(time.Hour)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				s.Add(at, fmt.Sprintf("c%d", w), "r", "a", "m", UsageDelta{TotalTokens: 1, HasTotal: true}, true)
				if i%50 == 0 {
					_ = s.Save()
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < perWorker; i++ {
			_ = s.Snapshot()
			_ = s.UsageSnapshot(72, nil)
			_ = s.Buckets()
			_ = s.Describe()
		}
	}()
	wg.Wait()

	rep := s.UsageReport(0)
	if want := int64(workers * perWorker); rep.Totals.Requests != want || rep.Totals.TotalTokens != want {
		t.Errorf("totals = %+v, want %d requests / %d tokens", rep.Totals, want, want)
	}
	if got := s.Buckets(); got != workers {
		t.Errorf("Buckets() = %d, want %d", got, workers)
	}
	if got := s.Total(); got != int64(workers*perWorker) {
		t.Errorf("Total() = %d, want %d", got, workers*perWorker)
	}

	// The final save is complete and parseable.
	if err := s.Save(); err != nil {
		t.Fatalf("final Save: %v", err)
	}
	reloaded := NewPersistentUsageStore(64, path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.UsageReport(0).Totals; got != rep.Totals {
		t.Errorf("reloaded totals = %+v, want %+v", got, rep.Totals)
	}
}

// TestSessionIDForRecordsOnlyWhatTheCallerNamed pins the source of the
// 会话ID column: the conversation id the gateway resolved, then the option
// spellings the router also accepts, then the per-turn header.  The request's
// user field is deliberately not a session: writing it would claim an identity
// the caller never sent as a conversation.
func TestSessionIDForRecordsOnlyWhatTheCallerNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *core.ChatRequest
		want string
	}{
		{"nil", nil, ""},
		{"resolved conversation id", &core.ChatRequest{ConversationID: "conv-1"}, "conv-1"},
		{"resolved id is trimmed", &core.ChatRequest{ConversationID: "  conv-1  "}, "conv-1"},
		{"option spelling", &core.ChatRequest{Options: map[string]any{"conversation_id": "conv-opt"}}, "conv-opt"},
		{"prompt cache key", &core.ChatRequest{Options: map[string]any{"prompt_cache_key": "cache-9"}}, "cache-9"},
		{"turn id is the last resort", &core.ChatRequest{ConversationRequestID: "turn-7"}, "turn-7"},
		{"a user id is not a session", &core.ChatRequest{User: "u-1"}, ""},
		{"resolved id beats everything", &core.ChatRequest{ConversationID: "conv-1", Options: map[string]any{"prompt_cache_key": "cache-9"}, ConversationRequestID: "turn-7"}, "conv-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionIDFor(tc.req); got != tc.want {
				t.Fatalf("sessionIDFor = %q, want %q", got, tc.want)
			}
		})
	}
}
