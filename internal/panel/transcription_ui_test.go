package panel

import (
	"strings"
	"testing"
)

// TestAccountPoolShowsTranscriptionCallInfo pins the account-pool view for a
// module that has no accounts but does serve POST /v1/audio/transcriptions.
// The platform is discovered from the capability payload; the endpoint and
// model names are rendered generically so adding a speech module does not need
// another JavaScript branch.
func TestAccountPoolShowsTranscriptionCallInfo(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="accTranscription"`,
		`id="accTranscriptionURL"`,
		`id="accTranscriptionModels"`,
		`id="btnCopyTranscriptionURL"`,
		`id="btnCopyTranscriptionModel"`,
		"caps.transcription",
		"caps.transcription_models",
		"renderTranscriptionCallInfo",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("账号池缺少转写调用信息 %q", want)
		}
	}
}

// TestTranscriptionCallInfoBuildsEndpointAndRouteGenerically guards the two
// mistakes that would make the copied values unhelpful: hard-coding the Doubao
// module/model, and dropping the gateway's own origin or the platform prefix.
func TestTranscriptionCallInfoBuildsEndpointAndRouteGenerically(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderTranscriptionCallInfo")
	if strings.Contains(body, "doubaoime") || strings.Contains(body, "whisper-1") {
		t.Error("转写调用信息里写死了豆包平台或模型")
	}

	endpoint := poolStatsFuncBody(t, src, "transcriptionEndpoint")
	for _, want := range []string{"location.origin", `"/v1/audio/transcriptions"`} {
		if !strings.Contains(endpoint, want) {
			t.Errorf("转写地址没有从当前面板地址生成：缺 %q", want)
		}
	}
	for _, want := range []string{"caps.transcription_models", `CUR + "/" +`} {
		if !strings.Contains(body, want) {
			t.Errorf("模型名没有按当前平台生成路由：缺 %q", want)
		}
	}
}

// TestTranscriptionOnlyEmptyStateExplainsWhyThereIsNoAddButton keeps the
// account-pool empty state from telling a speech-only module that it merely
// "does not support account management" without telling the operator what to
// do instead.
func TestTranscriptionOnlyEmptyStateExplainsWhyThereIsNoAddButton(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderAccounts")
	if !strings.Contains(body, "caps.transcription") {
		t.Fatal("空态渲染没有读取转写 capability")
	}
	if !strings.Contains(body, "不需要账号") {
		t.Error("转写平台的无账号空态没有说明可以直接调用")
	}
}
