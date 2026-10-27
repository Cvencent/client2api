package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	if strings.Contains(src, `id="accSortNote"`) {
		t.Error("non-comparable column \"状态说明\" became sortable")
	}
}

func TestAccountPoolOmitsTheFieldsColumn(t *testing.T) {
	src := poolStatsUISource(t)
	for _, unwanted := range []string{"col-fields", "fieldSummary"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("account pool still carries the removed fields column: found %q", unwanted)
		}
	}
	start := strings.Index(src, `<table class="acc">`)
	if start < 0 {
		t.Fatal("account pool table not found")
	}
	end := strings.Index(src[start:], `</thead>`)
	if end < 0 {
		t.Fatal("account pool table header not closed")
	}
	if strings.Contains(src[start:start+end], "<th>字段</th>") {
		t.Error("account pool header still has a fields column")
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

// accSortSource 把排序状态机的几个顶层函数拼成一段可执行的 JS。这些函数都以
// 行首的 "}" 结束，所以 poolStatsFuncBody 截出来再补回 "}" 就能直接交给 node 跑。
// 排序方向这件事只有真跑起来才看得清（字符串匹配挡不住「某个方向永远走不到」），
// 所以下面几个用例的语义断言都走 node；没有 node 就跳过。
func accSortSource(t *testing.T, src string, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		b.WriteString(poolStatsFuncBody(t, src, name))
		b.WriteString("\n}\n")
	}
	return b.String()
}

// accSortNodeResult 是下面那段 node 脚本的返回形状。
type accSortNodeResult struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error"`
	Defaults map[string]string `json:"defaults"`
	Cycles   []struct {
		Col  string   `json:"col"`
		Def  string   `json:"def"`
		Dirs []string `json:"dirs"`
	} `json:"cycles"`
	SwitchCase []struct {
		Name string   `json:"name"`
		Dirs []string `json:"dirs"`
	} `json:"switchCase"`
	Headers []struct {
		Name  string `json:"name"`
		Cells []struct {
			Col  string `json:"col"`
			Aria string `json:"aria"`
			On   bool   `json:"on"`
			Mark string `json:"mark"`
		} `json:"cells"`
	} `json:"headers"`
}

// runAccSortStateMachine 用 node 把 accSortDefaultDir / accSortFor / syncAccSortHeader
// 拉出来实际跑一遍：十列的点击循环、换列后的重新起头、以及 aria-sort 的三种状态。
func runAccSortStateMachine(t *testing.T) accSortNodeResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the sort cycle and aria-sort behavior need it to run the inline functions")
	}
	src := poolStatsUISource(t)
	stateBlock, err := json.Marshal(accSortSource(t, src, "accSortDefaultDir", "accSortFor"))
	if err != nil {
		t.Fatalf("encoding the sort state machine: %v", err)
	}
	headerBlock, err := json.Marshal(accSortSource(t, src, "syncAccSortHeader"))
	if err != nil {
		t.Fatalf("encoding syncAccSortHeader: %v", err)
	}

	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const stateBlock = ` + string(stateBlock) + `;
  const headerBlock = ` + string(headerBlock) + `;
  const cols = ["account", "state", "models", "prio", "balance", "rate", "inflight", "usage", "last", "expiry"];
  // 每个场景都从空的 ACC_SORT 起，模拟「刚打开面板」。
  function sortMachine(state) {
    const ACC_SORT = state ? { c: state } : {};
    const CUR = "c";
    const accSort = n => ACC_SORT[n] || { col: "", dir: "" };
    const f = new Function("ACC_SORT", "CUR", "accSort", stateBlock + "\nreturn { accSortDefaultDir, accSortFor };")(ACC_SORT, CUR, accSort);
    return f;
  }
  const defaults = {};
  const cycles = [];
  for (const col of cols) {
    const f = sortMachine(null);
    defaults[col] = f.accSortDefaultDir(col);
    const dirs = [];
    // 连点五次，够看出「默认 → 反向 → 撤销 → 默认 → 反向」的完整循环。
    for (let i = 0; i < 5; i++) dirs.push(f.accSortFor(col).dir);
    cycles.push({ col: col, def: f.accSortDefaultDir(col), dirs: dirs });
  }
  // 换列必须重新起头：换到另一列时用那一列自己的默认方向，而不是接着上一列的方向。
  const sw = sortMachine(null);
  const seq = [];
  const click = c => { const r = sw.accSortFor(c); seq.push(c + ":" + (r.dir || "none")); };
  click("account"); click("account"); click("balance"); click("account");

  function mkTh(col) {
    const attrs = {}, cls = {}, mark = { textContent: "" };
    return {
      dataset: { sort: col }, attrs: attrs, cls: cls, mark: mark,
      classList: { toggle: (c, on) => { cls[c] = !!on; } },
      setAttribute: (k, v) => { attrs[k] = v; },
      querySelector: sel => (sel === ".sort-mark" ? mark : null),
    };
  }
  function headerCells(state) {
    const ths = cols.map(mkTh);
    const document = { querySelectorAll: () => ths };
    const ACC_SORT = state ? { c: state } : {};
    const CUR = "c";
    const accSort = n => ACC_SORT[n] || { col: "", dir: "" };
    const sync = new Function("ACC_SORT", "CUR", "accSort", "document", headerBlock + "\nreturn syncAccSortHeader;")(ACC_SORT, CUR, accSort, document);
    sync();
    return ths.map(th => ({ col: th.dataset.sort, aria: th.attrs["aria-sort"] || "", on: !!th.cls["sort-on"], mark: th.mark.textContent }));
  }
  const headers = [
    { name: "none", cells: headerCells(null) },
    { name: "balance desc", cells: headerCells({ col: "balance", dir: "desc" }) },
    { name: "account asc", cells: headerCells({ col: "account", dir: "asc" }) },
  ];
  fs.writeFileSync(out, JSON.stringify({ ok: true, defaults: defaults, cycles: cycles, switchCase: [{ name: "account x2, balance, account", dirs: seq }], headers: headers }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "acc-sort.js")
	result := filepath.Join(dir, "acc-sort.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run the sort state machine: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got accSortNodeResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the sort state machine threw: %s", got.Error)
	}
	return got
}

// TestAccountPoolSortUsesColumnDirectionStateAndCyclesThreeWays 钉住「每个列头都
// 能升序、也能降序」这条用户可见的契约。早先的实现在这里匹配的是一行固定写死的
// 表达式（asc → desc → 撤销），恰好就是 bug 本身：默认方向是 desc 的列第二次点击
// 就撤销，asc 永远走不到。现在改成真跑一遍状态机，断言三态循环与「两个方向都可达」。
func TestAccountPoolSortUsesColumnDirectionStateAndCyclesThreeWays(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `const ACC_SORT = {};`) {
		t.Error("account sort state is not remembered per client")
	}
	if !strings.Contains(src, `{ col: "", dir: "" }`) {
		t.Error("account sort state is not initialized as {col, dir}")
	}
	// 默认方向必须由 accSortDefaultDir 决定，不能被写死成一个固定循环。
	if !strings.Contains(poolStatsFuncBody(t, src, "accSortFor"), "accSortDefaultDir(col)") {
		t.Error("accSortFor no longer derives its starting direction from the column default")
	}

	got := runAccSortStateMachine(t)
	if len(got.Cycles) != len(accountSortHeaders()) {
		t.Fatalf("state machine exercised %d columns, want %d", len(got.Cycles), len(accountSortHeaders()))
	}
	for _, c := range got.Cycles {
		base, flip := c.Def, "asc"
		if c.Def == "asc" {
			flip = "desc"
		}
		// 默认 → 反向 → 撤销 → 默认 → 反向：两个方向都必须在这条循环里出现。
		want := []string{base, flip, "", base, flip}
		if !slices.Equal(c.Dirs, want) {
			t.Errorf("column %s (default %s) cycles %q, want %q: one of the two directions is unreachable", c.Col, c.Def, c.Dirs, want)
		}
		if !slices.Contains(c.Dirs, "asc") || !slices.Contains(c.Dirs, "desc") {
			t.Errorf("column %s never reaches both directions: %q", c.Col, c.Dirs)
		}
	}

	// 换列要重新起头：新列用自己的默认方向，而不是接着上一列的方向。
	var switchDirs []string
	for _, c := range got.SwitchCase {
		switchDirs = c.Dirs
	}
	want := []string{"account:asc", "account:desc", "balance:desc", "account:asc"}
	if !slices.Equal(switchDirs, want) {
		t.Errorf("switching columns keeps a stale direction: got %q, want %q", switchDirs, want)
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

// TestAccountPoolSortDefaultsAndNullValues 守两件事：每列首次点击的方向符合
// 「默认看最该看的那些行」（账号按名字升序、余额从多到少…），以及比较主体对 null
// 的处理不变（缺值的行永远沉底，且同值时保持原顺序）。
func TestAccountPoolSortDefaultsAndNullValues(t *testing.T) {
	src := poolStatsUISource(t)

	got := runAccSortStateMachine(t)
	want := map[string]string{
		"account": "asc", "state": "asc", "models": "asc", "prio": "asc",
		"balance": "desc", "rate": "desc", "inflight": "desc",
		"usage": "desc", "last": "desc", "expiry": "asc",
	}
	for col, dir := range want {
		if got.Defaults[col] != dir {
			t.Errorf("accSortDefaultDir(%q) = %q, want %q", col, got.Defaults[col], dir)
		}
	}
	if len(got.Defaults) != len(accountSortHeaders()) {
		t.Errorf("accSortDefaultDir answered %d columns, header set has %d", len(got.Defaults), len(accountSortHeaders()))
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

// TestAccountPoolSortHeaderStateReflectsActiveColumnOnly 检查列头显示：只有当前
// 排序列带 sort-on 和方向箭头，其他列一律回"↕"。aria-sort 之前把命中列一律写成
// "ascending"，降序时读屏软件会念反，所以这里直接跑真实状态校验三种取值。
func TestAccountPoolSortHeaderStateReflectsActiveColumnOnly(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "syncAccSortHeader")

	for _, want := range []string{
		`const sort = accSort(CUR);`,
		`querySelectorAll("th[data-sort]")`,
		`th.dataset.sort === sort.col`,
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

	// 行为断言：aria-sort 必须报真实方向，未排序列一律 "none"。
	got := runAccSortStateMachine(t)
	cases := map[string]struct {
		onCol string
		aria  string
		mark  string
	}{
		"none":         {"", "none", "↕"},
		"balance desc": {"balance", "descending", "↓"},
		"account asc":  {"account", "ascending", "↑"},
	}
	if len(got.Headers) != len(cases) {
		t.Fatalf("node returned %d header scenarios, want %d", len(got.Headers), len(cases))
	}
	for _, h := range got.Headers {
		want, ok := cases[h.Name]
		if !ok {
			t.Fatalf("unexpected header scenario %q", h.Name)
		}
		if len(h.Cells) != len(accountSortHeaders()) {
			t.Errorf("%s: %d header cells, want %d", h.Name, len(h.Cells), len(accountSortHeaders()))
		}
		for _, cell := range h.Cells {
			isOn := cell.Col == want.onCol
			wantAria := "none"
			wantMark := "↕"
			if isOn {
				wantAria = want.aria
				wantMark = want.mark
			}
			if cell.Aria != wantAria {
				t.Errorf("%s: column %s aria-sort = %q, want %q", h.Name, cell.Col, cell.Aria, wantAria)
			}
			if cell.On != isOn {
				t.Errorf("%s: column %s sort-on = %v, want %v", h.Name, cell.Col, cell.On, isOn)
			}
			if cell.Mark != wantMark {
				t.Errorf("%s: column %s arrow = %q, want %q", h.Name, cell.Col, cell.Mark, wantMark)
			}
		}
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
