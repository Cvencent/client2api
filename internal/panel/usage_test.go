package panel

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// ---------------------------------------------------------------------------
// helpers (names are unique to this file: the other panel tests share the
// package, so nothing here may shadow an existing helper)
// ---------------------------------------------------------------------------

func usageNum(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("field %q = %v (%T), want a number in %v", key, m[key], m[key], m)
	}
	return v
}

func usageRows(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	rows, ok := m[key].([]any)
	if !ok {
		t.Fatalf("field %q = %v (%T), want an array", key, m[key], m[key])
	}
	return rows
}

func usageRow(t *testing.T, rows []any, i int) map[string]any {
	t.Helper()
	if len(rows) <= i {
		t.Fatalf("row %d missing from %v", i, rows)
	}
	row, ok := rows[i].(map[string]any)
	if !ok {
		t.Fatalf("row %d = %v (%T), want an object", i, rows[i], rows[i])
	}
	return row
}

// ---------------------------------------------------------------------------
// The dashboard's traffic view
// ---------------------------------------------------------------------------

// TestUsageEndpointReportsEveryDimension drives the handler through the wire
// form, so it pins the field names the frontend reads as well as the numbers.
func TestUsageEndpointReportsEveryDimension(t *testing.T) {
	store := gateway.NewPersistentUsageStore(64, filepath.Join(t.TempDir(), "usage.json"))
	base := time.Now().Add(-2 * time.Hour)
	store.Record(gateway.UsageRecord{
		At: base, Client: "trae", Realm: "r1", Account: "a1", Model: "trae/m",
		PromptTokens: 900, CompletionTokens: 400, TotalTokens: 1300,
		LatencyMs: 2000, HasLatency: true,
	})
	store.Record(gateway.UsageRecord{At: base, Client: "trae", Realm: "r1", Account: "a1", Model: "trae/m", Failed: true})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	client := &fakeAccountClient{
		fakeClient: &fakeClient{name: "wb", status: core.Status{Ready: true}},
		accounts:   []core.AccountRecord{{ID: "a1", Label: "prod-1", Enabled: true}},
	}
	h := New(Options{Registry: registryOf(client), Usage: store, Started: time.Now()})

	rec := get(t, h, "/panel/api/usage?window=72")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)

	if n := usageNum(t, got, "window_hours"); n != 72 {
		t.Errorf("window_hours = %v, want 72", n)
	}

	totals := usageRow(t, []any{got["totals"]}, 0)
	if n := usageNum(t, totals, "requests"); n != 2 {
		t.Errorf("totals.requests = %v, want 2", n)
	}
	if n := usageNum(t, totals, "failures"); n != 1 {
		t.Errorf("totals.failures = %v, want 1", n)
	}
	if n := usageNum(t, totals, "total_tokens"); n != 1300 {
		t.Errorf("totals.total_tokens = %v, want 1300", n)
	}
	// The failed attempt carried no timing, so it is not averaged in.
	if n := usageNum(t, totals, "avg_latency_ms"); n != 2000 {
		t.Errorf("totals.avg_latency_ms = %v, want 2000", n)
	}
	if n := usageNum(t, totals, "avg_tokens_per_second"); n != 200 {
		t.Errorf("totals.avg_tokens_per_second = %v, want 200", n)
	}

	byClient := usageRows(t, got, "by_client")
	if row := usageRow(t, byClient, 0); row["name"] != "trae" {
		t.Errorf("by_client[0].name = %v, want trae", row["name"])
	}

	byRealm := usageRows(t, got, "by_realm")
	realmRow := usageRow(t, byRealm, 0)
	if realmRow["name"] != "r1" || realmRow["realm"] != "r1" {
		t.Errorf("by_realm[0] = %v, want name and realm r1", realmRow)
	}

	byAccount := usageRows(t, got, "by_account")
	accRow := usageRow(t, byAccount, 0)
	if accRow["name"] != "a1" || accRow["realm"] != "r1" {
		t.Errorf("by_account[0] = %v, want a1 in r1", accRow)
	}
	// The label comes from the module's account list, not from the record.
	if accRow["extra"] != "prod-1" {
		t.Errorf("by_account[0].extra = %v, want the label prod-1", accRow["extra"])
	}
	if n := usageNum(t, accRow, "requests"); n != 2 {
		t.Errorf("by_account[0].requests = %v, want 2", n)
	}
	if n := usageNum(t, accRow, "failures"); n != 1 {
		t.Errorf("by_account[0].failures = %v, want 1", n)
	}
	if n := usageNum(t, accRow, "avg_latency_ms"); n != 2000 {
		t.Errorf("by_account[0].avg_latency_ms = %v, want 2000", n)
	}
	if n := usageNum(t, accRow, "avg_tokens_per_second"); n != 200 {
		t.Errorf("by_account[0].avg_tokens_per_second = %v, want 200", n)
	}

	byModel := usageRows(t, got, "by_model")
	if row := usageRow(t, byModel, 0); row["name"] != "trae/m" {
		t.Errorf("by_model[0].name = %v, want trae/m", row["name"])
	}

	if n := usageNum(t, got, "buckets"); n < 1 {
		t.Errorf("buckets = %v, want at least 1", n)
	}
	if n := usageNum(t, got, "file_bytes"); n <= 0 {
		t.Errorf("file_bytes = %v, want the size of the persisted history", n)
	}
	if got["since"] == "" || got["generated"] == "" {
		t.Errorf("since/generated = %v/%v, want both set", got["since"], got["generated"])
	}

	series := usageRows(t, got, "series")
	point := usageRow(t, series, 0)
	if point["scope"] != "hour" {
		t.Errorf("series[0].scope = %v, want hour", point["scope"])
	}
	if point["t"] == "" {
		t.Errorf("series[0].t = %v, want a timestamp", point["t"])
	}
}

// TestUsageEndpointOmitsUnmeasuredAverages: when nothing measured a request,
// the averages must be absent rather than present as 0 — the frontend renders
// what it is given, and a 0 ms average is a claim about the upstream.
func TestUsageEndpointIncludesNewestRecentCallsFirst(t *testing.T) {
	store := gateway.NewUsageStore(16)
	base := time.Now().Add(-time.Minute)
	store.Record(gateway.UsageRecord{At: base, Client: "cline", Model: "cline/old", Account: "a1"})
	store.Record(gateway.UsageRecord{At: base.Add(time.Second), Client: "trae", Model: "trae/new", Account: "a2", PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10, LatencyMs: 500, HasLatency: true})
	store.Record(gateway.UsageRecord{At: base.Add(2 * time.Second), Client: "trae", Model: "trae/try2", Account: "a3", Candidate: 2, Failed: true})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	got := decodeMap(t, get(t, h, "/panel/api/usage"))
	recent := usageRows(t, got, "recent")
	if len(recent) != 3 {
		t.Fatalf("recent has %d rows, want 3", len(recent))
	}
	first := usageRow(t, recent, 0)
	if first["client"] != "trae" || first["model"] != "trae/try2" || first["account"] != "a3" || first["failed"] != true {
		t.Fatalf("recent[0] = %v, want the newest failed trae call", first)
	}
	if n := usageNum(t, first, "candidate"); n != 2 {
		t.Errorf("recent[0].candidate = %v, want 2", n)
	}
	if _, ok := first["total_tokens"]; ok {
		t.Errorf("recent[0] carries total_tokens for a failed candidate: %v", first)
	}
	if _, ok := first["latency_ms"]; ok {
		t.Errorf("recent[0] carries latency_ms for a failed candidate: %v", first)
	}
}

func TestUsageEndpointOmitsUnmeasuredAverages(t *testing.T) {
	store := gateway.NewUsageStore(16)
	store.Record(gateway.UsageRecord{
		At: time.Now().Add(-time.Minute), Client: "trae", Model: "trae/m",
		PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15,
	})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	got := decodeMap(t, get(t, h, "/panel/api/usage"))

	totals := usageRow(t, []any{got["totals"]}, 0)
	for _, key := range []string{"avg_latency_ms", "avg_tokens_per_second"} {
		if _, ok := totals[key]; ok {
			t.Errorf("totals carry %q with no sample: %v", key, totals)
		}
	}
	row := usageRow(t, usageRows(t, got, "by_client"), 0)
	for _, key := range []string{"avg_latency_ms", "avg_tokens_per_second"} {
		if _, ok := row[key]; ok {
			t.Errorf("by_client[0] carries %q with no sample: %v", key, row)
		}
	}
	if n := usageNum(t, row, "total_tokens"); n != 15 {
		t.Errorf("total_tokens = %v, want 15", n)
	}
}

// TestUsageEndpointSurvivesAnUnlistableModule: a module that cannot report its
// accounts must not remove rows from the report, only their labels.
func TestUsageEndpointSurvivesAnUnlistableModule(t *testing.T) {
	store := gateway.NewUsageStore(16)
	store.Add(time.Now().Add(-time.Minute), "trae", "r1", "a1", "trae/m", gateway.UsageDelta{HasTotal: true, TotalTokens: 5}, true)

	client := &fakeAccountClient{
		fakeClient:  &fakeClient{name: "wb", status: core.Status{Ready: true}},
		accountsErr: errors.New("upstream returned a secret-looking error"),
	}
	h := New(Options{Registry: registryOf(client), Usage: store, Started: time.Now()})

	row := usageRow(t, usageRows(t, decodeMap(t, get(t, h, "/panel/api/usage")), "by_account"), 0)
	if row["name"] != "a1" {
		t.Errorf("by_account[0].name = %v, want a1", row["name"])
	}
	if extra, ok := row["extra"]; ok && extra != "" {
		t.Errorf("by_account[0].extra = %v, want absent when the module cannot list accounts", extra)
	}
}

func TestUsageEndpointWithoutStoreListsArrays(t *testing.T) {
	h := New(Options{Registry: registryOf(), Started: time.Now()})
	rec := get(t, h, "/panel/api/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{"by_client", "by_realm", "by_account", "by_model", "series"} {
		if !strings.Contains(body, `"`+key+`":[]`) {
			t.Errorf("empty report does not serialise %q as []: %s", key, body)
		}
	}

	got := decodeMap(t, rec)
	if n := usageNum(t, got, "window_hours"); n != defaultUsageWindowHours {
		t.Errorf("window_hours = %v, want %d", n, defaultUsageWindowHours)
	}
	if n := usageNum(t, got, "buckets"); n != 0 {
		t.Errorf("buckets = %v, want 0", n)
	}
	if n := usageNum(t, got, "file_bytes"); n != 0 {
		t.Errorf("file_bytes = %v, want 0 without a store", n)
	}
}

// TestUsageWindowBoundsTheWholeReport: the window applies to every card, not
// just to the series, and 0 means the whole retained history.
func TestUsageWindowBoundsTheWholeReport(t *testing.T) {
	store := gateway.NewUsageStore(64)
	store.Record(gateway.UsageRecord{At: time.Now().Add(-100 * time.Hour), Client: "old", Model: "old/m"})
	store.Record(gateway.UsageRecord{At: time.Now().Add(-time.Hour), Client: "new", Model: "new/m"})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	recent := decodeMap(t, get(t, h, "/panel/api/usage?window=72"))
	if n := usageNum(t, usageRow(t, []any{recent["totals"]}, 0), "requests"); n != 1 {
		t.Errorf("window=72 counted %v requests, want 1", n)
	}
	if rows := usageRows(t, recent, "by_client"); len(rows) != 1 || usageRow(t, rows, 0)["name"] != "new" {
		t.Errorf("window=72 by_client = %v, want only new", rows)
	}

	all := decodeMap(t, get(t, h, "/panel/api/usage?window=0"))
	if n := usageNum(t, all, "window_hours"); n != 0 {
		t.Errorf("window_hours = %v, want 0", n)
	}
	if n := usageNum(t, usageRow(t, []any{all["totals"]}, 0), "requests"); n != 2 {
		t.Errorf("window=0 counted %v requests, want all 2", n)
	}
}

// TestUsagePanelSnapshotRanksHeaviestFirst: the dashboard sorts by weight, so
// the top row is the busiest account/model rather than the alphabetically first.
func TestUsagePanelSnapshotRanksHeaviestFirst(t *testing.T) {
	store := gateway.NewUsageStore(32)
	at := time.Now().Add(-time.Minute)
	store.Add(at, "trae", "r1", "a-zzz", "trae/m", gateway.UsageDelta{HasTotal: true, TotalTokens: 10}, true)
	store.Add(at, "trae", "r1", "a-aaa", "trae/m", gateway.UsageDelta{HasTotal: true, TotalTokens: 90}, true)
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	rows := usageRows(t, decodeMap(t, get(t, h, "/panel/api/usage")), "by_account")
	if name := usageRow(t, rows, 0)["name"]; name != "a-aaa" {
		t.Errorf("by_account[0].name = %v, want the heaviest account a-aaa", name)
	}
}

// ---------------------------------------------------------------------------
// Forcing a flush
// ---------------------------------------------------------------------------

// TestUsageSaveFlushesToDisk: the route exists so an operator can put the
// history on disk before stopping the process by hand, so the assertion is the
// file, not the status code.  A store that answers ok:true without writing is
// the failure this test is here to catch.
func TestUsageSaveFlushesToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	store := gateway.NewPersistentUsageStore(64, path)
	store.Record(gateway.UsageRecord{At: time.Now().Add(-time.Minute), Client: "trae", Model: "trae/m"})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	rec := post(t, h, "/panel/api/usage/save")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := decodeMap(t, rec)["ok"]; got != true {
		t.Errorf("ok = %v, want true", got)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the save did not reach the disk: %v", err)
	}
	if !strings.Contains(string(blob), "trae") {
		t.Errorf("the file holds no trace of the recorded bucket: %s", blob)
	}
}

// TestUsageSaveRejectsTheWrongMethod: only an explicit POST may rewrite the
// history file, so a stray GET cannot truncate or rewrite it as a side effect.
func TestUsageSaveRejectsTheWrongMethod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	store := gateway.NewPersistentUsageStore(64, path)
	store.Record(gateway.UsageRecord{At: time.Now(), Client: "trae", Model: "trae/m"})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	if rec := get(t, h, "/panel/api/usage/save"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405, body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a GET wrote the usage file (stat err = %v)", err)
	}
}

// TestUsageSaveWithoutAStore: with no store the honest answer is "there is
// none"; a cheerful ok would claim a write that never happened.
func TestUsageSaveWithoutAStore(t *testing.T) {
	h := New(Options{Registry: registryOf(), Started: time.Now()})
	rec := post(t, h, "/panel/api/usage/save")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
}

// TestUsageSaveReportsAWriteFailure: a store pointed at a directory cannot be
// written, and the operator has to be told that instead of reading ok:true.
func TestUsageSaveReportsAWriteFailure(t *testing.T) {
	store := gateway.NewPersistentUsageStore(64, t.TempDir())
	store.Record(gateway.UsageRecord{At: time.Now(), Client: "trae", Model: "trae/m"})
	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})

	rec := post(t, h, "/panel/api/usage/save")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "usage.json") {
		t.Errorf("the failure body leaks the path: %s", rec.Body.String())
	}
}
