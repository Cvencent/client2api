package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 对话测试页的「语音转写」测试方式。
//
// doubaoime 这类模块只提供 POST /v1/audio/transcriptions，对话测试原来的
// /v1/chat/completions 对它必然失败。这组静态回归钉住四件事：
//   1. 录音面板的元素都在：录音按钮、录音状态、response_format、回听、
//      结果区和状态行；
//   2. 录音走浏览器 MediaRecorder，停止后立刻把这段音频以 multipart 的
//      file 字段直接打真实网关 /v1/audio/transcriptions，不经过文件；
//   3. 模式跟着模型条目的 extra.kind=transcription 走（别名看 extra.target），
//      不写死任何平台或模型名；
//   4. 切回对话模式会掐掉在录的音频，且那段音频不会再自动上传。
// ---------------------------------------------------------------------------

func TestChatViewHasMicrophoneTranscriptionPane(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="chatTurn"`,
		`id="chatTranscribe" hidden`,
		`id="chatChatOpts"`,
		`id="chatSysRow"`,
		`id="chatAccountRow"`,
		`id="chatResponseFormatRow"`,
		`id="btnChatRecord">开始录音</button>`,
		`id="chatRecordStatus" aria-live="polite"`,
		`id="chatTransAudio" controls hidden`,
		`id="chatTransOut" aria-live="polite"`,
		`id="chatTransDiag"`,
		`id="chatResponseFormat"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("对话测试缺少录音转写元素 %q", want)
		}
	}
	for _, format := range []string{"json", "text", "verbose_json", "srt", "vtt"} {
		if !strings.Contains(src, `<option value="`+format+`"`) {
			t.Errorf("response_format 缺少选项 %q", format)
		}
	}
}

func TestChatTranscriptionRecordsMicrophoneAndPostsMultipart(t *testing.T) {
	src := poolStatsUISource(t)

	start := poolStatsFuncBody(t, src, "chatRecordStart")
	for _, want := range []string{
		"navigator.mediaDevices.getUserMedia({ audio: true })",
		"new MediaRecorder(",
		"rec.ondataavailable",
		"rec.onstop",
		"TRANS.model",
	} {
		if !strings.Contains(start, want) {
			t.Errorf("录音启动缺少 %q", want)
		}
	}

	// 容器按浏览器能力挑：Chrome/Edge 出 WebM-Opus，Safari 只出 MP4。
	mime := poolStatsFuncBody(t, src, "chatRecordMime")
	for _, want := range []string{
		"MediaRecorder.isTypeSupported",
		`"audio/webm;codecs=opus"`,
		`"audio/webm"`,
		`"audio/mp4"`,
	} {
		if !strings.Contains(mime, want) {
			t.Errorf("录音容器选择缺少 %q", want)
		}
	}

	stop := poolStatsFuncBody(t, src, "chatRecordStop")
	for _, want := range []string{"rec.stop()", "getTracks()"} {
		if !strings.Contains(stop, want) {
			t.Errorf("停止录音缺少 %q", want)
		}
	}

	// 停止后必须直接上传：面板里没有落盘、没有"选文件"这一步。
	finish := poolStatsFuncBody(t, src, "chatRecordFinish")
	if !strings.Contains(finish, "chatTranscribe(") {
		t.Error("停止录音后没有自动发起转写")
	}

	upload := poolStatsFuncBody(t, src, "chatTranscribe")
	for _, want := range []string{
		"new FormData()",
		`fd.append("file"`,
		`fd.append("model"`,
		`fd.append("response_format"`,
		`"/v1/audio/transcriptions"`,
		`"Bearer " + KEY`,
		"res.status",
	} {
		if !strings.Contains(upload, want) {
			t.Errorf("转写上传缺少 %q", want)
		}
	}
	if strings.Contains(upload, "chat/completions") {
		t.Error("转写路径打到了 /v1/chat/completions")
	}

	draw := poolStatsFuncBody(t, src, "chatTransDraw")
	for _, want := range []string{"JSON.stringify(", "textContent", `"HTTP "`} {
		if !strings.Contains(draw, want) {
			t.Errorf("转写结果没有原样渲染：缺 %q", want)
		}
	}
}

func TestChatTranscriptionModeFollowsModelKind(t *testing.T) {
	src := poolStatsUISource(t)

	kind := poolStatsFuncBody(t, src, "chatModelKind")
	for _, want := range []string{"extra.kind", "extra.target"} {
		if !strings.Contains(kind, want) {
			t.Errorf("模式识别缺少 %q", want)
		}
	}
	for _, banned := range []string{"doubaoime", "whisper-1", "doubao"} {
		if strings.Contains(kind, banned) {
			t.Errorf("模式识别写死了 %q", banned)
		}
	}
	if isTrans := poolStatsFuncBody(t, src, "chatIsTranscription"); !strings.Contains(isTrans, "transcription") {
		t.Error("没有把 transcription 这一 kind 判定出来")
	}

	sync := poolStatsFuncBody(t, src, "chatModeSync")
	for _, want := range []string{
		`$("#chatTurn").hidden`,
		`$("#chatTranscribe").hidden`,
		`$("#chatChatOpts").hidden`,
		`$("#chatSysRow").hidden`,
		`$("#chatAccountRow").hidden`,
		`$("#chatResponseFormatRow").hidden`,
		`$("#chatNote").textContent`,
		"/v1/audio/transcriptions",
		"/v1/chat/completions",
		"chatRecordStop()",
		"TRANS.drop",
	} {
		if !strings.Contains(sync, want) {
			t.Errorf("模式切换缺少 %q", want)
		}
	}

	// 用户换模型必须立即重新判定模式，而不是等下一次刷新。
	line := ""
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, `$("#chatModel").addEventListener("change"`) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("没有找到模型下拉的 change 监听")
	}
	if !strings.Contains(line, "chatModeSync()") {
		t.Error("模型下拉的 change 监听没有重新判定测试模式")
	}
}
