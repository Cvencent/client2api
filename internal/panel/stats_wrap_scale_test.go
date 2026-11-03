package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 文字放大后的统计带（issue #159）。
//
// .stats 的换行判据原本是**视口宽度**（1160px 断点），而 WCAG 1.4.4 的
// 200% 文字路径不改变 innerWidth——字放大了、格子没变宽（min-width: 112px
// 是绝对像素），标签被 ellipsis 就地吃掉（200% 实测 9/10 截断，两格连
// tooltip 都没有）。看守：换行常开、格子宽度随字号一起变（em）、
// 两格 div.stat 补上可读回的 title。
// ---------------------------------------------------------------------------

// TestStatsWrapNotViewportGated 钉住换行判据：flex-wrap 必须常开（不被
// @media (max-width: 1160px) 门控），min-width 必须是 em（随字号变宽）。
func TestStatsWrapNotViewportGated(t *testing.T) {
	src := poolStatsUISource(t)

	// 统计带的权威定义（-- 31. 统计带（权威层））里 flex-wrap 必须是 wrap。
	i := strings.Index(src, ".stats {\n  display: flex;")
	if i < 0 {
		t.Fatal(".stats 的权威定义不在：看守失去锚点（issue #159）")
	}
	head := src[i : i+900]
	if !strings.Contains(head, "flex-wrap: wrap;") {
		t.Error(".stats 的换行不是常开：文字放大路径（不改变视口宽度）走不进断点，标签被 ellipsis 吃掉（issue #159）")
	}
	if strings.Contains(head, "flex-wrap: nowrap") {
		t.Error(".stats 回到 nowrap：200% 字号下 9/10 标签截断（issue #159）")
	}
	// min-width 是 em（随字号一起变宽），不是绝对像素。
	if !strings.Contains(src, "min-width: 7em;") {
		t.Error(".stat 的 min-width 不再是 7em：绝对像素不随字号涨，放大后格子不够宽（issue #159）")
	}
	if strings.Contains(src, "min-width: 112px;") {
		t.Error("min-width: 112px 回来了：绝对像素与 rem 字号不匹配（issue #159）")
	}
}

// TestStatsLabelsReadableBack 钉住「截断即可读回」对齐：两格无 title 的
// div.stat 必须补上（200% 下「已…」「就绪客…」连悬停都没有），其余八格的
// title 必须以标签原文开头（悬停内容和被截断的词是同一件事）。
func TestStatsLabelsReadableBack(t *testing.T) {
	src := poolStatsUISource(t)

	for _, pair := range [][2]string{
		{`id="sClients"`, `"已接入客户端`},
		{`id="sReady"`, `"就绪客户端`},
	} {
		i := strings.Index(src, pair[0])
		if i < 0 {
			t.Fatalf("%s 不在统计带标记里（issue #159）", pair[0])
		}
		frame := src[max(0, i-220):i]
		if !strings.Contains(frame, `class="stat" title="`) {
			t.Errorf("%s 所在的格没有 title：200%% 下只剩「已…」「就绪客…」且无任何可读回入口（issue #159）", pair[0])
		}
	}
	// 八个 button 格的 title 以标签原文开头（「标签：动作」两段式）。
	for _, lead := range []string{"账号总数：", "可用：", "冷却中：", "积分不足：", "风控：", "账号异常：", "已停用：", "粘性会话："} {
		if !strings.Contains(src, `title="`+lead) {
			t.Errorf("统计带 title 缺少以 %q 开头的两段式写法：悬停内容与被截断的标签不是同一段话（issue #159）", lead)
		}
	}
}
