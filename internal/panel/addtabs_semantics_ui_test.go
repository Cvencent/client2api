package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 添加账号弹层的三档切换：与 setupSubtabs 同一套 ARIA 语义（issue #150）。
//
// 三档（浏览器登录 / 导入凭据 / 手动添加）走的是 setAddTab 这条老路径，只切
// class；而同文件里四组结构一样的子页签（用量/任务中心/模型/平台）经由
// setupSubtabs 拿到了 role=tablist/tab、aria-selected、roving tabindex 和
// 方向键。读屏用户听到的是三颗一模一样的按钮，键盘用户要白走三次 Tab 且
// 方向键无效。行为判据（AX 树属性 + 真实键事件）在 cmd/panelsmoke 的
// assertAddTabsSemantics——它实测回退到裸按钮即红、逐条复现报告；这里钉
// 的是会静默漂移的标记与同步逻辑。
// ---------------------------------------------------------------------------

// TestAddTabsCarryTabSemantics pins the markup: the list announces itself as
// a tablist, every tab exposes role + aria-controls, and each panel is a
// labelled tabpanel.
func TestAddTabsCarryTabSemantics(t *testing.T) {
	src := poolStatsUISource(t)

	list := `<div class="tabs" id="addTabs" role="tablist" aria-label=`
	if !strings.Contains(src, list) {
		t.Fatal("#addTabs 没有 role=tablist 与可读名称：读屏把三档读成三颗普通按钮（#150）")
	}
	for _, id := range []string{"addTabLoginBtn", "addTabImportBtn", "addTabManualBtn"} {
		if !strings.Contains(src, `id="`+id+`" role="tab"`) {
			t.Errorf("档位按钮 %s 不是 role=tab（#150）", id)
		}
		if !strings.Contains(src, `aria-controls=`) {
			t.Error("档位没有 aria-controls：读屏不知道它控制哪块面板（#150）")
			break
		}
	}
	// 三个面板都要是 tabpanel，且由对应档位的按钮命名。
	for _, pair := range [][2]string{
		{"addTabLoginBtn", `id="addTabLogin" class="tab-panel" role="tabpanel" aria-labelledby="addTabLoginBtn"`},
		{"addTabImportBtn", `id="addTabImport" class="tab-panel" role="tabpanel" aria-labelledby="addTabImportBtn"`},
		{"addTabManualBtn", `id="addTabManual" class="tab-panel" role="tabpanel" aria-labelledby="addTabManualBtn"`},
	} {
		if !strings.Contains(src, pair[1]) {
			t.Errorf("面板 %s 的 tabpanel/aria-labelledby 语义缺失或对不上（#150）", pair[0])
		}
	}
}

// TestSetAddTabSyncsAriaState pins the sync: setAddTab drives class AND
// aria-selected AND roving tabindex, skipping hidden tabs (minimaxcode's
// login tab is hidden for modules without that capability and must not hold
// a Tab stop).
func TestSetAddTabSyncsAriaState(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "setAddTab")

	if !strings.Contains(body, `setAttribute("aria-selected"`) {
		t.Fatal("setAddTab 不再同步 aria-selected：读屏听不出当前是哪一档（#150）")
	}
	if !strings.Contains(body, "tabIndex") {
		t.Fatal("setAddTab 不同步 roving tabindex：三颗按钮占三个 Tab 停靠点（#150）")
	}
	// 隐藏档不占停靠点：先判 hidden 再置 0，顺序就是契约。
	onIdx := strings.Index(body, `x.dataset.tab === t`)
	hiddenIdx := strings.Index(body, "x.hidden")
	if onIdx < 0 || hiddenIdx < 0 {
		t.Fatal("setAddTab 的同步逻辑形状变了：请重新核对 aria-selected/tabIndex/hidden 三者的关系（#150）")
	}
	if !strings.Contains(body, "!x.hidden") {
		t.Error("隐藏的档没有从 Tab 序里排除：minimaxcode 这类模块的空页签会被键盘走到（#150）")
	}
}

// TestAddTabsKeyboardNavigation pins the key handling: the same gestures the
// four setupSubtabs groups answer (arrows wrap, Home/End), skipping hidden
// tabs, moving focus with the selection.
func TestAddTabsKeyboardNavigation(t *testing.T) {
	src := poolStatsUISource(t)

	// 处理器必须挂在 #addTabs 上且覆盖四种键。
	idx := strings.Index(src, `$("#addTabs").addEventListener("keydown"`)
	if idx < 0 {
		t.Fatal("#addTabs 没有键盘处理器：方向键在弹层里完全无效（#150）")
	}
	seg := src[idx:]
	if end := strings.Index(seg, "\n});"); end >= 0 {
		seg = seg[:end]
	}
	for _, key := range []string{`"ArrowLeft"`, `"ArrowRight"`, `"Home"`, `"End"`} {
		if !strings.Contains(seg, key) {
			t.Errorf("键盘处理器没有处理 %s（#150；四组 setupSubtabs 都处理了）", key)
		}
	}
	// 隐藏档必须被跳过——否则空页签（minimaxcode 的「浏览器登录」）会被走到。
	if !strings.Contains(seg, "!x.hidden") {
		t.Error("键盘导航没有跳过隐藏的档：能力不足的空页签会被键盘走到（#150）")
	}
	// 选中与焦点必须一起动（roving tabindex 的标准行为）。
	if !strings.Contains(seg, "next.focus()") {
		t.Error("方向键移动选中档后没有把焦点跟过去：读屏焦点停在旧档上（#150）")
	}
	// preventDefault：不拦住的话箭头键还会滚动页面。
	if !strings.Contains(seg, "preventDefault()") {
		t.Error("方向键没有 preventDefault：页面会跟着滚动（#150）")
	}
}
