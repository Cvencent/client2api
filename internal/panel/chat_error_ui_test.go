package panel

import (
	"strings"
	"testing"
)

// TestChatWorkspaceKeepsLocalErrorBubble 是一个真实回归：保存成功后从服务端重读
// 会把本地失败气泡（role=error，从不写进记录）从屏幕上抹掉，看起来像错误自己
// 消失了。所以保存只允许在成功回合重读，失败回合必须留在本地。
func TestChatWorkspaceKeepsLocalErrorBubble(t *testing.T) {
	src := poolStatsUISource(t)

	save := poolStatsFuncBody(t, src, "chatSaveThread")
	if !strings.Contains(save, "saved.messages = CHAT.msgs.slice()") {
		t.Error("chatSaveThread 会用服务端副本覆盖当前会话，失败气泡会被抹掉")
	}

	send := poolStatsFuncBody(t, src, "chatSend")
	if !strings.Contains(send, "turnOK") {
		t.Error("chatSend 没有区分成功/失败回合，失败后仍会重读并丢掉错误气泡")
	}
	if strings.Contains(send, "const fail = (msg, status)") {
		t.Error("fail 仍然是同步的：settle() 还没跑完函数就返回了")
	}
}
