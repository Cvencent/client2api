package panel

import (
	"strings"
	"testing"
)

// 钉住 #220（控件边界对比度）与 #221（统计带标签对齐）的修复形状。
//
// 对比度本身由 cmd/panelsmoke 的 controlBoundary 阶段在真浏览器里量（两套
// 主题各一遍，断言 ≥3:1——见该文件）；这里只钉「控件边界不走装饰性 token」
// 与「筛选提示不参与布局」这两条结构事实。

// #220：输入控件的边界必须用 --ctl-line，不能用承担卡片/表格线的 --line。
func TestControlBordersUseTheirOwnToken(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "--ctl-line:") {
		t.Fatal("--ctl-line 不见了：控件边界又回到装饰性 token 上（issue #220）")
	}
	// 两个 token 各在自己的主题块里定义（深色一份、浅色一份）。
	if strings.Count(src, "--ctl-line:") < 2 {
		t.Error("--ctl-line 只定义了一次：两套主题都要有自己的档位（issue #220）")
	}
	// 控件底色的声明不许再用 --line。
	if strings.Contains(src, "border-bottom-color: var(--line);") {
		// 允许表格分隔线（.acc tbody tr.acc-grp），那里是装饰性的。
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "border-bottom-color: var(--line);") &&
				!strings.Contains(line, "tr.acc-grp") {
				t.Errorf("第 %d 行的控件边界仍在用 --line：静止态会退回 1.1:1（issue #220）", i+1)
			}
		}
	}
	// 输入控件规则里必须有 --ctl-line。
	input := poolStatsUIBlock(t, src, "input, select, textarea {")
	if !strings.Contains(input, "var(--ctl-line)") {
		t.Error("input/select/textarea 的边界没有走 --ctl-line（issue #220）")
	}
}

// #221：统计带的「筛选」提示必须是绝对定位（不参与布局），否则可点格的
// 标签会被隐形占位推右，与不可点格分成两档。
func TestStatFilterBadgeDoesNotPushTheLabel(t *testing.T) {
	src := poolStatsUISource(t)

	i := strings.Index(src, ".stat.clickable::before {")
	if i < 0 {
		t.Fatal(".stat.clickable::before 不见了（issue #221）")
	}
	blockEnd := i + 320
	if blockEnd > len(src) {
		blockEnd = len(src)
	}
	block := src[i:blockEnd]
	if !strings.Contains(block, "position: absolute") {
		t.Error("筛选提示不是绝对定位：仍会撑开标签、把两组缩进分成两档（issue #221）")
	}
	if strings.Contains(block, `content: "筛选 "`) {
		t.Error(`筛选提示的 content 仍带尾随空格：拿字符串宽度当布局量具（issue #221）`)
	}
	// 旧的「.k::before 行内占位」形态不许回来。
	if strings.Contains(src, ".stat.clickable .k::before") {
		t.Error("行内占位形态回来了：可点格的标签会被推右（issue #221）")
	}
}

// poolStatsUIBlock 取源码里从 anchor 起到下一个空行为止的一段（够断言用）。
func poolStatsUIBlock(t *testing.T, src, anchor string) string {
	t.Helper()
	i := strings.Index(src, anchor)
	if i < 0 {
		t.Fatalf("找不到 %q", anchor)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n\n"); j > 0 {
		return rest[:j]
	}
	return rest
}
