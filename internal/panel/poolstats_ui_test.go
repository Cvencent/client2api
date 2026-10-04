package panel

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号池的「在途 / 粘性会话 / Redis 模式」三块界面的回归测试。
//
// 这三样数据的唯一来源是 GET /panel/api/status：overview 的每一行是
// core.Status 的子集，没有 pool / health，没有顶层 redis_mode，账号行也没有
// extra。而面板里这些数字全是 index.html 里的字符串拼接：Go 编译不管它们，
// shell_test.go 那只保证 $() 指的 id 存在。所以这里静态钉住四件事：
//   1. 界面读的键 == core.PoolStats / core.Health 的 json tag（错一个字母 =
//      这块数字永远是空的，而且两边都不报错）；
//   2. 三个降级分支都在：没有客户端报 pool → 留白、账号没有 in_flight 键 →
//      「—」、没有上限（0 / 没报）→ ∞；
//   3. 在途列的表头、行模板、两个空态的 colspan 列数一致；
//   4. accNote 与 #navRedis 是"追加"，不是覆盖已有文本。
// ---------------------------------------------------------------------------

// poolStatsUISource 返回内嵌的 shell 源码。
func poolStatsUISource(t *testing.T) string {
	t.Helper()
	src := string(indexHTML)
	if strings.TrimSpace(src) == "" {
		t.Fatal("index.html was not embedded")
	}
	return src
}

// poolStatsFuncBody 截取一个顶层 function 的函数体：这些函数都以行首的 "}" 结束，
// 到下一个 "\n}" 为止就是完整实现（够用来断言"这段代码里有没有这个分支"）。
func poolStatsFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("index.html has no function %s", name)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}"); end >= 0 {
		return rest[:end]
	}
	t.Fatalf("function %s looks unterminated", name)
	return ""
}

// TestPoolStatsUIReadsTheKeysTheGoStructsSend 是本文件唯一的跨包一致性检查：
// 界面读的字段名必须逐字等于 Go 侧的 json tag（tag 名错了、界面拼错了，
// 这块数字就永远是空的，而没有任何一边会失败）。最后再确认这些字段真的被
// 取回来并用起来了，而不是只出现在注释里。
func TestPoolStatsUIReadsTheKeysTheGoStructsSend(t *testing.T) {
	src := poolStatsUISource(t)

	for _, c := range []struct {
		path string
		keys []string
	}{
		{"../core/poolstats.go", []string{"in_flight", "in_flight_full", "sticky_sessions"}},
		{"../core/health.go", []string{"servable", "note"}},
	} {
		b, err := os.ReadFile(c.path)
		if err != nil {
			t.Skipf("cannot read %s: %v", c.path, err)
		}
		inGo := map[string]bool{}
		for _, m := range regexp.MustCompile(`json:"([a-z_]+)[,"]`).FindAllSubmatch(b, -1) {
			inGo[string(m[1])] = true
		}
		if len(inGo) == 0 {
			t.Fatalf("%s 里一个 json tag 都没解析到", c.path)
		}
		for _, k := range c.keys {
			if !inGo[k] {
				t.Errorf("%s 的 struct 没有 json tag %q", c.path, k)
			}
		}
	}

	// 面板真的从 status 里读走了这些字段。
	for _, want := range []string{
		`api("/panel/api/status")`, // 数据只在 status 里，必须真的去拉
		"d.redis_mode",             // 顶层：本地内存 / Redis 镜像
		"c.pool", "c.health",       // 每个客户端的池与健康
		"a.extra", "a.id", // 每账号的在途租约（extra 按账号 id 索引）
		"x.in_flight", "x.in_flight_limit",
		"hx.servable", "hx.note", // 不可服务时用来讲理由
		// #sSticky tile 是全网关口径；accNote 是当前客户端口径，见下一个断言。
		`poolSum("sticky_sessions")`,
		"px.pool.in_flight_full",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("界面里找不到 %q：这块数据没有真的取回来或用起来", want)
		}
	}

	// 在途列的数据源头：workbuddy 把每账号的租约写进 Status().Accounts[].Extra。
	// 这条链路断了，这一列会安静地永远显示「—」。
	const poolGo = "../../clients/workbuddy/pool.go"
	if b, err := os.ReadFile(poolGo); err == nil {
		for _, want := range []string{`extra["in_flight"]`, `extra["in_flight_limit"]`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s 不再写 %s：面板的在途列会永远是「—」", poolGo, want)
			}
		}
	} else {
		t.Logf("skip %s: %v", poolGo, err)
	}
}

// accountsHeaderCells 数出账号表的表头列数。
func accountsHeaderCells(t *testing.T, src string) int {
	t.Helper()
	// The accounts table carries a <colgroup> for its fixed-width columns, so
	// the header-cell counter has to step over it instead of falling through
	// to the next .acc table on the page (the tasks table).
	m := regexp.MustCompile(`(?s)<table class="acc">\s*(?:<colgroup>.*?</colgroup>\s*)?<thead><tr>(.*?)</tr></thead>`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal(`找不到账号表的 <table class="acc"> <thead>`)
	}
	return strings.Count(m[1], "<th")
}

// accountsRowCells 数出 accRowHTML 里行模板的 <td 个数。
//
// 行模板在 accRowHTML 里而不是 renderAccounts 里：renderAccounts 现在按账号分组，
// 一行可能是一个账号，也可能是它下面的一个通道，但两者都是同一个函数渲染的，
// 所以列数只需要在那一处对齐。
func accountsRowCells(t *testing.T, src string) int {
	t.Helper()
	body := poolStatsFuncBody(t, src, "accRowHTML")
	start := strings.Index(body, "return '<tr'")
	if start < 0 {
		t.Fatal("accRowHTML 里找不到行模板 return '<tr'")
	}
	rest := body[start:]
	end := strings.Index(rest, "';")
	if end < 0 {
		t.Fatal("accRowHTML 的行模板看起来没结束")
	}
	return strings.Count(rest[:end], "'<td")
}

// TestInFlightColumnIsWiredEndToEnd 钉住「在途」列从头到尾都在：表头一格、
// 行模板一格、两个空态的 colspan 跟着 +1。少任何一处，空表或整行都会错位。
func TestInFlightColumnIsWiredEndToEnd(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "<th>在途</th>") {
		t.Error("账号表表头没有「在途」列")
	}
	if h, r := accountsHeaderCells(t, src), accountsRowCells(t, src); h != r {
		t.Errorf("账号表表头 %d 列、行模板 %d 格：多一列不改另一处会整行错位", h, r)
	}
	// colspan 不写死数字：列数一变它就错位，而错位只在空表上看得见。
	// 余额列让这件事多了一层——它在模块没实现 BalanceProvider 时整列收起，
	// 所以 colspan 必须从「余额列在不在」推出来，而且两个空态共用同一个值。
	// 这里拿表头的列数去要求那一行，再加一列时它会红，改对了就不必再动测试。
	n := accountsHeaderCells(t, src)
	if want := fmt.Sprintf("const cols = showBal ? %d : %d;", n, n-1); !strings.Contains(src, want) {
		t.Errorf("renderAccounts 的 colspan 没有跟着表头的 %d 列走：缺 %q", n, want)
	}
	for _, want := range []string{
		`colspan="' + cols + '" class="hint">没有已注册的客户端`,
		`colspan="' + cols + '" class="hint">' + (rec.unsupported`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("账号表的空态没有跟着列数改 colspan：缺 %q", want)
		}
	}
	// 行里那格必须走 inflightCell（它才是留白规则的唯一实现），
	// 而不是就地拼一个数字。
	if !strings.Contains(src, `'<td class="num">' + inflightCell(CUR, a) + '</td>'`) {
		t.Error("行模板没有用 inflightCell 渲染在途列")
	}
}

// TestInFlightCellDegradesInsteadOfInventingZeros 钉住三个降级分支。它们不是
// 文案细节：把「模块不报在途」画成 0/0 会让操作员以为池子是空的。
func TestInFlightCellDegradesInsteadOfInventingZeros(t *testing.T) {
	src := poolStatsUISource(t)

	body := poolStatsFuncBody(t, src, "inflightCell")
	if !strings.Contains(body, `if (!x) return '<span class="hint">—</span>';`) {
		t.Error("没有 in_flight 键的账号必须画「—」，不能编一个 0/0")
	}
	if !strings.Contains(body, `(lim == null || lim === 0) ? "∞" : lim`) {
		t.Error("in_flight_limit 为 0（或模块没报上限）时必须画 ∞")
	}

	// 判空的口径是「in_flight 这个键在不在」，不是「值是不是 0」。
	extra := poolStatsFuncBody(t, src, "acctExtra")
	for _, want := range []string{"own.in_flight != null", "st.in_flight != null"} {
		if !strings.Contains(extra, want) {
			t.Errorf("acctExtra 缺少 %q：留白判据不再是「键存在与否」", want)
		}
	}

	// 全网关没有任何客户端报 pool 时，粘性会话这块留白。
	if sum := poolStatsFuncBody(t, src, "poolSum"); !strings.Contains(sum, "if (!pools.length) return null;") {
		t.Error("poolSum 在没有池时返回的不是 null：会把「没有池」画成 0")
	}
	if stats := poolStatsFuncBody(t, src, "renderStats"); !strings.Contains(stats, `(sticky == null) ? "-" : sticky`) {
		t.Error("#sSticky 没有留白分支（没有池时要显示 -，不是 0）")
	}
	// 只要有一个客户端报了 pool，0 就是真的 0（池是空的）——不能被当成留白。
	if stats := poolStatsFuncBody(t, src, "renderStats"); !strings.Contains(stats, `poolSum("sticky_sessions")`) {
		t.Error("renderStats 没有用跨客户端的 poolSum 取粘性会话数")
	}
}

// TestAccNoteAndNavRedisAppendInsteadOfReplacing 钉住两处"追加"：原文本必须先
// 拼出来，新信息挂后面；为零时一个字都不动。
func TestAccNoteAndNavRedisAppendInsteadOfReplacing(t *testing.T) {
	src := poolStatsUISource(t)
	acc := poolStatsFuncBody(t, src, "renderAccounts")

	// 计数口径是「账号」而不是「凭据」：一个账号有多个登录通道时它仍然只有一个，
	// 这和表里按 identity 分组后的行数必须是同一个数字。
	if !strings.Contains(src, `CUR + " · " + groups.length + " 个账号"`) {
		t.Error("accNote 原有的「客户端 · N 个账号」被顶掉了（而且 N 必须按账号数，不按凭据数）")
	}
	if !strings.Contains(acc, "let accText = CUR +") {
		t.Error("accNote 没有先拼出原文本、再往 accText 上追加")
	}
	if !strings.Contains(acc, "if (full) accText +=") {
		t.Error("「N 个账号在途占满」必须被 if (full) 门控：为 0 时不该改文本")
	}
	if !strings.Contains(acc, `accText += " · " + full + " 个账号在途占满"`) {
		t.Error("accNote 没有在 in_flight_full 非 0 时追加「N 个账号在途占满」")
	}
	// accNote 紧挨着 "CUR · N 个账号"，口径必须是当前客户端；用全网关的
	// poolSum 会在这张表上说另一个客户端的闲话（workbuddy 0 个账号旁边
	// 挂着「2 个账号在途占满」）。
	if !strings.Contains(acc, "px.pool.in_flight_full") {
		t.Error("accNote 的 in_flight_full 没有按当前客户端（poolClient(CUR)）取")
	}
	if strings.Contains(acc, `poolSum("in_flight_full")`) {
		t.Error("accNote 的 in_flight_full 用了全网关汇总：与这张表的 CUR 口径不一致")
	}
	if !strings.Contains(acc, "hx && hx.servable === false") || !strings.Contains(acc, "hx.note") {
		t.Error("health.servable 为假时没有把模块给的 note 当理由显示")
	}

	nav := poolStatsFuncBody(t, src, "setNav")
	if !strings.Contains(nav, `NAMES.length + " 客户端 / " + ready + " 就绪 · " + redisLabel()`) {
		t.Error("#navRedis 没有保留客户端计数并追加 redis 模式")
	}
	label := poolStatsFuncBody(t, src, "redisLabel")
	if !strings.Contains(label, `m === "upstash" ? "Redis 镜像" : "本地内存"`) {
		t.Error(`redisLabel 没照抄原版措辞（upstash → Redis 镜像，其余 → 本地内存）`)
	}
}

// TestPoolStatsPoolIndexKeepsOrMissesTheOptionalBlocks 是降级规则的数据侧：
// poolIndex 必须把 «没有这个键» 与 «这个键是空对象» 分开——前者是 null（留白），
// 后者是 {}（真值，表示这个模块确实报了池，只是全是 0）。
func TestPoolStatsPoolIndexKeepsOrMissesTheOptionalBlocks(t *testing.T) {
	src := poolStatsUISource(t)
	idx := poolStatsFuncBody(t, src, "poolIndex")
	for _, want := range []string{
		"c.pool || null",
		"c.health || null",
		"(c.accounts || []).forEach",
		"if (a && a.extra)",
	} {
		if !strings.Contains(idx, want) {
			t.Errorf("poolIndex 缺少 %q：缺席的 pool / health / extra 会变成假数据", want)
		}
	}
}

// TestPoolStatsIDGuardCatchesARenamedTile 是负控：shell_test.go 的守卫是这块
// 界面唯一的自动检查，把 sSticky 改名后它必须报出来——否则上面那些
// strings.Contains 就可能是在自证。
func TestPoolStatsIDGuardCatchesARenamedTile(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src, `$("#sSticky")`, `$("#sStickyRenamed")`, -1)
	if broken == src {
		t.Fatal(`测试自己失效了：源码里没有 $(" #sSticky")`)
	}
	for _, id := range shellDanglingIDs(broken) {
		if id == "sStickyRenamed" {
			return
		}
	}
	t.Error("shell id 守卫对改名后的 #sSticky 没意见：它守不住这一块界面")
}
