package panel

import (
	"strings"
	"testing"
)

// 钉住「面板脚本必须能通过语法解析」这条底线。
//
// 2026-11-06 两次事故都躲过了 go test：一次是搬块把函数定义搬没了（运行时
// ReferenceError），一次是括号错位让 rlRenderEvents 里引用了不存在的 `l`
// （同样只在浏览器里报）。两者都躺在 index.html 里，Go 侧一个字都看不到。
// 真浏览器验证是主防线（见 AGENTS.md「面板 JS 改动纪律」），这里再补一条
// 成本最低的结构检查：script 标签必须成对、且顶层不含明显不配平的括号。
//
// 完整的 JS 语法校验依赖 node（构建机上不一定有），所以这里只断言结构性
// 事实：<script> 与 </script> 数量一致，且 rlRenderEvents 里 t.lanes 的
// map 回调必须把 `l.` 引用包在自己的表达式内（本次事故的具体形状）。
func TestPanelScriptIsStructurallySane(t *testing.T) {
	src := poolStatsUISource(t)

	if n, c := countTag(src, "<script"), countTag(src, "</script>"); n != c {
		t.Fatalf("<script> 与 </script> 不配对：开 %d / 闭 %d", n, c)
	}

	// 事故形状钉：t.lanes 的 map 回调里，`l.` 必须在 `(l =>` 之后且闭合在
	// 同一段表达式内；括号层数写错会得到 `l is not defined`（浏览器才知道）。
	rl := poolStatsFuncBody(t, src, "rlRenderEvents")
	if rl == "" {
		t.Fatal("rlRenderEvents 不见了")
	}
	if !strings.Contains(rl, ".map(l => (l.account_label || l.account || l.client || \"\")") {
		t.Error("rlRenderEvents 里 lanes 的 map 回调没有把属性访问包进自己的表达式：括号错位会让 l 落到 map 作用域之外（2026-11-06 事故）")
	}
}

func countTag(src, tag string) int { return strings.Count(src, tag) }
