package panel

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号池分页的界面回归测试。
//
// 背景：账号池一次把全部账号铺出来，几十个号的时候表就长到要一直往下滚，
// 操作员既数不清总数，也很容易看漏某一行。修法是在账号组这一层切页（不是
// 在凭据行那一层——一个账号的多条凭据永远在同一页，不会被切成两半）。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到；shell_test.go 只保证
// $() 指的 id 存在。所以这里静态钉住五件事：
//   1. 分页切的是「账号组」而不是凭据行，页内渲染的还是按组 map 出来的行；
//   2. 页码夹在 [1, pages]：搜索把结果从 300 筛到 5 时不会停在一张空表上；
//   3. 只有一页时整个分页条收起来——一个永远点不动的分页器像坏了的控件；
//   4. 「每页多少个」的下拉框默认选中的那一项，必须等于 ACC_PAGE_SIZE 的初值
//      （曾经出过：选项里没有 selected，浏览器显示 20 而状态是 50，控制和状态
//      各说各话，翻页条上写的数字和表里实际行数对不上）；
//   5. 换客户端、换关键词、改每页条数都回到第 1 页：旧页码在新集合里通常没有意义。
// ---------------------------------------------------------------------------

// accPagerWiring 返回分页链路里缺失的环节。返回空切片表示全都在。
//
// 之所以返回列表而不是就地断言，是为了让负控能证明这些检查真的会红。
func accPagerWiring(src string) []string {
	var miss []string
	for _, c := range []struct{ what, want string }{
		{"ACC_PAGE 没有声明", "let ACC_PAGE = 1;"},
		{"ACC_PAGE_SIZE 没有声明", "let ACC_PAGE_SIZE = "},
		{"hideAccPager 没有定义", "function hideAccPager("},
		{"renderAccPager 没有定义", "function renderAccPager("},
		{"accGoPage 没有定义", "function accGoPage("},
		{"accTurn 没有定义（翻页后要把表格滚回顶部）", "function accTurn("},
		{"accShownGroups 没有定义", "function accShownGroups("},
		{"页码没有夹进 [1, pages]", "if (ACC_PAGE > pages) ACC_PAGE = pages;"},
		{"页码没有兜底下界", "if (ACC_PAGE < 1) ACC_PAGE = 1;"},
		// 切页必须切在组上：pageRows 是 accShownGroups() 的切片，不是逐条凭据的
		// 切片。一个 zcode 账号的两份凭据被拆到两页，操作员会以为账号少了一个。
		{"分页没有按组切片", "const pageRows = shown.slice(from, from + ACC_PAGE_SIZE);"},
		{"页内渲染的还是整份列表", "tb.innerHTML = pageRows.map(g => {"},
		// 渲染和翻页必须共用同一份「哪些组该显示」的算法，否则改了一处就会翻到
		// 和当前筛选条件对不上的页上。
		{"renderAccounts 没有走共享的筛选函数", "const shown = accShownGroups();"},
		{"accGoPage 没有走共享的筛选函数", "Math.ceil(accShownGroups().length / ACC_PAGE_SIZE)"},
		// 每页条数改了以后，页码按旧尺寸算出来的没有意义，必须先归位。
		{"换搜索词没有回到第 1 页", "accSearchTick = 0; ACC_PAGE = 1; renderAccounts();"},
		{"换每页条数没有回到第 1 页", "ACC_PAGE_SIZE = n;\n  ACC_PAGE = 1;"},
		// 一页放得下时不该出现分页条。
		{"单页时没有收起分页条", "  if (pages <= 1) { hideAccPager(); return; }"},
		{"收起时没有清掉分页条上的旧数字", "  $(\"#accPageInfo\").textContent = \"\";"},
		{"收起时没有清掉页码", "  $(\"#accPageNow\").textContent = \"\";"},
		{"到头时首页/上一页没有禁用", "  $(\"#accPageFirst\").disabled = ACC_PAGE <= 1;\n  $(\"#accPagePrev\").disabled = ACC_PAGE <= 1;"},
		{"到头时下一页/末页没有禁用", "  $(\"#accPageNext\").disabled = ACC_PAGE >= pages;\n  $(\"#accPageLast\").disabled = ACC_PAGE >= pages;"},
		{"页码夹取没有上界", "  if (ACC_PAGE > pages) ACC_PAGE = pages;"},
		{"页码夹取没有下界", "  if (ACC_PAGE < 1) ACC_PAGE = 1;"},
	} {
		if !strings.Contains(src, c.want) {
			miss = append(miss, c.what+"（缺 "+c.want+"）")
		}
	}
	return miss
}

// accPagerHiddenWhenSinglePage 校验：单页时藏起分页条。
func TestAccPagerHidesWhenSinglePage(t *testing.T) {
	src := poolStatsUISource(t)
	if miss := accPagerWiring(src); len(miss) != 0 {
		t.Errorf("账号池分页链路缺了 %d 处：%s", len(miss), strings.Join(miss, "；"))
	}
}

// TestAccPagerSlicesGroupsNotRows 钉住切页切在账号组上。一个账号的多条凭据
// 必须永远在同一页——zcode 的账号在同一页里是「组头 + 两条通道行」，被切成两页
// 会让操作员以为账号凭空少了一个。
func TestAccPagerSlicesGroupsNotRows(t *testing.T) {
	src := poolStatsUISource(t)

	// 分页的那一刀必须作用在组数组上。
	if !strings.Contains(src, "const pageRows = shown.slice(from, from + ACC_PAGE_SIZE);") {
		t.Error("分页没有按账号组切片：shown 应该是 accGroups(...) 之后的组数组")
	}
	// 渲染进 tbody 的必须是 pageRows，不能是整份 shown。
	if !strings.Contains(src, "tb.innerHTML = pageRows.map(g => {") {
		t.Error("tbody 渲染的不是 pageRows：分页切了组但表里还是整份列表，等于没分")
	}
	// 反向检查：整份列表不应该再被直接渲染。
	if strings.Contains(src, "tb.innerHTML = shown.map(g => {") {
		t.Error("tbody 还在渲染整份 shown：分页条会翻页，但表里内容不变")
	}
}

// TestAccPageSizeSelectMatchesDefault 钉住「每页多少个」下拉框的默认选中项等于
// ACC_PAGE_SIZE 的初值。曾经这个 option 忘了写 selected，浏览器显示的是列表里的
// 第一项（20），而状态是 50——操作员在框里看到的和表里实际的行数对不上。
func TestAccPageSizeSelectMatchesDefault(t *testing.T) {
	src := poolStatsUISource(t)

	// 取 ACC_PAGE_SIZE 的初值。
	m := regexp.MustCompile(`let ACC_PAGE_SIZE = (\d+);`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("找不到 ACC_PAGE_SIZE 的初值声明")
	}
	defaultSize, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("ACC_PAGE_SIZE 的初值 %q 不是一个整数", m[1])
	}

	// 找到 #accPageSize 这个 select 的全部 option。
	selStart := strings.Index(src, `id="accPageSize"`)
	if selStart < 0 {
		t.Fatal("找不到 #accPageSize")
	}
	rest := src[selStart:]
	selEnd := strings.Index(rest, "</select>")
	if selEnd < 0 {
		t.Fatal("#accPageSize 的 select 看起来没闭合")
	}
	selBody := rest[:selEnd]

	optRe := regexp.MustCompile(`<option value="(\d+)"([^>]*)>`)
	opts := optRe.FindAllStringSubmatch(selBody, -1)
	if len(opts) == 0 {
		t.Fatal("#accPageSize 里一个 option 都没有")
	}

	var selected int
	var selectedCount int
	for _, o := range opts {
		if strings.Contains(o[2], "selected") {
			selectedCount++
			v, err := strconv.Atoi(o[1])
			if err != nil {
				t.Fatalf("selected 的 option value=%q 不是整数", o[1])
			}
			selected = v
		}
	}
	if selectedCount == 0 {
		t.Errorf("#accPageSize 没有任何 option 带 selected：浏览器会显示第一项（%s），和 ACC_PAGE_SIZE=%d 对不上", opts[0][1], defaultSize)
	} else if selectedCount > 1 {
		t.Errorf("#accPageSize 有 %d 个 option 带 selected：HTML 里只会认第一个，操作员改一次就再也回不去默认值", selectedCount)
	} else if selected != defaultSize {
		t.Errorf("#accPageSize 默认选中 %d，但 ACC_PAGE_SIZE 的初值是 %d：控制框和状态各说各话，表里实际行数和分页条上写的对不上", selected, defaultSize)
	}
}

// TestAccPagerResetsPageOnContextChange 钉住三处「回到第 1 页」：换搜索词、换
// 每页条数、换客户端。旧页码在新集合里通常没有意义——
//   - 搜索把 138 个筛成 5 个时停在第 3 页 = 一张空表；
//   - 每页从 50 改成 20 时第 3 页仍然存在，但表已经全变了；
//   - 换客户端时另一个池子可能只有 2 页。
func TestAccPagerResetsPageOnContextChange(t *testing.T) {
	src := poolStatsUISource(t)

	// 换搜索词：输入和 Esc 清空都要归位。
	if !strings.Contains(src, "accSearchTick = 0; ACC_PAGE = 1; renderAccounts();") {
		t.Error("换搜索词没有把 ACC_PAGE 归 1：搜索把 138 个筛成 5 个时会停在第 3 页，一张空表")
	}
	// Esc 清空搜索框那条路径也要归位。
	escStart := strings.Index(src, `$("#accSearch").addEventListener("keydown"`)
	if escStart < 0 {
		t.Fatal("找不到 #accSearch 的 keydown 绑定")
	}
	escBody := src[escStart:]
	escEnd := strings.Index(escBody, "\n});")
	if escEnd < 0 {
		t.Fatal("#accSearch 的 keydown 绑定看起来没结束")
	}
	if !strings.Contains(escBody[:escEnd], "ACC_PAGE = 1;") {
		t.Error("Esc 清空搜索框时没有把 ACC_PAGE 归 1")
	}

	// 换每页条数：改尺寸后先归位再渲染。
	sizeStart := strings.Index(src, `$("#accPageSize").addEventListener("change"`)
	if sizeStart < 0 {
		t.Fatal("找不到 #accPageSize 的 change 绑定")
	}
	sizeBody := src[sizeStart:]
	sizeEnd := strings.Index(sizeBody, "\n});")
	if sizeEnd < 0 {
		t.Fatal("#accPageSize 的 change 绑定看起来没结束")
	}
	sizeSeg := sizeBody[:sizeEnd]
	if !strings.Contains(sizeSeg, "ACC_PAGE_SIZE = n;") {
		t.Error("#accPageSize 的 change 没有写回 ACC_PAGE_SIZE")
	}
	if !strings.Contains(sizeSeg, "ACC_PAGE = 1;") {
		t.Error("换每页条数后没有把 ACC_PAGE 归 1：会停在旧尺寸算出来的、并不存在的页上")
	}

	// 换客户端：点导航 chip 归位。
	chipStart := strings.Index(src, `$("#clientChips").addEventListener("click"`)
	if chipStart < 0 {
		t.Fatal("找不到 #clientChips 的 click 绑定")
	}
	chipBody := src[chipStart:]
	chipEnd := strings.Index(chipBody, "\n});")
	if chipEnd < 0 {
		t.Fatal("#clientChips 的 click 绑定看起来没结束")
	}
	if !strings.Contains(chipBody[:chipEnd], "CUR = b.dataset.client; ACC_PAGE = 1;") {
		t.Error("换客户端时没有把 ACC_PAGE 归 1：新池子可能只有 2 页，旧页码没有意义")
	}
}

// TestAccPagerGoPageClampsAndNoopAtEdge 钉住 accGoPage 的两条性质：
//  1. 页码夹在 [1, pages]——键盘或脚本直接调它也不会翻出界；
//  2. 已经在目标页时直接返回，不做无谓的重画（重画会丢掉滚动位置和焦点）。
func TestAccPagerGoPageClampsAndNoopAtEdge(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accGoPage")

	if !strings.Contains(body, "Math.min(pages, Math.max(1, p))") {
		t.Error("accGoPage 没有把页码夹进 [1, pages]：直接调它能翻到不存在的页上")
	}
	if !strings.Contains(body, "if (next === ACC_PAGE) return;") {
		t.Error("accGoPage 没有「已经在这一页就直接返回」：每次点当前页都会白白重画一张表")
	}
	if !strings.Contains(body, "renderAccounts();") {
		t.Error("accGoPage 翻完页没有重画：页码变了但表里还是上一页的行")
	}
}

// TestAccPagerHasButtonsForAllFourMoves 钉住首页 / 上一页 / 下一页 / 末页四个按钮
// 都在，并且各自绑到了 accTurn 上。四缺一的话操作员就得靠搜索框跳页。
func TestAccPagerHasButtonsForAllFourMoves(t *testing.T) {
	src := poolStatsUISource(t)

	for _, c := range []struct{ id, want string }{
		{"accPageFirst", `accTurn(1)`},
		{"accPagePrev", `accTurn(ACC_PAGE - 1)`},
		{"accPageNext", `accTurn(ACC_PAGE + 1)`},
		{"accPageLast", `accTurn(1e9)`},
	} {
		// 按钮在 HTML 里存在。
		if !strings.Contains(src, `id="`+c.id+`"`) {
			t.Errorf("分页条上找不到按钮 #%s", c.id)
		}
		// 并且绑到了 accTurn 上。
		if !strings.Contains(src, `$("#`+c.id+`").addEventListener("click", () => `+c.want+`);`) {
			t.Errorf("#%s 没有绑定到 %s", c.id, c.want)
		}
	}

	// 到头了就禁用对应的那一侧，而不是让操作员点一下发现没反应。
	pagerBody := poolStatsFuncBody(t, src, "renderAccPager")
	for _, want := range []string{
		`$("#accPageFirst").disabled = ACC_PAGE <= 1;`,
		`$("#accPagePrev").disabled = ACC_PAGE <= 1;`,
		`$("#accPageNext").disabled = ACC_PAGE >= pages;`,
		`$("#accPageLast").disabled = ACC_PAGE >= pages;`,
	} {
		if !strings.Contains(pagerBody, want) {
			t.Errorf("renderAccPager 缺少 %q：到头了按钮还能点，操作员点了发现没反应", want)
		}
	}
}

// TestAccPagerTurnScrollsTableBackToTop 钉住翻页后把表格滚回顶部。不滚的话操作员
// 点完「下一页」看到的还是半屏上一页的行，会以为没翻动。
func TestAccPagerTurnScrollsTableBackToTop(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accTurn")

	if !strings.Contains(body, "accGoPage(p);") {
		t.Error("accTurn 没有调 accGoPage")
	}
	if !strings.Contains(body, "wrap.scrollTop = 0;") {
		t.Error("accTurn 翻完页没有把表格滚回顶部：操作员看到的还是上一页那半屏，会以为没翻动")
	}
}

// TestAccPagerCountsStillUseTheFullList 钉住 accNote 里的计数说的是全量，不是当前
// 这一页。分页是给眼睛看的，数字得始终回答「我一共管着多少个账号」——
// 写成 pageRows.length 的话，操作员在第 2 页会看到「共 50 个账号」。
func TestAccPagerCountsStillUseTheFullList(t *testing.T) {
	src := poolStatsUISource(t)

	// accNote 的总账号数来自 groups.length（全量），不是 pageRows.length。
	if !strings.Contains(src, `CUR + " · " + groups.length + " 个账号"`) {
		t.Error("accNote 没有按全量 groups.length 计数")
	}
	if strings.Contains(src, `pageRows.length + " 个账号"`) {
		t.Error("accNote 按当前页的行数报总数：操作员在第 2 页会以为池子里只有 50 个账号")
	}

	// 分页条上自己那行字要给出「第几到第几 / 共多少」，操作员不用回头数表格。
	pagerBody := poolStatsFuncBody(t, src, "renderAccPager")
	if !strings.Contains(pagerBody, `"第 " + (from + 1) + "-" + (from + shownCount) + " 个，共 " + total + " 个账号"`) {
		t.Error("分页条上没有「第 N-M 个，共 X 个账号」：操作员不知道自己看到的是哪一段")
	}
	// 页码也在 accNote 里追加一句，扫一眼表头就知道自己在第几页。
	if !strings.Contains(src, `accText += " · 第 " + ACC_PAGE + "/" + pages + " 页"`) {
		t.Error("accNote 没有追加「第 N/M 页」")
	}
}

// TestAccPagerGuardCatchesBreakage 是负控：把上面每一条检查里的关键片段分别改坏，
// 同一个检查函数必须报出缺失。这一条是为了证明上面那些断言不是只在自证。
func TestAccPagerGuardCatchesBreakage(t *testing.T) {
	base := "let ACC_PAGE = 1;\nlet ACC_PAGE_SIZE = 50;\n" +
		"function hideAccPager() {\n  $(\"#accPageInfo\").textContent = \"\";\n  $(\"#accPageNow\").textContent = \"\";\n}\n" +
		"function renderAccPager() {\n  if (pages <= 1) { hideAccPager(); return; }\n" +
		"  $(\"#accPageFirst\").disabled = ACC_PAGE <= 1;\n" +
		"  $(\"#accPagePrev\").disabled = ACC_PAGE <= 1;\n" +
		"  $(\"#accPageNext\").disabled = ACC_PAGE >= pages;\n" +
		"  $(\"#accPageLast\").disabled = ACC_PAGE >= pages;\n" +
		"  $(\"#accPageInfo\").textContent = \"第 \" + (from + 1) + \"-\" + (from + shownCount) + \" 个，共 \" + total + \" 个账号\";\n}\n" +
		"function accGoPage() {\n  const pages = Math.max(1, Math.ceil(accShownGroups().length / ACC_PAGE_SIZE));\n" +
		"  const next = Math.min(pages, Math.max(1, p));\n  if (next === ACC_PAGE) return;\n  renderAccounts();\n}\n" +
		"function accTurn(p) {\n  accGoPage(p);\n  wrap.scrollTop = 0;\n}\n" +
		"function accShownGroups() { return groups; }\n" +
		"const pageRows = shown.slice(from, from + ACC_PAGE_SIZE);\n" +
		"tb.innerHTML = pageRows.map(g => {\n" +
		"  if (ACC_PAGE > pages) ACC_PAGE = pages;\n  if (ACC_PAGE < 1) ACC_PAGE = 1;\n" +
		"  const shown = accShownGroups();\n" +
		"  accSearchTick = 0; ACC_PAGE = 1; renderAccounts();\n" +
		"  ACC_PAGE_SIZE = n;\n  ACC_PAGE = 1;\n" +
		"  CUR = b.dataset.client; ACC_PAGE = 1;\n" +
		"  accText += \" · 第 \" + ACC_PAGE + \"/\" + pages + \" 页\"\n" +
		"  \" + CUR + \" · \" + groups.length + \" 个账号\"\n"

	if miss := accPagerWiring(base); len(miss) != 0 {
		t.Fatalf("负控自己失效了：完好的样例源码被报成缺 %d 处（%s）", len(miss), strings.Join(miss, "；"))
	}

	// 逐条改坏，每一条都必须被抓出来。
	for _, c := range []struct{ what, broken string }{
		{"切在凭据行上而不是组上", "const pageRows = shown.slice(from, from + ACC_PAGE_SIZE);\n"},
		{"页码没有夹上界", "  if (ACC_PAGE > pages) ACC_PAGE = pages;\n"},
		{"页码没有兜底下界", "  if (ACC_PAGE < 1) ACC_PAGE = 1;\n"},
		{"单页时没收起分页条", "  if (pages <= 1) { hideAccPager(); return; }\n"},
		{"渲染没有走共享筛选", "  const shown = accShownGroups();\n"},
		{"翻页没有走共享筛选", "const pages = Math.max(1, Math.ceil(accShownGroups().length / ACC_PAGE_SIZE));\n"},
		{"翻页没有归位", "accSearchTick = 0; ACC_PAGE = 1; renderAccounts();\n"},
		{"按钮没有禁用", "  $(\"#accPageNext\").disabled = ACC_PAGE >= pages;\n"},
		{"收起时留着上一个池子的旧数字", "  $(\"#accPageInfo\").textContent = \"\";\n"},
		{"按钮没有禁用（首页）", "  $(\"#accPageFirst\").disabled = ACC_PAGE <= 1;\n"},
	} {
		broken := strings.Replace(base, c.broken, "", 1)
		if broken == base {
			t.Fatalf("负控自己失效了：样例源码里找不到 %q", c.broken)
		}
		if miss := accPagerWiring(broken); len(miss) == 0 {
			t.Errorf("把 %s 改坏之后检查仍然全绿：这条检查挡不住这种改坏", c.what)
		}
	}
}
