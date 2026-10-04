package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// The accounts view polls /panel/api/overview every five seconds.  It used to
// follow that up with one GET per module, because the traffic slice the table
// renders (requests / failures / in-flight) only existed on the per-client
// endpoint -- so every module was asked for the very list the overview had just
// handed over.  This pins the fix: the single poll carries the accounts, the
// stats, and the flag that tells the shell it can skip the follow-up.
func TestOverviewCarriesAccountsAndStatsSoTheShellNeedNotReAsk(t *testing.T) {
	acct := &fakeAccountClient{
		fakeClient: &fakeClient{name: "alpha", status: core.Status{Ready: true}},
		accounts: []core.AccountRecord{
			{ID: "A1", Label: "一号", Enabled: true, State: "ready"},
		},
	}

	usage := gateway.NewUsageStore(16)
	usage.Record(gateway.UsageRecord{
		At:          time.Now(),
		Client:      "alpha",
		Account:     "A1",
		Model:       "alpha/m",
		TotalTokens: 12,
	})

	h := New(Options{
		Registry: registryOf(acct),
		Version:  "test",
		Listen:   "127.0.0.1:0",
		Started:  time.Now(),
		Stats:    gateway.NewStats(),
		Logs:     gateway.NewLogRing(8),
		Usage:    usage,
	})

	rec := get(t, h, "/panel/api/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = HTTP %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("overview is not JSON: %v", err)
	}

	if out["full_accounts"] != true {
		t.Errorf("full_accounts = %v, want true so the shell skips the per-client round trips",
			out["full_accounts"])
	}

	// An always-present object, not an omitted field: "no traffic yet" and
	// "this build cannot tell you" must not look the same to the page.
	stats, ok := out["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats = %v (%T), want an object", out["stats"], out["stats"])
	}
	bucket, ok := stats["A1"].(map[string]any)
	if !ok {
		t.Fatalf("stats[A1] = %v, want the account's traffic bucket in %v", stats["A1"], stats)
	}
	if req := bucket["requests"]; req != float64(1) {
		t.Errorf("stats[A1].requests = %v, want 1", req)
	}

	// The management list still rides along, which is what the table renders.
	clients, ok := out["clients"].([]any)
	if !ok || len(clients) != 1 {
		t.Fatalf("clients = %v, want the one registered module", out["clients"])
	}
	row, _ := clients[0].(map[string]any)
	if row["name"] != "alpha" {
		t.Errorf("clients[0].name = %v, want alpha", row["name"])
	}
	accounts, ok := row["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("clients[0].accounts = %v, want the one account", row["accounts"])
	}
	first, _ := accounts[0].(map[string]any)
	if first["id"] != "A1" {
		t.Errorf("clients[0].accounts[0].id = %v, want A1", first["id"])
	}
}
