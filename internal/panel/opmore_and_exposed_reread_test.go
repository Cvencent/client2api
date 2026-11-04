package panel

import (
	"strings"
	"testing"
)

// 钉住两处面板交互修复（issue #181 / #182）。

// #181：对外模型子页的「重新读取」不得静默丢掉没保存的勾选——同页「模型
// 能力」的「重新获取」会贴回编辑（mdReapplyEdits），两个按钮挨在同一个工具
// 栏里，行为必须一致。
func TestExposedModelsRereadKeepsUnsavedSelection(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderExposedModels")
	for _, want := range []string{
		"const prevMode = EX_MODE",
		"prevSelected = new Set(EX_SELECTED)",
		"const hadEdits = EX_LOADED && exDirty()",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderExposedModels 缺少 %q：强制重读会静默丢掉未保存的勾选（issue #181）", want)
		}
	}
	// 贴回逻辑：相对 baseline 的增减两份差集都要算，且加回的键必须在新的
	// 服务端集合里仍然存在（known）。
	if !strings.Contains(render, "!prevBaseline.selected.has(k)") ||
		!strings.Contains(render, "if (known.has(k)) EX_SELECTED.add(k)") {
		t.Error("renderExposedModels 的贴回逻辑不完整：要按「相对旧 baseline 的增减」贴，且以下架的键为准（issue #181）")
	}
}

// #182：.opmore 下拉的三条退场路径（选中收起 / 点外部收起 / Esc 收起）必须
// 是统一委托，不能只有 #tcMore 一处写死。
func TestOpmoreMenusHaveAllThreeExits(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `.opmore .opmenu button`) {
		t.Fatal("没有针对 .opmore 菜单项的统一点击委托：选完不收起（issue #182）")
	}
	if !strings.Contains(src, `.opmore[open]`) {
		t.Fatal("没有批量收起 open 着的 .opmore：点外部不收起（issue #182）")
	}
	// 旧的特例必须已收编（同名 handler 不该再有第二份口径）。
	if strings.Contains(src, `$("#tcMore").addEventListener("click"`) {
		t.Error("#tcMore 的特例还挂着：统一委托已经覆盖它，两处口径会打架（issue #182）")
	}
	// Esc 退场：主 keydown 分支里，closeTopVeil 之后还有一批收 .opmore 的
	// 语句（弹层优先，菜单是第三档）。
	kdn := strings.LastIndex(src, `window.addEventListener("keydown"`)
	if kdn < 0 {
		t.Fatal("找不到全局 keydown 监听")
	}
	escAt := strings.Index(src[kdn:], `if (ev.key === "Escape")`)
	if escAt < 0 {
		t.Fatal("全局 keydown 里没有 Escape 分支")
	}
	escBlock := src[kdn+escAt:]
	if end := strings.Index(escBlock, "});"); end > 0 {
		escBlock = escBlock[:end]
	}
	if !strings.Contains(escBlock, `.opmore[open]`) {
		t.Error("Escape 分支没有收 .opmore：只用键盘的用户关不掉这类菜单（issue #182）")
	}
}
