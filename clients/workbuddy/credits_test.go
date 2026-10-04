package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// meterEnvelope wraps a `Response.Data.Accounts` list the way the billing meter
// really nests it (doJSON has already stripped the gateway envelope).
func meterEnvelope(accounts string) string {
	return wbEnvelope(`{"Response":{"Data":{"Accounts":[` + accounts + `]}}}`)
}

func TestWorkbuddyCreditPackagesReadsTheBreakdown(t *testing.T) {
	const accounts = `{
	  "PackageName":"Authorisation gift","PackageCode":"pkg-a","SubProductCode":"sub-a","SubProductName":"CodeBuddy",
	  "CapacityRemain":700,"CapacityUsed":300,"CapacitySize":1000,
	  "CycleCapacityRemain":0,"CycleCapacityUsed":0,"CycleCapacitySize":0,
	  "ExpiredTime":"2026-03-01 00:00:00","PackageEndTime":"2026-02-01 00:00:00","CycleEndTime":"2026-01-15 00:00:00",
	  "CreateTime":1767225600000
	},{
	  "PackageName":"Campaign reward","PackageCode":"pkg-b",
	  "CapacityRemain":0,"CapacityUsed":0,"CapacitySize":0,
	  "CycleCapacityRemain":90,"CycleCapacityUsed":10,"CycleCapacitySize":100,
	  "CycleEndTime":"2026-01-10 00:00:00"
	}`
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	packs, remain, size, err := c.CreditPackages(context.Background(), a)
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}

	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	// The CN realm only exposes the /v2 variant.
	if req.URL.Path != billingMeterPathV2 {
		t.Fatalf("path = %s, want %s", req.URL.Path, billingMeterPathV2)
	}
	body := wbBody(t, req)
	if body["ProductCode"] != "p_tcaca" || body["PageSize"] != float64(100) || body["PageNumber"] != float64(1) {
		t.Fatalf("query body = %v, want the p_tcaca page query", body)
	}
	if statuses, ok := body["Status"].([]any); !ok || len(statuses) != 2 {
		t.Fatalf("Status = %v, want a two-element list", body["Status"])
	}
	begin, _ := body["PackageEndTimeRangeBegin"].(string)
	end, _ := body["PackageEndTimeRangeEnd"].(string)
	if begin == "" || end == "" || end <= begin {
		t.Fatalf("end-time window = %q..%q, want a wide ascending window", begin, end)
	}

	if len(packs) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(packs), packs)
	}
	// Sorted by face value, descending: the 1000 pack first.
	gift := packs[0]
	if gift.Name != "Authorisation gift" || gift.Size != 1000 || gift.Remain != 700 || gift.Used != 300 {
		t.Fatalf("gift = %+v, want 700/300/1000", gift)
	}
	// ExpiredTime wins over PackageEndTime and CycleEndTime.
	if gift.EndTime != "2026-03-01 00:00:00" {
		t.Fatalf("EndTime = %q, want the ExpiredTime to win", gift.EndTime)
	}
	want, _ := time.ParseInLocation(packageEndLayout, "2026-03-01 00:00:00", softRateResetLoc)
	if gift.ExpiresAt != want.UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want %d", gift.ExpiresAt, want.UnixMilli())
	}
	if gift.CreatedAt != time.UnixMilli(1767225600000).Format(time.RFC3339) {
		t.Fatalf("CreatedAt = %q, want the RFC3339 form of CreateTime", gift.CreatedAt)
	}
	if gift.PackageCode != "pkg-a" || gift.SubProductCode != "sub-a" || gift.SubProductName != "CodeBuddy" {
		t.Fatalf("gift codes = %+v, want the origin identifiers", gift)
	}
	if gift.Cycle {
		t.Fatalf("gift.Cycle = true, want false (no cycle capacity)")
	}

	campaign := packs[1]
	if !campaign.Cycle {
		t.Fatalf("campaign.Cycle = false, want true for a CycleCapacitySize package")
	}
	if campaign.Size != 100 || campaign.Remain != 90 || campaign.Used != 10 {
		t.Fatalf("campaign = %+v, want 90/10/100", campaign)
	}
	if remain != 790 || size != 1100 {
		t.Fatalf("sums = %d/%d, want 790/1100", remain, size)
	}
}

// A package whose cycle is empty must not be mistaken for a cycle package, and
// the flat size-remain difference must be used when Used is not reported.
func TestWorkbuddyCreditPackagesInfersTheUsedField(t *testing.T) {
	const accounts = `{"PackageName":"Legacy","CapacityRemain":40,"CapacitySize":100,"CreateTime":0}`
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	packs, _, _, err := c.CreditPackages(context.Background(), wbCNAuth(t, c))
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("got %d packages, want 1", len(packs))
	}
	if packs[0].Used != 60 {
		t.Fatalf("Used = %d, want the inferred 60", packs[0].Used)
	}
	if packs[0].CreatedAt != "" {
		t.Fatalf("CreatedAt = %q, want empty for CreateTime=0", packs[0].CreatedAt)
	}
	if packs[0].EndTime != "" || packs[0].ExpiresAt != 0 {
		t.Fatalf("end time = %q/%d, want unset when upstream reports none", packs[0].EndTime, packs[0].ExpiresAt)
	}
}

// The global realm has two meter paths and the second must catch what the first
// refuses.
func TestWorkbuddyCreditPackagesFallsBackToTheSecondMeterPath(t *testing.T) {
	var paths []string
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.URL.Path == billingMeterPath {
			return jsonResponse(http.StatusBadGateway, `{"code":1,"msg":"no capacity here"}`), nil
		}
		return jsonResponse(200, meterEnvelope(`{"PackageName":"P","CapacityRemain":5,"CapacitySize":5}`)), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	packs, remain, size, err := c.CreditPackages(context.Background(), wbIntlAuth(t, c))
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 || remain != 5 || size != 5 {
		t.Fatalf("packs/remain/size = %+v/%d/%d, want one 5/5 pack", packs, remain, size)
	}
	if strings.Join(paths, ",") != billingMeterPath+","+billingMeterPathV2 {
		t.Fatalf("paths = %v, want the bare path then the /v2 path", paths)
	}
}

func TestWorkbuddyUserResourceDetailedReportsExpiringCredit(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Format(packageEndLayout)
	soon := time.Now().Add(72 * time.Hour).Format(packageEndLayout)
	late := time.Now().Add(30 * 24 * time.Hour).Format(packageEndLayout)
	expired := time.Now().Add(-24 * time.Hour).Format(packageEndLayout)
	accounts := `
	  {"CapacityRemain":100,"CapacitySize":100,"CycleEndTime":"` + future + `"},
	  {"CapacityRemain":50,"CapacitySize":50,"CycleEndTime":"` + soon + `"},
	  {"CapacityRemain":900,"CapacitySize":900,"CycleEndTime":"` + late + `"},
	  {"CapacityRemain":7,"CapacitySize":7,"CycleEndTime":"` + expired + `"},
	  {"CapacityRemain":3,"CapacitySize":3}`
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	remain, total, expiring, earliestAt, earliestRemaining, err :=
		c.UserResourceDetailedWithExpiry(context.Background(), a, 72*time.Hour)
	if err != nil {
		t.Fatalf("UserResourceDetailedWithExpiry: %v", err)
	}
	if remain != 1060 || total != 1060 {
		t.Fatalf("remain/total = %d/%d, want 1060/1060", remain, total)
	}
	// Only the package expiring inside 72h contributes.
	if expiring != 150 {
		t.Fatalf("expiring = %d, want 150 (100 + 50)", expiring)
	}
	if earliestAt.IsZero() {
		t.Fatal("earliestAt is zero, want the earliest future expiry")
	}
	if earliestRemaining != 100 {
		t.Fatalf("earliestRemaining = %d, want 100", earliestRemaining)
	}

	remain2, total2, err := c.UserResource(context.Background(), a)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain2 != 1060 || total2 != 1060 {
		t.Fatalf("UserResource = %d/%d, want 1060/1060", remain2, total2)
	}
}

// A narrow `soon` must leave the expiring bucket empty without touching the
// totals; the bucket is a subset of remain.
func TestWorkbuddyUserResourceDetailedExpiringIsASubset(t *testing.T) {
	late := time.Now().Add(30 * 24 * time.Hour).Format(packageEndLayout)
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(
			`{"CapacityRemain":10,"CapacitySize":10,"CycleEndTime":"`+late+`"}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	remain, _, expiring, err := c.UserResourceDetailed(context.Background(), wbCNAuth(t, c), time.Hour)
	if err != nil {
		t.Fatalf("UserResourceDetailed: %v", err)
	}
	if remain != 10 {
		t.Fatalf("remain = %d, want 10", remain)
	}
	if expiring != 0 {
		t.Fatalf("expiring = %d, want 0 with a one-hour window", expiring)
	}
}

func TestWorkbuddyPackageRemainUsed(t *testing.T) {
	tests := []struct {
		name               string
		in                 respAccount
		remain, used, size int64
	}{
		{
			"flat fields",
			respAccount{CapacityRemain: 40, CapacityUsed: 10, CapacitySize: 100},
			40, 10, 100,
		},
		{
			"flat fields with no used reported",
			respAccount{CapacityRemain: 40, CapacitySize: 100},
			40, 60, 100,
		},
		{
			"cycle fields win",
			respAccount{CapacityRemain: 999, CapacitySize: 999, CycleCapacityRemain: 25, CycleCapacitySize: 100},
			25, 75, 100,
		},
		{
			"cycle remain is clamped when upstream overstates it",
			respAccount{CycleCapacityRemain: 500, CycleCapacitySize: 100},
			100, 0, 100,
		},
		{
			"cycle used wins when it is the larger number",
			respAccount{CycleCapacityRemain: 90, CycleCapacityUsed: 50, CycleCapacitySize: 100},
			50, 50, 100,
		},
		{
			"a negative cycle remain is clamped to zero",
			respAccount{CycleCapacityRemain: -5, CycleCapacitySize: 100},
			0, 100, 100,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			remain, used, size := packageRemainUsed(tc.in)
			if remain != tc.remain || used != tc.used || size != tc.size {
				t.Fatalf("packageRemainUsed = %d/%d/%d, want %d/%d/%d",
					remain, used, size, tc.remain, tc.used, tc.size)
			}
		})
	}
}

func TestWorkbuddyParsePackageEndTime(t *testing.T) {
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, softRateResetLoc)
	got, ok := parsePackageEndTime("2026-02-01 00:00:00")
	if !ok {
		t.Fatal("a valid end time was rejected")
	}
	if !got.Equal(want) {
		t.Fatalf("parsed = %s, want %s", got, want)
	}
	if _, ok := parsePackageEndTime(""); ok {
		t.Fatal("an empty end time was accepted")
	}
	if _, ok := parsePackageEndTime("2026-02-01T00:00:00Z"); ok {
		t.Fatal("an RFC3339 end time was accepted; the upstream format is wall-clock")
	}
}

func TestWorkbuddyCreditPackagesSurfacesAParseFailure(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope(`"not an object"`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	if _, _, _, err := c.CreditPackages(context.Background(), wbCNAuth(t, c)); err == nil {
		t.Fatal("a non-object payload returned no error")
	} else if !strings.Contains(err.Error(), "packages parse") {
		t.Fatalf("error = %q, want it to name the parse step", err)
	}
}
