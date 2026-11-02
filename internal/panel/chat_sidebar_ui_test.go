package panel

import (
	"strings"
	"testing"
)

// TestChatWorkspaceShortcutRevealsAdvancedSettings 原来钉住一个真实的死路：筛选框
// 在高级设置里，折叠状态下 "/" 直接 focus 一个 hidden 元素等于没反应，快捷键必须
// 先展开。issue #106 换掉了这条路径——"/" 现在解析「当前视图内第一个可见的搜索
// 框」：对话页可见的是会话搜索框 (#chatSearch)，侧栏收起时看不到它就退到主消息框
// (#chatInput)，两者都不会去聚焦 hidden 元素。这里改钉新口径 + 保住
// chatOpenAdvanced 本身（工具条按钮仍要它展开并返回筛选框）。
func TestChatWorkspaceShortcutRevealsAdvancedSettings(t *testing.T) {
	src := poolStatsUISource(t)
	// 可见性过滤是这条路径不再聚焦 hidden 元素的保证（issue #106）。
	if !strings.Contains(src, "r.width > 0 && r.height > 0") {
		t.Error(`"/" 快捷键没有按可见性过滤目标：仍可能聚焦 hidden 输入框`)
	}
	// 对话页的退路是主消息框，不是被强制展开的高级面板。
	if !strings.Contains(src, `VIEW === "chat" ? $("#chatInput") : null`) {
		t.Error(`对话页 "/" 的退路不是主消息框`)
	}
	if strings.Contains(src, "chatOpenAdvanced(true)") {
		t.Error(`"/" 快捷键仍会强制展开高级设置：issue #106 已改口径`)
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
