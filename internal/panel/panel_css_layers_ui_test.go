package panel

import (
	"strings"
	"testing"
)

// 面板 CSS「层」缺陷一类（issue #109 + #111）：两单都不是某个元素写错了，
// 而是同一属性被好几层规则说话、后写的那层把先写的吃掉，读源码时看不出来。
//
//   - #109：位置选择器（.box > header …）靠 :not() 抬权重，越过了语义规则
//     （.chips .chip.on），选中档的强调底被换成卡片底色；
//   - #111：媒体查询给 grid 写重排，而容器早已是 flex——规则一直在，就是空转。
//
// 两单的共同取证法是「把该属性在本文件里的所有说话者排出来看谁最后生效」，
// 所以下面的断言都不看单个规则，只看「同一属性还有没有更晚的、更高权重的
// 说话者」。

// TestHeaderSecondaryRuleSparesTheSelectedChip 钉住 #109：卡片头那条次级化
// 规则必须同时豁免 .primary（#86）与 .on（#109）。:not() 每加一层权重加一，
// 少一个豁免就退回「选中档白底白字」。
func TestHeaderSecondaryRuleSparesTheSelectedChip(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		".box > header button.xs:not(.primary):not(.on) { background: var(--surface); border-color: var(--line); }",
		".box > header button.xs:not(.primary):not(.on):hover:not(:disabled)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("卡片头次级化规则缺少 %q：选中档的 accent 底会被换成卡片底色（issue #109）", want)
		}
	}
	// 只豁免 .primary 的中间写法不许回来——那正是 #109 引入时的形状。
	if strings.Contains(src, ".box > header button.xs:not(.primary) { background:") {
		t.Error("卡片头次级化规则只豁免了 .primary：会压过 .chips .chip.on 的 (0,3,0)（issue #109 回归）")
	}
}

// TestChipHoverRulesSpareTheSelectedChip 钉住 #109 的第二个来源：三条 hover
// 规则都与 .chips .chip.on 同权重且更后，选中档的文字色会被它们改成 --ink /
// --accent-text，鼠标进出时在「看得见 / 看不见」之间翻。
func TestChipHoverRulesSpareTheSelectedChip(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		".chips .chip:hover:not(.on) {",
		".chip:hover:not(.on), .chips .chip:hover:not(.on) {",
		".chips .chip:hover:not(.on), button.chip:hover:not(.on) {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("hover 规则 %q 没有豁免选中档：选中档 hover 时文字色会翻（issue #109）", want)
		}
	}
	// 无豁免的整族 hover 写法不许回来。
	for _, bad := range []string{
		"\n.chip:hover, .chips .chip:hover {",
		"\n.chips .chip:hover, button.chip:hover {",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("hover 规则退回无豁免写法 %q（issue #109 回归）", bad)
		}
	}
	// 选中档本身必须仍然由 .chips .chip.on 给出实心底与 accent-ink 文字。
	if !strings.Contains(src, "color: var(--accent-ink); font-weight: 620;") {
		t.Error("选中档的强调渐变底 + accent-ink 文字不见了；#109 的修复不是删样式而是让豁免生效")
	}
}

// TestStatsBandReflowsInsteadOfBeingClipped 钉住 #111：统计带在窄窗必须真的
// 换行重排。三条只写 grid-template-columns 的媒体查询是 10-28 改成 flex 之后
// 的残留（容器不是 grid，同属性更早出现），留着会让「重排」看起来已经实现。
func TestStatsBandReflowsInsteadOfBeingClipped(t *testing.T) {
	src := poolStatsUISource(t)

	// 死代码不许回来：容器是 flex，只有 grid 版的写法等于没写。
	for _, dead := range []string{
		"@media (max-width: 1500px) { .stats { grid-template-columns: repeat(4, 1fr); } }",
		"@media (max-width: 980px) { .stats { grid-template-columns: repeat(3, 1fr); } }",
		"@media (max-width: 560px) { .stats { grid-template-columns: repeat(2, 1fr); } }",
	} {
		if strings.Contains(src, dead) {
			t.Errorf("对 flex 容器无效的死重排规则 %q 回来了（issue #111）", dead)
		}
	}
	// 真实的重排必须存在，且用的是 flex 认识的口径。
	if !strings.Contains(src, "@media (max-width: 1160px) {") {
		t.Fatal("窄窗没有统计带重排断点：10 格 × 112px 在 1024 一类的窗口下放不下（issue #111）")
	}
	if !strings.Contains(src, ".stats { flex-wrap: wrap; row-gap: 6px; }") {
		t.Error("重排断点没有让 .stats 换行；格子仍会被 .viewwrap 的 overflow-x 裁掉（issue #111）")
	}
	if !strings.Contains(src, ".stats > .stat, .stats > button.stat { flex: 0 1 25%; }") {
		t.Error("重排断点没有用 flex-basis 退列：flex-basis 才是 flex 容器认识的口径（issue #111）")
	}
	// 窄窗下选中底边必须收回格内：它写的是 bottom: -5px，换行后会画穿下一行。
	if !strings.Contains(src, ".stats > button.stat.on::after { bottom: 0; }") {
		t.Error("窄窗没有把选中底边收回格内：换行后那条 2px 会压到下一行顶部（issue #111）")
	}
	// 重排必须写在权威定义之后。容器最终生效的是 flex-wrap，而 :first-child
	// 的 flex 简写也在权威块里——同权重时后置生效，写在前面会被它覆盖。
	auth := strings.Index(src, ".stats > .stat:first-child { flex: 1.4 1 0;")
	narrow := strings.Index(src, "@media (max-width: 1160px) {")
	if auth < 0 || narrow < 0 || narrow < auth {
		t.Error("窄窗重排写在 .stats 权威定义之前：会被那里的 :first-child { flex: 1.4 1 0 } 覆盖（issue #111）")
	}
	// 换行之后不再需要横向滚动，旧的可滚写法（只覆盖 ≥900px 的一小段）不许回来。
	if strings.Contains(src, ".stats { overflow-x: auto; scrollbar-width: none; }") {
		t.Error("统计带退回了横向可滚写法：900~1160px 之间的窗口仍会「放不下也滚不动」（issue #111）")
	}
}
