package panel

import (
	"strings"
	"testing"
)

// 钉住 #218（横向滚动容器）与 #219（空值口径）的修复。
//
// 2026-11-06 的事故教训：这些断言只做「文本存在 + 位置/顺序」，真正的
// 「页面能不能开」由真浏览器逐视图验证把关（见 AGENTS.md 的面板 JS 纪律）。

// #218：三个横向滚动容器要有「能滚」的视觉线索与滚动手段。
func TestHScrollContainersHaveAffordances(t *testing.T) {
	src := poolStatsUISource(t)

	// 渐隐工具类（复用 .tbl-wrap.scrollable-x 的同一条渐隐）。
	if !strings.Contains(src, ".hscroll.more-right::after") {
		t.Fatal(".hscroll 渐隐工具类不见了（issue #218）")
	}
	// 三个容器挂类；ustiles 额外要 tabindex（容器内零可聚焦元素）。
	for _, want := range []string{
		`class="cmdviews hscroll"`,
		`class="ustiles hscroll" id="usTiles" tabindex="0"`,
		`class="chips hscroll" id="clientChips"`,
		`class="chips hscroll" id="crChips"`,
		`class="chips hscroll" id="scChips"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("容器缺 %q（issue #218）", want)
		}
	}
	// 三个 JS 手段都必须存在，且定义在使用之前（const/function 混用会 TDZ）。
	for _, fn := range []string{"function markHScroll", "function scrollCmdViewIntoView"} {
		if strings.Count(src, fn) != 1 {
			t.Errorf("%q 定义缺失或重复（issue #218）", fn)
		}
	}
	if !strings.Contains(src, `ev.target.closest(".hscroll")`) {
		t.Error("竖轮转横向的处理不见了：普通鼠标在容器上推不动（issue #218）")
	}
	// go() 尾部要把当前项滚进视野。
	goBody := poolStatsFuncBody(t, src, "go")
	if !strings.Contains(goBody, "scrollCmdViewIntoView()") {
		t.Error("go() 没有调 scrollCmdViewIntoView：当前项仍会被切出视野（issue #218）")
	}
	// 换行断点从 900 提到 1130（消掉 901–1129 凹陷）。
	if !strings.Contains(src, "@media (max-width: 1130px)") {
		t.Error("换行断点没有提到 1130：901–1129 段当前项仍会被切出视野（issue #218）")
	}
	// 事故回归钉：markHScroll 的新增块必须在 #165 守卫窗口之外。
	// 守卫把 markScrollableTables 的 body 截止在下一个 window.addEventListener。
	i := strings.Index(src, "function markScrollableTables()")
	j := strings.Index(src[i:], "\nwindow.addEventListener")
	if i < 0 || j < 0 {
		t.Fatal("找不到 #165 守卫的边界（markScrollableTables / window.addEventListener）")
	}
	guardWindow := src[i : i+j]
	if strings.Contains(guardWindow, "markHScroll") {
		t.Fatal("markHScroll 落在 #165 守卫的函数体窗口内：会被「读写交替」守卫误判，且历史上正是这里搬块搬丢了定义（issue #218 事故）")
	}
}

// #219：空值口径统一为 NO_VALUE 常量。
func TestNoValueIsASingleConstant(t *testing.T) {
	src := poolStatsUISource(t)

	declAt := strings.Index(src, `const NO_VALUE = "—"`)
	if declAt < 0 {
		t.Fatal(`NO_VALUE 常量不见了（issue #219）`)
	}
	// const 不提升：声明必须早于第一次使用。
	firstUse := strings.Index(src, "NO_VALUE +")
	otherUse := strings.Index(src, ": NO_VALUE")
	useAt := firstUse
	if useAt < 0 || (otherUse >= 0 && otherUse < useAt) {
		useAt = otherUse
	}
	if useAt < 0 {
		t.Fatal("NO_VALUE 没有被任何地方使用（issue #219）")
	}
	if useAt < declAt {
		t.Fatal("NO_VALUE 在声明之前被使用：const 不提升，运行时 TDZ（issue #219）")
	}

	// 四个账号格子与两个格式化函数都走常量，不留内联字面量。
	for _, fn := range []string{"accRateCell", "accUsageCell", "accLastCell", "accExpiryCell", "fmtRate", "fmtMs"} {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "NO_VALUE") {
			t.Errorf("%s 没有走 NO_VALUE：空值口径仍是分散的（issue #219）", fn)
		}
		if strings.Contains(body, `">-"`) || strings.Contains(body, `">—"`) || strings.Contains(body, `return "-"`) {
			t.Errorf("%s 仍有内联空值字面量（issue #219）", fn)
		}
	}
}
