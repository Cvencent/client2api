package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 控件高度的一套刻度（issue #160）。
//
// 「同一行控件一样高」原本只写在注释里：button.xs 一个类在同一个文件里被
// 定义了五次（26/28 互斥），.box > header .xs 又给它单独抬一档，工具条里
// 还有一颗单点写死的 30px——「一颗 xs 按钮多高」取决于它落在哪个容器
// 而不是设计。看守把这些分叉钉死：样式源码里高度声明必须只有一个来源。
// ---------------------------------------------------------------------------

// TestButtonXSHasOneDefinition 钉 button.xs 的权威定义唯一：再多一份写法
// （26/28 互斥的五份同源）就等于没有设计——高度随落点漂移。
func TestButtonXSHasOneDefinition(t *testing.T) {
	src := poolStatsUISource(t)

	// 声明点统计：button.xs { 的权威定义 + 其他选择器尾缀 .xs { 的改写点
	// （子串计数不含权威定义本身，避免双计）。
	n := strings.Count(src, "button.xs {") +
		strings.Count(src, "\n.xs {") +
		strings.Count(src, ".xs,") - 1
	// .xs 泛选择器（比如 .box > header .xs）也算改写权。
	for _, rewrite := range []string{".box > header .xs {", ".box > header button.xs { min-height", ".box > header .xs, "} {
		if strings.Contains(src, rewrite) {
			t.Errorf("发现对 button.xs 样式的改写点 %q：它曾把 button.xs 又抬到 28px，与权威值冲突（issue #160）", rewrite)
		}
	}
	if n > 2 {
		t.Errorf("button.xs/.xs 的规则声明有 %d 处（>2）：五份互相覆盖的分叉回来了（issue #160）", n)
	}
	// 权威值必须是 26px 且带注释指向 issue #160 的收编说明。
	if !strings.Contains(src, "button.xs { min-height: 26px; padding: 2px 9px;") {
		t.Error("button.xs 的权威定义（min-height: 26px）不在：高度来源丢失（issue #160）")
	}
	if !strings.Contains(src, "issue #160）：同文件曾有五份互相覆盖的") {
		t.Error("权威定义的收编注释不在：下一次重写轮会重新分叉而不自知（issue #160）")
	}
}

// TestNoSinglePointControlHeights 钉住「单点写死的高度」这一族：工具条里的
// 单点 30px 曾与 --ctl-h（34px 并存，同一行两个相邻搜索框高度来源不同。
func TestNoSinglePointControlHeights(t *testing.T) {
	src := poolStatsUISource(t)

	// 修正后工具条的搜索框引用 --ctl-h；工具条一族的 30px 单点不再存在。
	if !strings.Contains(src, `.tblbar input[type="search"] { min-height: var(--ctl-h); padding: 6px 13px; }`) {
		t.Error(`.tblbar input[type="search"] 不再引用 --ctl-h：单点 30px 回来了（issue #160）`)
	}
	// 全文件里 min-height: 30px 只允许存量五处（.subtabs .subtab 的页签条、
	// .pk-expiry-row 的网格行与 mark 色条等）——它们不在「工具条控件」的
	// 口径里；存量若是往下扩散，才说明单点高度在回来。
	if n := strings.Count(src, "min-height: 30px"); n > 5 {
		t.Errorf("min-height: 30px 有 %d 处（≤5 为本轮存量）：单点高度在扩散（issue #160）", n)
	}
	// 同一行混着 34px 下拉与 26px xs 按钮的容器必须 center，矮的不再被拉伸。
	if !strings.Contains(src, ".acct-card .ac-foot { align-items: center; }") {
		t.Error(".ac-foot 不是 center：26px 的 xs 按钮会被同行的 34px 下拉拉高（issue #160）")
	}
	// 令牌本身必须存在（旧文件里 --ctl-h-sm 是零使用点的死令牌，本轮删除
	// 与否不改变高度唯一性，但 --ctl-h 必须真的在用）。
	if !strings.Contains(src, "--ctl-h: 34px;") {
		t.Error("--ctl-h 令牌不在：工具条控件高度失去了唯一来源（issue #160）")
	}
}
