package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 刷新按钮的界面回归测试。
//
// 背景：刷新按钮散落在 9 个视图里、5 个不同的位置（顶栏、各卡片 header、
// 各子面板 header），而且有两个视图根本没有。对操作员来说，"这个页面怎么
// 重新读数据"要在整页里找一遍按钮才知道；对话测试和运行日志两个页面则根本
// 找不到。
//
// 现在统一到顶栏一个常驻按钮：它在任何视图都可见，点击时按当前视图分派到
// 真正干活的那个函数（而不是笼统的 refreshAll），并把卡片里那些重复的按钮
// 撤掉，免得同一个动作在页面上出现两次、点了还不确定点的是哪个。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉住。
// ---------------------------------------------------------------------------

// refreshButtonWiring 返回刷新按钮的接错之处。返回空表示全对。
//
// 返回列表而不是就地断言，是为了让负控能用同一段代码证明它真的会红。
func refreshButtonWiring(t *testing.T, src string) []string {
	t.Helper()
	var miss []string

	// 1. 顶栏必须有一个常驻刷新按钮，且不能再被视图切换藏起来。
	if !strings.Contains(src, `id="btnRefresh"`) {
		miss = append(miss, "顶栏没有刷新按钮 btnRefresh")
	}
	// setView 里那句 `$("#btnRefresh").hidden = !onAccounts;` 是"只在账号池显示"
	// 的旧行为：统一之后任何视图都要能刷新，这行必须消失。
	if strings.Contains(src, `$("#btnRefresh").hidden = !onAccounts;`) {
		miss = append(miss, `setView 还在按视图隐藏顶栏刷新按钮（$("#btnRefresh").hidden = !onAccounts），统一之后它必须常驻`)
	}
	if strings.Contains(src, `$("#btnRefresh").hidden`) {
		miss = append(miss, "顶栏刷新按钮仍会被隐藏；它必须在任何视图都可见")
	}

	// 2. 顶栏按钮必须走分派函数，而不是一刀切的 refreshAll。
	if !strings.Contains(src, "function refreshCurrentView(") {
		miss = append(miss, "面板里没有 refreshCurrentView 分派函数")
	}
	if !strings.Contains(src, `$("#btnRefresh").addEventListener("click", refreshCurrentView)`) {
		miss = append(miss, "顶栏刷新按钮没有接到 refreshCurrentView")
	}

	// 3. 每个视图都要有分派：11 个视图，一个都不能漏。对话测试和运行日志原本
	//    没有刷新按钮，正是这次要补上的两个。
	for _, v := range refreshViews {
		if !strings.Contains(src, "\""+v+"\":") {
			miss = append(miss, "refreshCurrentView 没有覆盖视图 "+v)
		}
	}
	// 4. 新补上的两个视图必须真的调用了渲染函数，不能只占一个键位。
	for _, fn := range []string{"renderChat", "renderLogs"} {
		if !strings.Contains(src, fn) {
			miss = append(miss, "刷新分派里没有 "+fn+"（对话测试 / 运行日志 原来没有刷新按钮）")
		}
	}
	return miss
}

// refreshViews 是导航里能到达的每一个视图。少一个键位就意味着那个页面没有
// 刷新按钮——这正是要修的毛病，所以它同时是覆盖清单。
var refreshViews = []string{
	"accounts", "chat", "usage", "alerts", "credits", "packages",
	"taskscenter", "models", "platforms", "config", "logs",
}

func TestRefreshButtonIsUnifiedInTheTopbar(t *testing.T) {
	if miss := refreshButtonWiring(t, poolStatsUISource(t)); len(miss) > 0 {
		t.Fatalf("刷新按钮接错了：\n  - %s", strings.Join(miss, "\n  - "))
	}
}

// TestRefreshViewsCoverEveryNavigableView 独立钉住覆盖清单本身：如果以后导航
// 又加了一个视图而忘了加进分派，这条会先红。
func TestRefreshViewsCoverEveryNavigableView(t *testing.T) {
	src := poolStatsUISource(t)
	// TITLES 是导航项的唯一来源，它有的每个视图都必须在分派表里。
	i := strings.Index(src, "const TITLES = {")
	if i < 0 {
		t.Fatal("找不到 TITLES")
	}
	// 从 "{" 之后开始切，否则第一片是 "const TITLES = { accounts" 这种半截。
	body := src[i+len("const TITLES = {"):]
	if end := strings.Index(body, "};"); end >= 0 {
		body = body[:end]
	}
	seen := map[string]bool{}
	for _, m := range strings.Split(body, ",") {
		kv := strings.SplitN(m, ":", 2)
		if len(kv) != 2 {
			continue
		}
		seen[strings.Trim(strings.TrimSpace(kv[0]), `"`)] = true
	}
	if len(seen) == 0 {
		t.Fatalf("TITLES 解析不出视图名：%s", body)
	}
	for v := range seen {
		if v == "" {
			continue
		}
		if !strings.Contains(src, "\""+v+"\":") {
			t.Errorf("导航里有视图 %s，但刷新分派里没有它", v)
		}
		if !inList(refreshViews, v) {
			t.Errorf("导航里新增了视图 %s，请把它加进 refreshViews 覆盖清单", v)
		}
	}
	for _, v := range refreshViews {
		if !seen[v] {
			t.Errorf("refreshViews 里有 %s，但导航里没有这个视图", v)
		}
	}
}

// inList is slice membership.  The package already has contains(s, sub) in
// quota_test.go and that one is a substring test, which is not what the
// coverage check above needs.
func inList(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestScatteredRefreshButtonsAreGone 钉住"撤掉重复按钮"这件事：卡片 header 里
// 那些和顶栏干同一件事的按钮必须消失，否则一页上出现两个"刷新"，点了不
// 知道点的是哪个。
func TestScatteredRefreshButtonsAreGone(t *testing.T) {
	src := poolStatsUISource(t)
	// btnRefresh 自己。注意 btnVcRefresh 不在列：它在"开学季 · 我的券码"弹窗
	// 里，不是页面视图，不归顶栏这个按钮管。
	//
	// 后四个（btnScReload / btnScDefaultsReload / btnPfReload / btnCfgReload）当初
	// 被当成"不同动作"留下，其实它们点的就是同一个 renderXxx()，顶栏刷新已经
	// 覆盖了；留下只会在同一个页面上摆两个意思一样的按钮。
	dupes := []string{
		`id="btnUsage"`,
		`id="btnUsageRecentRefresh"`,
		`id="btnAlerts"`,
		`id="btnCr"`,
		`id="btnScRunsReload"`,
		`id="btnTbReload"`,
		`id="btnScReload"`,
		`id="btnScDefaultsReload"`,
		`id="btnPfReload"`,
		`id="btnCfgReload"`,
	}
	for _, d := range dupes {
		if strings.Contains(src, d) {
			t.Errorf("卡片里还留着重复的刷新按钮 %s；它已并入顶栏的统一刷新", d)
		}
	}
	// 这几个做的是顶栏刷新做不了的事（让服务端去问上游、重新读号池、探测能力），
	// 必须留着——但要确认它们还在，免得为了统一把真功能删了。
	keep := []string{
		`id="btnRefreshAll"`, // 重新读取号池
		`id="btnBalanceAll"`, // 重新读取余额
		`id="btnPk"`,         // 重新探测能力
		`id="btnModels"`,     // 重新拉模型目录
		`id="btnCfgRestart"`, // 重启进程
	}
	for _, k := range keep {
		if !strings.Contains(src, k) {
			t.Errorf("按钮 %s 不见了；它做的是和统一刷新不同的事，不该被合并掉", k)
		}
	}
}
