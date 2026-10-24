package panel

import (
	"strings"
	"testing"
)

// TestChatWorkspaceShortcutRevealsAdvancedSettings 钉住一个真实的死路：筛选框在
// 高级设置里，折叠状态下 "/" 直接 focus 一个 hidden 元素等于没反应。快捷键必须先
// 展开。
func TestChatWorkspaceShortcutRevealsAdvancedSettings(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "chatOpenAdvanced(true)") {
		t.Error(`"/" 快捷键没有先展开高级设置就去聚焦筛选框`)
	}
	open := poolStatsFuncBody(t, src, "chatOpenAdvanced")
	for _, want := range []string{`$("#chatAdvanced").hidden = false`, `return $("#chatModelFilt")`} {
		if !strings.Contains(open, want) {
			t.Errorf("chatOpenAdvanced 缺少 %q", want)
		}
	}
}

// TestChatWorkspaceCollapseHasAReachableToggle 折叠按钮原来在侧栏自己里面：一收起
// 就再也没有入口展开。它现在在头部，侧栏隐藏时仍然可见。
func TestChatWorkspaceCollapseHasAReachableToggle(t *testing.T) {
	src := poolStatsUISource(t)
	head := src[strings.Index(src, `id="chatTurn"`):]
	head = head[:strings.Index(head, `id="chatTranscribe"`)]
	if !strings.Contains(head, `id="chatSideToggle"`) {
		t.Error("折叠按钮不在会话头部，收起侧栏后就无法展开")
	}
	side := src[strings.Index(src, `id="chatSide"`):strings.Index(src, `id="chatTurn"`)]
	if strings.Contains(side, `id="chatSideToggle"`) {
		t.Error("折叠按钮仍在侧栏内部，会被 side-hidden 一起藏掉")
	}
}
