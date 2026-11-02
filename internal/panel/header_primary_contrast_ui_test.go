package panel

import (
	"strings"
	"testing"
)

// 卡片头主按钮的对比度回归（issue #86）。
//
// .box > header button.xs（次级化规则）的权重 (0,2,1) 高于 button.primary
// (0,1,1) 且更后，不豁免会把卡片头主按钮的 accent 底换成卡片底色——浅色主题
// 下白底白字（对比度 1:1，按钮文字不可见），深色下主操作与旁边的次级控件
// 同貌。修复：次级化规则与 hover 文字色都用 :not(.primary) 豁免主按钮。

func TestHeaderSecondaryRuleSparesPrimaryButtons(t *testing.T) {
	src := poolStatsUISource(t)

	sec := poolStatsFuncBody(t, src, "renderPlatforms")
	_ = sec // renderPlatforms 不是样式所在；真正的断言在全文上。

	// 次级化规则必须带 :not(.primary) 豁免。
	if !strings.Contains(src, ".box > header button.xs:not(.primary) { background: var(--surface); border-color: var(--line); }") {
		t.Fatal("卡片头次级化规则没有豁免 .primary：主按钮的 accent 底被换成卡片底色，浅色主题白底白字（issue #86）")
	}
	if !strings.Contains(src, ".box > header button.xs:not(.primary):hover:not(:disabled)") {
		t.Fatal("卡片头 hover 规则没有豁免 .primary：主按钮 hover 会变深字压蓝底（issue #86）")
	}
	// 旧的整族规则（无豁免）不许回来。
	if strings.Contains(src, ".box > header button.xs { background:") {
		t.Error("卡片头次级化规则退回了无豁免写法（issue #86 回归）")
	}
	// 两个受影响按钮的 markup 带 primary 类，豁免才有意义。
	for _, id := range []string{"btnSourceAdd", "btnScSave"} {
		i := strings.Index(src, `id="`+id+`"`)
		if i < 0 {
			t.Errorf("找不到 #%s 的 markup", id)
			continue
		}
		// 向前找同一行（到最近的 > 为止）里的 class 声明。
		lineStart := strings.LastIndex(src[:i], "<button")
		seg := src[lineStart:i]
		if !strings.Contains(seg, `class="xs primary"`) {
			t.Errorf("#%s 的 class 不含 primary，豁免对它无效（issue #86）", id)
		}
	}
}
