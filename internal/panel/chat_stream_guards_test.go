package panel

import (
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 对话页滚动与等待态（issue #161/#162）与 scrollIntoView 的 reduce 关（#163）。
//
// 三条互为映照的缺陷都在同一页：流式分片把视口平滑拽回底部（日志页有「本来
// 就在底部」判据、对话页没有）、CSS 的 scroll-behavior: smooth 让「渲染完
// 落到底」变成 400ms 动画、思考阶段只累加不重绘 + 写好了却从未播过的
// .chatmsg.pending 光标。看守把这些分叉钉死。
// ---------------------------------------------------------------------------

// TestChatScrollBottomHasTheAtEndGuard 钉住 chatScrollBottom 的判据：调用前
// 先看「用户是不是本来就在底部」，不在底部就不动（阈值与日志页对齐 24px）。
func TestChatScrollBottomHasTheAtEndGuard(t *testing.T) {
	src := poolStatsUISource(t)

	i := strings.Index(src, "function chatScrollBottom(")
	if i < 0 {
		t.Fatal("chatScrollBottom 不在：看守失去锚点（issue #161）")
	}
	body := src[i : i+520]
	if !strings.Contains(body, "atEnd") {
		t.Error("chatScrollBottom 没有「本来就在底部」判据：流式期间每来一片都把翻上去读历史的用户拽回底部（issue #161）")
	}
	if !strings.Contains(body, "scrollHeight - 24") {
		t.Error("chatScrollBottom 的阈值与日志页不对齐（24px）：两页同一动作两个口径（issue #161）")
	}
	if !strings.Contains(body, "force") {
		t.Error("chatScrollBottom 没有 force 通道：发送那一刻的主动落位会被判据误伤（issue #161）")
	}

	// 发送时刻必须 force（用户就在输入行，这一次的落位是用户动作的一部分）。
	j := strings.Index(src, "CHAT.ctrl = new AbortController();")
	if j > 0 && !strings.Contains(src[j:j+400], "chatScrollBottom(true)") {
		t.Error("发送时刻没有 force 落位：新消息可能落不到视口（issue #161）")
	}

	// CSS 不再带 smooth：落位瞬时（动画走 JS、reduce 下也能全关）。只看规则
	// 本体，不看引用它的注释。
	j2 := strings.Index(src, ".chat-scroll {")
	if j2 < 0 || strings.Contains(src[j2:j2+200], "scroll-behavior") {
		t.Error(".chat-scroll 回到 scroll-behavior: smooth：开长对话的 400ms 自滚回来了（issue #161）")
	}
}

// TestStreamDrawsReasoningAndPending 钉住思考阶段的三重缺失（不重绘、无
// pending 光标、无等待文案）——deepseek/GLM 这类模型思考阶段比正文长得多，
// 只累加字符串的话屏幕上就是 4px 的空白。
func TestStreamDrawsReasoningAndPending(t *testing.T) {
	src := poolStatsUISource(t)

	// 推理分支必须有重绘调用（正文分支同类处理）。
	i := strings.Index(src, "if (d.reasoning_content)")
	if i < 0 {
		t.Fatal("推理分支不在流循环里：看守失去锚点（issue #162）")
	}
	seg := src[i : i+320]
	if !strings.Contains(seg, "chatDraw(true)") {
		t.Error("推理分片不重绘：思考阶段屏幕空白，直到正文第一片才一次性出现（issue #162）")
	}
	if !strings.Contains(seg, "out.pending = false") {
		t.Error("推理分片不解开 pending：思考阶段结束光标还闪着（issue #162）")
	}

	// pending 档位真的被写进 chatmsg 的 class 拼装（add("pending") 的等价物）。
	if !strings.Contains(src, `" pending" :`) && !strings.Contains(src, `m.pending ? " pending"`) {
		t.Error(`chatmsg 的 class 拼装没有 pending 档：.chatmsg.pending 的样式（含 chatCaret 光标）从 v0.3.3 起从未生效（issue #162）`)
	}

	// 发送时挂上 pending；settle 收尾必须清掉（落盘的记录不带等待态）。
	if !strings.Contains(src, "pending: true") {
		t.Error("发送时刻没有挂 pending：等待首字的状态没有入口（issue #162）")
	}
	k := strings.Index(src, "function chatSettle(")
	if k < 0 || !strings.Contains(src[k:k+120], "out.pending = false") {
		t.Error("chatSettle 不清 pending：收尾后记录还带着等待态（issue #162）")
	}

	// 等待文案：首字未到时 #chatDiag 不该空着。
	if !strings.Contains(src, `chatDiag("等待首字…")`) {
		t.Error("发送时刻没有写「等待首字…」进诊断行：#chatDiag 要等整轮结束才有内容（issue #162）")
	}
}

// TestNoScrollIntoViewSmoothLiteral 钉住 #163 的机械守卫：全文件的 JS 不允许
// 再出现 scrollIntoView 的 behavior:"smooth" 字面量——CSS 的
// scroll-behavior:auto !important 管不住 JS 显式传的选项，省掉参数走 CSS。
func TestNoScrollIntoViewSmoothLiteral(t *testing.T) {
	src := poolStatsUISource(t)

	jsStart := strings.Index(src, "scrollIntoView")
	if jsStart < 0 {
		t.Fatal("找不到 scrollIntoView：看守失去锚点（issue #163）")
	}
	// 只看存在 scrollIntoView 的那些行；把行注释与块注释剥掉。
	code := ""
	for _, ln := range strings.Split(src, "\n") {
		if !strings.Contains(ln, "scrollIntoView") {
			continue
		}
		if cut := strings.Index(ln, "//"); cut >= 0 {
			ln = ln[:cut]
		}
		code += ln + "\n"
	}
	re := regexp.MustCompile(`behavior:\s*["']smooth["']`)
	if loc := re.FindStringIndex(code); loc != nil {
		ctx := code[loc[0]:int(min(len(code), loc[1]+80))]
		t.Errorf("JS 仍在给 scrollIntoView 显式传 behavior:\"smooth\"：%q -- CSS 的 auto !important 管不住它，reduce 用户会被强加位移（issue #163）", strings.TrimSpace(ctx))
	}
}
