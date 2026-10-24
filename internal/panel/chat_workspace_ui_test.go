package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 对话测试页重做后的「聊天工作区」静态回归。
//
// 这一页的 HTML/JS 全是 index.html 里的手写字符串，Go 编译器看不见它们。
// 这组断言钉住重做的四条主线：
//   1. 左栏标题、新建按钮、搜索框、历史列表都在；点历史走服务端 API，
//      不再只把消息留在内存里；
//   2. 每条历史能重命名和删除，清空全部走 DELETE 集合接口；
//   3. 主区仍是完整的多轮上下文，发送时把该会话的消息整段回传；
//   4. 刷新/切页以后从服务端重新读记录，而不是靠 localStorage。
// ---------------------------------------------------------------------------

func TestChatWorkspaceHasHistorySidebar(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="chatWorkspace"`,
		`id="chatSide"`,
		`id="btnChatNew"`,
		`id="chatSearch"`,
		`id="chatThreads"`,
		`id="chatThreadEmpty"`,
		`id="btnChatClearAll"`,
		`id="chatSideToggle"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("聊天工作区缺少 %q", want)
		}
	}
}

func TestChatWorkspaceReadsServerHistory(t *testing.T) {
	src := poolStatsUISource(t)
	load := poolStatsFuncBody(t, src, "chatLoadThreads")
	for _, want := range []string{`"/panel/api/chat/conversations"`, "conversations"} {
		if !strings.Contains(load, want) {
			t.Errorf("chatLoadThreads 没有读取服务端历史：缺 %q", want)
		}
	}
	if strings.Contains(load, "localStorage") {
		t.Error("对话记录不该回退到 localStorage")
	}

	create := poolStatsFuncBody(t, src, "chatNewThread")
	for _, want := range []string{`method: "POST"`, `"/panel/api/chat/conversations"`} {
		if !strings.Contains(create, want) {
			t.Errorf("chatNewThread 没有调用服务端新建接口：缺 %q", want)
		}
	}
}

func TestChatWorkspaceCanDeleteAndRenameThreads(t *testing.T) {
	src := poolStatsUISource(t)

	del := poolStatsFuncBody(t, src, "chatDeleteThread")
	for _, want := range []string{`method: "DELETE"`, "/panel/api/chat/conversations/"} {
		if !strings.Contains(del, want) {
			t.Errorf("chatDeleteThread 缺少 %q", want)
		}
	}

	ren := poolStatsFuncBody(t, src, "chatRenameThread")
	for _, want := range []string{`method: "PATCH"`, "title"} {
		if !strings.Contains(ren, want) {
			t.Errorf("chatRenameThread 缺少 %q", want)
		}
	}

	clear := poolStatsFuncBody(t, src, "chatClearAllThreads")
	for _, want := range []string{`method: "DELETE"`, `"/panel/api/chat/conversations"`} {
		if !strings.Contains(clear, want) {
			t.Errorf("chatClearAllThreads 缺少 %q", want)
		}
	}
}

func TestChatWorkspacePersistsTurnsToTheThread(t *testing.T) {
	src := poolStatsUISource(t)

	save := poolStatsFuncBody(t, src, "chatSaveThread")
	for _, want := range []string{`method: "PATCH"`, "messages"} {
		if !strings.Contains(save, want) {
			t.Errorf("chatSaveThread 缺少 %q", want)
		}
	}

	send := poolStatsFuncBody(t, src, "chatSend")
	for _, want := range []string{"chatSaveThread(", "chatLoadThreads("} {
		if !strings.Contains(send, want) {
			t.Errorf("chatSend 没有把新消息写回记录：缺 %q", want)
		}
	}
	if strings.Contains(send, "CHAT.msgs = []") {
		t.Error("发送后不该丢弃会话消息")
	}
}

func TestChatWorkspaceContinuesTheSelectedThread(t *testing.T) {
	src := poolStatsUISource(t)

	open := poolStatsFuncBody(t, src, "chatOpenThread")
	for _, want := range []string{"CHAT.current", "chatDraw()"} {
		if !strings.Contains(open, want) {
			t.Errorf("chatOpenThread 缺少 %q", want)
		}
	}

	draw := poolStatsFuncBody(t, src, "chatDrawThreads")
	for _, want := range []string{"data-thread", "chatOpenThread", "chatDeleteThread", "chatRenameThread"} {
		if !strings.Contains(draw, want) {
			t.Errorf("chatDrawThreads 缺少 %q", want)
		}
	}
}

func TestChatComposerUsesAdvancedSettings(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="chatAdvanced"`,
		`id="chatComposer"`,
		`id="btnChatAdvanced"`,
		`aria-expanded`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("聊天工作区缺少 %q", want)
		}
	}
}
