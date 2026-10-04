package tabbit

import (
	"context"
	"encoding/json"
	"testing"

	"client2api/internal/core"
)

// balance_test.go drives the Tabbit quota-to-balance conversion offline.

func TestWebUsageAcceptsVendorStringNumbers(t *testing.T) {
	var u webUsage
	raw := `{"member_level":"free","usage_percentage":"71.68%","remaining_reset_hours":"28.95","current_cycle_end":"2026.10.05"}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if float64(u.UsagePercentage) != 71.68 {
		t.Fatalf("usage_percentage = %v, want 71.68", u.UsagePercentage)
	}
	if float64(u.RemainingResetHours) != 28.95 {
		t.Fatalf("remaining_reset_hours = %v, want 28.95", u.RemainingResetHours)
	}
}

func TestWebUsageAcceptsPlainNumbers(t *testing.T) {
	var u webUsage
	if err := json.Unmarshal([]byte(`{"usage_percentage":12.5,"remaining_reset_hours":5}`), &u); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if float64(u.UsagePercentage) != 12.5 || float64(u.RemainingResetHours) != 5 {
		t.Fatalf("parsed = %+v", u)
	}
}

func TestTabbitAccountBalanceReportsRemainingPercent(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)

	bal, err := c.AccountBalance(context.Background(), rec.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 88 || bal.Total != 100 || bal.Used != 12.5 || bal.Unit != balanceUnit {
		t.Fatalf("balance = %+v, want 88/100 used 12.5 unit %%", bal)
	}
	if n := f.hits("usage"); n != 1 {
		t.Fatalf("quota endpoint hits = %d, want 1", n)
	}
}

func TestTabbitQuotaBalanceClampsOutOfRangeValues(t *testing.T) {
	cases := []struct {
		used      float64
		remaining int64
	}{
		{used: -5, remaining: 100},
		{used: 0, remaining: 100},
		{used: 100, remaining: 0},
		{used: 140, remaining: 0},
	}
	for _, tc := range cases {
		got := quotaBalance(webUsage{UsagePercentage: flexNumber(tc.used)})
		if got.Credits != tc.remaining || got.Total != 100 || got.Unit != balanceUnit {
			t.Errorf("quotaBalance(%v) = %+v, want remaining %d", tc.used, got, tc.remaining)
		}
	}
}

func TestTabbitAccountBalanceRejectsSidecarAccount(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"kind": "sidecar", "base_url": "http://127.0.0.1:50124"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	if _, err := c.AccountBalance(context.Background(), rec.ID, 0); err == nil {
		t.Fatal("expected a sidecar account to refuse the quota read")
	}
}

func TestTabbitAccountBalanceRejectsUnknownAccount(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	if _, err := c.AccountBalance(context.Background(), "nobody", 0); err == nil {
		t.Fatal("expected an unknown-account error")
	}
}

func TestTabbitCapabilitiesAdvertiseBalance(t *testing.T) {
	f := newFakeWeb(t)
	c, _ := newWebClient(t, f)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Balance {
		t.Fatal("caps.Balance = false for the tabbit client")
	}
}

func TestTabbitAccountBalanceReportsUpstreamFailure(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)
	f.Close()

	if _, err := c.AccountBalance(context.Background(), rec.ID, 0); err == nil {
		t.Fatal("expected an upstream failure")
	}
}
