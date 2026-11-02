package panel

import (
	"strings"
	"testing"
)

// combobox 的 aria-activedescendant 回归（issue #101）。
//
// 两处手搓 combobox（Ctrl+K 命令面板、模型页客户端筛选）都是「焦点保留在
// 输入框」范式：上下键在视觉上挪高亮，但没有 aria-activedescendant，读屏
// 全程只听空输入框。修复：option 带稳定 id，选中项变化后输入框的关系指过去。

func TestCmdkOptionsCarryIdsAndTheInputPointsAtThem(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "cmdkRender")

	// option 生成时带稳定 id（aria-activedescendant 的锚点）。
	if !strings.Contains(body, `id="cmdk-opt-' + i + '"`) {
		t.Fatal("cmdk option 没有稳定 id：aria-activedescendant 无处可指（issue #101）")
	}
	// 指向选中项；空列表摘掉。
	if !strings.Contains(body, `inp.setAttribute("aria-activedescendant", sel.id)`) ||
		!strings.Contains(body, `inp.removeAttribute("aria-activedescendant")`) {
		t.Fatal("cmdkRender 没有把 aria-activedescendant 指到选中项（issue #101）")
	}
	// 锚点必须在 option 的 id 上，而不是别的元素。
	idLine := strings.Index(body, `id="cmdk-opt-' + i + '"`)
	roleLine := strings.LastIndex(body[:idLine], "role=\"option\"")
	if roleLine < 0 || idLine-roleLine > 200 {
		t.Fatal("cmdk-opt-<i> 没有落在 role=option 的元素上（issue #101）")
	}
}

func TestMdComboOptionsCarryIdsAndTheInputPointsAtThem(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "mdComboPaint")

	if !strings.Contains(body, `id="mdclient-opt-' + i + '"`) {
		t.Fatal("mdcombo option 没有稳定 id：aria-activedescendant 无处可指（issue #101）")
	}
	if !strings.Contains(body, `inp.setAttribute("aria-activedescendant", act.id)`) ||
		!strings.Contains(body, `inp.removeAttribute("aria-activedescendant")`) {
		t.Fatal("mdComboPaint 没有把 aria-activedescendant 指到高亮项（issue #101）")
	}
}
