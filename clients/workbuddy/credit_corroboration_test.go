package workbuddy

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// credit_corroboration_test.go pins the rule that decides whether a wallet
// reading may be trusted.  The upstream 体验版 package keeps advertising a full
// lifetime CapacityRemain after its cycle is spent, and the reply's own
// TotalDosage copies that stale figure, so a raw zero from this vendor is not
// proof that the account is empty.  These tests drive the real decode in
// ReadCredit, not just the predicate, because the panic the predicate guards
// against would otherwise only surface on a live account.

func TestReadCreditDistrustsASelfContradictingTrialPackage(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour).Format(packageEndLayout)
	// The observed trial shape: the cycle view is spent (0/500) while the
	// lifetime view still claims the full 500.
	accounts := `{
	  "CapacityRemain":500,"CapacityUsed":0,"CapacitySize":500,
	  "CycleCapacityRemain":0,"CycleCapacityUsed":500,"CycleCapacitySize":500,
	  "ExpiredTime":"","CycleEndTime":"` + future + `"
	}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	rep, err := c.ReadCredit(context.Background(), wbCNAuth(t, c), 0)
	if err != nil {
		t.Fatalf("ReadCredit: %v", err)
	}
	// The conservative cycle number is still what the panel shows...
	if rep.Remain != 0 || rep.Total != 500 {
		t.Fatalf("remain/total = %d/%d, want 0/500", rep.Remain, rep.Total)
	}
	// ...but it must not be treated as a known-empty wallet.
	if rep.Corroborated {
		t.Fatal("a self-contradicting trial package was trusted; a working account would be parked")
	}
}

func TestReadCreditTrustsAWalletThatAgreesWithItself(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour).Format(packageEndLayout)
	// Both views report the package is spent: nothing contradicts the zero.
	spent := `{
	  "CapacityRemain":0,"CapacityUsed":500,"CapacitySize":500,
	  "CycleCapacityRemain":0,"CycleCapacityUsed":500,"CycleCapacitySize":500,
	  "CycleEndTime":"` + future + `"
	}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(spent)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	rep, err := c.ReadCredit(context.Background(), wbCNAuth(t, c), 0)
	if err != nil {
		t.Fatalf("ReadCredit: %v", err)
	}
	if rep.Remain != 0 {
		t.Fatalf("remain = %d, want 0", rep.Remain)
	}
	if !rep.Corroborated {
		t.Fatal("an agreeing zero was distrusted; the account should stay parked")
	}
}

func TestReadCreditTrustsAFundedWallet(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour).Format(packageEndLayout)
	funded := `{
	  "CapacityRemain":500,"CapacityUsed":0,"CapacitySize":500,
	  "CycleCapacityRemain":500,"CycleCapacityUsed":0,"CycleCapacitySize":500,
	  "CycleEndTime":"` + future + `"
	}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(funded)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	rep, err := c.ReadCredit(context.Background(), wbCNAuth(t, c), 0)
	if err != nil {
		t.Fatalf("ReadCredit: %v", err)
	}
	if rep.Remain != 500 || rep.Total != 500 {
		t.Fatalf("remain/total = %d/%d, want 500/500", rep.Remain, rep.Total)
	}
	if !rep.Corroborated {
		t.Fatal("a funded wallet was distrusted")
	}
}

// A stale lifetime figure on an *expired* package is ordinary: the package is
// gone, so its contradiction says nothing about the wallet, and the zero is
// still trustworthy.
func TestReadCreditTrustsAZeroOnAnExpiredPackage(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour).Format(packageEndLayout)
	accounts := `{
	  "CapacityRemain":500,"CapacityUsed":0,"CapacitySize":500,
	  "CycleCapacityRemain":0,"CycleCapacityUsed":500,"CycleCapacitySize":500,
	  "CycleEndTime":"` + past + `"
	}`
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, meterEnvelope(accounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	rep, err := c.ReadCredit(context.Background(), wbCNAuth(t, c), 0)
	if err != nil {
		t.Fatalf("ReadCredit: %v", err)
	}
	if !rep.Corroborated {
		t.Fatal("an expired package's stale lifetime figure was treated as a live contradiction")
	}
}

func TestCreditViewsDisagree(t *testing.T) {
	tests := []struct {
		name string
		in   respAccount
		want bool
	}{
		{
			"trial package: spent cycle, full lifetime",
			respAccount{CapacityRemain: 500, CapacitySize: 500, CycleCapacityRemain: 0, CycleCapacityUsed: 500, CycleCapacitySize: 500},
			true,
		},
		{
			"both views agree on zero",
			respAccount{CapacityRemain: 0, CapacitySize: 500, CycleCapacityRemain: 0, CycleCapacityUsed: 500, CycleCapacitySize: 500},
			false,
		},
		{
			"cycle still has room",
			respAccount{CapacityRemain: 500, CapacitySize: 500, CycleCapacityRemain: 25, CycleCapacitySize: 500},
			false,
		},
		{
			"no cycle package, nothing to contradict",
			respAccount{CapacityRemain: 0, CapacitySize: 500},
			false,
		},
		{
			"lifetime overstated with no cycle size",
			respAccount{CapacityRemain: 500, CapacitySize: 0},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := creditViewsDisagree(tc.in); got != tc.want {
				t.Fatalf("creditViewsDisagree = %v, want %v", got, tc.want)
			}
		})
	}
}
