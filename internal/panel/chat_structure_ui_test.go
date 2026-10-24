package panel

import (
	"strings"
	"testing"
)

// TestChatWorkspaceStructureIsCoherent 是这组改造的结构总检：记录栏、头部、高级
// 设置、消息区、输入区的元素必须同时存在且闭合，DOM 顺序也要对（侧栏在会话之前，
// 输入区在消息区之后）。前面的用例各自守一个行为，这一条守它们没有互相顶掉。
func TestChatWorkspaceStructureIsCoherent(t *testing.T) {
	src := poolStatsUISource(t)

	order := []string{
		`id="chatWorkspace"`,
		`id="chatSide"`,
		`id="btnChatNew"`,
		`id="chatSearch"`,
		`id="chatThreads"`,
		`id="btnChatClearAll"`,
		`id="chatTurn"`,
		`id="chatTitle"`,
		`id="chatClient"`,
		`id="chatModel"`,
		`id="btnChatAdvanced"`,
		`id="chatAdvanced"`,
		`id="chatScroll"`,
		`id="chatLog"`,
		`id="chatComposer"`,
		`id="chatInput"`,
		`id="btnChatSend"`,
		`id="btnChatStop"`,
	}
	last := -1
	for _, id := range order {
		at := strings.Index(src, id)
		if at < 0 {
			t.Fatalf("缺少 %s", id)
		}
		if at < last {
			t.Fatalf("%s 的 DOM 顺序不对", id)
		}
		last = at
	}

	// 头部必须能看到历史和高级设置两个入口；缺一个都会让常用动作变远。
	head := src[strings.Index(src, `id="chatTurn"`):strings.Index(src, `id="chatAdvanced"`)]
	for _, want := range []string{`id="chatSideToggle"`, `id="btnChatAdvanced"`, `id="btnChatClear"`} {
		if !strings.Contains(head, want) {
			t.Errorf("会话头部缺少 %q", want)
		}
	}

	// 历史记录必须由服务端提供：整份 shell 里不该再有把对话写进 localStorage 的路径。
	if strings.Contains(src, `localStorage.setItem("c2a.chat`) {
		t.Error("对话记录仍被写进 localStorage")
	}
}
