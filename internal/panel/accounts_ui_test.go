package panel

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号按「账号身份」分组的界面回归测试。
//
// 背景：面板列的一直是「凭据」，不是「账号」。zcode 的同一个账号在
// config.json 里存了两份名字不同的凭据，kimi 的同一个 user_id 有网页登录和 CLI
// 两条通道——于是列表里出现三行，而操作员以为有三个账号。修法是给每条记录带上
// 厂商自己的账号 id（AccountRecord.Identity），前端按它分组：一个账号一行，
// 它下面的每个登录方式各占一个子行，各自还能单独启停、单独测试。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到；shell_test.go 只保证
// $() 指的 id 存在。所以这里静态钉住四件事：
//   1. 界面读的键 == core 侧两个 struct 的 json tag（错一个字母 = 永远分不了组，
//      而且两边都不报错，界面看起来完全正常）；
//   2. 分组函数真的被三处计数用上了（导航角标、统计磁贴、accNote），而不是
//      只定义了没人调用；
//   3. 单通道的账号渲染得和以前一模一样——另外五个模块不能因为这次改动变样；
//   4. 组头那一行没有按钮（每个通道单独控制是需求；一个批量按钮会把两份凭据的
//      状态混在一起），以及 identity 跟着别的字段一起脱敏；
//   5. 多通道账号默认收起（collapseWiring）——展开状态存在一个跨重画的集合里，
//      收起的通道行是 hidden 而不是没渲染，组头整行可点、键盘也能开合。
// ---------------------------------------------------------------------------

// accountIdentityJSONTag 从一份 Go 源码里取出 `Identity string` 字段的 json tag。
//
// 单独拎出来是为了能做负控：把 tag 改名后这个函数必须报出不同的值，否则下面
// 「界面读的键 == Go 的 tag」那条断言可能只是在自证。
func accountIdentityJSONTag(src string) (string, bool) {
	m := regexp.MustCompile("Identity string\\s+`json:\"([a-zA-Z_]+)").FindStringSubmatch(src)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// accountGroupingWiring 返回分组链路里缺失的环节。返回空切片表示全都在。
//
// 之所以返回列表而不是就地断言，是为了让负控（TestAccountGroupingGuardCatches
// ARenamedHelper）能用同一段代码证明它真的会红。
func accountGroupingWiring(t *testing.T, src string) []string {
	t.Helper()
	var miss []string
	for _, c := range []struct{ what, want string }{
		{"accGroups 没有定义", "function accGroups("},
		{"accGroupStat 没有定义", "function accGroupStat("},
		{"accRowHTML 没有定义", "function accRowHTML("},
		// accGroups 必须真的读 a.identity，否则分组永远只有一个「未知」桶。
		{"accGroups 没有读 a.identity", "a.identity"},
		// 三处计数：导航角标、统计磁贴、accNote 的行数。
		// The chip badge is now "usable/total", but both halves must still come
		// from the grouped account list rather than the raw credential rows.
		{"renderChips 的角标没有按账号数分组", "const groups = accGroups(ACCTS[n] && ACCTS[n].accounts);"},
		{"renderStats 没有按账号数统计", "accGroups(ACCTS[n] && ACCTS[n].accounts).forEach"},
		{"renderAccounts 没有按账号分组", "const groups = accGroups(list);"},
		// 单通道的账号走同一条行模板，且标记为「不是通道」——这样另外五个模块
		// 的表格和改动前逐字一致。
		{"单通道账号没有走 accRowHTML", "return accRowHTML(g.rows[0], caps, acts, showBal, false);"},
		{"多通道账号的子行没有标记成通道", "accRowHTML(a, caps, acts, showBal, true, open, g.key)"},
		{"accRowHTML 没有区分通道行", `' class="acc-chan" data-chan="'`},
	} {
		if !strings.Contains(src, c.want) {
			miss = append(miss, c.what+"（缺 "+c.want+"）")
		}
	}
	return miss
}

// groupHeaderTemplate 取出 renderAccounts 里组头那一行的模板片段。
func groupHeaderTemplate(t *testing.T, src string) string {
	t.Helper()
	body := poolStatsFuncBody(t, src, "renderAccounts")
	start := strings.Index(body, `'<tr class="acc-grp" data-grp="'`)
	if start < 0 {
		t.Fatal(`renderAccounts 里找不到组头模板 '<tr class="acc-grp" data-grp="'`)
	}
	rest := body[start:]
	end := strings.Index(rest, "'</tr>'")
	if end < 0 {
		t.Fatal("组头模板看起来没结束")
	}
	return rest[:end]
}

// TestAccountIdentityKeyMatchesTheGoStructs 是本文件唯一的跨包一致性检查：
// 界面读 a.identity，Go 侧必须真的把 Identity 序列化成 "identity"。两个 struct
// 都要有——AccountRecord 是账号列表的来源，AccountStatus 是别的接口用的同名字段。
func TestAccountIdentityKeyMatchesTheGoStructs(t *testing.T) {
	// 界面读的键由 accGroups 决定；这里顺带确认它确实读了 a.identity。
	if src := poolStatsUISource(t); !strings.Contains(poolStatsFuncBody(t, src, "accGroups"), "a.identity") {
		t.Error("accGroups 没有读 a.identity：分组会永远只有一个「未知」桶")
	}

	for _, path := range []string{"../core/accounts.go", "../core/core.go"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("cannot read %s: %v", path, err)
		}
		key, ok := accountIdentityJSONTag(string(b))
		if !ok {
			t.Errorf("%s 里没有 Identity 字段：界面读的 a.identity 永远是 undefined", path)
			continue
		}
		if key != "identity" {
			t.Errorf("%s 的 Identity 序列化成 %q，界面读的是 \"identity\"：分组会永远只有一个桶", path, key)
		}
	}

	// 负控：把 tag 改名后同一个解析器必须给出别的值，否则上面两条是在自证。
	const real = `Identity string ` + "`json:\"identity,omitempty\"`"
	renamed := strings.Replace(real, `"identity,omitempty"`, `"identity_renamed,omitempty"`, 1)
	if renamed == real {
		t.Fatal("负控自己失效了：样例源码里没有 identity 的 json tag")
	}
	if key, _ := accountIdentityJSONTag(renamed); key == "identity" {
		t.Error("改名后解析器仍报 identity：这条检查挡不住 tag 改名")
	}
}

// TestAccountGroupingIsWiredIntoEveryCount 钉住分组函数真的被用上了。
func TestAccountGroupingIsWiredIntoEveryCount(t *testing.T) {
	src := poolStatsUISource(t)
	if miss := accountGroupingWiring(t, src); len(miss) != 0 {
		t.Errorf("账号分组链路缺了 %d 处：%s", len(miss), strings.Join(miss, "；"))
	}
	// accNote 的数字必须是账号数。用 list.length（凭据数）会让 zcode 显示
	// 「3 个账号」而表里只有 2 行，两边对不上。
	if !strings.Contains(src, `CUR + " · " + groups.length + " 个账号"`) {
		t.Error("accNote 没有按账号数（groups.length）计数")
	}
	if !strings.Contains(src, `"（其中 " + multi + " 个有多种登录方式）"`) {
		t.Error("accNote 没有在有多通道账号时说明数量：操作员会以为账号凭空少了")
	}
}

// TestAccountGroupingGuardCatchesARenamedHelper 是负控：把 accGroups 改名后，
// 上面那条断言必须报出缺失，否则它可能只是在自证。
func TestAccountGroupingGuardCatchesARenamedHelper(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src, "accGroups(", "accGroupsRenamed(", -1)
	if broken == src {
		t.Fatal("测试自己失效了：源码里没有 accGroups(")
	}
	if miss := accountGroupingWiring(t, broken); len(miss) == 0 {
		t.Error("改名后分组链路一条缺失都没报出来：这条守卫守不住")
	}
}

// TestAccountGroupIsHealthyWhenAnyChannelIsHealthy 钉住组头的状态口径：一个通道
// 挂了、另一个还能用，账号就是能用的。反过来（任一通道挂了就算账号坏了）会让
// 一个正常工作的账号在列表里看着像坏的。
func TestAccountGroupIsHealthyWhenAnyChannelIsHealthy(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accGroupStat")

	if !strings.Contains(body, `on.some(r => (r.state || "unknown") === "ready")`) {
		t.Error("accGroupStat 没有「只要有任一已启用通道 ready 就算 ready」这条规则")
	}
	// 一个已启用的通道都没有时，退回第一个已启用的（再退回第一行），
	// 否则组头会显示 unknown 而不是「停用」。
	if !strings.Contains(body, "on[0] || g.rows[0]") {
		t.Error("accGroupStat 没有在「没有 ready 通道」时退回首个已启用通道")
	}
	// 通道计数只数已启用的。
	if !strings.Contains(body, "g.rows.filter(r => r.enabled)") {
		t.Error("accGroupStat 的 on/of 没有按 enabled 过滤")
	}
	// 统计磁贴必须走 accGroupStat，不能自己再数一遍凭据。
	if stats := poolStatsFuncBody(t, src, "renderStats"); !strings.Contains(stats, "const st = accGroupStat(g);") {
		t.Error("renderStats 没有用 accGroupStat：磁贴口径会和表里的分组不一致")
	}
}

// TestPoolTilesUseTheSameStateClassifierAsTheRows pins the fix for "下面显示冷却中、
// 上面记成禁用": the tiles must bucket accounts through accountState -- the same
// function that paints each row's chip -- instead of re-listing raw state words.
// A literal list drifts: exhausted is a cooling reason but read as "bad", and
// low_credit / quota_exceeded / rate_limited matched no bucket at all.
func TestPoolTilesUseTheSameStateClassifierAsTheRows(t *testing.T) {
	src := poolStatsUISource(t)
	stats := poolStatsFuncBody(t, src, "renderStats")

	for _, want := range []string{
		"const st = accGroupStat(g);",
		"const cls = accountState(st);",
		`if (cls.key === "ready") ready++;`,
		`else if (cls.key === "cooling") cool++;`,
		`else if (cls.key === "disabled") bad++;`,
	} {
		if !strings.Contains(stats, want) {
			t.Errorf("renderStats 没有按行里同一套分类分桶，缺：%s", want)
		}
	}
	// 负控：旧的按字面值分桶必须消失，否则「冷却中」被记成「禁用」会回来。
	for _, bad := range []string{`st.state === "exhausted"`, `st.state === "cooling"`, "if (!st.enabled) bad++;"} {
		if strings.Contains(stats, bad) {
			t.Errorf("renderStats 又按原始 state 字面值分桶了：%s", bad)
		}
	}
}

// TestAccountGroupHeaderHasNoActionButtons 钉住组头那一行是纯信息行。每个通道
// 单独启停、单独测试是需求本身；组头再挂一个批量按钮会把两份凭据的状态混在
// 一起，而且没有明显正确的语义。
func TestRecentCallAccountPrefersHumanReadableName(t *testing.T) {
	src := string(indexHTML)
	body := poolStatsFuncBody(t, src, "accountDisplayName")
	for _, want := range []string{
		`consider(a.operator_note, -1);`,
		`consider(f.phone || f.mobile, 0);`,
		`consider(label, 2);`,
		`consider(f.user_id, 5);`,
		`consider(a.identity, 6);`,
		`return best || key;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recent-call account display is missing %s", want)
		}
	}
	// 备注（手机号 / 邮箱）必须在模块 label 之前被考虑：重登时要认的就是它，
	// 排在 label 后面会被凭据类型名（如 ZCode plan JWT）盖掉。
	note := strings.Index(body, "consider(a.operator_note")
	label := strings.Index(body, "consider(label, 2)")
	if note < 0 || label < 0 || note > label {
		t.Error("账号列没有把操作员备注排在模块 label 前面")
	}
	if !strings.Contains(src, `const accName = accountDisplayName(x.account) || "-";`) {
		t.Error("最近调用的账号列没有用 accountDisplayName 渲染")
	}
}

func TestClientChipsShowUsableOverTotalAccounts(t *testing.T) {
	src := string(indexHTML)
	body := poolStatsFuncBody(t, src, "renderChips")
	for _, want := range []string{
		`const groups = accGroups(ACCTS[n] && ACCTS[n].accounts);`,
		`const usable = groups.filter(g => { const st = accGroupStat(g); return st.enabled && st.state === "ready"; }).length;`,
		`const label = usable + "/" + cnt;`,
		`esc(n) + ' <b>' + esc(label) + '</b>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client chip account ratio is missing %s", want)
		}
	}
}

func TestAccountStateLabelsAreChinese(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{`label: "可用"`, `label: "冷却中"`, `label: "禁用"`} {
		if !strings.Contains(src, want) {
			t.Errorf("account state label is missing %s", want)
		}
	}
}

func TestAccountPriorityColumnStaysNarrow(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`<col class="col-prio">`,
		`#view-accounts table.acc .col-prio { width: 6.5%; }`,
		`#view-accounts table.acc .accPrio { width: 52px; min-width: 52px;`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("account priority column sizing is missing %s", want)
		}
	}
}

func TestAccountColumnsSplitTheTableEvenly(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`#view-accounts table.acc .col-account { width: 10%; }`,
		`#view-accounts table.acc .col-usage { width: 14%; }`,
		`#view-accounts table.acc .col-last { width: 8%; }`,
		`#view-accounts table.acc .col-acts { width: 11%; }`,
		`#view-accounts table.acc td.c-usage,`,
		`#view-accounts table.acc td.c-last { overflow: hidden; }`,
		`<td class="c-usage">' + accUsageCell(a) + '</td>`,
		`<td class="c-last">' + accLastCell(a) + '</td>`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("account column sizing is missing %s", want)
		}
	}

	widths := regexp.MustCompile(`#view-accounts table\.acc \.col-[a-z]+ \{ width: ([0-9.]+)%; \}`).FindAllStringSubmatch(src, -1)
	if len(widths) != 13 {
		t.Fatalf("expected 13 account column widths, got %d", len(widths))
	}
	total := 0.0
	for _, m := range widths {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("parse column width %q: %v", m[1], err)
		}
		total += v
	}
	if total < 99.9 || total > 100.1 {
		t.Errorf("account column widths total %.1f%%, want 100%%", total)
	}
}

func TestAccountPriorityIsEditableFromThePoolPage(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		"<th>优先级</th>",
		"function accPriorityCell(",
		"accPrio",
		"async function saveAccountPriority(",
		"account_priorities",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("account priority UI is missing %q", want)
		}
	}
}

func TestAccountGroupHeaderHasNoActionButtons(t *testing.T) {
	src := poolStatsUISource(t)
	head := groupHeaderTemplate(t, src)

	if strings.Contains(head, "data-do") {
		t.Error("组头那一行带了按钮：每个通道要能单独控制，组头不该有批量动作")
	}
	if strings.Contains(head, "<button") {
		t.Error("组头那一行有 <button>：组头唯一的动作是展开/收起，整行可点就够了")
	}
	// 反过来说，展开/收起这个动作必须挂在组头上，否则多通道账号就收不起来。
	if !strings.Contains(head, `data-grp="'`) {
		t.Error("组头没有 data-grp：整行点不动，多通道账号收不起来")
	}
	if !strings.Contains(head, `g.rows.length + ' 个通道</span></div>'`) {
		t.Error("组头没有显示通道数")
	}
	// 组头是 3 格（标记、账号名、状态），其余宽度靠 colspan 吃掉；列数一变
	// 组头就会错位，而错位只在有多通道账号时看得见。
	// 组头本身是 3 格（标记、账号名、状态）；余额列存在时再多一格，其余宽度
	// 靠 colspan 吃掉。列数一变组头就会错位，而错位只在有多通道账号时看得见。
	if !strings.Contains(head, `colspan="' + (showBal ? cols - 5 : cols - 4) + '"`) {
		t.Error("组头的 colspan 没有跟着表头列数走（showBal ? cols - 5 : cols - 4）")
	}
	// 收起时也要看得到余额：同一个账号只有部分通道能报余额（ZCode 只有 JWT
	// 通道能报计划余额），只看第一条通道会让余额永远显示不出来。
	if !strings.Contains(head, `accGroupBalanceCell(g.rows)`) {
		t.Error("组头没有显示组内可用余额（accGroupBalanceCell）")
	}
	if !strings.Contains(head, "同一账号的不同登录方式") {
		t.Error("组头没有说明这些子行是同一个账号的不同登录方式")
	}
	// 组头渲染在 accRowHTML 之外，所以它自己必须用 stateChip，不能就地拼一个。
	if !strings.Contains(head, "stateChip(st)") {
		t.Error("组头的状态没有走 stateChip")
	}
}

// TestAccountIdentityIsRedacted 钉住 identity 跟着别的字段一起过脱敏。它本身
// 不是秘密（是厂商的账号 id），但脱敏函数是这块唯一一处「不要把上游字符串原样
// 吐给浏览器」的关口，漏掉一个字段就是漏掉一个口子。
func TestAccountIdentityIsRedacted(t *testing.T) {
	b, err := os.ReadFile("panel.go")
	if err != nil {
		t.Skipf("cannot read panel.go: %v", err)
	}
	if !strings.Contains(string(b), "a.Identity = core.Redact(a.Identity)") {
		t.Error("redactAccount 没有脱敏 Identity")
	}
}

// ---------------------------------------------------------------------------
// 多通道账号默认收起。
//
// 需求原话是「多通道的账号应该要折叠在账号下面」。三件事必须同时成立，少一件
// 这个功能就是坏的：
//   1. 默认收起——展开状态只存在 ACC_OPEN 里，而它是空的；
//   2. 收起的通道行是 hidden，不是「不渲染」——账号表每 5 秒被 refreshVisible
//      整体重画一次，靠不渲染实现收起的话，用户刚点开就会被下一次刷新收回去；
//   3. 组头整行可点、键盘也能开合——组头没有任何别的按钮，所以整行当开关是
//      安全的，也是唯一点得到的地方。
// ---------------------------------------------------------------------------

// collapseWiring 返回「默认收起」这条链路上缺失的环节。返回空切片表示全都在。
//
// 和 accountGroupingWiring 一样返回列表而不是就地断言，好让负控用同一段代码
// 证明它真的会红。
func collapseWiring(t *testing.T, src string) []string {
	t.Helper()
	body := poolStatsFuncBody(t, src, "renderAccounts")
	var miss []string
	add := func(what string, ok bool) {
		if !ok {
			miss = append(miss, what)
		}
	}

	// 1. 默认收起。
	add("ACC_OPEN 不是空集合（默认收起）", strings.Contains(src, "const ACC_OPEN = new Set();"))
	add("accIsOpen 没有真的去查 ACC_OPEN（默认收起失效）",
		strings.Contains(src, "return ACC_OPEN.has(accOpenKey(n, key)); }"))
	add("renderAccounts 没有按 ACC_OPEN 决定展开状态",
		strings.Contains(body, "const open = accIsOpen(CUR, g.key);"))
	// 渲染是只读的，唯一例外是搜索命中那一次。多一处写入就等于某个账号被
	// 「顺手」永久展开了，而用户从没点过它。
	add("renderAccounts 里往 ACC_OPEN 写入的地方不是只有搜索那一处",
		strings.Count(body, "ACC_OPEN.add(") == 1)
	add("搜索命中没有把多通道账号展开一次（命中落在通道行上时看不出为什么算命中）",
		strings.Contains(body, "shown.forEach(g => { if (g.rows.length > 1) ACC_OPEN.add(accOpenKey(CUR, g.key)); });"))
	// 4. 自动展开只做一次。没有这个守卫时，只要搜索框里有字，每一次重画都会把命中
	// 的多通道账号重新塞回 ACC_OPEN：点标题想收起时，点击先删掉键、紧接着的重画又
	// 加回来，表现为「有筛选的时候怎么点都收不起来」。
	add("ACC_EXPANDED_FOR 没有声明", strings.Contains(src, `let ACC_EXPANDED_FOR = "";`))
	add("搜索自动展开没有按「客户端 + 关键词」去重（有筛选时点标题收不起来）",
		strings.Contains(body, "if (q && qKey !== ACC_EXPANDED_FOR) {"))

	// 2. 收起 = hidden，不是不渲染。
	add("收起的通道行没有 hidden 属性", strings.Contains(src, `(open ? "" : " hidden")`))
	add("没有为 tr[hidden] 显式声明 display:none（浏览器给 tr 的 display:table-row 会盖掉它）",
		strings.Contains(src, ".acc tbody tr[hidden] { display: none; }"))
	add("通道行没有拿到所属分组的键（data-chan）",
		strings.Contains(body, "accRowHTML(a, caps, acts, showBal, true, open, g.key)"))

	// 3. 组头是开关。
	add("组头没有 data-grp（整行点不动）", strings.Contains(body, `'<tr class="acc-grp" data-grp="'`))
	add("组头没有 tabindex（键盘聚焦不到）", strings.Contains(body, `tabindex="0"`))
	add("组头没有 aria-expanded（读屏用户听不出开合）",
		strings.Contains(body, `aria-expanded="' + (open ? "true" : "false") + '"`))
	add("组头没有随开合切换提示文案",
		strings.Contains(body, `(open ? "；每一行可以单独启用、停用和测试" : "；点这一行展开")`))
	add("点组头不会开合", strings.Contains(src, `ev.target.closest("tr[data-grp]")`))
	add("组头有 tabindex 却没有键盘处理（键盘用户点不开）",
		strings.Contains(src, `$("#accBody").addEventListener("keydown"`))
	add("没有开合函数", strings.Contains(src, "function accToggleOpen(n, key)"))
	return miss
}

// TestMultiChannelAccountsStartCollapsed 钉住「多通道账号默认收起」。
func TestMultiChannelAccountsStartCollapsed(t *testing.T) {
	src := poolStatsUISource(t)
	if miss := collapseWiring(t, src); len(miss) != 0 {
		t.Errorf("多通道账号的收起链路缺了 %d 处：%s", len(miss), strings.Join(miss, "；"))
	}
}

// TestCollapseWiringCatchesADefaultOpenMutation 是负控：把 accIsOpen 改成恒真
// （等于默认展开、也等于收起之后再点不开），上面那条断言必须报出来。
func TestCollapseWiringCatchesADefaultOpenMutation(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src,
		"function accIsOpen(n, key) { return ACC_OPEN.has(accOpenKey(n, key)); }",
		"function accIsOpen(n, key) { return true; }", 1)
	if broken == src {
		t.Fatal("测试自己失效了：源码里没有 accIsOpen 的实现")
	}
	if miss := collapseWiring(t, broken); len(miss) == 0 {
		t.Error("改成恒展开后一条缺失都没报出来：这条守卫守不住")
	}
}

// TestCollapseWiringCatchesAVisibleChannelRow 是第二个负控：去掉通道行的 hidden
// 之后，收起就只剩下一个转来转去的箭头，行还在屏幕上。
func TestCollapseWiringCatchesAVisibleChannelRow(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src, `(open ? "" : " hidden")`, `""`, 1)
	if broken == src {
		t.Fatal("测试自己失效了：源码里没有 hidden 那处")
	}
	if miss := collapseWiring(t, broken); len(miss) == 0 {
		t.Error("通道行永远可见时一条缺失都没报出来：这条守卫守不住")
	}
}

// TestCollapseWiringCatchesUnguardedAutoExpand 是第三个负控：把搜索自动展开的
// 去重守卫拿掉（回到「每次重画都展开」），上面那条断言必须报出来——否则「有筛选
// 时点标题收不起来」这个缺陷会再溜回来。
func TestCollapseWiringCatchesUnguardedAutoExpand(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src,
		"if (q && qKey !== ACC_EXPANDED_FOR) {",
		"if (q) {", 1)
	if broken == src {
		t.Fatal("测试自己失效了：源码里没有搜索自动展开的守卫")
	}
	if miss := collapseWiring(t, broken); len(miss) == 0 {
		t.Error("自动展开不再去重后一条缺失都没报出来：这条守卫守不住")
	}
}

// ---------------------------------------------------------------------------
// OpenRouter 的「用自己的 API Key」直通车道。
//
// 背景：浏览器授权（PKCE）签发的是厂商发给 client2api 这把应用的 Key，额度与
// 限流都记在这个应用头上；操作员自己的 Key 记在他自己的账号上。两者是不同的
// 账号，所以面板要在登录 tab 里直接给一条「填自己的 Key」的路，而不是让人去翻
// 通用的「手动添加」表单。这条路是模块自己声明的：capabilities.direct_key 非空
// 才渲染，且面板写入的字段就是它声明的那一个。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉住：
//   1. 弹层里有这个块，且默认 hidden（没声明能力的模块不能看到它）；
//   2. 渲染函数只认 capabilities.direct_key，并用它去 AccountFields 里找字段；
//   3. 保存走的是和其它添加路径同一个 POST /accounts 接口；
//   4. 加完之后把「其它还在轮询的 Key」摆出来供一键停用——不这样的话，授权那
//      一把会继续和新 Key 一起被轮询，用户「只用自己那把」的诉求就没达成。
// ---------------------------------------------------------------------------

func TestOwnKeyShortcutIsGatedOnTheModuleCapability(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `id="addOwnKey"`) {
		t.Fatal(`the add dialog has no own-key block (id="addOwnKey")`)
	}
	// 默认 hidden：能力是模块声明出来的，不是面板默认给的。
	if !strings.Contains(src, `id="addOwnKey" class="ownkey" hidden`) {
		t.Error("the own-key block is not hidden until a module opts in")
	}

	render := poolStatsFuncBody(t, src, "renderOwnKey")
	for _, want := range []string{`$("#addOwnKey")`, "direct_key", ".hidden = true", "fields.find"} {
		if !strings.Contains(render, want) {
			t.Errorf("renderOwnKey does not mention %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "saveOwnKey")
	for _, want := range []string{"direct_key", `"/accounts"`, "body.fields[key] = val"} {
		if !strings.Contains(save, want) {
			t.Errorf("saveOwnKey does not mention %q", want)
		}
	}

	// 加完自己的 Key 后必须能一键停用其它 Key，否则授权那把会继续一起轮询。
	others := poolStatsFuncBody(t, src, "renderOwnKeyOthers")
	if !strings.Contains(others, "a.enabled") {
		t.Error("renderOwnKeyOthers does not filter to the keys still in rotation")
	}
	if !strings.Contains(src, `data-ownkey-off=`) {
		t.Error("the other-key list renders no disable control")
	}
	if !strings.Contains(src, `$("#ownKeyOthers").addEventListener("click"`) {
		t.Error("the other-key disable control has no handler")
	}
}

// ---------------------------------------------------------------------------
// 多平台源（openai-compat）的「选中平台 → 给获取 Key 的链接」。
//
// 模块用 capabilities.key_pages 把「平台 id → 控制台申请页」交给面板；面板在
// 手动添加表单里渲染一条链接，并随 provider 下拉的 change 重画。缺了它，操作员
// 只能自己猜 groq / cerebras 的 Key 页面在哪。同样是字符串拼接，静态钉住。
// ---------------------------------------------------------------------------
func TestManualFormLinksToTheProviderKeyPage(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `id="manKeyLink"`) {
		t.Error(`the manual add tab has no key-page link anchor (id="manKeyLink")`)
	}

	render := poolStatsFuncBody(t, src, "renderManualKeyLink")
	for _, want := range []string{"key_pages", "manualFieldByKey(\"provider\")", "box.hidden = true", "target=\"_blank\""} {
		if !strings.Contains(render, want) {
			t.Errorf("renderManualKeyLink does not mention %q", want)
		}
	}
	wire := poolStatsFuncBody(t, src, "wireManualKeyLink")
	if !strings.Contains(wire, `addEventListener("change"`) {
		t.Error("the provider picker does not refresh the key-page link on change")
	}
	manual := poolStatsFuncBody(t, src, "renderManual")
	if !strings.Contains(manual, "wireManualKeyLink(n)") {
		t.Error("renderManual does not wire the key-page link after painting the form")
	}
}
