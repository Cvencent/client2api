package panel

import (
	"strings"
	"testing"
)

func accountSortHeaders() []struct {
	ID    string
	Col   string
	Label string
} {
	return []struct {
		ID    string
		Col   string
		Label string
	}{
		{"accSortAccount", "account", "账号"},
		{"accSortState", "state", "状态"},
		{"accSortModels", "models", "模型"},
		{"accSortPrio", "prio", "优先级"},
		{"accSortBal", "balance", "余额"},
		{"accSortRate", "rate", "成功 / 失败"},
		{"accSortInflight", "inflight", "在途"},
		{"accSortUsage", "usage", "用量"},
		{"accSortLast", "last", "最近成功"},
		{"accSortExpiry", "expiry", "到期"},
	}
}

func TestAccountPoolSortableHeadersCoverComparableColumns(t *testing.T) {
	src := poolStatsUISource(t)

	for _, h := range accountSortHeaders() {
		idAt := strings.Index(src, `id="`+h.ID+`"`)
		if idAt < 0 {
			t.Errorf("sortable header #%s is missing", h.ID)
			continue
		}
		start := strings.LastIndex(src[:idAt], "<th")
		if start < 0 {
			t.Errorf("sortable header #%s has no opening <th>", h.ID)
			continue
		}
		end := strings.Index(src[start:], "</th>")
		if end < 0 {
			t.Fatalf("sortable header #%s has no closing </th>", h.ID)
		}
		tag := src[start : start+end]
		for _, want := range []string{
			"sortable",
			`data-sort="` + h.Col + `"`,
			`tabindex="0"`,
			`aria-sort="none"`,
			`<span class="sort-mark" aria-hidden="true">`,
			h.Label,
		} {
			if !strings.Contains(tag, want) {
				t.Errorf("sortable header #%s is missing %q", h.ID, want)
			}
		}
		if strings.Contains(tag, `role="button"`) {
			t.Errorf("sortable header #%s overrides the table column role", h.ID)
		}
	}

	for _, nonSortable := range []string{"状态说明", "<th>字段</th>"} {
		if strings.Contains(src, `id="accSortNote"`) || strings.Contains(src, `id="accSortFields"`) {
			t.Errorf("non-comparable column %q became sortable", nonSortable)
		}
	}
}

func TestAccountPoolStateFilterOffersAccountStateBuckets(t *testing.T) {
	src := poolStatsUISource(t)
	start := strings.Index(src, `id="accStateFilter"`)
	if start < 0 {
		t.Fatal(`account pool toolbar has no state filter (#accStateFilter)`)
	}
	end := strings.Index(src[start:], "</select>")
	if end < 0 {
		t.Fatal(`#accStateFilter has no closing </select>`)
	}
	sel := src[start : start+end]

	for _, want := range []string{
		`<option value="">全部</option>`,
		`<option value="ready">可用</option>`,
		`<option value="cooling">冷却中</option>`,
		`<option value="risk">风控</option>`,
		`<option value="fault">账号异常</option>`,
		`<option value="disabled">已停用</option>`,
		`<option value="unknown">未知</option>`,
	} {
		if !strings.Contains(sel, want) {
			t.Errorf("state filter is missing %q", want)
		}
	}
	for _, bad := range []string{`value="banned"`, `value="invalid"`, `value="exhausted"`} {
		if strings.Contains(sel, bad) {
			t.Errorf("state filter exposes a raw module state %q instead of an accountState bucket", bad)
		}
	}
}

func TestAccountPoolShownGroupsFiltersByGroupedStateThenSearchesThenSorts(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accShownGroups")

	for _, want := range []string{
		`const state = accStateFilter();`,
		`accGroups(rec.accounts || [])`,
		`g.rows.some(a => matchText(a, q))`,
		`accountState(accGroupStat(g)).key === state`,
		`return accSortGroups(groups, accSort(CUR));`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accShownGroups is missing %q", want)
		}
	}
	if strings.Index(body, "accountState(accGroupStat(g)).key === state") > strings.Index(body, "matchText(a, q)") {
		t.Error("state filtering should happen before search filtering")
	}
}

func TestAccountPoolSortUsesColumnDirectionStateAndCyclesThreeWays(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `const ACC_SORT = {};`) {
		t.Error("account sort state is not remembered per client")
	}
	if !strings.Contains(src, `{ col: "", dir: "" }`) {
		t.Error("account sort state is not initialized as {col, dir}")
	}

	sortBody := poolStatsFuncBody(t, src, "accSortFor")
	for _, want := range []string{
		`accSort(CUR)`,
		`col === x.col`,
		`x.dir ? (x.dir === "asc" ? "desc" : "") : accSortDefaultDir(col)`,
		`if (next) ACC_SORT[CUR] = { col: col, dir: next }; else delete ACC_SORT[CUR];`,
	} {
		if !strings.Contains(sortBody, want) {
			t.Errorf("accSortFor is missing %q", want)
		}
	}

	clickBody := poolStatsFuncBody(t, src, "accSortBy")
	for _, want := range []string{
		`accSortFor(col)`,
		`ACC_PAGE = 1`,
		`renderAccounts()`,
		`wrap.scrollTop = 0`,
	} {
		if !strings.Contains(clickBody, want) {
			t.Errorf("accSortBy is missing %q", want)
		}
	}
	for _, want := range []string{
		`ev.target.closest("th[data-sort]")`,
		`keydown`,
		`ev.key !== "Enter" && ev.key !== " "`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("sort header event wiring is missing %q", want)
		}
	}
}

func TestAccountPoolSortDefaultsAndNullValues(t *testing.T) {
	src := poolStatsUISource(t)

	defaults := poolStatsFuncBody(t, src, "accSortDefaultDir")
	for _, c := range []struct{ col, dir string }{
		{"account", "asc"}, {"state", "asc"}, {"models", "asc"}, {"prio", "asc"},
		{"balance", "desc"}, {"rate", "desc"}, {"inflight", "desc"},
		{"usage", "desc"}, {"last", "desc"}, {"expiry", "asc"},
	} {
		want := `"` + c.col + `"`
		if !strings.Contains(defaults, want) {
			t.Errorf("accSortDefaultDir has no default for %s", c.col)
		}
	}
	if !strings.Contains(defaults, `return "asc"`) || !strings.Contains(defaults, `return "desc"`) {
		t.Error("accSortDefaultDir does not define both asc and desc defaults")
	}

	sortBody := poolStatsFuncBody(t, src, "accSortGroups")
	for _, want := range []string{
		`const av = accSortValue(a.g, col), bv = accSortValue(b.g, col);`,
		`if (av == null && bv == null) return a.i - b.i;`,
		`if (av == null) return 1;`,
		`if (bv == null) return -1;`,
		`if (av === bv) return a.i - b.i;`,
		`const cmp = av < bv ? -1 : 1;`,
		`return dir === "asc" ? cmp : -cmp;`,
	} {
		if !strings.Contains(sortBody, want) {
			t.Errorf("accSortGroups is missing %q", want)
		}
	}
}

func TestAccountPoolSortValueExtractsEachColumn(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accSortValue")

	for _, want := range []string{
		`case "account":`,
		`accountIdentity(g.rows[0])`,
		`case "state":`,
		`accountState(accGroupStat(g)).label`,
		`case "models":`,
		`case "prio":`,
		`case "balance":`,
		`accGroupBalanceValue(g.rows)`,
		`case "rate":`,
		`accGroupRateValue(g.rows)`,
		`case "inflight":`,
		`accGroupInflightValue(g.rows)`,
		`case "usage":`,
		`accGroupUsageValue(g.rows)`,
		`case "last":`,
		`accGroupLastValue(g.rows)`,
		`case "expiry":`,
		`accGroupExpiryValue(g.rows)`,
		`return null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accSortValue is missing %q", want)
		}
	}
}

func TestAccountPoolGroupSummarySortFunctionsConsumeRowsArgument(t *testing.T) {
	src := poolStatsUISource(t)

	for _, name := range []string{
		"accGroupRateValue",
		"accGroupInflightValue",
		"accGroupUsageValue",
		"accGroupLastValue",
		"accGroupExpiryValue",
	} {
		body := poolStatsFuncBody(t, src, name)
		if !strings.HasPrefix(body, "function "+name+"(rows) {") {
			t.Errorf("%s must accept the rows array passed by accSortValue", name)
		}
		if !strings.Contains(body, "(rows || []).forEach") {
			t.Errorf("%s must iterate its rows argument, not a group object's missing .rows field", name)
		}
		if strings.Contains(body, "(g.rows || []).forEach") {
			t.Errorf("%s still reads a missing g.rows field", name)
		}
	}
}

func TestAccountPoolSortHeaderStateReflectsActiveColumnOnly(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "syncAccSortHeader")

	for _, want := range []string{
		`const sort = accSort(CUR);`,
		`querySelectorAll("th[data-sort]")`,
		`th.dataset.sort === sort.col`,
		`on ? "ascending" : sort.dir === "desc" ? "descending" : "none"`,
		`th.classList.toggle("sort-on", on)`,
		`mark.textContent = on ? (sort.dir === "asc" ? "↑" : "↓") : "↕"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("syncAccSortHeader is missing %q", want)
		}
	}
	if !strings.Contains(poolStatsFuncBody(t, src, "renderAccounts"), "syncAccSortHeader(CUR)") {
		t.Error("renderAccounts does not refresh the visible sort state")
	}
}

func TestAccountPoolPassiveRerenderDoesNotResetPageOrScroll(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderAccounts")

	if strings.Contains(body, "ACC_PAGE = 1; renderAccounts") || strings.Contains(body, "ACC_PAGE = 1;\n  renderAccounts") {
		t.Error("renderAccounts resets the page during passive polling; only user actions should reset it")
	}
	if !strings.Contains(body, "if (ACC_PAGE < 1) ACC_PAGE = 1;") {
		t.Error("renderAccounts no longer clamps an invalid page back into range")
	}
	if strings.Contains(body, "scrollTop = 0") {
		t.Error("renderAccounts resets the table scroll during passive polling; only user actions should scroll to top")
	}
}
