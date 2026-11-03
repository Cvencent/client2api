package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 页头标题的截断与上限（issue #143 的前端一半）。
//
// 服务端那一半在 chat_title_cap_test.go；这里钉的是**样式表与脚本里会漂移
// 的那几条**。真正的几何判据（宽标题会不会盖住下拉）在 cmd/panelsmoke 的真
// 浏览器阶段（assertChatHeader），因为「元素多宽、谁盖住谁」只有浏览器知道。
//
// 这一组存在的理由是本仓反复出现的那个坑：**同权重后写者胜**（#86 / #109 /
// #111 / #123 / #139 都栽在这上面）。所以断言不是「某处有 text-overflow」，
// 而是「那条截断和它的 white-space 在同一组声明里」——否则一次重排就能让
// 截断静默失效，而没有任何测试会红。
// ---------------------------------------------------------------------------

// cssRulesFor 抽出 `.selector { ... }` 的**全部**声明块。样式表里同一个选择器
// 常有两条（基础 + 尺寸/主题覆盖），只看第一条会读到另一条，断言就成了运气。
func cssRulesFor(src, selector string) []string {
	needle := selector + " {"
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], needle)
		if j < 0 {
			break
		}
		j += i
		rest := src[j:]
		open := strings.Index(rest, "{")
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		end := strings.Index(rest, "}")
		if end < 0 {
			break
		}
		out = append(out, rest[:end])
		i = j + 1
	}
	return out
}

// TestTheHeaderTitleTruncatesInTheSameRule pins that the three declarations and
// the nowrap live together.  A separate later rule would pass a naive
// "contains" check and still be one reorder away from silently dying.
func TestTheHeaderTitleTruncatesInTheSameRule(t *testing.T) {
	src := poolStatsUISource(t)

	// 深色/主题覆盖里那一份只有字号，没有 white-space；取带 nowrap 的那条。
	body := ""
	for _, sel := range []string{".chat-head-title h3"} {
		// 页面上这个选择器出现两次（基础 + 尺寸覆盖）。必须**每一条**都不
		// 单独宣告 white-space:nowrap，而截断那条要自己带全三条。
		if n := strings.Count(src, sel+" {"); n == 0 {
			t.Fatalf("没有 %q 的规则", sel)
		}
		_ = body
	}

	// 逐条扫：任何一条只说 nowrap 而不说 ellipsis 的就是半拉截断。
	idx := 0
	found := 0
	for {
		i := strings.Index(src[idx:], ".chat-head-title h3 {")
		if i < 0 {
			break
		}
		i += idx
		rest := src[i:]
		open := strings.Index(rest, "{")
		end := strings.Index(rest[open:], "}")
		block := rest[open+1 : open+end]
		found++
		if strings.Contains(block, "white-space: nowrap") {
			for _, want := range []string{"min-width: 0", "overflow: hidden", "text-overflow: ellipsis"} {
				if !strings.Contains(block, want) {
					t.Errorf("`.chat-head-title h3` 的这条声明里有 white-space:nowrap 但没有 %q：\n%s\n"+
						"这就是 issue #143 报的「写着可截断却零截断」——同一个声明块里补齐，别靠后置规则", want, strings.TrimSpace(block))
				}
			}
		}
		idx = i + 1
	}
	if found == 0 {
		t.Fatal("没有找到 .chat-head-title h3 的任何规则")
	}

	// 反向：不能有第二条 `.chat-head-title h3` 规则来「补」截断——那正是
	// 会被一次重排干掉的写法。截断必须在宣告 nowrap 的那条里。
	if n := strings.Count(src, ".chat-head-title h3 {"); n > 2 {
		t.Errorf("`.chat-head-title h3` 出现了 %d 条规则：截断应当写进宣告 nowrap 的那一条，"+
			"而不是再叠一条（同权重后写者胜，本仓已出过四次事故）", n)
	}
}

// TestTheHeaderTitleSupportsFullTextOnHover is #143's suggestion 4: if the
// operator's title is going to be clipped, the clipped part needs an outlet.
func TestTheHeaderTitleSupportsFullTextOnHover(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "chatSyncHead")

	if !strings.Contains(body, "#chatTitle") {
		t.Fatal("chatSyncHead 不再写 #chatTitle")
	}
	if !strings.Contains(body, "h3.title = ") && !strings.Contains(body, ".title = title") {
		t.Error("页头标题被省略号截断（#143），但没有挂 title 全文：被裁掉的尾部没有出口")
	}
	// 挂的必须是**全文**，不是被截断后的副本（那等于把同一个信息丢掉两次）。
	if !strings.Contains(body, "title = t ? chatTitleOf(t) : \"新对话\"") {
		t.Error("chatSyncHead 挂 title 的不是会话全名：悬停提示和显示用了两个来源")
	}
}

// TestTheRenamePromptCarriesTheSameLimit: 前端那道闸门和服务端必须是同一个
// 数字，否则两端各说各话——页面拦住 121 字，服务端却照收。
func TestTheRenamePromptCarriesTheSameLimit(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "const CHAT_TITLE_MAX = ") {
		t.Fatal("index.html 没有 CHAT_TITLE_MAX：前端的判据散在代码里，改服务端就必须全仓库找")
	}
	// 数字必须等于服务端那份。两边都是同一份契约的两个副本，一个改了另一个
	// 就是谎话（#143 的两条证据分别打在这两端上）。
	body := poolStatsFuncBody(t, src, "chatRenameThread")
	if !strings.Contains(body, "CHAT_TITLE_MAX") {
		t.Error("chatRenameThread 没有用 CHAT_TITLE_MAX：重命名路径不受前端上限约束")
	}
	if !strings.Contains(body, "length > CHAT_TITLE_MAX") {
		t.Error("chatRenameThread 没有在提交前比对上限：超长会被服务端拒，操作员却只看到一句「重命名失败」")
	}
	// 拒绝必须是提示 + 不提交，不能静默截断后提交（#121/#122 的教训）。
	if !strings.Contains(body, "toast(") {
		t.Error("超限的重命名没有给出提示")
	}
	if i, j := strings.Index(body, "length > CHAT_TITLE_MAX"), strings.Index(body, "await api("); i >= 0 && j >= 0 && i > j {
		t.Error("上限判据排在发出请求之后：超长已经写进服务端了")
	}
}

// TestTheHeaderTitleIsNotClippedByAnAncestor pins the other half of #143's
// geometry: `.chat-head-title` is the flex item whose min-content size (the
// unbreakable title) used to win over `flex: 1 1 220px`.  Without min-width:0
// the shrink never happens, so the h3 rule above would never even be reached.
func TestTheHeaderTitleIsNotClippedByAnAncestor(t *testing.T) {
	src := poolStatsUISource(t)

	// 这个选择器有多条规则（尺寸覆盖里也有一条），所以判据是「**宣告 flex
	// 基宽的那条**必须自己带 min-width:0」，而不是「某处出现过 min-width:0」。
	var withFlex []string
	for _, block := range cssRulesFor(src, ".chat-head-title") {
		if strings.Contains(block, "flex: 1 1 220px") {
			withFlex = append(withFlex, block)
		}
	}
	if len(withFlex) == 0 {
		t.Fatal(".chat-head-title 不再是 flex: 1 1 220px 的可伸缩项：页头布局变了，标题截断的前提也就变了")
	}
	for _, block := range withFlex {
		if !strings.Contains(block, "min-width: 0") {
			t.Errorf(`宣告 flex: 1 1 220px 的那条 .chat-head-title 缺 min-width:0：
%s
flex 项的 min-width 默认是 auto（= 不换行标题的 min-content 宽度），长标题下「可收缩」失效，
页头照旧被撑开（#143 的 8,000,489px scrollWidth）`, strings.TrimSpace(block))
		}
	}
}
