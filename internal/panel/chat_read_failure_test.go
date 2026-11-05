package panel

import (
	"strings"
	"testing"
)

// 钉住对话页读失败的两条契约（issue #202 / #203）：
//
//	#202：正文读失败不得把 CHAT.msgs 赋成空数组（与真·空会话同形，下一句
//	发送的整段 PATCH 会覆盖历史）；写路径必须有「读到过」的门。
//	#203：chatOpenThread 的指针提交必须在读取成功之后——失败分支不得改动
//	CHAT.current，否则界面停在原会话而下一条消息写进没在看的会话。
func TestChatReadFailureKeepsTheTranscriptAndThePointer(t *testing.T) {
	src := poolStatsUISource(t)

	// #202：chatLoadThreads 的失败分支内不得出现「msgs 赋空」的赋值
	// （成功路径的三元赋值在 full 必非 null 之后，合法）。
	load := poolStatsFuncBody(t, src, "chatLoadThreads")
	failAt := strings.Index(load, "if (cur && !full) {")
	retAt := strings.Index(load[failAt:], "return true;")
	failBranch := load[failAt : failAt+retAt]
	if strings.Contains(failBranch, "CHAT.msgs =") {
		t.Fatal("chatLoadThreads 的失败分支仍在给 CHAT.msgs 赋值：读失败会落成空数组，与真·空会话同形（issue #202）")
	}
	if !strings.Contains(load, "CHAT.loadedFor = null") || !strings.Contains(load, "chatDrawError") {
		t.Error("chatLoadThreads 的失败分支没有画错误态/清 loadedFor：失败必须与空不同形（issue #202）")
	}

	// #203：指针提交在成功之后——失败分支只有 toast，不改 CHAT.current。
	openBody := poolStatsFuncBody(t, src, "chatOpenThread")
	openFailAt := strings.Index(openBody, `if (!full) {`)
	if openFailAt < 0 {
		t.Fatal("chatOpenThread 没有 !full 失败分支")
	}
	commitAt := strings.Index(openBody, "CHAT.current = id;")
	if commitAt < 0 {
		t.Fatal("chatOpenThread 没有指针提交")
	}
	if commitAt < openFailAt {
		t.Fatal("chatOpenThread 的指针提交在读取之前：读失败后指针已落地，下一条消息会写进没在看的会话（issue #203）")
	}
	if !strings.Contains(openBody[openFailAt:commitAt], "return;") {
		t.Error("失败分支没有 return：会继续走到指针提交")
	}

	// 写路径的门：chatSend 必须校验 loadedFor === threadID，不一致先重读、再失败拒绝发送。
	send := poolStatsFuncBody(t, src, "chatSend")
	if !strings.Contains(send, "CHAT.loadedFor !== threadID") {
		t.Fatal("chatSend 没有「读到过」的门：冷读失败后发一句会整段覆盖历史（issue #202）")
	}
	if !strings.Contains(send, "已停止发送") {
		t.Error("重读失败时必须拒绝发送并提示，而不是带着空 msgs 继续走")
	}
}
