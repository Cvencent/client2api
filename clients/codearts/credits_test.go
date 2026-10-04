package codearts

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreditTotalsReadNestedMetricAmounts(t *testing.T) {
	s := pluginStatistics{
		Metrics: []creditMetric{{Name: "usageTotalPackageCredit", Remain: 5499.92, Amount: 5500, Used: 0.08}},
	}
	// The response carries the useful numbers on the metric object, not on the
	// statistics root. A zero root field must not hide a non-zero remainder.

	got, ok := s.creditTotals()
	if !ok {
		t.Fatal("creditTotals declined a well-formed total metric")
	}
	if math.Abs(got.Remain-5499.92) > 0.0001 || math.Abs(got.Total-5500) > 0.0001 || math.Abs(got.Used-0.08) > 0.0001 {
		t.Fatalf("creditTotals = %+v, want remain=5499.92 total=5500 used=0.08", got)
	}
}

func TestCreditTotalsSumNestedCategoryRemainWhenTotalMetricIsMissing(t *testing.T) {
	s := pluginStatistics{
		Metrics: []creditMetric{
			{Name: "usageBasicPackageCredit", Remain: 500, Amount: 500},
			{Name: "usageBonusPackageCredit", Remain: 4999.92, Amount: 5000, Used: 0.08},
		},
	}

	got, ok := s.creditTotals()
	if !ok {
		t.Fatal("creditTotals declined a category-only response")
	}
	if math.Abs(got.Remain-5499.92) > 0.0001 || math.Abs(got.Total-5500) > 0.0001 || math.Abs(got.Used-0.08) > 0.0001 {
		t.Fatalf("creditTotals = %+v, want summed remain=5499.92 total=5500 used=0.08", got)
	}
}

func TestAccountBalanceReportsRemainingTotalAndUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != statisticsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, statisticsPath)
		}
		_, _ = w.Write([]byte(`{"package":{"is_credit_package":true},"metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":5500,"package_credit_remain":5499.92,"package_credit_used":0.08}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK"}`)
	entries := c.pool.all()
	if len(entries) != 1 {
		t.Fatalf("pool entries = %d, want 1", len(entries))
	}

	bal, err := c.AccountBalance(context.Background(), entries[0].id(), 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 5499 || bal.Total != 5500 || math.Abs(bal.Used-0.08) > 0.0001 {
		t.Fatalf("balance = %+v, want credits=5499 total=5500 used=0.08", bal)
	}
}
