package panel

import (
	"strings"
	"testing"
)

// 钉住星图态的键盘/读屏与路由契约（issue #196 / #197）。

// #196：canvas 是 aria-hidden 的纹理，13 个领域必须有 DOM 等价物
// （role=button、data-view、名字），清单与命令面板同源；抽屉未打开时 panel
// 摘出焦点树（inert + atlasBack tabIndex=-1），打开时解除。
func TestAtlasHasAKeyboardReachableZoneList(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `id="atlasZonesNav"`) {
		t.Fatal(".atlas 里没有领域列表容器：读屏在星图态 0/13（issue #196）")
	}
	if !strings.Contains(src, `aria-label="全部领域"`) {
		t.Error("领域列表没有 aria-label：读屏不知道这块是什么")
	}
	// 清单从 TITLES 生成（与命令面板同源，13 个视图自动跟上）。
	if !strings.Contains(src, "Object.keys(TITLES).map") {
		t.Error("领域清单没有从 TITLES 生成：与命令面板不同源，视图增减会不同步")
	}
	// 点击走与鼠标同一个 atlasOpen。
	if !strings.Contains(src, `atlasOpen(b.dataset.view)`) {
		t.Error("领域按钮没有接到 atlasOpen：程序化等价物缺失（issue #196）")
	}
	// 抽屉未打开时 panel 摘出焦点树。
	if !strings.Contains(src, `'<aside class="atlas-panel" id="atlasPanel" inert>'`) {
		t.Error("atlas-panel 初始没有 inert：视口外的 atlasBack 是隐形 Tab 停靠点（issue #196）")
	}
	if !strings.Contains(src, `id="atlasBack" title="返回星图（Esc）" tabindex="-1"`) {
		t.Error("atlasBack 初始 tabindex 不是 -1（issue #196）")
	}
	if !strings.Contains(src, `ATLAS.panel.removeAttribute("inert")`) {
		t.Error("atlasOpen 没有解除 inert：抽屉打开后读屏仍看不到正文（issue #196）")
	}
	// skip-link 在星图态指向领域列表，退出还原。
	if !strings.Contains(src, `skip.setAttribute("href", "#atlasZonesNav")`) {
		t.Error("skip-link 在星图态仍指向 #main：那是一块 visibility:hidden 的区域（issue #196）")
	}
}

// #197：「当前视图」只有一个真相——atlasOpen 写 hash；星图态下 go() 落到
// atlasOpen（与鼠标点区域同一函数），命令面板的视图命令不再是静默空操作。
func TestAtlasModeRoutesThroughOneTruth(t *testing.T) {
	src := poolStatsUISource(t)

	open := poolStatsFuncBody(t, src, "atlasOpen")
	if !strings.Contains(open, "history.replaceState") {
		t.Fatal("atlasOpen 不写 hash：ATLAS.open 与 location.hash 必然分叉（issue #197）")
	}
	if !strings.Contains(open, `ATLAS.panel.removeAttribute("inert")`) {
		t.Error("atlasOpen 没有解除 panel inert（issue #196/#197 连带）")
	}

	goBody := poolStatsFuncBody(t, src, "go")
	if !strings.Contains(goBody, `document.body.classList.contains("atlas-on")`) ||
		!strings.Contains(goBody, "atlasOpen(v)") {
		t.Fatal("go() 在星图态没有落到 atlasOpen：命令面板的视图命令仍是静默空操作（issue #197）")
	}
	// 同一视图的重复 go 是无操作（避免抖动）。
	if !strings.Contains(goBody, "ATLAS.open === v") {
		t.Error("go() 没有对「已在当前抽屉视图」短路：重复导航会重演抽屉动画")
	}
}
