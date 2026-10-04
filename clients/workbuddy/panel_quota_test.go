package workbuddy

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// panel_quota_test.go proves the panel-facing credit/voucher mapping offline.
// Nothing here reaches the network: every case drives the shared fakeRT and the
// temp data directory, exactly like credits_test.go and school_test.go.
//
// These tests are deliberately about the mapping and the call count, not about
// "no error": a balance that loses its expiring bucket, a package that loses a
// field or an unknown account that still costs an upstream call are all silent
// regressions a no-error assertion would miss.

// panelUpstreamCalls is the number of requests the fake transport has seen.
func panelUpstreamCalls(rt *fakeRT) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.calls)
}

// TestWorkbuddyPanelAccountBalanceMapsEveryBucket checks that user-resource
// read-back lands on core.Balance unchanged, including the expiring bucket,
// and that a declined window is passed through rather than defaulted.
func TestWorkbuddyPanelAccountBalanceMapsEveryBucket(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Format(packageEndLayout)
	soon := time.Now().Add(72 * time.Hour).Format(packageEndLayout)
	late := time.Now().Add(30 * 24 * time.Hour).Format(packageEndLayout)
	expired := time.Now().Add(-24 * time.Hour).Format(packageEndLayout)
	// Two packages expire inside the 72h window (100 + 50), one never does, one
	// is already expired and one has no expiry at all.  The totals include every
	// row: 100+50+900+7+3 = 1060.
	accounts := `
	  {"CapacityRemain":100,"CapacitySize":100,"CycleEndTime":"` + future + `"},
	  {"CapacityRemain":50,"CapacitySize":50,"CycleEndTime":"` + soon + `"},
	  {"CapacityRemain":900,"CapacitySize":900,"CycleEndTime":"` + late + `"},
	  {"CapacityRemain":7,"CapacitySize":7,"CycleEndTime":"` + expired + `"},
	  {"CapacityRemain":3,"CapacitySize":3}`

	tests := []struct {
		name      string
		soon      time.Duration
		expiring  int64
		earliest  int64
		wantEarly bool
	}{
		{"the expiring bucket arrives intact", 72 * time.Hour, 150, 100, true},
		// A declined window (soon <= 0) gates only the expiring bucket.  The
		// earliest future expiry batch is a property of the read itself, so it
		// stays reported rather than being zeroed along with the window.
		{"a zero window declines the bucket but keeps the earliest reading", 0, 0, 100, true},
		{"a negative window is passed through too", -time.Hour, 0, 100, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, meterEnvelope(accounts)), nil
			}}
			c, _ := panelClient(t, rt, cnAccountFiles())

			got, err := c.AccountBalance(context.Background(), "uid-cn-0001", tc.soon)
			if err != nil {
				t.Fatalf("AccountBalance: %v", err)
			}
			if got.Credits != 1060 || got.Total != 1060 {
				t.Fatalf("credits/total = %d/%d, want 1060/1060 (every row counts)", got.Credits, got.Total)
			}
			if got.Expiring != tc.expiring {
				t.Fatalf("expiring = %d, want %d", got.Expiring, tc.expiring)
			}
			if got.EarliestRemaining != tc.earliest {
				t.Fatalf("earliestRemaining = %d, want %d", got.EarliestRemaining, tc.earliest)
			}
			if got.EarliestAt.IsZero() == tc.wantEarly {
				t.Fatalf("earliestAt = %s (zero=%v), want a future expiry: %v",
					got.EarliestAt, got.EarliestAt.IsZero(), tc.wantEarly)
			}
			// One read-through per balance call: the panel must not fan out.
			if n := panelUpstreamCalls(rt); n != 1 {
				t.Fatalf("made %d upstream call(s), want exactly 1", n)
			}
		})
	}
}

// TestWorkbuddyPanelQuotaUnknownAccountMakesNoUpstreamCall is the sibling of the
// "unknown ID" rule the panel relies on: the lookup is local, so a bad id must
// cost zero vendor calls and must still be an error.
func TestWorkbuddyPanelQuotaUnknownAccountMakesNoUpstreamCall(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		t.Errorf("an unknown account reached the vendor: %s %s", req.Method, req.URL)
		return jsonResponse(http.StatusBadGateway, `{"code":1,"msg":"unexpected"}`), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	ctx := context.Background()

	tests := []struct {
		name   string
		wantID string
		call   func() error
	}{
		{"AccountBalance", "nobody", func() error {
			_, err := c.AccountBalance(ctx, "nobody", time.Hour)
			return err
		}},
		{"AccountPackages", "nobody", func() error {
			_, err := c.AccountPackages(ctx, "nobody")
			return err
		}},
		{"AccountVouchers", "nobody", func() error {
			_, err := c.AccountVouchers(ctx, "nobody")
			return err
		}},
		{"a blank id", "", func() error {
			_, err := c.AccountBalance(ctx, "   ", time.Hour)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := panelUpstreamCalls(rt)
			err := tc.call()
			if err == nil {
				t.Fatalf("%s on an unknown account returned no error", tc.name)
			}
			if tc.wantID != "" && !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("error = %q, want it to name the id %q", err, tc.wantID)
			}
			if after := panelUpstreamCalls(rt); after != before {
				t.Fatalf("%s made %d upstream call(s), want 0", tc.name, after-before)
			}
		})
	}
}

// TestWorkbuddyPanelAccountPackagesMapsFieldForField pins the whole conversion:
// the panel's package is a different type, and a missed field would render as a
// zero value on the quota page.
func TestWorkbuddyPanelAccountPackagesMapsFieldForField(t *testing.T) {
	const accounts = `{
	  "PackageName":"Authorisation gift","PackageCode":"pkg-a","SubProductCode":"sub-a","SubProductName":"CodeBuddy",
	  "CapacityRemain":700,"CapacityUsed":300,"CapacitySize":1000,
	  "ExpiredTime":"2026-03-01 00:00:00","PackageEndTime":"2026-02-01 00:00:00","CycleEndTime":"2026-01-15 00:00:00",
	  "CreateTime":1767225600000
	},{
	  "PackageName":"Campaign reward","PackageCode":"pkg-b",
	  "CapacityRemain":0,"CapacityUsed":0,"CapacitySize":0,
	  "CycleCapacityRemain":90,"CycleCapacityUsed":10,"CycleCapacitySize":100,
	  "CycleEndTime":"2026-01-10 00:00:00"
	}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountPackages(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}

	expires := func(wall string) int64 {
		t.Helper()
		at, perr := time.ParseInLocation(packageEndLayout, wall, softRateResetLoc)
		if perr != nil {
			t.Fatalf("fixture end time %q does not parse: %v", wall, perr)
		}
		return at.UnixMilli()
	}
	want := core.PackageReport{
		Remain: 790,
		Size:   1100,
		Packages: []core.CreditPackage{
			{
				Name:           "Authorisation gift",
				Remain:         700,
				Used:           300,
				Size:           1000,
				EndTime:        "2026-03-01 00:00:00",
				ExpiresAt:      expires("2026-03-01 00:00:00"),
				CreatedAt:      time.UnixMilli(1767225600000).Format(time.RFC3339),
				PackageCode:    "pkg-a",
				SubProductCode: "sub-a",
				SubProductName: "CodeBuddy",
				Cycle:          false,
			},
			{
				Name:        "Campaign reward",
				Remain:      90,
				Used:        10,
				Size:        100,
				EndTime:     "2026-01-10 00:00:00",
				ExpiresAt:   expires("2026-01-10 00:00:00"),
				PackageCode: "pkg-b",
				Cycle:       true,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountPackages =\n  %+v\nwant\n  %+v", got, want)
	}
}

// An account with no packages holds an empty breakdown, not a nil one: the panel
// serialises this straight onto the quota page.
func TestWorkbuddyPanelAccountPackagesKeepsAnEmptyBreakdownEmpty(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope("")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountPackages(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if got.Packages == nil {
		t.Fatal("an account with no packages reported a nil breakdown, want an empty slice")
	}
	if len(got.Packages) != 0 || got.Remain != 0 || got.Size != 0 {
		t.Fatalf("report = %+v, want an empty 0/0 report", got)
	}
}

// TestWorkbuddyPanelAccountVouchersMapsFieldForField pins the voucher
// conversion the same way.
func TestWorkbuddyPanelAccountVouchersMapsFieldForField(t *testing.T) {
	const data = `{"items":[
	  {"grant_id":7,"draw_uuid":"d-7","sku_code":"sku-7","prize_name":"会员月卡","code":"WB-AAAA-1111",
	   "valid_from":"2026-01-01","valid_to":"2026-02-01","granted_at":"2026-01-02T03:04:05Z"},
	  {"grant_id":8,"prize_name":"积分","code":"WB-BBBB-2222"}
	]}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope(data)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountVouchers(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AccountVouchers: %v", err)
	}
	want := []core.Voucher{
		{
			GrantID:   7,
			DrawUUID:  "d-7",
			SKUCode:   "sku-7",
			PrizeName: "会员月卡",
			Code:      "WB-AAAA-1111",
			ValidFrom: "2026-01-01",
			ValidTo:   "2026-02-01",
			GrantedAt: "2026-01-02T03:04:05Z",
		},
		{GrantID: 8, PrizeName: "积分", Code: "WB-BBBB-2222"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountVouchers =\n  %+v\nwant\n  %+v", got, want)
	}
}

// No prizes is a fact, not a failure: an empty list and a nil error.
func TestWorkbuddyPanelAccountVouchersKeepsAnEmptyListEmpty(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope(`{"items":[]}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountVouchers(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AccountVouchers: %v", err)
	}
	if got == nil {
		t.Fatal("an account with no vouchers reported nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("got %d vouchers, want none", len(got))
	}
}

// A vendor refusal must survive as the vendor's own wording (the panel redacts
// it), and must not carry the account's bearer token.
func TestWorkbuddyPanelQuotaKeepsTheVendorWording(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, wbRefusal(11010, "the activity is over")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	_, err := c.AccountVouchers(context.Background(), "uid-cn-0001")
	if err == nil {
		t.Fatal("a refused voucher list returned no error")
	}
	if !strings.Contains(err.Error(), "11010") {
		t.Fatalf("error = %q, want the vendor's business code", err)
	}
	if strings.Contains(err.Error(), "access-token") {
		t.Fatalf("error = %q, want no credential in it", err)
	}
}
