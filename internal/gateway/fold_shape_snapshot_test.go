package gateway

import (
	"strings"
	"testing"

	"client2api/internal/core"
)

// #240 重开：fold 请求被厂商拒时，消息级形状快照必须落日志（不含内容本体），
// 否则第二版盲修没有证据。
func TestFoldShapeSnapshotMarksShapesWithoutContent(t *testing.T) {
	msgs := []core.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "secret user text that must not appear"},
		{Role: "assistant", Content: "", ToolCalls: []core.ToolCall{{ID: "call-9"}}},
		{Role: "tool", ToolCallID: "call-9", Content: "secret tool output"},
	}
	got := foldShapeSnapshot(msgs)
	for _, want := range []string{
		"[0]system(len=8",
		"[1]user(len=37",
		"[2]assistant(len=0,calls=call-9",
		"[3]tool(len=18,for=call-9",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot 缺 %q：%s", want, got)
		}
	}
	// 内容本体不得进快照。
	if strings.Contains(got, "secret user text") || strings.Contains(got, "secret tool output") {
		t.Errorf("snapshot 泄漏了内容本体：%s", got)
	}
}
