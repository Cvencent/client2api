package panel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestBalancesCarryUsedWhenTheModuleReportsIt(t *testing.T) {
	c := quotaClient("codearts", core.AccountRecord{ID: "a1"})
	c.balances["a1"] = core.Balance{Credits: 5499, Total: 5500, Used: 0.08, Unit: "CodeArts credit"}
	p, h := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})
	seedBalances(t, p, c)

	rec := get(t, h, "/panel/api/clients/codearts/balances")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	row := rowsOf(t, decodeMap(t, rec)["accounts"])[0]
	if row["used"] != float64(0.08) {
		t.Fatalf("used = %v, want 0.08", row["used"])
	}
}

// TestBalanceColumnShowsUsedCredit pins the shell side of the same number: the
// Go handler can relay used all day, but the column only shows it if the cell,
// its tooltip and the per-account toast read it.
func TestBalanceColumnShowsUsedCredit(t *testing.T) {
	src := poolStatsUISource(t)
	cell := poolStatsFuncBody(t, src, "accBalanceCell")
	for _, want := range []string{"Number(b.used)", "bal-used", "已用 "} {
		if !strings.Contains(cell, want) {
			t.Errorf("accBalanceCell does not show used credit: missing %q", want)
		}
	}
	if !strings.Contains(src, `msg += "，已用 " + b.used`) {
		t.Error("the per-account balance toast does not append used credit")
	}
}
