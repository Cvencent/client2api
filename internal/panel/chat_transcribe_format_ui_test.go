package panel

import (
	"strings"
	"testing"
)

// TestChatTranscriptionFormatLivesWithTheRecorder 是一个真实回归：response_format
// 原来在高级设置里，而选中转写模型时整个对话工作区（连同高级设置）都被隐藏，
// 录音面板就拿不到这个必填项。它现在和录音按钮在同一张卡片里。
func TestChatTranscriptionFormatLivesWithTheRecorder(t *testing.T) {
	src := poolStatsUISource(t)

	trans := src[strings.Index(src, `id="chatTranscribe"`):]
	trans = trans[:strings.Index(trans, `</section>`)]
	for _, want := range []string{`id="btnChatRecord"`, `id="chatResponseFormatRow"`, `id="chatResponseFormat"`} {
		if !strings.Contains(trans, want) {
			t.Errorf("转写卡片缺少 %q", want)
		}
	}

	workspace := src[strings.Index(src, `id="chatWorkspace"`):strings.Index(src, `id="chatTranscribe"`)]
	if strings.Contains(workspace, `id="chatResponseFormat"`) {
		t.Error("response_format 仍在会被隐藏的对话工作区里，转写模式拿不到")
	}
}
