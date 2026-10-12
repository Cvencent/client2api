package autoclaw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAccountBalanceListsEachWallet pins the second-balance contract: AutoClaw
// reports a summed credit total plus a list of displayable wallets. The panel
// mirrors the desktop client by showing the total as the primary balance and
// each wallet as a line, so a non-credit wallet (a token grant) is surfaced
// when the vendor actually returns one. Hidden wallets never leak.
func TestAccountBalanceListsEachWallet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathWalletsV2 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":1500,"wallets":[
		  {"public_wallet_type":"daily","display_name":"每日积分","balance":500,"display":true},
		  {"public_wallet_type":"token","display_name":"Token","balance":1000,"display":true},
		  {"public_wallet_type":"hidden","display_name":"Hidden","balance":9,"display":false}
		]}}`))
	}))
	defer srv.Close()

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.load([]account{{credential: credential{AccessToken: "tok-a", AccountID: "a1"}}})
	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, pool: p}

	bal, err := c.AccountBalance(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 1500 || bal.Total != 1500 || bal.Unit != "积分" {
		t.Fatalf("primary balance = %+v, want total 1500 积分", bal)
	}
	got := map[string]int64{}
	for _, line := range bal.Lines {
		if line.Amount == nil {
			t.Fatalf("line %q has no amount", line.Label)
		}
		got[line.Label] = *line.Amount
	}
	if got["Token"] != 1000 {
		t.Fatalf("balance lines = %+v, want the Token wallet at 1000", bal.Lines)
	}
	if got["每日积分"] != 500 {
		t.Fatalf("balance lines = %+v, want the daily credits wallet at 500", bal.Lines)
	}
	if _, ok := got["Hidden"]; ok {
		t.Fatalf("a hidden wallet leaked into the balance lines: %+v", bal.Lines)
	}
}
