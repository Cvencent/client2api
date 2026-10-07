package panel

import (
	"strings"
	"testing"
)

func TestAccountPoolBalanceHeaderIsSortable(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		`id="accSortBal"`,
		`class="c-bal sortable"`,
		`aria-label="按余额排序"`,
		`tabindex="0"`,
		`aria-sort="none"`,
		`<span class="sort-mark" aria-hidden="true">`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("balance sort header is missing %q", want)
		}
	}
	if !strings.Contains(src, "#view-accounts table.acc th.sortable") {
		t.Error("balance sort header has no interactive style")
	}
}

func TestAccountPoolBalanceSortHeaderKeepsColumnHeaderSemantics(t *testing.T) {
	src := poolStatsUISource(t)
	start := strings.Index(src, `id="accSortBal"`)
	if start < 0 {
		t.Fatal("balance sort header is missing")
	}
	end := strings.Index(src[start:], "</th>")
	if end < 0 {
		t.Fatal("balance sort header has no closing th")
	}
	tag := src[start : start+end]
	if strings.Contains(tag, `role="button"`) {
		t.Error("balance sort header overrides the table column role")
	}
	if !strings.Contains(tag, `aria-sort="none"`) {
		t.Error("balance sort header lost its aria-sort state")
	}
}
func TestAccountPoolBalanceSortRunsAfterSearchFiltering(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accShownGroups")

	if !strings.Contains(body, "accSortGroups(groups, accSortDir(CUR))") {
		t.Error("accShownGroups does not sort the filtered account groups by balance")
	}
}

func TestAccountPoolBalanceSortHandlesUnknownBalances(t *testing.T) {
	src := poolStatsUISource(t)

	value := poolStatsFuncBody(t, src, "accGroupBalanceValue")
	for _, want := range []string{
		`if (b.unlimited) return Number.POSITIVE_INFINITY;`,
		`if (b.credits == null) continue;`,
		`const n = Number(b.credits);`,
		`Number.isFinite(n) ? n : null`,
	} {
		if !strings.Contains(value, want) {
			t.Errorf("accGroupBalanceValue is missing %q", want)
		}
	}

	sortBody := poolStatsFuncBody(t, src, "accSortGroups")
	for _, want := range []string{
		`av == null && bv == null`,
		`if (av == null) return 1;`,
		`if (bv == null) return -1;`,
		`return dir > 0 ? av - bv : bv - av;`,
	} {
		if !strings.Contains(sortBody, want) {
			t.Errorf("accSortGroups is missing %q", want)
		}
	}
	if !strings.Contains(src, `const ACC_SORT = {};`) {
		t.Error("account balance sort direction is not remembered per client")
	}
}

func TestAccountPoolBalanceSortCyclesAscendingDescendingDefault(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accSortBalance")

	for _, want := range []string{
		`const cur = accSortDir(CUR);`,
		`cur === "asc" ? "desc" : cur === "desc" ? "" : "asc"`,
		`delete ACC_SORT[CUR]`,
		`ACC_PAGE = 1`,
		`renderAccounts()`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accSortBalance is missing %q", want)
		}
	}

	for _, want := range []string{
		`$("#accSortBal").addEventListener("click", accSortBalance)`,
		`$("#accSortBal").addEventListener("keydown"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("balance sort control is not wired: missing %q", want)
		}
	}
}

func TestAccountPoolBalanceSortReflectsHeaderState(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "syncAccSortHeader")

	for _, want := range []string{
		`const dir = accSortDir(CUR);`,
		`dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none"`,
		`th.classList.toggle("sort-on", !!dir)`,
		`mark.textContent = dir === "asc" ? "↑" : dir === "desc" ? "↓" : "↕"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("syncAccSortHeader is missing %q", want)
		}
	}
	if !strings.Contains(poolStatsFuncBody(t, src, "renderAccounts"), "syncAccSortHeader(CUR)") {
		t.Error("renderAccounts does not refresh the visible balance sort state")
	}
}
